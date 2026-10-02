package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/killabayte/niten/internal/config"
	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/engine"
	"github.com/killabayte/niten/internal/store"
)

// environ is a test hook for the environment model processes inherit.
var environ = os.Environ

const (
	runUsage    = "usage: niten run RUN_ID [--config FILE] [--json]"
	statusUsage = "usage: niten status RUN_ID [--config FILE] [--json]"
	resumeUsage = "usage: niten resume RUN_ID [--answers FILE] [--max-invocations N] [--max-time DURATION] [--max-repairs N] [--config FILE] [--json]"
)

// openStore locates the store of the effective configuration.
func openStore(configPath string) (*store.Store, error) {
	explicit := configPath != ""
	path := configPath
	if !explicit {
		var err error
		if path, err = config.DefaultPath(getenv); err != nil {
			return nil, err
		}
	}
	loaded, err := config.Load(path, explicit, getenv)
	if err != nil {
		return nil, err
	}
	return store.Open(loaded.Config.StoreDir)
}

func runCmd(args []string, stdout, stderr io.Writer) contract.ExitCode {
	return drive("run", args, stdout, stderr)
}

func resumeCmd(args []string, stdout, stderr io.Writer) contract.ExitCode {
	return drive("resume", args, stdout, stderr)
}

// drive runs or resumes a run in the foreground. Progress goes to stderr; the
// result goes to stdout. SIGINT and SIGTERM stop the current model call and
// pause the run with exit 130.
func drive(cmd string, args []string, stdout, stderr io.Writer) contract.ExitCode {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "config file (default: $XDG_CONFIG_HOME/niten/config.toml or ~/.config/niten/config.toml)")
	asJSON := fs.Bool("json", false, "print the result as JSON on stdout")
	usage := runUsage
	var answers *string
	var maxInv, maxRepairs *int
	var maxTime *time.Duration
	if cmd == "resume" {
		usage = resumeUsage
		answers = fs.String("answers", "", "a JSON file of user decisions: step_continue, attestations, answers")
		maxInv = fs.Int("max-invocations", 0, "raise the invocation limit to N")
		maxTime = fs.Duration("max-time", 0, "raise the active time limit to DURATION")
		maxRepairs = fs.Int("max-repairs", 0, "raise the repair budget per step to N")
	}
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return contract.ExitFormat
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, usage)
		return contract.ExitFormat
	}
	var ro engine.ResumeOptions
	if cmd == "resume" {
		ro.MaxInvocations, ro.MaxActiveTime, ro.MaxRepairs = *maxInv, *maxTime, *maxRepairs
		if *answers != "" {
			b, err := readAnswers(*answers)
			if err != nil {
				fmt.Fprintf(stderr, "niten %s: --answers: %v\n", cmd, err)
				return contract.ExitFormat
			}
			ro.Answers = b
		}
	}
	st, err := openStore(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "niten %s: %v\n", cmd, err)
		return contract.ExitFormat
	}
	e, err := engine.Open(engine.Options{Store: st, RunID: pos[0], Out: stderr, Getenv: getenv, Environ: environ})
	if err != nil {
		var le *store.LockedError
		if errors.As(err, &le) {
			fmt.Fprintf(stderr, "niten %s: run %s is held by another coordinator (pid %d); nothing was changed\n", cmd, pos[0], le.Owner.PID)
		} else {
			fmt.Fprintf(stderr, "niten %s: %v\n", cmd, err)
		}
		return contract.ExitFormat
	}
	defer e.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var out engine.Outcome
	if cmd == "run" {
		out, err = e.Run(ctx)
	} else {
		out, err = e.Resume(ctx, ro)
	}
	if err != nil {
		fmt.Fprintf(stderr, "niten %s: %v\n", cmd, err)
	}
	report(stdout, pos[0], out, e.State(), *asJSON)
	return out.Exit
}

// readAnswers reads a regular answers file, not through a symlink.
func readAnswers(p string) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > 1<<20 {
		return nil, fmt.Errorf("%s must be a regular file of at most 1 MiB", p)
	}
	return os.ReadFile(p)
}

