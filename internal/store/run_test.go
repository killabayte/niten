package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the test binary act as a second coordinator holding a lock.
func TestMain(m *testing.M) {
	if spec := os.Getenv("NITEN_TEST_LOCK_HOLDER"); spec != "" {
		root, id, _ := strings.Cut(spec, "|")
		s, err := Open(root)
		if err != nil {
			fmt.Println("open:", err)
			os.Exit(2)
		}
		if _, _, err := s.OpenRun(id); err != nil {
			fmt.Println("lock:", err)
			os.Exit(2)
		}
		fmt.Println("locked")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func newRun(t *testing.T) (*Store, string) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "niten"))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := NewRunID(time.Now())
	st, err := s.Stage(id)
	if err != nil {
		t.Fatal(err)
	}
	st.Write("state.json", []byte(`{"state":"prepared"}`), 0o600)
	if _, err := st.Commit(); err != nil {
		t.Fatal(err)
	}
	return s, id
}

func TestLockIsExclusiveAndReleased(t *testing.T) {
	s, id := newRun(t)
	r, events, err := s.OpenRun(id)
	if err != nil || len(events) != 0 {
		t.Fatalf("open: %v %v", events, err)
	}
	var le *LockedError
	if _, _, err := s.OpenRun(id); !errors.As(err, &le) || le.Owner.PID != os.Getpid() || le.Owner.StartMicros == 0 {
		t.Fatalf("second open: %v", err)
	}
	r.Close()
	r2, _, err := s.OpenRun(id)
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	r2.Close()
}

// A coordinator that dies holding the lock leaves nothing to break: the kernel
// releases the flock. The owner record names the dead process, and nothing is
// signalled on its strength.
func TestLockOfACrashedCoordinatorIsFree(t *testing.T) {
	s, id := newRun(t)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "NITEN_TEST_LOCK_HOLDER="+s.Root+"|"+id)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "locked" {
		t.Fatalf("holder: %q", line)
	}
	var le *LockedError
	if _, _, err := s.OpenRun(id); !errors.As(err, &le) || le.Owner.PID != cmd.Process.Pid {
		t.Fatalf("while held: %v", err)
	}
	cmd.Process.Signal(syscall.SIGKILL)
	cmd.Wait()
	r, _, err := s.OpenRun(id)
	if err != nil {
		t.Fatalf("open after the holder died: %v", err)
	}
	defer r.Close()
}

func TestJournalAppendAndReplay(t *testing.T) {
	s, id := newRun(t)
	r, _, _ := s.OpenRun(id)
	for i := 1; i <= 3; i++ {
		ev, err := r.Append("tick", map[string]int{"n": i})
		if err != nil || ev.Seq != int64(i) {
			t.Fatalf("append %d: %+v %v", i, ev, err)
		}
	}
	r.Close()
	r, events, err := s.OpenRun(id)
	if err != nil || len(events) != 3 || events[2].Seq != 3 || events[0].Type != "tick" {
		t.Fatalf("replay: %+v %v", events, err)
	}
	if ev, err := r.Append("tick", nil); err != nil || ev.Seq != 4 {
		t.Fatalf("append after replay: %+v %v", ev, err)
	}
	r.Close()
}

