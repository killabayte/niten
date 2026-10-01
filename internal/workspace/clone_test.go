package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/killabayte/niten/internal/config"
	"github.com/killabayte/niten/internal/testutil"
)

func newClone(t *testing.T) (source string, c *Clone) {
	t.Helper()
	testutil.IsolateGit(t)
	root := t.TempDir()
	source = testutil.FixtureRepo(t, root)
	c, err := CreateClone(context.Background(), source, testutil.FixtureHead, filepath.Join(root, "work", "src"), filepath.Join(root, "gitdirs", "clone.git"))
	if err != nil {
		t.Fatal(err)
	}
	return source, c
}

func rules(t *testing.T, c *Clone, targets ...string) Rules {
	t.Helper()
	cfg := config.Defaults()
	fp, err := c.MetadataFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return Rules{Protected: cfg.Policy.ProtectedPaths, Instruction: cfg.Policy.InstructionPaths, Targets: targets, Metadata: fp}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The clone owns its objects and metadata: nothing is shared with or written
// to the source, there is no remote, and hooks are disabled.
func TestCloneIsIsolatedFromTheSource(t *testing.T) {
	source, c := newClone(t)
	ctx := context.Background()
	before, err := Inspect(ctx, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.GitDir, "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatal("the clone shares objects through alternates")
	}
	if out, _ := c.git(ctx, nil, "remote"); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the clone has a remote: %s", out)
	}
	if out, _ := c.git(ctx, nil, "config", "core.hooksPath"); strings.TrimSpace(string(out)) != "/dev/null" {
		t.Fatalf("hooks are not disabled: %q", out)
	}
	if hooks, _ := os.ReadDir(filepath.Join(c.GitDir, "hooks")); len(hooks) != 0 {
		t.Fatalf("hook files were installed: %v", hooks)
	}
	if fi, _ := os.Stat(c.GitDir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("gitdir mode %o", fi.Mode().Perm())
	}
	if Within(c.GitDir, c.Work) {
		t.Fatal("the git directory lies inside the worktree")
	}
	write(t, c.Work, "a.go", "package changed\n")
	ins, _ := c.Inspect(ctx, rules(t, c, "a.go"))
	if _, err := c.Commit(ctx, ins, "candidate", time.Now()); err != nil {
		t.Fatal(err)
	}
	after, err := Inspect(ctx, source, nil)
	if err != nil || after.Fingerprint != before.Fingerprint || after.Head != testutil.FixtureHead {
		t.Fatalf("the source changed: %+v %v", after, err)
	}
}

