//go:build darwin

package sandbox

import (
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

// kinfo_proc layout on 64-bit Darwin: extern_proc starts with the start time
// (struct timeval); eproc.e_ppid and eproc.e_pgid sit at bytes 560 and 564.
// The size is checked against the returned length; TestProcInfoLayout pins
// the offsets.
const (
	kinfoProcSize = 648
	offStartSec   = 0
	offStartUsec  = 8
	offPPID       = 560
	offPGID       = 564
	sysSysctl     = 202 // SYS___sysctl
)

// errNoProcess means the process no longer exists.
var errNoProcess = errors.New("no such process")

// procInfo returns a process's kernel start time (microsecond precision) and
// parent PID from sysctl kern.proc.pid.<pid>.
func procInfo(pid int) (start time.Time, ppid int, err error) {
	buf, err := kinfo(pid)
	if err != nil {
		return time.Time{}, 0, err
	}
	sec := int64(binary.LittleEndian.Uint64(buf[offStartSec:]))
	usec := int64(int32(binary.LittleEndian.Uint32(buf[offStartUsec:])))
	ppid = int(int32(binary.LittleEndian.Uint32(buf[offPPID:])))
	return time.Unix(sec, usec*int64(time.Microsecond)), ppid, nil
}

// procGroup returns a process's process group from kern.proc.pid.<pid>.
func procGroup(pid int) (int, error) {
	buf, err := kinfo(pid)
	if err != nil {
		return 0, err
	}
	return int(int32(binary.LittleEndian.Uint32(buf[offPGID:]))), nil
}

// kinfo reads struct kinfo_proc for pid.
func kinfo(pid int) ([]byte, error) {
	mib := [4]int32{1, 14, 1, int32(pid)} // CTL_KERN, KERN_PROC, KERN_PROC_PID
	buf := make([]byte, 1024)
	n := uintptr(len(buf))
	_, _, e := syscall.Syscall6(sysSysctl, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0, 0)
	if e != 0 {
		return nil, e
	}
	if n == 0 {
		return nil, errNoProcess
	}
	if n != kinfoProcSize {
		return nil, fmt.Errorf("kern.proc.pid returned %d bytes, want %d", n, kinfoProcSize)
	}
	return buf[:n], nil
}
