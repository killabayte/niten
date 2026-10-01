package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/killabayte/niten/internal/config"
	"github.com/killabayte/niten/internal/plan"
	"github.com/killabayte/niten/internal/testutil"
)

func manifestRepo(t *testing.T, fixture string) plan.ManifestRepo {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testutil.FixtureDir(fixture), "plan.manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := plan.DecodeManifest(b)
	if err != nil {
		t.Fatal(err)
	}
	return m.Repos[0]
}

// The port reproduces Shogun's fingerprints: the clean fixture repository, and the same
// repository with the untracked file Shogun saw when it planned fast-dirty.
func TestInspectMatchesShogunFingerprints(t *testing.T) {
	testutil.IsolateGit(t)
	ctx := context.Background()
	repo := testutil.FixtureRepo(t, t.TempDir())
	s, err := Inspect(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	clean := manifestRepo(t, "fast-min")
	if s.Head != clean.Head || s.Fingerprint != clean.Fingerprint || s.Dirty() {
		t.Fatalf("clean: %+v vs %+v", s, clean)
	}
	os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("local change\n"), 0o644)
	s, err = Inspect(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	dirty := manifestRepo(t, "fast-dirty")
	if s.UntrackedSHA256 != dirty.UntrackedSHA256 || s.DiffSHA256 != dirty.DiffSHA256 || s.Fingerprint != dirty.Fingerprint {
		t.Fatalf("dirty: %+v vs %+v", s, dirty)
	}
	if !s.Dirty() || s.TrackedChanges || len(s.Untracked) != 1 || s.Untracked[0] != "untracked.txt" {
		t.Fatalf("dirty diagnostics %+v", s)
	}
	// Excluded own outputs and Shogun's run directories are not drift.
	os.MkdirAll(filepath.Join(repo, ".shogun", "runs", "x"), 0o755)
	os.WriteFile(filepath.Join(repo, ".shogun", "runs", "x", "state.json"), []byte("{}"), 0o644)
	canon, _ := Canonical(filepath.Join(repo, "untracked.txt"))
	s, _ = Inspect(ctx, repo, []string{canon})
	if s.Dirty() {
		t.Fatalf("excluded paths counted as changes: %+v", s)
	}
	os.WriteFile(filepath.Join(repo, "a.go"), []byte("package b\n"), 0o644)
	s, _ = Inspect(ctx, repo, []string{canon})
	if !s.TrackedChanges || s.Fingerprint == clean.Fingerprint {
		t.Fatalf("tracked change not seen: %+v", s)
	}
}

func TestInspectRejectsNonRepositories(t *testing.T) {
	testutil.IsolateGit(t)
	ctx := context.Background()
	plain := t.TempDir()
	if _, err := Inspect(ctx, plain, nil); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("plain dir: %v", err)
	}
	repo := testutil.FixtureRepo(t, t.TempDir())
	os.MkdirAll(filepath.Join(repo, "sub"), 0o755)
	if _, err := Inspect(ctx, filepath.Join(repo, "sub"), nil); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("subdirectory: %v", err)
	}
	unborn := filepath.Join(t.TempDir(), "u")
	os.MkdirAll(unborn, 0o755)
	testutil.Git(t, unborn, "init", "-q")
	if _, err := Inspect(ctx, unborn, nil); err == nil || !strings.Contains(err.Error(), "unborn") {
		t.Fatalf("unborn: %v", err)
	}
	// GIT_DIR in the environment does not redirect the inspection to another repository.
	other := testutil.FixtureRepo(t, t.TempDir())
	t.Setenv("GIT_DIR", filepath.Join(unborn, ".git"))
	if s, err := Inspect(ctx, other, nil); err != nil || s.Head != testutil.FixtureHead {
		t.Fatalf("GIT_DIR redirected the inspection: %v", err)
	}
}