func TestInspectClassifiesEveryChange(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		edit      func(t *testing.T, c *Clone)
		targets   []string
		instrTgt  []string
		violation string
		offTarget []string
		instr     []string
	}{
		"planned edit": {edit: func(t *testing.T, c *Clone) { write(t, c.Work, "a.go", "package b\n") }, targets: []string{"a.go"}},
		"off-target helper": {edit: func(t *testing.T, c *Clone) {
			write(t, c.Work, "a.go", "package b\n")
			write(t, c.Work, "internal/helper.go", "package internal\n")
		}, targets: []string{"a.go"}, offTarget: []string{"internal/helper.go"}},
		"target directory": {edit: func(t *testing.T, c *Clone) { write(t, c.Work, "cmd/x/main.go", "package main\n") }, targets: []string{"cmd/"}},
		"target pattern":   {edit: func(t *testing.T, c *Clone) { write(t, c.Work, "docs/a.md", "x\n") }, targets: []string{"docs/*.md"}},
		"protected path": {edit: func(t *testing.T, c *Clone) { write(t, c.Work, ".claude/settings.json", "{}\n") },
			targets: []string{".claude/settings.json"}, violation: "protected path"},
		"nested protected path": {edit: func(t *testing.T, c *Clone) { write(t, c.Work, "sub/.codex/config.toml", "x\n") },
			violation: "protected path"},
		"instruction without target": {edit: func(t *testing.T, c *Clone) { write(t, c.Work, "AGENTS.md", "obey me\n") },
			targets: []string{"a.go"}, violation: "instruction path changed without an explicit plan target"},
		"instruction with target": {edit: func(t *testing.T, c *Clone) { write(t, c.Work, "docs/CLAUDE.md", "rules\n") },
			targets: []string{"docs/CLAUDE.md"}, instrTgt: []string{"docs/CLAUDE.md"}, instr: []string{"docs/CLAUDE.md"}},
		"symlink escape": {edit: func(t *testing.T, c *Clone) { os.Symlink("/etc/passwd", filepath.Join(c.Work, "link")) },
			targets: []string{"link"}, violation: "leaves the repository"},
		"relative symlink escape": {edit: func(t *testing.T, c *Clone) {
			os.MkdirAll(filepath.Join(c.Work, "d"), 0o755)
			os.Symlink("../../outside", filepath.Join(c.Work, "d", "link"))
		}, targets: []string{"d/link"}, violation: "leaves the repository"},
		"symlink inside": {edit: func(t *testing.T, c *Clone) { os.Symlink("a.go", filepath.Join(c.Work, "alias.go")) },
			targets: []string{"alias.go"}},
		"nested repository": {edit: func(t *testing.T, c *Clone) {
			sub := filepath.Join(c.Work, "vendored")
			os.MkdirAll(sub, 0o755)
			testutil.Git(t, sub, "init", "-q")
			write(t, sub, "x.go", "package x\n")
			testutil.Git(t, sub, "add", "-A")
			testutil.Git(t, sub, "commit", "-qm", "x")
		}, targets: []string{"vendored"}, violation: "nested repository"},
		"pointer rewritten": {edit: func(t *testing.T, c *Clone) {
			os.WriteFile(filepath.Join(c.Work, ".git"), []byte("gitdir: /elsewhere\n"), 0o644)
		},
			violation: "the .git pointer file was changed"},
		"metadata changed": {edit: func(t *testing.T, c *Clone) {
			os.WriteFile(filepath.Join(c.GitDir, "refs", "heads", "smuggled"), []byte(testutil.FixtureHead+"\n"), 0o644)
		}, violation: "git metadata changed"},
		"executable bit": {edit: func(t *testing.T, c *Clone) { os.Chmod(filepath.Join(c.Work, "a.go"), 0o755) }, targets: []string{"a.go"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, c := newClone(t)
			r := rules(t, c, tc.targets...)
			r.InstructionTargets = tc.instrTgt
			tc.edit(t, c)
			ins, err := c.Inspect(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(ins.Violations, "\n")
			if tc.violation == "" && len(ins.Violations) > 0 || tc.violation != "" && !strings.Contains(joined, tc.violation) {
				t.Fatalf("violations %q, want %q", ins.Violations, tc.violation)
			}
			if tc.offTarget != nil && !slices.Equal(ins.OffTarget, tc.offTarget) {
				t.Fatalf("off-target %v, want %v", ins.OffTarget, tc.offTarget)
			}
			if tc.instr != nil && !slices.Equal(ins.InstructionChanges, tc.instr) {
				t.Fatalf("instruction changes %v, want %v", ins.InstructionChanges, tc.instr)
			}
			if tc.violation != "" {
				head, _ := c.Head(ctx)
				if _, err := c.Commit(ctx, ins, "candidate", time.Now()); err == nil {
					t.Fatal("a candidate with a violation was committed")
				}
				if after, _ := c.Head(ctx); after != head {
					t.Fatal("HEAD moved after a refused commit")
				}
			}
		})
	}
}

