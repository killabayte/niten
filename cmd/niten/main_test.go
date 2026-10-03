package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/killabayte/niten/internal/contract"
)

func runCLI(args ...string) (contract.ExitCode, string, string) {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		if code, out, _ := runCLI(args...); code != contract.ExitOK || !strings.Contains(out, "Usage: niten") {
			t.Fatalf("%v: code %d, out %q", args, code, out)
		}
	}
	code, out, _ := runCLI("version")
	if code != contract.ExitOK || !strings.HasPrefix(out, "niten "+version+" "+runtime.GOOS) {
		t.Fatalf("version: code %d, out %q", code, out)
	}
}

func TestNoCommandAndUnknownCommand(t *testing.T) {
	if code, _, errb := runCLI(); code != contract.ExitFormat || !strings.Contains(errb, "Usage: niten") {
		t.Fatalf("no command: code %d, err %q", code, errb)
	}
	if code, _, errb := runCLI("frobnicate"); code != contract.ExitFormat || !strings.Contains(errb, "unknown command") {
		t.Fatalf("unknown: code %d, err %q", code, errb)
	}
}

func TestUnimplementedCommandsRefuseExplicitly(t *testing.T) {
	for cmd, stage := range plannedStage {
		code, out, errb := runCLI(cmd, "whatever")
		if code != contract.ExitFormat || out != "" || !strings.Contains(errb, "not implemented") || !strings.Contains(errb, stage) {
			t.Errorf("%s: code %d, out %q, err %q", cmd, code, out, errb)
		}
	}
}

func TestDoctorOffline(t *testing.T) {
	code, out, _ := runCLI("doctor")
	if !strings.Contains(out, "contract schemas: ") || !strings.Contains(out, "never calls Claude or Codex") {
		t.Fatalf("doctor output incomplete:\n%s", out)
	}
	if runtime.GOOS == "darwin" {
		if code != contract.ExitOK || !strings.Contains(out, "verifier sandbox self-test: ok") {
			t.Fatalf("doctor on macOS: code %d\n%s", code, out)
		}
	} else if code != contract.ExitRejected || !strings.Contains(out, "unavailable") {
		t.Fatalf("doctor off macOS must fail closed: code %d\n%s", code, out)
	}
}

// Without the model CLIs the live probe refuses before it creates anything.
func TestDoctorLiveNeedsTheCLIs(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "niten.toml")
	os.WriteFile(cfg, []byte("claude_command = \"niten-no-such-claude\"\nstore_dir = \""+filepath.Join(dir, "state")+"\"\n"), 0o600)
	code, out, errb := runCLI("doctor", "--live", "--config", cfg)
	if code != contract.ExitFormat || out != "" || !strings.Contains(errb, "niten-no-such-claude") {
		t.Fatalf("doctor --live: code %d, out %q, err %q", code, out, errb)
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatal("a refused live probe created the store")
	}
	if code, _, _ := runCLI("doctor", "extra"); code != contract.ExitFormat {
		t.Fatalf("doctor with a positional argument must be rejected, got %d", code)
	}
}

// The offline doctor resolves the model CLIs and Shogun but runs only their
// version and help commands, never a session.
func TestDoctorRunsOnlyVersionCommands(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := filepath.Join(dir, "fake-cli")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >> '"+log+"'\ncase \"$1\" in help) echo 'shogun verify [--require-manifest] <plan.md>';; *) echo 'fake 1.0';; esac\n"), 0o755)
	cfg := filepath.Join(dir, "niten.toml")
	os.WriteFile(cfg, []byte("claude_command = \""+script+"\"\ncodex_command = \""+script+"\"\nshogun_command = \""+script+"\"\nstore_dir = \""+filepath.Join(dir, "state")+"\"\n"), 0o600)
	_, out, _ := runCLI("doctor", "--config", cfg)
	for _, want := range []string{"config: " + cfg, "git: git version", "store: " + filepath.Join(dir, "state") + " (not created yet", "claude: " + script, "fake 1.0", "supports verify --require-manifest"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
	calls, _ := os.ReadFile(log)
	for _, c := range strings.Fields(string(calls)) {
		if c != "--version" && c != "version" && c != "help" {
			t.Fatalf("doctor ran %q:\n%s", c, calls)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatal("doctor created the store")
	}
	if code, _, _ := runCLI("doctor", "--config", filepath.Join(dir, "absent.toml")); code == contract.ExitOK {
		t.Fatal("doctor accepted a missing explicit config")
	}
}
