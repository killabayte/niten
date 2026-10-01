//go:build darwin

package sandbox

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/killabayte/niten/internal/holders"
)

// detachedSource is a helper program for the cleanup regressions.
//
//	parent <argv...>     starts argv in a new session (setsid) and exits
//	setpgid              tries to become a process group leader
//	background <marker>  starts "late-write <marker>" in the same group and exits
//	late-write <marker>  sleeps one second and writes the marker
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
	case "setpgid":
		if err := syscall.Setpgid(0, 0); err != nil {
			fmt.Println(err)
			os.Exit(2)
		}
		fmt.Println("became a group leader")
	case "background":
		c := exec.Command(os.Args[0], "late-write", os.Args[2])
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

// runErr runs a command under the harness policy and returns the error as well.
func (h *harness) runErr(t *testing.T, s *Seatbelt, argv ...string) (Result, string, error) {
	t.Helper()
	var out bytes.Buffer
	res, err := s.Run(t.Context(), h.policy, Command{Argv: argv, Dir: h.src, Env: h.env, Stdout: &out, Stderr: &out, Timeout: 10 * time.Second})
	return res, out.String(), err
}

// The profile keeps every descendant in the child's process group: setsid and
// setpgid are denied, so the group kill reaches all of them.
func TestSetsidAndSetpgidAreDenied(t *testing.T) {
	s := backend(t)
	h := newHarness(t)
	bin := buildDetached(t, h)
	res, out := h.run(t, s, 10*time.Second, nil, bin, "parent", "/bin/sleep", "30")
	if res.ExitCode != 2 || !strings.Contains(out, "operation not permitted") {
		t.Fatalf("setsid was not denied: %+v %q", res, out)
	}
	res, out = h.run(t, s, 10*time.Second, nil, bin, "setpgid")
	if res.ExitCode != 2 || !strings.Contains(out, "operation not permitted") {
		t.Fatalf("setpgid was not denied: %+v %q", res, out)
	}
}

// A background child that outlives its parent stays in the group and dies with
// the run: it never writes after Run returned.
func TestOrphanInTheGroupDiesWithTheRun(t *testing.T) {
	s := backend(t)
	h := newHarness(t)
	bin := buildDetached(t, h)
	marker := filepath.Join(h.scratch, "after-return.txt")
	res, out := h.run(t, s, 10*time.Second, nil, bin, "background", marker)
	if res.ExitCode != 0 {
		t.Fatalf("background helper failed: %s", out)
	}
	child, _ := strconv.Atoi(strings.TrimSpace(out))
	if child == 0 || alive(child) {
		t.Fatalf("the orphan %q is alive after Run", out)
	}
	time.Sleep(2 * time.Second)
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("an orphan of the group wrote after Run returned: %s", b)
	}
}

// Review regression (round 3): posix_spawn's POSIX_SPAWN_SETSID leaves the
// group although setsid is denied. Such a descendant is not provably the
// attempt's, so it is not killed; the run is refused instead.
func TestPosixSpawnEscapeIsRefusedNotKilled(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler to build the posix_spawn helper")
	}
	s := backend(t)
	h := newHarness(t)
	src := filepath.Join(h.scratch, "spawn.c")
	mustWrite(t, src, `#include <spawn.h>
#include <stdio.h>
extern char **environ;
int main(void) {
  posix_spawnattr_t a; posix_spawnattr_init(&a);
  posix_spawnattr_setflags(&a, 0x0400); /* POSIX_SPAWN_SETSID */
  pid_t pid; char *argv[] = {"/bin/sleep", "30", NULL};
  int rc = posix_spawn(&pid, "/bin/sleep", NULL, &a, argv, environ);
  if (rc) { printf("posix_spawn: %d\n", rc); return 2; }
  printf("%d\n", pid);
  return 0;
}
`)
	bin := filepath.Join(h.src, "spawn")
	if out, err := exec.Command(cc, "-o", bin, src).CombinedOutput(); err != nil {
		t.Skipf("cc failed: %v %s", err, out)
	}
	res, out, err := h.runErr(t, s, bin)
	child, _ := strconv.Atoi(strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]))
	if child > 0 {
		defer syscall.Kill(child, syscall.SIGKILL)
	}
	if child == 0 || res.ExitCode != 0 {
		t.Fatalf("helper: %+v %q", res, out)
	}
	if !errors.Is(err, ErrUnavailable) || !slices.Contains(res.ForeignPIDs, child) {
		t.Fatalf("an escaped descendant holding the roots must refuse the run: err=%v %+v", err, res)
	}
	if !alive(child) {
		t.Fatal("a process outside the group was killed")
	}
}