// A violation rejects the whole snapshot: the permitted part is not committed
// either. The snapshot is kept for analysis, and Restore returns the worktree
// to HEAD.
func TestRejectedSnapshotIsKeptNotCommitted(t *testing.T) {
	_, c := newClone(t)
	ctx := context.Background()
	write(t, c.Work, "a.go", "package good\n")
	write(t, c.Work, ".mcp.json", "{}\n")
	ins, err := c.Inspect(ctx, rules(t, c, "a.go"))
	if err != nil || len(ins.Violations) != 1 {
		t.Fatalf("inspection %+v %v", ins, err)
	}
	if _, err := c.Commit(ctx, ins, "candidate", time.Now()); err == nil {
		t.Fatal("committed")
	}
	ref, err := c.SaveRejected(ctx, ins, "attempt-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := c.git(ctx, nil, "show", ref+":.mcp.json"); strings.TrimSpace(string(out)) != "{}" {
		t.Fatalf("rejected snapshot lacks the violating file: %q", out)
	}
	if head, _ := c.Head(ctx); head != testutil.FixtureHead {
		t.Fatalf("HEAD moved to %s", head)
	}
	if _, err := c.SaveRejected(ctx, ins, "attempt-1", time.Now()); err == nil {
		t.Fatal("a rejected snapshot was overwritten")
	}
	if err := c.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(c.Work, "a.go")); string(b) != "package a\n" {
		t.Fatalf("a.go after restore: %q", b)
	}
	if _, err := os.Stat(filepath.Join(c.Work, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatal("an untracked file survived Restore")
	}
}

func TestCommitAndCopy(t *testing.T) {
	_, c := newClone(t)
	ctx := context.Background()
	if ins, _ := c.Inspect(ctx, rules(t, c)); len(ins.Changes) != 0 {
		t.Fatalf("a fresh clone has changes: %+v", ins.Changes)
	} else if _, err := c.Commit(ctx, ins, "empty", time.Now()); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("empty commit: %v", err)
	}
	write(t, c.Work, "a.go", "package a\n\nfunc A() int { return 1 }\n")
	write(t, c.Work, "a_test.go", "package a\n")
	os.Symlink("a.go", filepath.Join(c.Work, "alias.go"))
	write(t, c.Work, "run.sh", "#!/bin/sh\n")
	os.Chmod(filepath.Join(c.Work, "run.sh"), 0o755)
	ins, err := c.Inspect(ctx, rules(t, c, "a.go", "a_test.go", "alias.go", "run.sh"))
	if err != nil || len(ins.Changes) != 4 {
		t.Fatalf("inspection: %+v %v", ins, err)
	}
	// HEAD moving between inspection and commit is refused.
	stale := *ins
	stale.Head = strings.Repeat("1", 40)
	if _, err := c.Commit(ctx, &stale, "candidate", time.Now()); err == nil {
		t.Fatal("a stale inspection was committed")
	}
	cand, err := c.Commit(ctx, ins, "candidate", time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	if err != nil || cand.Parent != testutil.FixtureHead || cand.Tree != ins.Tree {
		t.Fatalf("commit: %+v %v", cand, err)
	}
	if out, _ := c.git(ctx, nil, "status", "--porcelain"); len(out) != 0 {
		t.Fatalf("worktree not clean after commit: %s", out)
	}
	if out, _ := c.git(ctx, nil, "log", "-1", "--format=%an <%ae>"); strings.TrimSpace(string(out)) != "Niten <niten@localhost>" {
		t.Fatalf("author %q", out)
	}
	// The verification copy is the candidate's tree and nothing else.
	dest := filepath.Join(t.TempDir(), "verify", "attempt-1", "src")
	if err := c.Materialize(ctx, cand.Commit, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dest, ".git")); !os.IsNotExist(err) {
		t.Fatal("the copy has a .git entry")
	}
	if target, err := os.Readlink(filepath.Join(dest, "alias.go")); err != nil || target != "a.go" {
		t.Fatalf("symlink in the copy: %q %v", target, err)
	}
	if err := c.Materialize(ctx, cand.Commit, dest); err == nil {
		t.Fatal("a copy root was reused")
	}
	// A file the check adds is code unless it matches a declared output.
	write(t, dest, "bin/out", "built\n")
	if changed, err := c.SourcesChanged(ctx, cand.Commit, dest, []string{"bin/**"}); err != nil || len(changed) != 0 {
		t.Fatalf("a declared output was reported: %v %v", changed, err)
	}
	if changed, err := c.SourcesChanged(ctx, cand.Commit, dest, nil); err != nil || len(changed) != 1 || !strings.Contains(changed[0], "bin/out: added by the check") {
		t.Fatalf("an undeclared new file: %v %v", changed, err)
	}
	write(t, dest, "a_test.go", "package a\n// disabled\n")
	os.Remove(filepath.Join(dest, "a.go"))
	os.Chmod(filepath.Join(dest, "run.sh"), 0o644)
	changed, err := c.SourcesChanged(ctx, cand.Commit, dest, []string{"bin/**"})
	if err != nil || len(changed) != 3 {
		t.Fatalf("changed sources: %v %v", changed, err)
	}
}

