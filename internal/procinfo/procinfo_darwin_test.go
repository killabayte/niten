//go:build darwin

package procinfo

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestLayoutAndIdentity(t *testing.T) {
	before := time.Now()
	c := exec.Command("/bin/sleep", "5")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	defer c.Process.Kill()
	i, err := Get(c.Process.Pid)
	if err != nil || i.PPID != os.Getpid() || i.PGID != syscall.Getpgrp() {
		t.Fatalf("child: %+v err %v (want ppid %d pgid %d)", i, err, os.Getpid(), syscall.Getpgrp())
	}
	if i.Start.Before(before.Truncate(time.Microsecond)) || i.Start.After(after) {
		t.Fatalf("start %s outside [%s, %s]", i.Start, before, after)
	}
	if !Same(c.Process.Pid, i.StartMicros()) || Same(c.Process.Pid, i.StartMicros()+1) {
		t.Fatal("Same must match exactly the recorded start time")
	}
	leader := exec.Command("/bin/sleep", "5")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	defer leader.Process.Kill()
	if l, err := Get(leader.Process.Pid); err != nil || l.PGID != leader.Process.Pid {
		t.Fatalf("group leader %+v %v", l, err)
	}
	done := exec.Command("/usr/bin/true")
	done.Run()
	if _, err := Get(done.Process.Pid); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("exited process: %v", err)
	}
	if Same(done.Process.Pid, i.StartMicros()) {
		t.Fatal("an exited process matched")
	}
}
