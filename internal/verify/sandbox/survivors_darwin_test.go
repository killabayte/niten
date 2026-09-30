//go:build darwin

package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// detachedSource is a helper program. "parent <argv...>" starts argv in a new
// session (setsid) and exits at once, so the child escapes the parent's process
// group while keeping the Seatbelt profile. "late-write <marker>" sleeps one
// second and then writes the marker (the review regression).
const detachedSource = `package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func main() {
	switch os.Args[1] {
	case "parent":
		c := exec.Command(os.Args[2], os.Args[3:]...)
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := c.Start(); err != nil {
			fmt.Println(err)
			os.Exit(2)
		}
		fmt.Println(c.Process.Pid)
	case "late-write":
		time.Sleep(time.Second)
		if err := os.WriteFile(os.Args[2], []byte("child ran after parent finished"), 0o600); err != nil {
			os.Exit(3)
		}
	}
}
`

func buildDetached(t *testing.T, h *harness) string {
	t.Helper()
	src := filepath.Join(h.scratch, "detached.go")
	if err := os.WriteFile(src, []byte(detachedSource), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(h.src, "detached")
	cmd := exec.Command(h.gobin, "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "GOFLAGS=", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build detached helper: %v\n%s", err, out)
	}
	return bin
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// TestDetachedDescendantIsKilledAfterRun is the review regression: a setsid
// child must not outlive Run, and Run must report that it existed.
func TestDetachedDescendantIsKilledAfterRun(t *testing.T) {
	s := backend(t)
	h := newHarness(t)
	bin := buildDetached(t, h)
	marker := filepath.Join(h.scratch, "after-return.txt")
	res, out := h.run(t, s, 10*time.Second, nil, bin, "parent", bin, "late-write", marker)
	if res.ExitCode != 0 {
		t.Fatalf("parent failed: %s", out)
	}
	if !res.Stragglers || len(res.SurvivorPIDs) == 0 {
		t.Fatalf("detached child was not reported: %+v", res)
	}
	for _, pid := range res.SurvivorPIDs {
		if alive(pid) {
			t.Fatalf("survivor %d is still alive", pid)
		}
	}
	time.Sleep(2 * time.Second)
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("descendant survived and wrote: %s", b)
	}
}

// TestSealRetiresRootsAndKillsLateHolders covers the second half of the
// defence. A system shell detached with setsid, cwd "/", holds nothing during
// Run's sweep; it opens a file in scratch only three seconds later. Seal must
// rename the roots, find the holder through the renamed path and kill it, and
// the sealed tree must not change afterwards.
func TestSealRetiresRootsAndKillsLateHolders(t *testing.T) {
	s := backend(t)
	h := newHarness(t)
	bin := buildDetached(t, h)
	held := filepath.Join(h.scratch, "held.txt")
	script := "cd /; sleep 3; exec 3>>" + held + "; echo opened >&3; sleep 6; echo late >&3"
	res, out := h.run(t, s, 10*time.Second, nil, bin, "parent", "/bin/sh", "-c", script)
	if res.ExitCode != 0 {
		t.Fatalf("parent failed: %s", out)
	}
	if res.Stragglers {
		t.Fatalf("the detached shell holds nothing yet; Run should not have seen it: %+v", res)
	}
	time.Sleep(4500 * time.Millisecond) // the shell has opened held.txt by now
	sealed, err := s.Seal(h.policy)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	for _, p := range []string{sealed.SourceRoot, sealed.ScratchRoot} {
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			t.Fatalf("sealed root missing: %s (%v)", p, err)
		}
	}
	if _, err := os.Stat(h.scratch); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("old scratch path still present after seal: %v", err)
	}
	if len(sealed.Killed) == 0 {
		t.Fatal("Seal did not find the late holder")
	}
	for _, pid := range sealed.Killed {
		if alive(pid) {
			t.Fatalf("holder %d survived Seal", pid)
		}
	}
	time.Sleep(5 * time.Second) // past the moment the holder would have appended "late"
	b, err := os.ReadFile(filepath.Join(sealed.ScratchRoot, "held.txt"))
	if err != nil {
		t.Fatalf("held file missing in sealed scratch: %v", err)
	}
	if strings.TrimSpace(string(b)) != "opened" {
		t.Fatalf("sealed tree changed after Seal: %q", b)
	}
	if _, err := s.Run(t.Context(), h.policy, Command{Argv: []string{"/usr/bin/true"}, Dir: h.src, Env: h.env, Timeout: time.Second}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("a policy whose roots were sealed must be unusable, got %v", err)
	}
}