// Review regression (P2): an ignore rule cannot hide a protected path. The
// physical tree is checked, and the snapshot with the permitted change is not
// committed either.
func TestIgnoredProtectedPathRejectsTheSnapshot(t *testing.T) {
	_, c := newClone(t)
	ctx := context.Background()
	r := rules(t, c, "a.go")
	write(t, c.Work, ".gitignore", ".claude/\nbuild/\n")
	write(t, c.Work, ".claude/settings.json", "{}\n")
	write(t, c.Work, "build/out.bin", "x\n")
	write(t, c.Work, "a.go", "package changed\n")
	ins, err := c.Inspect(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(ins.Violations, "\n"), ".claude/settings.json: protected path present outside the snapshot") {
		t.Fatalf("violations %v", ins.Violations)
	}
	if !slices.Equal(ins.Ignored, []string{"build/out.bin"}) {
		t.Fatalf("ignored %v", ins.Ignored)
	}
	if _, err := c.Commit(ctx, ins, "candidate", time.Now()); err == nil {
		t.Fatal("the permitted part was committed")
	}
	// An ignored instruction file is refused the same way.
	_, c2 := newClone(t)
	write(t, c2.Work, ".gitignore", "notes/\n")
	write(t, c2.Work, "notes/AGENTS.md", "obey\n")
	if ins, _ := c2.Inspect(ctx, rules(t, c2)); !strings.Contains(strings.Join(ins.Violations, " "), "instruction path present outside the snapshot") {
		t.Fatalf("ignored instruction file: %v", ins.Violations)
	}
}