func report(w io.Writer, runID string, out engine.Outcome, st *engine.State, asJSON bool) {
	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{"run_id": runID, "state": out.State, "reason": out.Reason, "detail": nonNil(out.Detail),
			"exit_code": int(out.Exit), "invocations": st.Invocations, "max_invocations": st.Limits.MaxInvocations}, "", " ")
		fmt.Fprintln(w, string(b))
		return
	}
	fmt.Fprintf(w, "run %s: %s", runID, out.State)
	if out.Reason != "" {
		fmt.Fprintf(w, " (%s)", out.Reason)
	}
	fmt.Fprintln(w)
	for _, d := range out.Detail {
		fmt.Fprintf(w, "  - %s\n", d)
	}
	if n := len(st.Receipts); n > 0 {
		fmt.Fprintf(w, "receipt: %s\n", st.Receipts[n-1].Ref)
	}
}

// statusCmd prints the run's saved projection without taking the run lock and
// without calling any model.
func statusCmd(args []string, stdout, stderr io.Writer) contract.ExitCode {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "config file (default: $XDG_CONFIG_HOME/niten/config.toml or ~/.config/niten/config.toml)")
	asJSON := fs.Bool("json", false, "print the state as JSON")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return contract.ExitFormat
	}
	if len(pos) != 1 || !store.ValidRunID(pos[0]) {
		fmt.Fprintln(stderr, statusUsage)
		return contract.ExitFormat
	}
	st, err := openStore(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "niten status: %v\n", err)
		return contract.ExitFormat
	}
	p := filepath.Join(st.RunDir(pos[0]), "state.json")
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		fmt.Fprintf(stderr, "niten status: no run %s in %s\n", pos[0], st.Root)
		return contract.ExitFormat
	}
	b, err := os.ReadFile(p)
	if err != nil {
		fmt.Fprintf(stderr, "niten status: %v\n", err)
		return contract.ExitFormat
	}
	var s engine.State
	if err := json.Unmarshal(b, &s); err != nil {
		fmt.Fprintf(stderr, "niten status: state.json: %v\n", err)
		return contract.ExitFormat
	}
	if *asJSON {
		var v any
		_ = json.Unmarshal(b, &v)
		out, _ := json.MarshalIndent(v, "", " ")
		fmt.Fprintln(stdout, string(out))
		return contract.ExitOK
	}
	printStatus(stdout, &s)
	return contract.ExitOK
}

func printStatus(w io.Writer, s *engine.State) {
	fmt.Fprintf(w, "run %s: %s", s.RunID, s.State)
	if s.Reason != "" {
		fmt.Fprintf(w, " (%s)", s.Reason)
	}
	fmt.Fprintln(w)
	for _, d := range s.Detail {
		fmt.Fprintf(w, "  - %s\n", d)
	}
	for _, u := range append(append([]*engine.StepView{}, s.Steps...), s.Final) {
		if u == nil {
			continue
		}
		line := fmt.Sprintf("%-6s %s", u.ID, u.State)
		if u.AcceptedAt != "" {
			line += " at " + short(u.AcceptedAt)
		} else if u.Candidate != nil {
			line += ", candidate " + short(u.Candidate.Commit)
		}
		if u.Repairs > 0 || u.Reviews > 0 {
			line += fmt.Sprintf(" (repairs %d, reviews %d)", u.Repairs, u.Reviews)
		}
		fmt.Fprintln(w, line)
	}
	if s.Limits.MaxInvocations > 0 {
		fmt.Fprintf(w, "invocations %d of %d, active time %s of %s\n", s.Invocations, s.Limits.MaxInvocations,
			(time.Duration(s.ActiveMS) * time.Millisecond).Round(time.Second), s.Limits.MaxActiveTime)
	}
	for _, f := range s.Findings {
		fmt.Fprintf(w, "finding %s %s %s: %s\n", f.FindingID, f.Severity, f.State, f.DefectScenario)
	}
	if g := s.Gate; g != nil {
		fmt.Fprintf(w, "gate %s: %s accepted at %s; answer with a step_continue for this gate and SHA\n", g.ID, g.Unit, g.Candidate)
	}
	for _, q := range s.Questions {
		if !q.Answered {
			fmt.Fprintf(w, "question %s from the %s: %s\n", q.ID, q.From, q.Text)
		}
	}
	if n := len(s.Receipts); n > 0 {
		fmt.Fprintf(w, "receipt: %s (%s)\n", s.Receipts[n-1].Ref, s.Receipts[n-1].Status)
	}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
