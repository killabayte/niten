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
	// A check that edits the code under test is detected; build outputs are not.
	write(t, dest, "bin/out", "built\n")
	if changed, err := c.SourcesChanged(ctx, cand.Commit, dest); err != nil || len(changed) != 0 {
		t.Fatalf("build output reported: %v %v", changed, err)
	}
	write(t, dest, "a_test.go", "package a\n// disabled\n")
	os.Remove(filepath.Join(dest, "a.go"))
	os.Chmod(filepath.Join(dest, "run.sh"), 0o644)
	changed, err := c.SourcesChanged(ctx, cand.Commit, dest)
	if err != nil || len(changed) != 3 {
		t.Fatalf("changed sources: %v %v", changed, err)
	}
}
