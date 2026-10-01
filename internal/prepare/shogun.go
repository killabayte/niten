package prepare

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/killabayte/niten/internal/plan"
)

// verifyTimeout bounds each `shogun` invocation; verify reads three small files.
const verifyTimeout = 30 * time.Second

// Shogun verify results and their exit codes (shogun cmd/shogun/other.go at S0).
var verifyExit = map[string]int{"valid": 0, "changed": 1, "unverifiable": 2, "invalid_format": 2}

// runShogunVerify runs `shogun version` and `shogun verify --require-manifest <plan>` on the
// staged copy. It never calls a model: verify only reads the triplet. The environment is
// reduced to PATH, HOME and TMPDIR, and the working directory is the staging directory.
func runShogunVerify(ctx context.Context, command, planPath string) (*plan.VerifyRecord, error) {
	rec := &plan.VerifyRecord{Command: command}
	path := command
	if !strings.ContainsRune(command, '/') {
		p, err := exec.LookPath(command)
		if err != nil {
			return rec, fmt.Errorf("shogun executable %q not found in PATH: set shogun_command to a Shogun build with S0 (verify --require-manifest)", command)
		}
		path = p
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return rec, err
	}
	rec.Path = abs
	if rec.RealPath, err = filepath.EvalSymlinks(abs); err != nil {
		return rec, fmt.Errorf("shogun executable %s: %v", abs, err)
	}
	data, err := os.ReadFile(rec.RealPath)
	if err != nil {
		return rec, fmt.Errorf("shogun executable %s: %v", rec.RealPath, err)
	}
	rec.BinarySHA256 = plan.Digest(data)
	dir := filepath.Dir(planPath)
	out, _, code, err := runTool(ctx, rec.RealPath, dir, "version")
	if err != nil || code != 0 {
		return rec, fmt.Errorf("`shogun version` failed (exit %d): %v", code, err)
	}
	rec.Version = strings.TrimSpace(out)
	rec.Args = []string{"verify", "--require-manifest", planPath}
	out, errOut, code, err := runTool(ctx, rec.RealPath, dir, rec.Args...)
	rec.ExitCode = code
	if err != nil {
		return rec, fmt.Errorf("`shogun verify` did not complete: %v", err)
	}
	result, note, _ := strings.Cut(strings.TrimSpace(out), "\t")
	rec.Result, rec.Note = result, note
	want, known := verifyExit[result]
	if !known || want != code {
		msg := strings.TrimSpace(errOut)
		if msg == "" {
			msg = strings.TrimSpace(out)
		}
		return rec, fmt.Errorf("`shogun verify --require-manifest` gave an unrecognized answer (exit %d): %s; Shogun with the S0 manifest sidecar is required", code, msg)
	}
	return rec, nil
}

// runTool runs a trusted local tool with a reduced environment and bounded output.
func runTool(ctx context.Context, bin, dir string, args ...string) (stdout, stderr string, code int, err error) {
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	for _, k := range []string{"PATH", "HOME", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var o, e limited
	o.max, e.max = 64<<10, 64<<10
	cmd.Stdout, cmd.Stderr = &o, &e
	runErr := cmd.Run()
	var exit *exec.ExitError
	switch {
	case runErr == nil:
		return o.String(), e.String(), 0, nil
	case errors.As(runErr, &exit) && ctx.Err() == nil:
		return o.String(), e.String(), exit.ExitCode(), nil
	default:
		return o.String(), e.String(), -1, runErr
	}
}

type limited struct {
	bytes.Buffer
	max int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.max - l.Len(); room > 0 {
		if len(p) > room {
			l.Buffer.Write(p[:room])
		} else {
			l.Buffer.Write(p)
		}
	}
	return len(p), nil
}

var _ io.Writer = (*limited)(nil)
