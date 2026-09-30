//go:build darwin

package sandbox

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// kinfo_proc layout on 64-bit Darwin: extern_proc starts with the start time
// (struct timeval), and eproc.e_ppid sits at byte 560. The sizes are checked
// against the returned length; TestProcInfoLayout pins them.
const (
	kinfoProcSize = 648
	offStartSec   = 0
	offStartUsec  = 8
	offPPID       = 560
	sysSysctl     = 202 // SYS___sysctl
)

// errNoProcess means the process no longer exists.
var errNoProcess = errors.New("no such process")

// procInfo returns a process's kernel start time (microsecond precision) and
// parent PID from sysctl kern.proc.pid.<pid>.
func procInfo(pid int) (start time.Time, ppid int, err error) {
	mib := [4]int32{1, 14, 1, int32(pid)} // CTL_KERN, KERN_PROC, KERN_PROC_PID
	buf := make([]byte, 1024)
	n := uintptr(len(buf))
	_, _, e := syscall.Syscall6(sysSysctl, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0, 0)
	if e != 0 {
		return time.Time{}, 0, e
	}
	if n == 0 {
		return time.Time{}, 0, errNoProcess
	}
	if n != kinfoProcSize {
		return time.Time{}, 0, fmt.Errorf("kern.proc.pid returned %d bytes, want %d", n, kinfoProcSize)
	}
	sec := int64(binary.LittleEndian.Uint64(buf[offStartSec:]))
	usec := int64(int32(binary.LittleEndian.Uint32(buf[offStartUsec:])))
	ppid = int(int32(binary.LittleEndian.Uint32(buf[offPPID:])))
	return time.Unix(sec, usec*int64(time.Microsecond)), ppid, nil
}

// owned decides whether pid can be a descendant of an attempt that started at
// since. A process that started before since cannot be. Otherwise its parent
// chain is walked: reaching the coordinator itself or launchd (an orphan that
// left the group with setsid) makes it the attempt's; reaching any other
// process that already existed at since makes it unrelated. Unknown means the
// chain could not be read; the caller refuses rather than kills.
func owned(pid int, since time.Time) (mine bool, err error) {
	since = since.Truncate(time.Microsecond)
	self := os.Getpid()
	cur := pid
	for depth := 0; depth < 64; depth++ {
		start, ppid, err := procInfo(cur)
		if err != nil {
			return false, err
		}
		if start.Before(since) {
			return false, nil
		}
		switch ppid {
		case self, 1:
			return true, nil
		case 0:
			return false, nil
		}
		cur = ppid
	}
	return false, fmt.Errorf("process %d: parent chain too deep", pid)
}
