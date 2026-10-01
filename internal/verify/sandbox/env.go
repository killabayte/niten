package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Scratch layout created by PrepareScratch.
const (
	scratchHome     = "home"
	scratchTmp      = "tmp"
	scratchGoCache  = "gocache"
	scratchModCache = "gomodcache"
)

// protectedEnv are variables the caller cannot override: they pin the child to
// the scratch area and the local toolchain.
var protectedEnv = map[string]bool{
	"PATH": true, "HOME": true, "TMPDIR": true, "GOTMPDIR": true,
	"GOENV": true, "GOTOOLCHAIN": true, "GOPROXY": true, "GOSUMDB": true, "CGO_ENABLED": true,
}

// PrepareScratch creates the per-attempt directories under scratch.
func PrepareScratch(scratch string) error {
	for _, d := range []string{scratchHome, scratchTmp, scratchGoCache, scratchModCache} {
		if err := os.MkdirAll(filepath.Join(scratch, d), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// Environment builds the complete child environment for a normalized policy.
// Nothing is read from the coordinator's own environment. overrides may add
// variables or replace the Go cache locations; GOCACHE must stay under the
// scratch root and GOMODCACHE under the scratch root or a read-only root.
func Environment(p Policy, overrides map[string]string) ([]string, error) {
	env := map[string]string{
		"PATH":        "/usr/bin:/bin",
		"HOME":        filepath.Join(p.ScratchRoot, scratchHome),
		"TMPDIR":      filepath.Join(p.ScratchRoot, scratchTmp),
		"GOTMPDIR":    filepath.Join(p.ScratchRoot, scratchTmp),
		"GOCACHE":     filepath.Join(p.ScratchRoot, scratchGoCache),
		"GOMODCACHE":  filepath.Join(p.ScratchRoot, scratchModCache),
		"GOENV":       "off",
		"GOTOOLCHAIN": "local",
		"GOPROXY":     "off",
		"GOSUMDB":     "off",
		"GOFLAGS":     "",
		"CGO_ENABLED": "0",
		"LANG":        "C",
		"LC_ALL":      "C",
	}
	for k, v := range overrides {
		if k == "" || strings.ContainsAny(k, "=\x00\n") || strings.ContainsAny(v, "\x00\n") {
			return nil, fmt.Errorf("%w: malformed environment override %q", ErrCommand, k)
		}
		if protectedEnv[k] {
			return nil, fmt.Errorf("%w: environment variable %s cannot be overridden", ErrCommand, k)
		}
		env[k] = v
	}
	if !within(env["GOCACHE"], p.ScratchRoot) {
		return nil, fmt.Errorf("%w: GOCACHE %s must lie inside the scratch root", ErrCommand, env["GOCACHE"])
	}
	modOK := within(env["GOMODCACHE"], p.ScratchRoot)
	for _, ro := range p.ReadOnly {
		if env["GOMODCACHE"] == ro || within(env["GOMODCACHE"], ro) {
			modOK = true
		}
	}
	if !modOK {
		return nil, fmt.Errorf("%w: GOMODCACHE %s must lie inside the scratch root or a read-only root", ErrCommand, env["GOMODCACHE"])
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out, nil
}
