// Command niten executes approved Shogun plans with an executor model and an
// independent reviewer model. This build implements the P0a deliverables
// (version/help, the offline doctor, the domain contracts and the verifier
// sandbox backend), the P1 import `niten prepare`, and the P3 sequential
// engine: `niten run`, `niten status` and `niten resume`. Commands that belong
// to later stages are present so that the interface is visible, and refuse to
// run until they are implemented.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/killabayte/niten/internal/config"
	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/verify/sandbox"
)

// version is set at build time with -ldflags "-X main.version=<tag>".
var version = "0.0.0-dev"

// plannedStage names the roadmap stage that delivers each unimplemented command.
var plannedStage = map[string]string{
	"verify": "P5", "export": "P5",
}

func main() { os.Exit(int(run(os.Args[1:], os.Stdout, os.Stderr))) }

func run(args []string, stdout, stderr io.Writer) contract.ExitCode {
	if len(args) == 0 {
		usage(stderr)
		return contract.ExitFormat
	}
	switch cmd := args[0]; cmd {
	case "help", "-h", "--help":
		usage(stdout)
		return contract.ExitOK
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "niten %s %s/%s %s\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return contract.ExitOK
	case "doctor":
		return doctor(args[1:], stdout, stderr)
	case "prepare":
		return prepareCmd(args[1:], stdout, stderr)
	case "run":
		return runCmd(args[1:], stdout, stderr)
	case "resume":
		return resumeCmd(args[1:], stdout, stderr)
	case "status":
		return statusCmd(args[1:], stdout, stderr)
	default:
		if stage, ok := plannedStage[cmd]; ok {
			fmt.Fprintf(stderr, "niten %s: not implemented in this build; planned for roadmap stage %s (docs/roadmap.md)\n", cmd, stage)
			return contract.ExitFormat
		}
		fmt.Fprintf(stderr, "niten: unknown command %q\n\n", cmd)
		usage(stderr)
		return contract.ExitFormat
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: niten <command> [options]

Commands available in this build:
  prepare PLAN.md    import an approved Shogun plan into a prepared run (no model is called)
  run RUN_ID         execute a prepared run: executor, coordinator checks, independent review
  resume RUN_ID      continue a paused or waiting run; --answers, --max-invocations, --max-time
  status RUN_ID      print the run's state (no lock, no model)
  version            print the build version
  doctor             check the local environment without calling any model
  help               show this text

Commands defined by the design and not implemented yet (exit 2):
  verify, export

Exit codes: 0 ok, 1 rejected result or failed gate, 2 format/configuration/protocol
error, 3 needs input, 4 paused, 5 implemented with pending external criteria, 130 SIGINT.
`)
}

// doctor reports the offline facts the P0a gate depends on: the embedded
// contract schemas and the verifier sandbox backend. It never starts a model;
// --live is refused because live probes need their own authorization.
func doctor(args []string, stdout, stderr io.Writer) contract.ExitCode {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	live := fs.Bool("live", false, "run the separately authorized live model probes (not available in this build)")
	configPath := fs.String("config", "", "config file (default: $XDG_CONFIG_HOME/niten/config.toml or ~/.config/niten/config.toml)")
	if err := fs.Parse(args); err != nil {
		return contract.ExitFormat
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: niten doctor [--config FILE] [--live]")
		return contract.ExitFormat
	}
	if *live {
		fmt.Fprintln(stderr, "niten doctor --live: live probes are not part of this build; they require a separately authorized run (docs/roadmap.md, P0a)")
		return contract.ExitFormat
	}

	ok := true
	fmt.Fprintf(stdout, "niten %s, %s/%s, %s\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Fprintf(stdout, "contract schemas: %d embedded (%s)\n", len(contract.Schemas()), strings.Join(contract.Schemas(), ", "))
	fmt.Fprintln(stdout, "models: not checked; the offline doctor never calls Claude or Codex")
	path, explicit := *configPath, *configPath != ""
	if !explicit {
		path, _ = config.DefaultPath(getenv)
	}
	if loaded, err := config.Load(path, explicit, getenv); err != nil {
		fmt.Fprintf(stdout, "config: invalid: %v\n", err)
		ok = false
	} else {
		environment(stdout, loaded)
	}

	profileDir, err := os.MkdirTemp("", "niten-doctor-")
	if err != nil {
		fmt.Fprintf(stdout, "verifier sandbox: unavailable: %v\n", err)
		return contract.ExitRejected
	}
	defer os.RemoveAll(profileDir)
	sb, err := sandbox.New(filepath.Join(profileDir, "profiles"))
	if err != nil {
		fmt.Fprintf(stdout, "verifier sandbox: unavailable: %v\n", err)
		ok = false
	} else {
		ver, build := sb.OS()
		fmt.Fprintf(stdout, "verifier sandbox: backend %s, launcher %s, macOS %s (%s)\n", sandbox.BackendSeatbelt, sandbox.Launcher, ver, build)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := sb.SelfTest(ctx); err != nil {
			fmt.Fprintf(stdout, "verifier sandbox self-test: failed: %v\n", err)
			ok = false
		} else {
			fmt.Fprintln(stdout, "verifier sandbox self-test: ok (default-deny profile started a process)")
		}
	}
	if !ok {
		fmt.Fprintln(stdout, "result: verification cannot run on this machine; there is no unsandboxed fallback")
		return contract.ExitRejected
	}
	fmt.Fprintln(stdout, "result: ok")
	return contract.ExitOK
}
