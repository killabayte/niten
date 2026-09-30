//go:build darwin

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// procInfo reads kinfo_proc by fixed offsets; this pins the layout on the
// running OS: a child's parent is the test process and its start time falls
// between the moments around Start.
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
	if mine, err := owned(c.Process.Pid, before); err != nil || !mine {
		t.Fatalf("a child started after since is the attempt's: %v %v", mine, err)
	}
	if mine, err := owned(c.Process.Pid, after.Add(time.Second)); err != nil || mine {
		t.Fatalf("a process started before since is not the attempt's: %v %v", mine, err)
	}
	if mine, err := owned(os.Getpid(), time.Now()); err != nil || mine {
		t.Fatalf("the coordinator itself is not the attempt's: %v %v", mine, err)
	}
	done := exec.Command("/usr/bin/true")
	if err := done.Run(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := procInfo(done.Process.Pid); !errors.Is(err, errNoProcess) {
		t.Fatalf("an exited process: %v", err)
	}
}
