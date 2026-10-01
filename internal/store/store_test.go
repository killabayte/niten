package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStageWriteCommit(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state", "niten"))
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(s.Root); fi.Mode().Perm() != 0o700 {
		t.Fatalf("store mode %04o", fi.Mode().Perm())
	}
	id, err := NewRunID(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	if err != nil || !ValidRunID(id) || id[:15] != "20260930-120000" {
		t.Fatalf("run id %q %v", id, err)
	}
	st, err := s.Stage(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write("inputs/plan.md", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.Write("inputs/plan.md", []byte("y"), 0o600); err == nil {
		t.Fatal("a run file was overwritten")
	}
	for _, bad := range []string{"../escape", "/abs", "a/../../b", ".."} {
		if err := st.Write(bad, []byte("x"), 0o600); err == nil {
			t.Fatalf("path %q was accepted", bad)
		}
	}
	if _, err := os.Stat(s.RunDir(id)); !os.IsNotExist(err) {
		t.Fatal("the run is visible before commit")
	}
	dir, err := st.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "inputs", "plan.md")); err != nil || string(b) != "x" {
		t.Fatalf("committed file: %q %v", b, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "inputs", "plan.md")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %04o", fi.Mode().Perm())
	}
	if _, err := s.Stage(id); err == nil {
		t.Fatal("a second run with the same id was staged")
	}
}

func TestAbortLeavesNothing(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "niten"))
	id, _ := NewRunID(time.Now())
	st, _ := s.Stage(id)
	st.Write("a", []byte("x"), 0o600)
	st.Abort()
	entries, _ := os.ReadDir(filepath.Join(s.Root, "runs"))
	if len(entries) != 0 {
		t.Fatalf("abort left %v", entries)
	}
}

func TestOpenRejectsSharedStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shared")
	os.Mkdir(root, 0o755)
	if _, err := Open(root); err == nil {
		t.Fatal("a group/world-readable store was accepted")
	}
	if _, err := Open("relative/path"); err == nil {
		t.Fatal("a relative store was accepted")
	}
}

func TestOpenRejectsSymlinkedRuns(t *testing.T) {
	base := t.TempDir()
	elsewhere := filepath.Join(base, "elsewhere")
	os.Mkdir(elsewhere, 0o700)
	root := filepath.Join(base, "niten")
	os.Mkdir(root, 0o700)
	if err := os.Symlink(elsewhere, filepath.Join(root, "runs")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("a symlinked runs directory was accepted")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("Open wrote through the symlink: %v", entries)
	}
	// A runs directory swapped for a symlink after Open is caught before staging.
	root2 := filepath.Join(base, "niten2")
	s, err := Open(root2)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(s.Root, "runs"))
	os.Symlink(elsewhere, filepath.Join(s.Root, "runs"))
	id, _ := NewRunID(time.Now())
	if _, err := s.Stage(id); err == nil {
		t.Fatal("staging followed a swapped runs symlink")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("staging wrote through the symlink: %v", entries)
	}
}

func TestLocateCreatesNothing(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	link := filepath.Join(base, "link")
	os.Symlink(filepath.Join(base, "real"), link)
	os.Mkdir(filepath.Join(base, "real"), 0o700)
	got, err := Locate(filepath.Join(link, "state", "niten"))
	if err != nil || got != filepath.Join(base, "real", "state", "niten") {
		t.Fatalf("Locate = %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(base, "real", "state")); !os.IsNotExist(err) {
		t.Fatal("Locate created a directory")
	}
	if _, err := Locate("relative"); err == nil {
		t.Fatal("a relative store was located")
	}
}
