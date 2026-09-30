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
	"strings"
	"time"
)

// Store is <root>/runs/<run-id>/ plus staging directories beside the runs.
type Store struct {
	Root string // canonical absolute path
}

// Open creates the store root with owner-only permissions, or checks that an existing
// root is a private directory. A symlinked root is resolved; its target must be private.
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
	fi, err := os.Stat(canon)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("store %s is not a directory", canon)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("store %s has mode %04o; it must be private to its owner (0700)", canon, fi.Mode().Perm())
	}
	if err := os.MkdirAll(filepath.Join(canon, "runs"), 0o700); err != nil {
		return nil, err
	}
	return &Store{Root: canon}, nil
}

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
	if _, err := os.Lstat(s.RunDir(id)); err == nil {
		return nil, fmt.Errorf("run %s already exists", id)
	}
	dir := filepath.Join(s.Root, "runs", ".staging-"+id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	return &Staging{store: s, id: id, Dir: dir}, nil
}

// Path resolves a slash-separated path relative to the staging directory, refusing to leave it.
func (st *Staging) Path(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.Contains(rel, "\\") {
		return "", fmt.Errorf("invalid run path %q", rel)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("run path %q leaves the run directory", rel)
	}
	return filepath.Join(st.Dir, clean), nil
}

// Write stores data at rel with the given mode: written to a temporary file, synced, then
// renamed into place. An existing entry at rel is an error; run files are written once.
func (st *Staging) Write(rel string, data []byte, mode fs.FileMode) error {
	p, err := st.Path(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(p); err == nil {
		return fmt.Errorf("run file %s already exists", rel)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), p); err != nil {
		return err
	}
	return syncDir(filepath.Dir(p))
}

// Commit publishes the staging directory under the run id. It fails if the id is taken.
func (st *Staging) Commit() (string, error) {
	if st.done {
		return "", errors.New("staging already finished")
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
