//go:build darwin

package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	sealed, err := s.Seal(h.policy, res.Started)
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

// Review regression (round 2): a process the run did not start that happens to
// hold the roots (a user's shell or editor, a monitor) is never killed. The run
// and Seal are refused instead, and the holder is reported.
func TestForeignHolderIsRefusedNotKilled(t *testing.T) {
	s := backend(t)
	h := newHarness(t)
	observer := exec.Command("/bin/sleep", "30")
	observer.Dir = h.src
	if err := observer.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- observer.Wait() }()
	defer observer.Process.Kill()
	pid := observer.Process.Pid
	res, err := s.Run(t.Context(), h.policy, Command{Argv: []string{"/usr/bin/true"}, Dir: h.src, Env: h.env, Timeout: 5 * time.Second})
	if !errors.Is(err, ErrUnavailable) || !slices.Contains(res.ForeignPIDs, pid) || res.Stragglers {
		t.Fatalf("run with a foreign holder: err=%v result=%+v", err, res)
	}
	sealed, err := s.Seal(h.policy, res.Started)
	if !errors.Is(err, ErrUnavailable) || !slices.Contains(sealed.Foreign, pid) || len(sealed.Killed) != 0 {
		t.Fatalf("seal with a foreign holder: err=%v sealed=%+v", err, sealed)
	}
	select {
	case err := <-done:
		t.Fatalf("the foreign holder %d was killed: %v", pid, err)
	default:
	}
}

// Review regression (round 2): an incomplete descriptor listing may hide a
// holder, and an open descriptor keeps writing into a renamed tree, so the
// sweep fails closed instead of reporting the roots clean.
func TestIncompleteListingFailsClosed(t *testing.T) {
	s := backend(t)
	h := newHarness(t)
	fake := filepath.Join(t.TempDir(), "lsof-partial")
	mustWrite(t, fake, "#!/bin/sh\nprintf 'p1234567\\nn/usr/bin/true\\n'\nprintf 'simulated incomplete descriptor listing\\n' >&2\nexit 1\n")
	if err := os.Chmod(fake, 0o700); err != nil {
		t.Fatal(err)
	}
	old := lsofPath
	lsofPath = fake
	t.Cleanup(func() { lsofPath = old })
	res, err := s.Run(t.Context(), h.policy, Command{Argv: []string{"/usr/bin/true"}, Dir: h.src, Env: h.env, Timeout: 5 * time.Second})
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "incomplete listing") {
		t.Fatalf("run with an incomplete listing: %v", err)
	}
	if _, err := s.Seal(h.policy, res.Started); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("seal with an incomplete listing: %v", err)
	}
}

// Review regression (round 2): a detached process that holds nothing when Seal
// runs and is told the sealed path afterwards still cannot write there, while
// the same process writes into the unsealed scratch (the positive control).
func TestSealedPathsAreOutsideTheProfile(t *testing.T) {
	for _, seal := range []bool{false, true} {
		name := "unsealed control"
		if seal {
			name = "sealed"
		}
		t.Run(name, func(t *testing.T) {
			s := backend(t)
			h := newHarness(t)
			bin := buildDetached(t, h)
			control := t.TempDir()
			h.policy.ReadOnly = append(h.policy.ReadOnly, control)
			ready := filepath.Join(control, "target")
			script := "cd /; n=0; while [ ! -s '" + ready + "' ]; do n=$((n+1)); [ $n -ge 50 ] && exit 8; sleep .1; done; target=$(cat '" + ready + "'); echo changed > \"$target/after-seal.txt\""
			res, out := h.run(t, s, 5*time.Second, nil, bin, "parent", "/bin/sh", "-c", script)
			if res.ExitCode != 0 || res.Stragglers {
				t.Fatalf("setup: %+v output=%s", res, out)
			}
			target := h.scratch
			if seal {
				sealed, err := s.Seal(h.policy, res.Started)
				if err != nil {
					t.Fatal(err)
				}
				target = sealed.ScratchRoot
			}
			if err := os.WriteFile(ready, []byte(target), 0o600); err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Second)
			_, err := os.ReadFile(filepath.Join(target, "after-seal.txt"))
			if seal && err == nil {
				t.Fatal("the sealed tree was modified after Seal returned")
			}
			if !seal && err != nil {
				t.Fatalf("positive control did not write: %v", err)
			}
		})
	}
}
