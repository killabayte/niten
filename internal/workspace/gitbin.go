package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
)

// GitBinary is the git every command runs: the one in PATH, except that on
// macOS the /usr/bin/git shim is resolved once to the developer tools' git it
// would start (honouring xcode-select). The shim looks the tool up through
// xcrun and a cache in the shared temporary directory on every call; under
// many concurrent calls it is a needless extra process per command.
var GitBinary = sync.OnceValue(func() string {
	p, err := exec.LookPath("git")
	if err != nil {
		return "git"
	}
	if runtime.GOOS == "darwin" && p == "/usr/bin/git" {
		out, err := exec.Command("/usr/bin/xcrun", "--find", "git").Output()
		if r := strings.TrimSpace(string(out)); err == nil && filepath.IsAbs(r) && r != p {
			if fi, err := os.Stat(r); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
				return r
			}
		}
	}
	return p
})

// crashed reports whether a command died from a crash signal: it produced no
// answer at all. A non-zero exit, or the kill a cancelled context sends, is an
// answer and is never repeated.
func crashed(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return false
	}
	switch ws.Signal() {
	case syscall.SIGSEGV, syscall.SIGBUS, syscall.SIGILL, syscall.SIGABRT, syscall.SIGTRAP:
		return true
	}
	return false
}

// runGit runs one git invocation built by mk, with in (if not nil) on stdin.
// A git that crashed is run once more: reads are idempotent, and the writes the
// coordinator makes are deterministic or compare-and-swap, so a repeat after a
// write that did land fails instead of doing it twice.
func runGit(ctx context.Context, mk func() *exec.Cmd, in []byte) ([]byte, []byte, error) {
	for try := 0; ; try++ {
		cmd := mk()
		if in != nil {
			cmd.Stdin = bytes.NewReader(in)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err == nil || try > 0 || ctx.Err() != nil || !crashed(err) {
			return out, stderr.Bytes(), err
		}
	}
}
