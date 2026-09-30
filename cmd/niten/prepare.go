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
	"strings"

	"github.com/killabayte/niten/internal/config"
	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/prepare"
)

// pairs collects repeatable ID=PATH flags.
type pairs map[string]string

func (p pairs) String() string { return fmt.Sprint(map[string]string(p)) }

func (p pairs) Set(v string) error {
	id, path, ok := strings.Cut(v, "=")
	if !ok || id == "" || path == "" {
		return fmt.Errorf("want ID=PATH, got %q", v)
	}
	if _, dup := p[id]; dup {
		return fmt.Errorf("%s is given twice", id)
	}
	p[id] = path
	return nil
}

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

// getenv is a test hook.
var getenv = os.Getenv

const prepareUsage = `usage: niten prepare PLAN.md [--repo ID=PATH] [--input ID=PATH]... [--human ID]...
                    [--gate-per-step] [--shogun-run RUN_DIR] [--config FILE] [--json]`

func prepareCmd(args []string, stdout, stderr io.Writer) contract.ExitCode {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repos, inputs := pairs{}, pairs{}
	var human list
	fs.Var(repos, "repo", "bind a plan repository id to a local checkout (default: the manifest locator)")
	fs.Var(inputs, "input", "supply a planning input by id; its bytes must match the recorded sha256 (repeatable)")
	fs.Var(&human, "human", "assign a criterion (R-001.C1) or verification (S-001/V-001) to a human (repeatable)")
	gate := fs.Bool("gate-per-step", false, "pause for a user decision after every accepted step")
	shogunRun := fs.String("shogun-run", "", "legacy Shogun run directory whose manifest.json replaces a missing sidecar")
	configPath := fs.String("config", "", "config file (default: $XDG_CONFIG_HOME/niten/config.toml or ~/.config/niten/config.toml)")
	asJSON := fs.Bool("json", false, "print the result as JSON on stdout")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return contract.ExitFormat
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, prepareUsage)
		return contract.ExitFormat
	}
	explicit := *configPath != ""
	path := *configPath
	if !explicit {
		if path, err = config.DefaultPath(getenv); err != nil {
			fmt.Fprintln(stderr, "niten prepare:", err)
			return contract.ExitFormat
		}
	}
	loaded, err := config.Load(path, explicit, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "niten prepare:", err)
		return contract.ExitFormat
	}
	opts := prepare.Options{PlanPath: pos[0], Repos: repos, Inputs: inputs, Human: human, ShogunRun: *shogunRun, Config: loaded, Version: version}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "gate-per-step" {
			opts.GatePerStep = gate
		}
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := prepare.Prepare(ctx, opts)
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "niten prepare: interrupted; no run was created")
		return contract.ExitInterrupted
	}
	var f *prepare.Failure
	if err != nil && !errors.As(err, &f) {
		f = &prepare.Failure{Exit: contract.ExitFormat, Reason: "internal_error", Details: []string{err.Error()}}
	}
	if *asJSON {
		out := map[string]any{"status": string(contract.RunPrepared)}
		if f != nil {
			out = map[string]any{"status": "refused", "reason": f.Reason, "details": f.Details, "exit_code": int(f.Exit)}
			if f.Exit == contract.ExitNeedsInput {
				out["status"] = string(contract.RunNeedsInput)
			}
		} else {
			out["run_id"], out["run_dir"], out["notes"] = res.RunID, res.RunDir, nonNil(res.Notes)
			out["plan_id"], out["plan_digest"], out["semantics_digest"] = res.Contract.Plan.PlanID, res.Contract.PlanDigest, res.Contract.SemanticsDigest
		}
		b, _ := json.MarshalIndent(out, "", " ")
		fmt.Fprintln(stdout, string(b))
	}
	if f != nil {
		label := "refused"
		if f.Exit == contract.ExitNeedsInput {
			label = "needs input"
		}
		fmt.Fprintf(stderr, "niten prepare: %s (%s)\n", label, f.Reason)
		for _, d := range f.Details {
			fmt.Fprintf(stderr, "  - %s\n", d)
		}
		fmt.Fprintln(stderr, "no run was created")
		return f.Exit
	}
	if !*asJSON {
		summary(stdout, res)
	}
	return contract.ExitOK
}

func summary(w io.Writer, res *prepare.Result) {
	c := res.Contract
	mandatory, humanOwned := 0, 0
	for _, cr := range c.Criteria {
		if cr.Mandatory {
			mandatory++
		}
		if cr.Owner == string(contract.OwnerHuman) {
			humanOwned++
		}
	}
	r := c.Repos[0]
	fmt.Fprintf(w, "prepared run %s\n", res.RunID)
	fmt.Fprintf(w, "  plan      %s revision %d, approved body %.12s\n", c.Plan.PlanID, c.Plan.Revision, c.Plan.BodySHA256)
	fmt.Fprintf(w, "  verified  shogun verify --require-manifest: %s (%s, manifest from the %s)\n", c.ShogunVerify.Result, c.ShogunVerify.Version, strings.ReplaceAll(c.Plan.ManifestSource, "_", " "))
	fmt.Fprintf(w, "  repo      %s = %s at %.12s, clean, fingerprint matches the approved base\n", r.ID, r.Path, r.BaseCommit)
	fmt.Fprintf(w, "  steps     %d, in order %s\n", len(c.Document.Steps), strings.Join(c.Order, " → "))
	fmt.Fprintf(w, "  criteria  %d (%d mandatory), %d owned by a human; %d end-to-end\n", len(c.Criteria), mandatory, humanOwned, len(c.Document.FinalCriteria))
	fmt.Fprintf(w, "  checks    %d planned verifications, none with a check_spec yet\n", len(c.Checks))
	fmt.Fprintf(w, "  inputs    %d carried, %d planning-only\n", len(c.Inputs.Execution), len(c.Inputs.Planning))
	fmt.Fprintf(w, "  gate      per step: %v\n", c.GatePerStep)
	fmt.Fprintf(w, "  store     %s\n", res.RunDir)
	for _, n := range res.Notes {
		fmt.Fprintf(w, "  note      %s\n", n)
	}
	fmt.Fprintf(w, "next: niten run %s (planned for roadmap stage P3; not in this build)\n", res.RunID)
}

// parseInterspersed allows flags after positional arguments; "--" ends flag parsing.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if len(args) > len(rest) && args[len(args)-len(rest)-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
