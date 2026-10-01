// Package provider runs the model CLIs. The supervisor starts one CLI process
// as the leader of a new process group, drains both streams concurrently into
// exclusive files with size limits, enforces cancellation and the deadline on
// the whole group, and reports the process's kernel identity before it waits,
// so a crashed coordinator's successor can tell the same process from a
// reused PID. The adapters turn a finished attempt into a validated result or
// a classified failure; a missing or broken stream is never a success.
package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/killabayte/niten/internal/procinfo"
)

// Stream limits. A larger event or stream kills the attempt.
const (
	DefaultMaxEventBytes  = 16 << 20
	DefaultMaxStreamBytes = 64 << 20
	DefaultGrace          = 5 * time.Second
)

// Identity names one process for its whole life: its PID, its kernel start
// time and its process group (the PID itself, as the leader).
type Identity struct {
	PID         int   `json:"pid"`
	StartMicros int64 `json:"start_us"`
	PGID        int   `json:"pgid"`
}

// Spec is one CLI invocation.
type Spec struct {
	Bin        string
	Args       []string
	Env        []string // complete child environment (see FilterEnv)
	Dir        string
	Stdin      []byte // written and closed with EOF
	StdoutPath string // created exclusively; must not exist
	StderrPath string
	Deadline   time.Time // zero means only ctx bounds the attempt
	Grace      time.Duration
	// MaxEventBytes bounds one stdout line (JSONL event); MaxStreamBytes each stream.
	MaxEventBytes  int64
	MaxStreamBytes int64
	// OnStart is called with the identity right after the process started and
	// before the supervisor waits. If it fails (the identity could not be made
	// durable), the group is killed and Supervise returns the error.
	OnStart func(Identity) error
}

// Outcome is what happened to the process; the streams are in the files.
type Outcome struct {
	Identity Identity  `json:"identity"`
	Exit     int       `json:"exit"` // -1 when the process did not exit normally
	Signal   string    `json:"signal,omitempty"`
	TimedOut bool      `json:"timed_out"`
	Canceled bool      `json:"canceled"`
	Limit    string    `json:"limit,omitempty"` // a stream limit that was exceeded; the attempt was killed
	Stray    bool      `json:"stray"`           // descendants kept the streams open after the process exited
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
}

// Clean reports an attempt that exited 0 on its own, within its limits.
func (o Outcome) Clean() bool {
	return o.Exit == 0 && o.Signal == "" && !o.TimedOut && !o.Canceled && o.Limit == ""
}

// Supervise runs one attempt to completion. An error means the attempt could
// not be performed or recorded as specified (start failure, OnStart failure,
// a stream write failure); a non-clean Outcome is a failed attempt.
func Supervise(ctx context.Context, s Spec) (Outcome, error) {
	if s.Grace <= 0 {
		s.Grace = DefaultGrace
	}
	if s.MaxEventBytes <= 0 {
		s.MaxEventBytes = DefaultMaxEventBytes
	}
	if s.MaxStreamBytes <= 0 {
		s.MaxStreamBytes = DefaultMaxStreamBytes
	}
	if s.Env == nil {
		return Outcome{Exit: -1}, errors.New("the child environment must be explicit")
	}
	outF, err := os.OpenFile(s.StdoutPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Outcome{Exit: -1}, fmt.Errorf("stdout file: %w", err)
	}
	defer outF.Close()
	errF, err := os.OpenFile(s.StderrPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Outcome{Exit: -1}, fmt.Errorf("stderr file: %w", err)
	}
	defer errF.Close()

	cmd := exec.Command(s.Bin, s.Args...)
	cmd.Dir, cmd.Env = s.Dir, s.Env
	cmd.Stdin = bytes.NewReader(s.Stdin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	st := &streamState{}
	st.kill = func() { killGroup(cmd, syscall.SIGKILL) }
	cmd.Stdout = &limitWriter{f: outF, lines: true, maxLine: s.MaxEventBytes, maxTotal: s.MaxStreamBytes, name: "stdout", st: st}
	cmd.Stderr = &limitWriter{f: errF, maxTotal: s.MaxStreamBytes, name: "stderr", st: st}
	cmd.WaitDelay = time.Second

	out := Outcome{Exit: -1}
	out.Started = time.Now()
	if err := cmd.Start(); err != nil {
		return out, fmt.Errorf("start %s: %w", s.Bin, err)
	}
	pid := cmd.Process.Pid
	out.Identity = Identity{PID: pid, PGID: pid}
	if info, err := procinfo.Get(pid); err == nil {
		out.Identity.StartMicros = info.StartMicros()
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if s.OnStart != nil {
		if err := s.OnStart(out.Identity); err != nil {
			killGroup(cmd, syscall.SIGKILL)
			<-done
			reap(pid)
			return out, fmt.Errorf("record the started process: %w", err)
		}
	}
	var deadline <-chan time.Time
	if !s.Deadline.IsZero() {
		t := time.NewTimer(time.Until(s.Deadline))
		defer t.Stop()
		deadline = t.C
	}
	var werr error
	select {
	case werr = <-done:
	case <-ctx.Done():
		out.Canceled = true
		werr = stop(cmd, done, s.Grace)
	case <-deadline:
		out.TimedOut = true
		werr = stop(cmd, done, s.Grace)
	}
	out.Finished = time.Now()
	killGroup(cmd, syscall.SIGKILL) // descendants that outlived the leader
	reap(pid)
	if ps := cmd.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			out.Signal = ws.Signal().String()
		} else {
			out.Exit = ps.ExitCode()
		}
	}
	out.Stray = errors.Is(werr, exec.ErrWaitDelay)
	limit, writeErr := st.get()
	out.Limit = limit
	if writeErr != nil {
		return out, fmt.Errorf("stream write failed: %w", writeErr)
	}
	return out, nil
}

