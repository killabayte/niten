package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A command that died from a crash signal is run once more with the same
// input; a non-zero exit is an answer and is not repeated, and a second crash
// is returned.
func TestRunGitRepeatsOnlyACrashOnce(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho x >> '"+log+"'\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	calls := func() int {
		b, _ := os.ReadFile(log)
		os.Remove(log)
		return strings.Count(string(b), "x")
	}
	ctx := context.Background()
	marker := filepath.Join(dir, "crashed-once")
	once := script("once", "if [ ! -f '"+marker+"' ]; then touch '"+marker+"'; kill -SEGV $$; fi\ncat\n")
	out, _, err := runGit(ctx, func() *exec.Cmd { return exec.Command(once) }, []byte("input"))
	if err != nil || string(out) != "input" || calls() != 2 {
		t.Fatalf("a crash then an answer: %q %v", out, err)
	}
	exit := script("exit", "exit 3\n")
	if _, _, err := runGit(ctx, func() *exec.Cmd { return exec.Command(exit) }, nil); err == nil || calls() != 1 {
		t.Fatal("a non-zero exit was repeated or swallowed")
	}
	always := script("always", "kill -SEGV $$\n")
	if _, _, err := runGit(ctx, func() *exec.Cmd { return exec.Command(always) }, nil); err == nil || !crashed(err) || calls() != 2 {
		t.Fatal("a repeated crash was not returned after one repeat")
	}
	killed := script("killed", "kill -KILL $$\n")
	if _, _, err := runGit(ctx, func() *exec.Cmd { return exec.Command(killed) }, nil); err == nil || calls() != 1 {
		t.Fatal("a kill, which a cancelled context also sends, was repeated")
	}
}

// On macOS the git every command runs is the developer tools' git, not the
// /usr/bin shim that looks it up on every call.
func TestGitBinaryBypassesTheShim(t *testing.T) {
	b := GitBinary()
	if runtime.GOOS != "darwin" {
		return
	}
	if _, err := exec.LookPath("xcrun"); err != nil {
		t.Skip("no xcrun")
	}
	if b == "/usr/bin/git" || !filepath.IsAbs(b) {
		t.Fatalf("git binary %q", b)
	}
	if out, err := exec.Command(b, "--version").Output(); err != nil || !strings.HasPrefix(string(out), "git version") {
		t.Fatalf("%s --version: %q %v", b, out, err)
	}
}
