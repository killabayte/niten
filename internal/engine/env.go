package engine

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/killabayte/niten/internal/config"
	"github.com/killabayte/niten/internal/provider"
)

//go:embed assets/claude-settings.template.json
var defaultSettingsTemplate []byte

// lookCommand resolves an executable name in PATH or checks an absolute path.
// Shell functions and aliases are never consulted.
func lookCommand(name, path string) (string, error) {
	if filepath.IsAbs(name) {
		fi, err := os.Stat(name)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("command %s is not an executable file", name)
		}
		return name, nil
	}
	if name == "" || strings.ContainsRune(name, '/') {
		return "", fmt.Errorf("command %q must be a name in PATH or an absolute path", name)
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("command %q is not in PATH", name)
}

type configCommand = config.Command

// policyCommands decodes the allowed check commands of the contract.
func (e *Engine) policyCommands() ([]config.Command, error) {
	var cmds []config.Command
	if err := json.Unmarshal(e.c.Policy.Commands, &cmds); err != nil {
		return nil, fmt.Errorf("%w: policy commands: %v", ErrIntegrity, err)
	}
	return cmds, nil
}

func (e *Engine) requiredChecks() ([]config.RequiredCheck, error) {
	var req []config.RequiredCheck
	if len(e.c.Policy.RequiredChecks) == 0 || string(e.c.Policy.RequiredChecks) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(e.c.Policy.RequiredChecks, &req); err != nil {
		return nil, fmt.Errorf("%w: required checks: %v", ErrIntegrity, err)
	}
	return req, nil
}

// toolchains resolves the read-only toolchain roots of the allowed commands for
// the verifier sandbox, and the tool version recorded with the evidence.
func (e *Engine) toolchains(ctx context.Context) ([]string, string, error) {
	cmds, err := e.policyCommands()
	if err != nil {
		return nil, "", err
	}
	seen := map[string]bool{}
	var roots, versions []string
	for _, c := range cmds {
		if len(c.Argv) == 0 || seen[c.Argv[0]] {
			continue
		}
		seen[c.Argv[0]] = true
		bin, err := lookCommand(c.Argv[0], e.o.Getenv("PATH"))
		if err != nil {
			return nil, "", fmt.Errorf("check command %s: %w", c.ID, err)
		}
		real, err := filepath.EvalSymlinks(bin)
		if err != nil {
			return nil, "", err
		}
		root := filepath.Dir(filepath.Dir(real))
		if filepath.Base(filepath.Dir(real)) != "bin" {
			return nil, "", fmt.Errorf("check command %s: %s is not inside a toolchain bin directory", c.ID, real)
		}
		if filepath.Base(real) == "go" {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			out, err := goCommand(cctx, real, "env", "GOROOT")
			if err == nil {
				if r, err := filepath.EvalSymlinks(strings.TrimSpace(out)); err == nil {
					root = r
				}
			}
			v, verr := goCommand(cctx, real, "version")
			cancel()
			if verr != nil {
				return nil, "", fmt.Errorf("check command %s: %s version: %v", c.ID, real, verr)
			}
			versions = append(versions, strings.TrimSpace(v))
		}
		if !seen["root:"+root] {
			seen["root:"+root] = true
			roots = append(roots, root)
		}
	}
	sort.Strings(versions)
	return roots, strings.Join(versions, "; "), nil
}

func goCommand(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = []string{"GOENV=off", "GOTOOLCHAIN=local", "HOME=" + os.TempDir(), "PATH=/usr/bin:/bin"}
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

// childEnv is a model process environment (see provider.ChildEnv).
func (e *Engine) childEnv(scratch string, extra ...string) (env, stripped []string, err error) {
	return provider.ChildEnv(e.o.Environ(), e.c.Policy.StripEnv, scratch, extra...)
}

// allowedBash are the executor's Bash permission prefixes (see provider.BashRules).
func (e *Engine) allowedBash() ([]string, error) {
	cmds, err := e.policyCommands()
	if err != nil {
		return nil, err
	}
	var argvs [][]string
	for _, c := range cmds {
		argvs = append(argvs, c.Argv)
	}
	return provider.BashRules(argvs), nil
}

// SettingsTemplate is the configured Claude settings template at path, or
// the built-in copy of examples/claude-settings.template.json when path is "".
// Runs and the live probe use the same bytes.
func SettingsTemplate(path string) ([]byte, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("claude settings template: %w", err)
		}
		return b, nil
	}
	return defaultSettingsTemplate, nil
}

func (e *Engine) settingsTemplate() ([]byte, error) {
	return SettingsTemplate(e.cfg.ClaudeSettingsTemplate)
}

// renderSettings writes the executor settings for one attempt into its control
// directory, outside every model write root.
func (e *Engine) renderSettings(control, scratch string, deny []string) (string, string, error) {
	tmpl, err := e.settingsTemplate()
	if err != nil {
		return "", "", err
	}
	home := e.o.Getenv("HOME")
	codexHome := e.o.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	b, err := provider.RenderSettings(tmpl, provider.SettingsPaths{
		ExecutorScratch: scratch, OriginalRepo: filepath.Clean(e.c.Repos[0].Path), Store: filepath.Join(e.o.Store.Root, "runs"),
		GitDir: e.clone.GitDir, Source: e.clone.Work, ClaudeHome: filepath.Join(home, ".claude"), CodexHome: codexHome, DenyPatterns: deny,
	})
	if err != nil {
		return "", "", err
	}
	p := filepath.Join(control, "settings.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return "", "", err
	}
	return p, digest(b), nil
}

// newDir creates a directory that must not exist yet: every attempt gets new roots.
func newDir(p string) error {
	if _, err := os.Lstat(p); err == nil {
		return fmt.Errorf("%s already exists; attempts never reuse roots", p)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.MkdirAll(p, 0o700)
}
