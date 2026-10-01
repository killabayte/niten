package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/killabayte/niten/internal/procinfo"
)

// Errors of an open run.
var (
	// ErrLocked means another coordinator holds the run. The holder is
	// described, never signalled: a recorded PID may already belong to someone
	// else.
	ErrLocked = errors.New("run is held by another coordinator")
	// ErrCorrupt means a complete journal line is unreadable or out of
	// sequence. The run stops; only an unterminated last line is tolerated.
	ErrCorrupt = errors.New("run journal is corrupt")
	// ErrBroken means a journal write or sync failed. No further event is
	// accepted in this process; the caller stops without claiming anything.
	ErrBroken = errors.New("run journal write failed")
)

// Fault hooks. Tests replace them to simulate a full disk or a failing sync;
// production code never changes them.
var (
	syncFile = func(f *os.File) error { return f.Sync() }
	writeAll = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
)

const (
	journalFile = "events.jsonl"
	lockFile    = "lock"
	stateFile   = "state.json"
	tailPrefix  = "events.tail-"
)

// Owner describes the coordinator that holds a run's lock.
type Owner struct {
	PID         int    `json:"pid"`
	StartMicros int64  `json:"start_us"`
	Host        string `json:"host"`
	Since       string `json:"since"`
}

// Event is one durable journal record. Seq starts at 1 and increases by one.
type Event struct {
	Seq  int64           `json:"seq"`
	Time string          `json:"time"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Run is an open run: its lock is held for the lifetime of the value. The lock
// is an flock on the run's lock file, so the kernel releases it when the
// holder exits for any reason; a crashed coordinator never leaves a lock that
// has to be broken by guessing whether a PID is still the same process.
type Run struct {
	ID  string
	Dir string

	mu      sync.Mutex
	lock    *os.File
	journal *os.File
	seq     int64
	broken  error
	now     func() time.Time
	observe func(Event) error
}

// Observe registers f to see every event this process appends to the run,
// whoever appends it, right after the event is durable. An error of f is
// returned by that Append; the event itself stays in the journal.
func (r *Run) Observe(f func(Event) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observe = f
}

// LockedError carries the holder recorded in the lock file.
type LockedError struct{ Owner Owner }

func (e *LockedError) Error() string {
	return fmt.Sprintf("%v: pid %d on %s since %s", ErrLocked, e.Owner.PID, e.Owner.Host, e.Owner.Since)
}

func (e *LockedError) Unwrap() error { return ErrLocked }

// OpenRun locks the run and replays its journal. An unterminated last line,
// left by a crash in the middle of a write, is moved to events.tail-<n> and cut
// off; any other unreadable or out-of-sequence line is ErrCorrupt.
func (s *Store) OpenRun(id string) (*Run, []Event, error) {
	if !ValidRunID(id) {
		return nil, nil, fmt.Errorf("invalid run id %q", id)
	}
	dir := s.RunDir(id)
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, nil, err
	}
	if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("run %s is not a directory", id)
	}
	if err := privateDir(dir, true); err != nil {
		return nil, nil, err
	}
	lf, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, err
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		var owner Owner
		b, _ := os.ReadFile(lf.Name())
		_ = json.Unmarshal(b, &owner)
		lf.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil, &LockedError{Owner: owner}
		}
		return nil, nil, err
	}
	r := &Run{ID: id, Dir: dir, lock: lf, now: time.Now}
	if err := r.writeOwner(); err != nil {
		r.Close()
		return nil, nil, err
	}
	events, err := r.replay()
	if err != nil {
		r.Close()
		return nil, nil, err
	}
	jf, err := os.OpenFile(filepath.Join(dir, journalFile), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		r.Close()
		return nil, nil, err
	}
	r.journal = jf
	return r, events, nil
}

func (r *Run) writeOwner() error {
	host, _ := os.Hostname()
	o := Owner{PID: os.Getpid(), Host: host, Since: r.now().UTC().Format(time.RFC3339Nano)}
	if i, err := procinfo.Get(o.PID); err == nil {
		o.StartMicros = i.StartMicros()
	}
	b, _ := json.Marshal(o)
	if err := r.lock.Truncate(0); err != nil {
		return err
	}
	if _, err := r.lock.WriteAt(append(b, '\n'), 0); err != nil {
		return err
	}
	return syncFile(r.lock)
}

func (r *Run) replay() ([]Event, error) {
	path := filepath.Join(r.Dir, journalFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	complete := data
	if i := bytes.LastIndexByte(data, '\n'); i < len(data)-1 {
		complete = data[:i+1]
		tail := data[i+1:]
		name := tailPrefix + strconv.FormatInt(r.now().UnixNano(), 10)
		if _, err := writeOnce(r.Dir, name, tail, 0o600); err != nil {
			return nil, fmt.Errorf("keep the unterminated journal tail: %w", err)
		}
		if err := os.Truncate(path, int64(len(complete))); err != nil {
			return nil, err
		}
		if f, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
			err = syncFile(f)
			f.Close()
			if err != nil {
				return nil, err
			}
		}
	}
	var events []Event
	for n, line := range bytes.Split(bytes.TrimSuffix(complete, []byte("\n")), []byte("\n")) {
		if len(complete) == 0 {
			break
		}
		var ev Event
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ev); err != nil || ev.Type == "" {
			return nil, fmt.Errorf("%w: line %d: %v", ErrCorrupt, n+1, err)
		}
		if ev.Seq != int64(len(events))+1 {
			return nil, fmt.Errorf("%w: line %d has seq %d, want %d", ErrCorrupt, n+1, ev.Seq, len(events)+1)
		}
		events = append(events, ev)
	}
	r.seq = int64(len(events))
	return events, nil
}

// Append writes one event and syncs the journal before returning. After any
// write or sync error the run is broken for this process: the error is
// returned and every later Append refuses, so nothing can be claimed on top of
// a record whose durability is unknown.
func (r *Run) Append(typ string, data any) (Event, error) {
	ev, observe, err := r.append(typ, data)
	if err != nil || observe == nil {
		return ev, err
	}
	return ev, observe(ev)
}

func (r *Run) append(typ string, data any) (Event, func(Event) error, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.broken != nil {
		return Event{}, nil, fmt.Errorf("%w: %v", ErrBroken, r.broken)
	}
	if r.journal == nil {
		return Event{}, nil, errors.New("run is closed")
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, nil, err
	}
	ev := Event{Seq: r.seq + 1, Time: r.now().UTC().Format(time.RFC3339Nano), Type: typ, Data: raw}
	line, err := json.Marshal(ev)
	if err != nil {
		return Event{}, nil, err
	}
	if _, err := writeAll(r.journal, append(line, '\n')); err != nil {
		r.broken = err
		return Event{}, nil, fmt.Errorf("%w: %v", ErrBroken, err)
	}
	if err := syncFile(r.journal); err != nil {
		r.broken = err
		return Event{}, nil, fmt.Errorf("%w: %v", ErrBroken, err)
	}
	r.seq = ev.Seq
	return ev, r.observe, nil
}

// Broken reports the write error that stopped the journal, if any.
func (r *Run) Broken() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.broken
}

// WriteArtifact stores data at rel once (temp file, sync, link, directory
// sync) and returns its SHA-256. Artifacts are written before the event that
// references them, so a replayed event never points at missing bytes.
func (r *Run) WriteArtifact(rel string, data []byte, mode fs.FileMode) (string, error) {
	return writeOnce(r.Dir, rel, data, mode)
}

// ReadArtifact reads rel and checks it against digest when digest is not empty.
func (r *Run) ReadArtifact(rel, digest string) ([]byte, error) {
	p, err := safeJoin(r.Dir, rel)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if digest != "" && sha(b) != digest {
		return nil, fmt.Errorf("artifact %s does not match its recorded digest", rel)
	}
	return b, nil
}

// Path resolves rel inside the run directory.
func (r *Run) Path(rel string) (string, error) { return safeJoin(r.Dir, rel) }

// SaveState replaces state.json atomically: temp file, sync, rename, directory
// sync. A failure leaves the previous state intact.
func (r *Run) SaveState(v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	_, err = r.Replace(stateFile, append(b, '\n'))
	return err
}

// Projections are the run files that are replaced rather than written once:
// the state projection and the current execution report. Every other run file
// is a write-once artifact.
var projections = map[string]bool{stateFile: true, "execution.json": true, "execution.md": true}

// Replace atomically replaces one projection file (temp file, sync, rename,
// directory sync) and returns the digest of the new content. A failure leaves
// the previous content intact.
func (r *Run) Replace(rel string, data []byte) (string, error) {
	if !projections[rel] {
		return "", fmt.Errorf("run file %s is not a projection; artifacts are written once", rel)
	}
	tmp, err := os.CreateTemp(r.Dir, "."+rel+".tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := writeAll(tmp, data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if err := syncFile(tmp); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(r.Dir, rel)); err != nil {
		return "", err
	}
	return sha(data), syncDir(r.Dir)
}

// LoadState decodes state.json into v.
func (r *Run) LoadState(v any) error {
	b, err := os.ReadFile(filepath.Join(r.Dir, stateFile))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Close releases the journal and the lock.
func (r *Run) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	if r.journal != nil {
		err = r.journal.Close()
		r.journal = nil
	}
	if r.lock != nil {
		_ = syscall.Flock(int(r.lock.Fd()), syscall.LOCK_UN)
		if cerr := r.lock.Close(); err == nil {
			err = cerr
		}
		r.lock = nil
	}
	return err
}

// writeOnce stores data at rel under dir exactly once.
func writeOnce(dir, rel string, data []byte, mode fs.FileMode) (string, error) {
	p, err := safeJoin(dir, rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	if _, err := os.Lstat(p); err == nil {
		return "", fmt.Errorf("run file %s already exists", rel)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := writeAll(tmp, data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return "", err
	}
	if err := syncFile(tmp); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tmp.Name(), p); err != nil {
		return "", err
	}
	if err := syncDir(filepath.Dir(p)); err != nil {
		return "", err
	}
	return sha(data), nil
}

// safeJoin resolves a slash-separated rel inside dir, refusing to leave it.
func safeJoin(dir, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.Contains(rel, "\\") {
		return "", fmt.Errorf("invalid run path %q", rel)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("run path %q leaves the run directory", rel)
	}
	return filepath.Join(dir, clean), nil
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
