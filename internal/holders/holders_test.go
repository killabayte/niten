package holders

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestListFindsHoldersAndFailsClosed(t *testing.T) {
	if _, err := os.Stat(lsofPath); err != nil {
		t.Skip("no lsof")
	}
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	c := exec.Command("/bin/sleep", "30")
	c.Dir = dir
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Process.Kill()
	pids, err := List([]string{dir})
	if err != nil || !slices.Contains(pids, c.Process.Pid) {
		t.Fatalf("holders %v %v (want %d)", pids, err, c.Process.Pid)
	}
	if none, err := List([]string{filepath.Join(dir, "nothing-here")}); err != nil || len(none) != 0 {
		t.Fatalf("no holders: %v %v", none, err)
	}
	fake := filepath.Join(t.TempDir(), "lsof")
	os.WriteFile(fake, []byte("#!/bin/sh\nprintf 'p1\\nn/x\\n'\nexit 1\n"), 0o700)
	defer SetLsofForTest(fake)()
	if _, err := List([]string{dir}); err == nil {
		t.Fatal("an incomplete listing was accepted")
	}
}
