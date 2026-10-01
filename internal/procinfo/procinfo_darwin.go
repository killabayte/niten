//go:build darwin

// Package procinfo reads a process's kernel identity on macOS: its start time
// (microsecond precision), parent and process group, from sysctl
// kern.proc.pid.<pid>. A PID alone can be reused; a PID together with its
// start time names one process for its whole life.
package procinfo

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
// The size is checked against the returned length; tests pin the offsets.
const (
	kinfoProcSize = 648
	offStartSec   = 0
	offStartUsec  = 8
	offPPID       = 560
	offPGID       = 564
	sysSysctl     = 202 // SYS___sysctl
)

// ErrNoProcess means the process does not exist (any more).
var ErrNoProcess = errors.New("no such process")

// Info is a process's kernel identity.
type Info struct {
	PID   int
	Start time.Time
	PPID  int
	PGID  int
}

// StartMicros is the start time in microseconds since the Unix epoch, the form
// stored in records.
func (i Info) StartMicros() int64 { return i.Start.UnixMicro() }

// Get reads the identity of pid.
func Get(pid int) (Info, error) {
	mib := [4]int32{1, 14, 1, int32(pid)} // CTL_KERN, KERN_PROC, KERN_PROC_PID
	buf := make([]byte, 1024)
	n := uintptr(len(buf))
	_, _, e := syscall.Syscall6(sysSysctl, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0, 0)
	if e != 0 {
		return Info{}, e
	}
	if n == 0 {
		return Info{}, ErrNoProcess
	}
	if n != kinfoProcSize {
		return Info{}, fmt.Errorf("kern.proc.pid returned %d bytes, want %d", n, kinfoProcSize)
	}
	sec := int64(binary.LittleEndian.Uint64(buf[offStartSec:]))
	usec := int64(int32(binary.LittleEndian.Uint32(buf[offStartUsec:])))
	return Info{
		PID:   pid,
		Start: time.Unix(sec, usec*int64(time.Microsecond)),
		PPID:  int(int32(binary.LittleEndian.Uint32(buf[offPPID:]))),
		PGID:  int(int32(binary.LittleEndian.Uint32(buf[offPGID:]))),
	}, nil
}

// Same reports whether pid is still the process that started at startMicros.
// A reused PID has a different start time and is not the same process.
func Same(pid int, startMicros int64) bool {
	i, err := Get(pid)
	return err == nil && i.StartMicros() == startMicros
}