func TestInventoryBase(t *testing.T) {
	testutil.IsolateGit(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "r")
	for p, c := range map[string]string{
		"AGENTS.md": "root rules\n", "docs/CLAUDE.md": "nested rules\n", ".claude/settings.json": "{}\n",
		"sub/.mcp.json": "{}\n", "main.go": "package main\n", ".gitattributes": "*.go text\n",
	} {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644)
	}
	os.Symlink("AGENTS.md", filepath.Join(dir, "CLAUDE.md"))
	testutil.Git(t, dir, "init", "-q")
	testutil.Git(t, dir, "add", "-A")
	testutil.Git(t, dir, "commit", "-qm", "base")
	head := testutil.Git(t, dir, "rev-parse", "HEAD")
	// A working-tree edit after the commit is not what the inventory reports.
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("edited in the working tree\n"), 0o644)
	cfg := config.Defaults()
	b, err := InventoryBase(ctx, dir, head, cfg.Policy.InstructionPaths, cfg.Policy.ProtectedPaths)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range b.Instructions {
		names = append(names, f.Path)
		if f.Path == "AGENTS.md" && string(f.Content) != "root rules\n" {
			t.Fatalf("instruction content came from the working tree: %q", f.Content)
		}
		if f.Path == "CLAUDE.md" && (f.SymlinkTarget != "AGENTS.md" || f.Content != nil) {
			t.Fatalf("symlinked instruction %+v", f)
		}
	}
	if strings.Join(names, ",") != "AGENTS.md,CLAUDE.md,docs/CLAUDE.md" {
		t.Fatalf("instructions %v", names)
	}
	names = nil
	for _, f := range b.Protected {
		names = append(names, f.Path)
	}
	if strings.Join(names, ",") != ".claude/settings.json,sub/.mcp.json" || len(b.Unsupported) != 0 || b.Tree == "" {
		t.Fatalf("protected %v, unsupported %v", names, b.Unsupported)
	}

	os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644)
	testutil.Git(t, dir, "add", "-A")
	testutil.Git(t, dir, "commit", "-qm", "lfs")
	sub := testutil.FixtureRepo(t, t.TempDir())
	testutil.Git(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "vendor/demo")
	testutil.Git(t, dir, "commit", "-qm", "submodule")
	b, err = InventoryBase(ctx, dir, testutil.Git(t, dir, "rev-parse", "HEAD"), cfg.Policy.InstructionPaths, cfg.Policy.ProtectedPaths)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(b.Unsupported, "; ")
	if !strings.Contains(joined, "submodule vendor/demo") || !strings.Contains(joined, "Git LFS attributes in .gitattributes") {
		t.Fatalf("unsupported %v", b.Unsupported)
	}
}

func TestWithin(t *testing.T) {
	for _, tc := range []struct {
		p, dir string
		want   bool
	}{{"/a/b", "/a", true}, {"/a", "/a", true}, {"/ab", "/a", false}, {"/a/../b", "/a", false}, {"/x", "/a", false}} {
		if got := Within(filepath.Clean(tc.p), tc.dir); got != tc.want {
			t.Errorf("Within(%q, %q) = %v", tc.p, tc.dir, got)
		}
	}
}

// partialClone makes a blob:none clone of a repository whose base commit carries an
// instruction file, so that blob is only available from the promisor remote.
func partialClone(t *testing.T) (checkout, head, blob string) {
	t.Helper()
	base := t.TempDir()
	origin := testutil.FixtureRepo(t, base)
	os.MkdirAll(filepath.Join(origin, "context"), 0o755)
	os.WriteFile(filepath.Join(origin, "context", "AGENTS.md"), []byte("Use the approved implementation contract.\n"), 0o644)
	testutil.Git(t, origin, "add", "-A")
	testutil.Git(t, origin, "commit", "-qm", "base with instructions")
	head = testutil.Git(t, origin, "rev-parse", "HEAD")
	blob = testutil.Git(t, origin, "rev-parse", "HEAD:context/AGENTS.md")
	testutil.Git(t, origin, "config", "uploadpack.allowFilter", "true")
	checkout = filepath.Join(base, "partial")
	testutil.Git(t, base, "clone", "-q", "--filter=blob:none", "--no-checkout", "file://"+origin, checkout)
	testutil.Git(t, checkout, "sparse-checkout", "init", "--cone")
	testutil.Git(t, checkout, "sparse-checkout", "set", "visible")
	testutil.Git(t, checkout, "checkout", "-q", "--force", "HEAD")
	return checkout, head, blob
}