func stop(cmd *exec.Cmd, done <-chan error, grace time.Duration) error {
	killGroup(cmd, syscall.SIGTERM)
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		killGroup(cmd, syscall.SIGKILL)
		return <-done
	}
}

func killGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, sig)
	}
}

// reap waits until the group is gone, at most a few seconds.
func reap(pgid int) {
	for deadline := time.Now().Add(3 * time.Second); syscall.Kill(-pgid, 0) == nil && time.Now().Before(deadline); {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		time.Sleep(20 * time.Millisecond)
	}
}

type streamState struct {
	mu       sync.Mutex
	limit    string
	writeErr error
	kill     func()
}

func (s *streamState) fail(limit string, err error) {
	s.mu.Lock()
	first := s.limit == "" && s.writeErr == nil
	if limit != "" && s.limit == "" {
		s.limit = limit
	}
	if err != nil && s.writeErr == nil {
		s.writeErr = err
	}
	s.mu.Unlock()
	if first {
		s.kill()
	}
}

func (s *streamState) get() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit, s.writeErr
}

// limitWriter writes through to a file and enforces the stream limit and, for
// JSONL streams, the per-event limit. After a violation it keeps draining
// without writing, so the child never blocks on a full pipe.
type limitWriter struct {
	f        *os.File
	lines    bool
	maxLine  int64
	maxTotal int64
	name     string
	st       *streamState
	total    int64
	line     int64
	dead     bool
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.dead {
		return len(p), nil
	}
	if w.total+int64(len(p)) > w.maxTotal {
		w.dead = true
		w.st.fail(fmt.Sprintf("%s exceeded %d bytes", w.name, w.maxTotal), nil)
		return len(p), nil
	}
	if w.lines {
		cur := w.line
		for _, b := range p {
			if b == '\n' {
				cur = 0
				continue
			}
			cur++
			if cur > w.maxLine {
				w.dead = true
				w.st.fail(fmt.Sprintf("a %s event exceeded %d bytes", w.name, w.maxLine), nil)
				return len(p), nil
			}
		}
		w.line = cur
	}
	if _, err := w.f.Write(p); err != nil {
		w.dead = true
		w.st.fail("", err)
		return len(p), nil
	}
	w.total += int64(len(p))
	return len(p), nil
}

// ErrNotOurs means a recorded identity no longer names a live process of the
// attempt; nothing was signalled.
var ErrNotOurs = errors.New("the recorded process is gone or its PID was reused")

// GroupRemainsError reports processes left in a recorded group whose leader is
// gone. Their ownership is not provable (the group may have emptied and its
// number been reused), so they are reported and never signalled; the caller
// refuses to continue while they exist.
type GroupRemainsError struct{ PIDs []int }

func (e *GroupRemainsError) Error() string {
	return fmt.Sprintf("the recorded process group still has %d process(es) whose ownership cannot be proven: %v", len(e.PIDs), e.PIDs)
}

// TerminateRecorded stops a recorded attempt after a crash. The group is
// signalled only while its leader is alive with the recorded start time: then
// every member joined the group through that leader. A reused PID, a dead
// leader or an unreadable process table are never grounds for a signal.
func TerminateRecorded(id Identity, grace time.Duration) error {
	if id.PID <= 0 || id.PGID != id.PID || id.StartMicros == 0 {
		return ErrNotOurs
	}
	if !procinfo.Same(id.PID, id.StartMicros) {
		members, err := procinfo.InGroup(id.PGID)
		if err != nil {
			return err
		}
		var pids []int
		for _, m := range members {
			if m.PID == id.PID {
				return ErrNotOurs // a new process holds the PID and leads the group
			}
			pids = append(pids, m.PID)
		}
		if len(pids) > 0 {
			return &GroupRemainsError{PIDs: pids}
		}
		return ErrNotOurs
	}
	_ = syscall.Kill(-id.PGID, syscall.SIGTERM)
	for deadline := time.Now().Add(grace); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if left, _ := procinfo.InGroup(id.PGID); len(left) == 0 {
			return nil
		}
	}
	_ = syscall.Kill(-id.PGID, syscall.SIGKILL)
	reap(id.PGID)
	return nil
}
