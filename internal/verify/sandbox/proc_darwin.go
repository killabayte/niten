//go:build darwin

package sandbox

import (
	"time"

	"github.com/killabayte/niten/internal/procinfo"
)

// errNoProcess means the process no longer exists.
var errNoProcess = procinfo.ErrNoProcess

// procInfo returns a process's kernel start time and parent PID.
func procInfo(pid int) (start time.Time, ppid int, err error) {
	i, err := procinfo.Get(pid)
	return i.Start, i.PPID, err
}

// procGroup returns a process's process group.
func procGroup(pid int) (int, error) {
	i, err := procinfo.Get(pid)
	return i.PGID, err
}
