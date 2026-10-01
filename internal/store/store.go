// Package store owns Niten's run store: a private directory outside every source
// repository and model write root. This P1 slice creates runs atomically: a run is built
// in a staging directory and renamed into place, so a crash never leaves a half-prepared
// run under its final id. The lock, the event journal and recovery arrive with P2.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// Store is <root>/runs/<run-id>/ plus staging directories beside the runs.
type Store struct {
	Root string // canonical absolute path
}

// Locate returns the canonical path the store root has or would have, without creating
// anything: the longest existing prefix is resolved through symlinks and the missing
// components are appended. Callers check boundaries with it before Open writes.
func Locate(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("store %q is not an absolute path", root)
	}
	cur := filepath.Clean(root)
	var missing []string
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{r}, missing...)...), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(root), nil
		}
		missing = append([]string{filepath.Base(cur)}, missing...)
		cur = parent
	}
}

// Open creates the store root with owner-only permissions, or checks that an existing
// root is a private directory owned by the caller. A symlinked root is resolved; its
// target must be private. The runs directory inside it must be a real private directory,
// never a symlink, so no run can be placed outside the checked root.
func Open(root string) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("store %q is not an absolute path", root)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if err := privateDir(canon, false); err != nil {
		return nil, err
	}
	runs := filepath.Join(canon, "runs")
	if err := os.Mkdir(runs, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	if err := privateDir(runs, true); err != nil {
		return nil, err
	}
	return &Store{Root: canon}, nil
}

// privateDir requires p to be a directory owned by the caller with no group or other
// permissions; with noLink, p itself must not be a symlink.
func privateDir(p string, noLink bool) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		if noLink {
			return fmt.Errorf("store directory %s is a symlink; runs are only written below the store root itself", p)
		}
		if fi, err = os.Stat(p); err != nil {
			return err
		}
	}
	if !fi.IsDir() {
		return fmt.Errorf("store path %s is not a directory", p)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("store directory %s is owned by uid %d, not by the current user", p, st.Uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("store directory %s has mode %04o; it must be private to its owner (0700)", p, fi.Mode().Perm())
	}
	return nil
}

// checkRuns re-checks the runs directory right before it is written to.
func (s *Store) checkRuns() error { return privateDir(filepath.Join(s.Root, "runs"), true) }

var reRunID = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

// NewRunID returns "<UTC yyyymmdd-hhmmss>-<6 random hex>".
func NewRunID(now time.Time) (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:]), nil
}

// ValidRunID reports whether id has the run id shape; ids never contain path separators.
func ValidRunID(id string) bool { return reRunID.MatchString(id) }

// RunDir is the final directory of a run.
func (s *Store) RunDir(id string) string { return filepath.Join(s.Root, "runs", id) }

// Staging is a run being built. Nothing in it is visible under the run id until Commit.
type Staging struct {
	store *Store
	id    string
	Dir   string
	done  bool
}

// Stage creates the staging directory of a new run.
func (s *Store) Stage(id string) (*Staging, error) {
	if !ValidRunID(id) {
		return nil, fmt.Errorf("invalid run id %q", id)
	}
	if err := s.checkRuns(); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(s.RunDir(id)); err == nil {
		return nil, fmt.Errorf("run %s already exists", id)
	}
	dir := filepath.Join(s.Root, "runs", ".staging-"+id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(dir); err != nil || real != dir {
		os.Remove(dir)
		return nil, fmt.Errorf("staging directory %s does not resolve to itself", dir)
	}
	return &Staging{store: s, id: id, Dir: dir}, nil
}

// Path resolves a slash-separated path relative to the staging directory, refusing to leave it.
func (st *Staging) Path(rel string) (string, error) { return safeJoin(st.Dir, rel) }

// Write stores data at rel with the given mode: written to a temporary file, synced, then
// linked into place. An existing entry at rel is an error; run files are written once.
func (st *Staging) Write(rel string, data []byte, mode fs.FileMode) error {
	_, err := writeOnce(st.Dir, rel, data, mode)
	return err
}

// Commit publishes the staging directory under the run id. It fails if the id is taken.
func (st *Staging) Commit() (string, error) {
	if st.done {
		return "", errors.New("staging already finished")
	}
	if err := st.store.checkRuns(); err != nil {
		return "", err
	}
	final := st.store.RunDir(st.id)
	if _, err := os.Lstat(final); err == nil {
		return "", fmt.Errorf("run %s already exists", st.id)
	}
	if err := syncTree(st.Dir); err != nil {
		return "", err
	}
	if err := os.Rename(st.Dir, final); err != nil {
		return "", err
	}
	st.done = true
	return final, syncDir(filepath.Dir(final))
}

// Abort removes the staging directory. It is safe to call after Commit.
func (st *Staging) Abort() {
	if !st.done {
		os.RemoveAll(st.Dir)
		st.done = true
	}
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncTree(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return syncDir(p)
		}
		return nil
	})
}
