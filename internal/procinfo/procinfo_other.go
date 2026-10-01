//go:build !darwin

// Package procinfo reads a process's kernel identity. Only macOS is supported
// in v0.1; elsewhere every lookup fails, so nothing is ever treated as a
// known process.
package procinfo

import (
	"errors"
	"time"
)

// ErrNoProcess means the process does not exist (any more).
var ErrNoProcess = errors.New("no such process")

// ErrUnsupported is returned off macOS.
var ErrUnsupported = errors.New("procinfo: unsupported OS")

// Info is a process's kernel identity.
type Info struct {
	PID   int
	Start time.Time
	PPID  int
	PGID  int
}

// StartMicros is the start time in microseconds since the Unix epoch.
func (i Info) StartMicros() int64 { return i.Start.UnixMicro() }

// Get always fails off macOS.
func Get(pid int) (Info, error) { return Info{}, ErrUnsupported }

// Same is always false off macOS.
func Same(pid int, startMicros int64) bool { return false }

// InGroup always fails off macOS.
func InGroup(pgid int) ([]Info, error) { return nil, ErrUnsupported }