func blobIsLocal(checkout, blob string) bool {
	return exec.Command("git", "--no-lazy-fetch", "-C", checkout, "cat-file", "-e", blob).Run() == nil
}

// Review regression: a read-only inspection of a partial clone must not fetch missing
// objects into the repository; the repository is refused as unsupported.
func TestPartialCloneIsRefusedWithoutFetching(t *testing.T) {
	testutil.IsolateGit(t)
	checkout, head, blob := partialClone(t)
	if blobIsLocal(checkout, blob) {
		t.Fatal("precondition: the instruction blob is already local")
	}
	packs := func() int { e, _ := os.ReadDir(filepath.Join(checkout, ".git", "objects", "pack")); return len(e) }
	before := packs()
	cfg := config.Defaults()
	if _, err := InventoryBase(context.Background(), checkout, head, cfg.Policy.InstructionPaths, cfg.Policy.ProtectedPaths); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("inventory of a partial clone: %v", err)
	}
	if _, err := Inspect(context.Background(), checkout, nil); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "partial clone") {
		t.Fatalf("inspection of a partial clone: %v", err)
	}
	if blobIsLocal(checkout, blob) || packs() != before {
		t.Fatalf("the inspection fetched objects into the repository: packs %d -> %d", before, packs())
	}
}

// With lazy fetching off, a missing object is an explicit error even when the partial
// clone markers were removed.
func TestMissingObjectIsAnError(t *testing.T) {
	testutil.IsolateGit(t)
	checkout, head, blob := partialClone(t)
	testutil.Git(t, checkout, "config", "--unset-all", "remote.origin.promisor")
	testutil.Git(t, checkout, "config", "--unset-all", "remote.origin.partialclonefilter")
	cfg := config.Defaults()
	_, err := InventoryBase(context.Background(), checkout, head, cfg.Policy.InstructionPaths, cfg.Policy.ProtectedPaths)
	if err == nil || !strings.Contains(err.Error(), "not readable from the local object store") || blobIsLocal(checkout, blob) {
		t.Fatalf("missing object: %v", err)
	}
}

func TestReplaceRefsAndGraftsAreUnsupported(t *testing.T) {
	testutil.IsolateGit(t)
	ctx := context.Background()
	repo := testutil.FixtureRepo(t, t.TempDir())
	os.WriteFile(filepath.Join(repo, "b.go"), []byte("package a\n"), 0o644)
	testutil.Git(t, repo, "add", "b.go")
	testutil.Git(t, repo, "commit", "-qm", "second")
	second := testutil.Git(t, repo, "rev-parse", "HEAD")
	testutil.Git(t, repo, "replace", second, testutil.FixtureHead)
	if _, err := Inspect(ctx, repo, nil); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "refs/replace/") {
		t.Fatalf("replace refs: %v", err)
	}
	testutil.Git(t, repo, "replace", "-d", second)
	os.MkdirAll(filepath.Join(repo, ".git", "info"), 0o755)
	os.WriteFile(filepath.Join(repo, ".git", "info", "grafts"), []byte(second+"\n"), 0o644)
	if _, err := Inspect(ctx, repo, nil); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "grafts") {
		t.Fatalf("grafts: %v", err)
	}
	os.Remove(filepath.Join(repo, ".git", "info", "grafts"))
	if s, err := Inspect(ctx, repo, nil); err != nil || s.Head != second {
		t.Fatalf("plain repository after cleanup: %+v %v", s, err)
	}
}
