package main

import (
	"context"
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
	"github.com/killabayte/niten/internal/probe"
	"github.com/killabayte/niten/internal/store"
)

// doctorLive runs the P0a live profile probe: one executor and one reviewer
// invocation with the exact profiles of a run, judged by tool events and host
// observations, and saves the certificate runs require.
func doctorLive(configPath string, stdout, stderr io.Writer) contract.ExitCode {
	explicit := configPath != ""
	path := configPath
	if !explicit {
		var err error
		if path, err = config.DefaultPath(getenv); err != nil {
			fmt.Fprintln(stderr, "niten doctor --live:", err)
			return contract.ExitFormat
		}
	}
	loaded, err := config.Load(path, explicit, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "niten doctor --live:", err)
		return contract.ExitFormat
	}
	cfg := loaded.Config
	var bins [2]string
	for i, c := range []string{cfg.ClaudeCommand, cfg.CodexCommand} {
		p, _, _, err := resolveToolIn(c, getenv("PATH"))
		if err != nil {
			fmt.Fprintf(stderr, "niten doctor --live: %s: %v\n", c, err)
			return contract.ExitFormat
		}
		bins[i] = p
	}
	tmpl, err := engine.SettingsTemplate(cfg.ClaudeSettingsTemplate)
	if err != nil {
		fmt.Fprintln(stderr, "niten doctor --live:", err)
		return contract.ExitFormat
	}
	var argvs [][]string
	for _, c := range cfg.Policy.Commands {
		argvs = append(argvs, c.Argv)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	b, err := probe.Bind(ctx, probe.BindInput{Claude: bins[0], Codex: bins[1], Executor: cfg.Executor, Reviewer: cfg.Reviewer,
		SettingsTemplate: tmpl, StripEnv: cfg.StripEnv, Commands: argvs, MaxBudgetUSD: cfg.ClaudeMaxBudgetUSD})
	if err != nil {
		fmt.Fprintln(stderr, "niten doctor --live:", err)
		return contract.ExitFormat
	}
	st, err := store.Open(cfg.StoreDir)
	if err != nil {
		fmt.Fprintln(stderr, "niten doctor --live:", err)
		return contract.ExitFormat
	}
	id, _ := store.NewRunID(time.Now())
	probes := filepath.Join(st.Root, "probes")
	if err := os.MkdirAll(probes, 0o700); err != nil {
		fmt.Fprintln(stderr, "niten doctor --live:", err)
		return contract.ExitFormat
	}
	fmt.Fprintf(stderr, "live probe %s: at most %d invocations (claude %s, then codex %s), sequential, %s each, %s in total, no retries\n",
		id, probe.MaxInvocations, b.Claude.Version, b.Codex.Version, probe.PerInvocation, probe.TotalTime)
	fmt.Fprintf(stderr, "binding %s\n", b.Fingerprint())
	c, err := probe.Run(ctx, probe.Options{Root: filepath.Join(probes, id), Binding: b, SettingsTemplate: tmpl, StripEnv: cfg.StripEnv,
		Commands: cfg.Policy.Commands, MaxBudgetUSD: cfg.ClaudeMaxBudgetUSD, Environ: environ, Getenv: getenv, Out: stderr})
	if err != nil {
		fmt.Fprintln(stderr, "niten doctor --live: the probe could not be carried out:", err)
		return contract.ExitFormat
	}
	saved, err := probe.Save(st.Root, c)
	if err != nil {
		fmt.Fprintln(stderr, "niten doctor --live: save the certificate:", err)
		return contract.ExitFormat
	}
	for _, x := range c.Controls {
		fmt.Fprintf(stdout, "%-8s %-20s %s\n", x.Role, x.Name, x.Status)
		if x.Status != probe.Pass {
			for _, ev := range x.Evidence {
				fmt.Fprintf(stdout, "    %s\n", ev)
			}
		}
	}
	fmt.Fprintf(stdout, "result: %s (%d invocations, certificate %s, probe world %s)\n", c.Result, c.Invocations, saved, c.World)
	if c.Result != probe.Pass {
		return contract.ExitRejected
	}
	return contract.ExitOK
}

// resolveToolIn resolves a command name in the given PATH, or checks an absolute path.
func resolveToolIn(cmd, path string) (string, string, string, error) {
	if filepath.IsAbs(cmd) {
		return resolveTool(cmd)
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, cmd)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return resolveTool(p)
		}
	}
	return "", "", "", fmt.Errorf("not found in PATH")
}