// Seal renames both roots, the outputs stay readable at the new paths, and the
// old policy can no longer be used.
func TestSealRetiresRoots(t *testing.T) {
	s := backend(t)
	h := newHarness(t)
	out := filepath.Join(h.scratch, "out.txt")
	if res, o := h.run(t, s, 10*time.Second, nil, "/bin/sh", "-c", "echo result > "+out); res.ExitCode != 0 {
		t.Fatalf("setup: %s", o)
	}
	sealed, err := s.Seal(h.policy)
	if err != nil || len(sealed.Foreign) != 0 {
		t.Fatalf("seal: %v %+v", err, sealed)
	}
	for _, p := range []string{h.src, h.scratch} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("old root %s still present after seal: %v", p, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(sealed.ScratchRoot, "out.txt")); err != nil || strings.TrimSpace(string(b)) != "result" {
		t.Fatalf("output in the sealed scratch: %q %v", b, err)
	}
	if _, err := s.Run(t.Context(), h.policy, Command{Argv: []string{"/usr/bin/true"}, Dir: h.src, Env: h.env, Timeout: time.Second}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("a policy whose roots were sealed must be unusable, got %v", err)
	}
}

// startObserver starts /bin/sleep with its cwd in dir, outside any sandbox.
func startObserver(t *testing.T, dir string) (pid int, exited <-chan error) {
	t.Helper()
	observer := exec.Command("/bin/sleep", "30")
	observer.Dir = dir
	if err := observer.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- observer.Wait() }()
	t.Cleanup(func() { observer.Process.Kill() })
	return observer.Process.Pid, done
}

func notKilled(t *testing.T, pid int, exited <-chan error) {
	t.Helper()
	select {
	case err := <-exited:
		t.Fatalf("the foreign holder %d was killed: %v", pid, err)
	default:
	}
}

// Review regression (round 2): a process that already held the roots when the
// run started (a user's shell or editor, a monitor) is never killed. The run
// and Seal are refused instead, and the holder is reported.
func TestForeignHolderIsRefusedNotKilled(t *testing.T) {
	s := backend(t)
	h := newHarness(t)
	pid, exited := startObserver(t, h.src)
	res, _, err := h.runErr(t, s, "/usr/bin/true")
	if !errors.Is(err, ErrUnavailable) || !slices.Contains(res.ForeignPIDs, pid) || res.Stragglers {
		t.Fatalf("run with a foreign holder: err=%v result=%+v", err, res)
	}
	sealed, err := s.Seal(h.policy)
	if !errors.Is(err, ErrUnavailable) || !slices.Contains(sealed.Foreign, pid) {
		t.Fatalf("seal with a foreign holder: err=%v sealed=%+v", err, sealed)
	}
	notKilled(t, pid, exited)
}

// Review regression (round 3): a process the coordinator starts while the
// sandboxed command runs is a sibling of the attempt, not a descendant, even
// though it is newer than the attempt and its parent is alive. It is refused,
// never killed.
func TestProcessStartedDuringRunIsRefusedNotKilled(t *testing.T) {
	s := backend(t)
	h := newHarness(t)
	ready := filepath.Join(h.scratch, "ready")
	type outcome struct {
		r Result
		e error
	}
	finished := make(chan outcome, 1)
	go func() {
		r, e := s.Run(t.Context(), h.policy, Command{Argv: []string{"/bin/sh", "-c", "echo ready > '" + ready + "'; sleep 2"}, Dir: h.src, Env: h.env, Timeout: 5 * time.Second})
		finished <- outcome{r, e}
	}()
	for i := 0; ; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if i == 250 {
			t.Fatal("the sandboxed command did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	pid, exited := startObserver(t, h.src)
	start, ppid, err := procInfo(pid)
	if err != nil || ppid != os.Getpid() {
		t.Fatalf("observer: ppid %d err %v", ppid, err)
	}
	out := <-finished
	if start.Before(out.r.Started) {
		t.Fatal("precondition: the observer must start during the run")
	}
	if !errors.Is(out.e, ErrUnavailable) || !slices.Contains(out.r.ForeignPIDs, pid) {
		t.Fatalf("a sibling holding the roots must refuse the run: err=%v %+v", out.e, out.r)
	}
	notKilled(t, pid, exited)
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
	t.Cleanup(holders.SetLsofForTest(fake))
	if _, _, err := h.runErr(t, s, "/usr/bin/true"); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "incomplete listing") {
		t.Fatalf("run with an incomplete listing: %v", err)
	}
	if _, err := s.Seal(h.policy); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("seal with an incomplete listing: %v", err)
	}
}

// Review regression (round 2): the sealed paths are outside the attempt's
// profile. A process under that profile writes into the live scratch (the
// positive control) but cannot write into the sealed one.
func TestSealedPathsAreOutsideTheProfile(t *testing.T) {
	s := backend(t)
	h := newHarness(t)
	res, out := h.run(t, s, 10*time.Second, nil, "/usr/bin/true")
	if res.ExitCode != 0 {
		t.Fatalf("setup: %s", out)
	}
	write := func(dir string) error {
		return exec.Command(Launcher, "-f", res.ProfilePath, "/bin/sh", "-c", "echo changed > '"+dir+"/after-seal.txt'").Run()
	}
	if err := write(h.scratch); err != nil {
		t.Fatalf("positive control could not write into the live scratch: %v", err)
	}
	sealed, err := s.Seal(h.policy)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(sealed.ScratchRoot, "after-seal.txt"))
	if err := write(sealed.ScratchRoot); err == nil {
		t.Fatal("a process under the attempt's profile wrote into the sealed scratch")
	}
	if _, err := os.Stat(filepath.Join(sealed.ScratchRoot, "after-seal.txt")); err == nil {
		t.Fatal("the sealed tree was modified")
	}
}