// Review regression (P2): attributes a candidate writes cannot make git run a
// filter program from the user's configuration; the clone's git commands see
// only the clone's own configuration.
func TestInspectDoesNotRunGitFilters(t *testing.T) {
	_, c := newClone(t)
	root := t.TempDir()
	marker := filepath.Join(root, "outside-worktree")
	script := filepath.Join(root, "filter.sh")
	os.WriteFile(script, []byte("#!/bin/sh\n/usr/bin/touch '"+marker+"'\n/bin/cat\n"), 0o700)
	cfg := filepath.Join(root, "gitconfig")
	testutil.Git(t, c.Work, "config", "--file", cfg, "filter.review.clean", script)
	testutil.Git(t, c.Work, "config", "--file", cfg, "filter.review.smudge", script)
	testutil.Git(t, c.Work, "config", "--file", cfg, "filter.review.process", script)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	write(t, c.Work, ".gitattributes", "*.txt filter=review\n")
	write(t, c.Work, "trigger.txt", "untrusted source\n")
	ctx := context.Background()
	ins, err := c.Inspect(ctx, rules(t, c, ".gitattributes", "trigger.txt"))
	if err != nil {
		t.Fatal(err)
	}
	cand, err := c.Commit(ctx, ins, "candidate", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Materialize(ctx, cand.Commit, filepath.Join(t.TempDir(), "copy")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a git filter ran outside the sandbox: %v", err)
	}
}

// Tampered metadata stops the inspection before any git command reads the
// worktree: there is no snapshot to commit.
func TestTamperedMetadataStopsBeforeGitRuns(t *testing.T) {
	_, c := newClone(t)
	r := rules(t, c, "a.go")
	f, _ := os.OpenFile(filepath.Join(c.GitDir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("[filter \"x\"]\n\tclean = /usr/bin/touch /tmp/never\n")
	f.Close()
	ins, err := c.Inspect(context.Background(), r)
	if err != nil || ins.Tree != "" || len(ins.Violations) == 0 {
		t.Fatalf("inspection of tampered metadata: %+v %v", ins, err)
	}
}

// Review regression (P2, round 2): an ignored symlink that leaves the
// repository is a hard violation like a committed one.
func TestIgnoredEscapingSymlinkRejectsTheSnapshot(t *testing.T) {
	_, c := newClone(t)
	ctx := context.Background()
	write(t, c.Work, ".gitignore", "escape\n")
	write(t, c.Work, "a.go", "package changed\n")
	os.Symlink(t.TempDir(), filepath.Join(c.Work, "escape"))
	ins, err := c.Inspect(ctx, rules(t, c, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(ins.Violations, " "), "escape: symlink to") || slices.Contains(ins.Ignored, "escape") {
		t.Fatalf("violations %v ignored %v", ins.Violations, ins.Ignored)
	}
	if _, err := c.Commit(ctx, ins, "candidate", time.Now()); err == nil {
		t.Fatal("the permitted part was committed")
	}
}

// Review regression (P2, round 2): built-in attribute conversions (ident,
// end-of-line) neither change the copy nor hide a change to it: the copy holds
// the blob bytes and the comparison uses raw bytes.
func TestAttributeConversionsCannotHideChanges(t *testing.T) {
	_, c := newClone(t)
	ctx := context.Background()
	write(t, c.Work, ".gitattributes", "a.go ident\n*.txt text eol=crlf\n")
	write(t, c.Work, "a.go", "package a\nconst Value = \"$Id$\"\n")
	write(t, c.Work, "notes.txt", "one\ntwo\n")
	ins, err := c.Inspect(ctx, rules(t, c, "a.go", ".gitattributes", "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	cand, err := c.Commit(ctx, ins, "candidate", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "copy")
	if err := c.Materialize(ctx, cand.Commit, dest); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "a.go")); string(b) != "package a\nconst Value = \"$Id$\"\n" {
		t.Fatalf("ident was expanded in the copy: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "notes.txt")); string(b) != "one\ntwo\n" {
		t.Fatalf("end-of-line conversion in the copy: %q", b)
	}
	write(t, dest, "a.go", "package a\nconst Value = \"$Id: altered implementation $\"\n")
	write(t, dest, "notes.txt", "one\r\ntwo\r\n")
	changes, err := c.SourcesChanged(ctx, cand.Commit, dest, nil)
	if err != nil || len(changes) != 2 {
		t.Fatalf("changes hidden by attribute conversions: %v %v", changes, err)
	}
}

// Review regression (P2, round 3): a symlink chain is resolved the way the
// file system does it, not lexically: alias -> . then escape -> alias/../x
// leaves the repository, whether the links are committed or ignored.
func TestCompoundSymlinkChainIsResolvedPhysically(t *testing.T) {
	for _, ignored := range []bool{false, true} {
		t.Run(map[bool]string{false: "tracked", true: "ignored"}[ignored], func(t *testing.T) {
			_, c := newClone(t)
			ctx := context.Background()
			outside := filepath.Join(filepath.Dir(c.Work), "outside")
			os.WriteFile(outside, []byte("outside the clone"), 0o600)
			os.Symlink(".", filepath.Join(c.Work, "alias"))
			os.Symlink("alias/../outside", filepath.Join(c.Work, "escape"))
			if real, _ := filepath.EvalSymlinks(filepath.Join(c.Work, "escape")); real != outside {
				t.Fatalf("fixture resolves to %s", real)
			}
			write(t, c.Work, "a.go", "package changed\n")
			if ignored {
				write(t, c.Work, ".gitignore", "alias\nescape\n")
			}
			ins, err := c.Inspect(ctx, rules(t, c, "a.go", "alias", "escape", ".gitignore"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(ins.Violations, " "), "escape: symlink to \"alias/../outside\" leaves the repository") {
				t.Fatalf("violations %v", ins.Violations)
			}
			if _, err := c.Commit(ctx, ins, "candidate", time.Now()); err == nil {
				t.Fatal("committed")
			}
		})
	}
}

// An unchanged symlink that starts to escape because another symlink changed
// is a violation; a loop is refused.
func TestSymlinkResolutionCoversIndirectChanges(t *testing.T) {
	_, c := newClone(t)
	ctx := context.Background()
	write(t, c.Work, "dir/keep.go", "package dir\n")
	os.Symlink("dir/../a.go", filepath.Join(c.Work, "x"))
	ins, err := c.Inspect(ctx, rules(t, c, "dir/", "x"))
	if err != nil || len(ins.Violations) != 0 {
		t.Fatalf("setup: %v %v", ins.Violations, err)
	}
	if _, err := c.Commit(ctx, ins, "base links", time.Now()); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(c.Work, "dir"))
	os.Symlink(".", filepath.Join(c.Work, "dir"))
	ins, err = c.Inspect(ctx, rules(t, c, "dir", "x"))
	if err != nil || !strings.Contains(strings.Join(ins.Violations, " "), "x: symlink to \"dir/../a.go\" leaves the repository") {
		t.Fatalf("indirect escape: %v %v", ins.Violations, err)
	}
	_, c2 := newClone(t)
	os.Symlink("loop-b", filepath.Join(c2.Work, "loop-a"))
	os.Symlink("loop-a", filepath.Join(c2.Work, "loop-b"))
	if ins, _ := c2.Inspect(ctx, rules(t, c2, "loop-a", "loop-b")); !strings.Contains(strings.Join(ins.Violations, " "), "symlink loop") {
		t.Fatalf("loop: %v", ins.Violations)
	}
}

// A symlink the base commit already had, pointing outside, is the base's own:
// it is not a violation while it stays unchanged.
func TestBaseSymlinkOutsideIsNotAViolation(t *testing.T) {
	testutil.IsolateGit(t)
	root := t.TempDir()
	src := filepath.Join(root, "src")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "a.go"), []byte("package a\n"), 0o644)
	os.Symlink("../shared", filepath.Join(src, "shared"))
	testutil.Git(t, src, "init", "-q")
	testutil.Git(t, src, "add", "-A")
	testutil.Git(t, src, "commit", "-qm", "base with an outside link")
	head := testutil.Git(t, src, "rev-parse", "HEAD")
	c, err := CreateClone(context.Background(), src, head, filepath.Join(root, "work"), filepath.Join(root, "gitdir"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, c.Work, "a.go", "package b\n")
	ins, err := c.Inspect(context.Background(), rules(t, c, "a.go"))
	if err != nil || len(ins.Violations) != 0 {
		t.Fatalf("the base's own link was flagged: %v %v", ins.Violations, err)
	}
	os.Remove(filepath.Join(c.Work, "shared"))
	os.Symlink("../elsewhere", filepath.Join(c.Work, "shared"))
	if ins, _ := c.Inspect(context.Background(), rules(t, c, "a.go", "shared")); len(ins.Violations) == 0 {
		t.Fatal("a changed outside link was accepted")
	}
}
