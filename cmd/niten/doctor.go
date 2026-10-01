package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/killabayte/niten/internal/config"
	"github.com/killabayte/niten/internal/provider"
)

// environment reports the local tools and locations the later stages need.
// It runs only version and help commands; it never starts a model session.
func environment(w io.Writer, loaded *config.Loaded) {
	cfg := loaded.Config
	if loaded.Path != "" {
		fmt.Fprintf(w, "config: %s (sha256 %.12s)\n", loaded.Path, loaded.SHA256)
	} else {
		fmt.Fprintln(w, "config: built-in defaults (no config file)")
	}
	if out, err := tool(context.Background(), "git", "--version"); err == nil {
		fmt.Fprintf(w, "git: %s\n", out)
	} else {
		fmt.Fprintf(w, "git: unavailable: %v\n", err)
	}
	switch fi, err := os.Stat(cfg.StoreDir); {
	case os.IsNotExist(err):
		fmt.Fprintf(w, "store: %s (not created yet; prepare creates it)\n", cfg.StoreDir)
	case err != nil:
		fmt.Fprintf(w, "store: %s: %v\n", cfg.StoreDir, err)
	case fi.Mode().Perm()&0o077 != 0:
		fmt.Fprintf(w, "store: %s has mode %04o; it must be private (0700)\n", cfg.StoreDir, fi.Mode().Perm())
	default:
		fmt.Fprintf(w, "store: %s (private)\n", cfg.StoreDir)
	}
	for _, c := range []struct{ name, cmd, versionArg string }{
		{"claude", cfg.ClaudeCommand, "--version"}, {"codex", cfg.CodexCommand, "--version"}, {"shogun", cfg.ShogunCommand, "version"},
	} {
		path, real, sum, err := resolveTool(c.cmd)
		if err != nil {
			fmt.Fprintf(w, "%s: not found (%s): %v\n", c.name, c.cmd, err)
			continue
		}
		ver, verr := tool(context.Background(), real, c.versionArg)
		if verr != nil {
			ver = "version unavailable: " + verr.Error()
		}
		fmt.Fprintf(w, "%s: %s -> %s (sha256 %.12s), %s\n", c.name, path, real, sum, ver)
		if c.name == "shogun" {
			help, _ := tool(context.Background(), real, "help")
			if strings.Contains(help, "--require-manifest") {
				fmt.Fprintln(w, "shogun: supports verify --require-manifest (S0); prepare can run")
			} else {
				fmt.Fprintln(w, "shogun: lacks verify --require-manifest; prepare needs a Shogun build with the S0 manifest sidecar")
			}
		}
	}
}

func resolveTool(cmd string) (path, real, sum string, err error) {
	path = cmd
	if !strings.ContainsRune(cmd, '/') {
		if path, err = exec.LookPath(cmd); err != nil {
			return "", "", "", err
		}
	}
	if real, err = filepath.EvalSymlinks(path); err != nil {
		return "", "", "", err
	}
	b, err := os.ReadFile(real)
	if err != nil {
		return "", "", "", err
	}
	s := sha256.Sum256(b)
	return path, real, hex.EncodeToString(s[:]), nil
}

// tool runs a local command with the filtered environment and a timeout and
// returns the first line of its output.
func tool(ctx context.Context, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	env, _ := provider.FilterEnv(os.Environ(), nil)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	if bin != "git" && strings.HasSuffix(bin, "shogun") && len(args) > 0 && args[0] == "help" {
		return string(out), nil
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), nil
}
