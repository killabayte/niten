//go:build darwin

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// kinfo_proc is read by fixed offsets; this pins the layout on the running OS:
// a child's parent is the test process, its start time falls between the
// moments around Start, and its process group is ours unless it was started as
// the leader of a new one.
func TestProcInfoLayout(t *testing.T) {
	before := time.Now()
	c := exec.Command("/bin/sleep", "5")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	defer c.Process.Kill()
	start, ppid, err := procInfo(c.Process.Pid)
	if err != nil || ppid != os.Getpid() {
		t.Fatalf("child: ppid %d (want %d) err %v", ppid, os.Getpid(), err)
	}
	if start.Before(before.Truncate(time.Microsecond)) || start.After(after) {
		t.Fatalf("child start %s outside [%s, %s]", start, before, after)
	}
	if pg, err := procGroup(c.Process.Pid); err != nil || pg != syscall.Getpgrp() {
		t.Fatalf("child group %d (want %d) err %v", pg, syscall.Getpgrp(), err)
	}
	leader := exec.Command("/bin/sleep", "5")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	defer leader.Process.Kill()
	if pg, err := procGroup(leader.Process.Pid); err != nil || pg != leader.Process.Pid {
		t.Fatalf("group leader: group %d (want %d) err %v", pg, leader.Process.Pid, err)
	}
	done := exec.Command("/usr/bin/true")
	if err := done.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := procGroup(done.Process.Pid); !errors.Is(err, errNoProcess) {
		t.Fatalf("an exited process: %v", err)
	}
}