// A crash in the middle of a write leaves an unterminated line. It is kept
// aside, cut off, and the journal continues after the last complete event.
func TestTornTailIsKeptAndCut(t *testing.T) {
	s, id := newRun(t)
	r, _, _ := s.OpenRun(id)
	r.Append("a", nil)
	r.Append("b", nil)
	r.Close()
	f, _ := os.OpenFile(filepath.Join(s.RunDir(id), journalFile), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"seq":3,"time":"2026-10-01T00:00:00Z","type":"c","da`)
	f.Close()
	r, events, err := s.OpenRun(id)
	if err != nil || len(events) != 2 {
		t.Fatalf("replay with a torn tail: %d events, %v", len(events), err)
	}
	tails, _ := filepath.Glob(filepath.Join(s.RunDir(id), tailPrefix+"*"))
	if len(tails) != 1 {
		t.Fatalf("tail not kept: %v", tails)
	}
	if b, _ := os.ReadFile(tails[0]); !strings.Contains(string(b), `"type":"c"`) {
		t.Fatalf("tail content %q", b)
	}
	if ev, err := r.Append("c", nil); err != nil || ev.Seq != 3 {
		t.Fatalf("append after the cut: %+v %v", ev, err)
	}
	r.Close()
	if _, events, err := s.OpenRun(id); err != nil || len(events) != 3 {
		t.Fatalf("final replay: %d %v", len(events), err)
	}
}

func TestCorruptJournalStopsTheRun(t *testing.T) {
	good := func(seq int) string {
		b, _ := json.Marshal(Event{Seq: int64(seq), Time: "2026-10-01T00:00:00Z", Type: "x", Data: json.RawMessage("null")})
		return string(b) + "\n"
	}
	for name, body := range map[string]string{
		"garbage in the middle":   good(1) + "not json\n" + good(3),
		"bad last complete line":  good(1) + "{\"seq\":2}\n",
		"sequence gap":            good(1) + good(3),
		"unknown field":           good(1) + `{"seq":2,"time":"t","type":"x","data":null,"forged":true}` + "\n",
		"sequence starts at zero": `{"seq":0,"time":"t","type":"x","data":null}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			s, id := newRun(t)
			os.WriteFile(filepath.Join(s.RunDir(id), journalFile), []byte(body), 0o600)
			if _, _, err := s.OpenRun(id); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("got %v", err)
			}
			// The refused open released the lock.
			if _, _, err := s.OpenRun(id); errors.Is(err, ErrLocked) {
				t.Fatal("a refused open kept the lock")
			}
		})
	}
}

// A full disk or a failing sync stops the journal for good in this process:
// the failed event is not acknowledged and nothing can be appended after it.
func TestWriteFailuresBreakTheJournal(t *testing.T) {
	for name, inject := range map[string]func(){
		"sync": func() { syncFile = func(*os.File) error { return syscall.EIO } },
		"write": func() {
			writeAll = func(f *os.File, b []byte) (int, error) { f.Write(b[:len(b)/2]); return len(b) / 2, syscall.ENOSPC }
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, id := newRun(t)
			r, _, _ := s.OpenRun(id)
			r.Append("before", nil)
			oldSync, oldWrite := syncFile, writeAll
			inject()
			_, err := r.Append("accepted", map[string]bool{"passed": true})
			syncFile, writeAll = oldSync, oldWrite
			if !errors.Is(err, ErrBroken) || r.Broken() == nil {
				t.Fatalf("failed append: %v", err)
			}
			if _, err := r.Append("after", nil); !errors.Is(err, ErrBroken) {
				t.Fatalf("append after a failure: %v", err)
			}
			r.Close()
			_, events, err := s.OpenRun(id)
			if err != nil {
				t.Fatal(err)
			}
			for _, ev := range events {
				if ev.Type == "after" {
					t.Fatal("an event was written after the journal broke")
				}
			}
		})
	}
}

func TestArtifactsAndStateUnderFaults(t *testing.T) {
	s, id := newRun(t)
	r, _, _ := s.OpenRun(id)
	defer r.Close()
	d, err := r.WriteArtifact("attempts/a1/result.json", []byte(`{"ok":true}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := r.ReadArtifact("attempts/a1/result.json", d); err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("read back: %q %v", b, err)
	}
	if _, err := r.ReadArtifact("attempts/a1/result.json", strings.Repeat("0", 64)); err == nil {
		t.Fatal("a digest mismatch was accepted")
	}
	if _, err := r.WriteArtifact("attempts/a1/result.json", []byte("x"), 0o600); err == nil {
		t.Fatal("an artifact was overwritten")
	}
	if _, err := r.WriteArtifact("../escape", []byte("x"), 0o600); err == nil {
		t.Fatal("an artifact left the run directory")
	}
	old := syncFile
	syncFile = func(*os.File) error { return syscall.ENOSPC }
	_, aerr := r.WriteArtifact("attempts/a2/result.json", []byte("x"), 0o600)
	serr := r.SaveState(map[string]string{"state": "running"})
	syncFile = old
	if aerr == nil || serr == nil {
		t.Fatalf("faults were swallowed: artifact %v state %v", aerr, serr)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "attempts", "a2", "result.json")); !os.IsNotExist(err) {
		t.Fatal("a failed artifact write left the file in place")
	}
	var st map[string]string
	if err := r.LoadState(&st); err != nil || st["state"] != "prepared" {
		t.Fatalf("state after a failed save: %v %v", st, err)
	}
	if err := r.SaveState(map[string]string{"state": "running"}); err != nil {
		t.Fatal(err)
	}
	if err := r.LoadState(&st); err != nil || st["state"] != "running" {
		t.Fatalf("state after save: %v %v", st, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(r.Dir, ".*tmp-*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
}
