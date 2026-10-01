package workspace

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/killabayte/niten/internal/pathglob"
)

// Clone is a run's owned clone: the worktree the executor writes to and a
// separate git directory outside every model write root. The worktree's .git
// is a pointer file; Niten runs every git command with an explicit git
// directory and work tree, hooks disabled and replacement objects ignored.
type Clone struct {
	Work   string
	GitDir string
}

// Branch is the branch candidates are committed on.
const Branch = "niten"

// Fixed identity of candidate commits.
const (
	commitName  = "Niten"
	commitEmail = "niten@localhost"
)

// ErrNoChanges means the snapshot equals HEAD: there is nothing to commit.
var ErrNoChanges = errors.New("the worktree has no changes")

// CreateClone clones source into a new worktree work with its git metadata in
// gitdir, and checks out base on the branch "niten". Objects are copied, not
// shared (--no-local, no alternates), no template or hooks are installed, and
// the remote is removed so nothing can be pushed. Both paths must not exist.
func CreateClone(ctx context.Context, source, base, work, gitdir string) (*Clone, error) {
	for _, p := range []string{work, gitdir} {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("clone path %q is not absolute", p)
		}
		if _, err := os.Lstat(p); err == nil {
			return nil, fmt.Errorf("clone path %s already exists", p)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
	}
	src, err := Canonical(source)
	if err != nil {
		return nil, err
	}
	if !gitSHA(base) {
		return nil, fmt.Errorf("base %q is not a full commit id", base)
	}
	args := []string{"--no-replace-objects", "--no-lazy-fetch", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"clone", "--quiet", "--no-local", "--no-checkout", "--template=", "--separate-git-dir", gitdir, "--", src, work}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git clone: %v: %s", err, bytes.TrimSpace(out))
	}
	c := &Clone{}
	if c.Work, err = Canonical(work); err != nil {
		return nil, err
	}
	if c.GitDir, err = Canonical(gitdir); err != nil {
		return nil, err
	}
	if err := os.Chmod(c.GitDir, 0o700); err != nil {
		return nil, err
	}
	for _, kv := range [][2]string{
		{"core.hooksPath", "/dev/null"}, {"core.fsmonitor", "false"}, {"core.autocrlf", "false"}, {"core.symlinks", "true"},
		{"gc.auto", "0"}, {"commit.gpgsign", "false"}, {"user.name", commitName}, {"user.email", commitEmail},
	} {
		if _, err := c.git(ctx, nil, "config", kv[0], kv[1]); err != nil {
			return nil, err
		}
	}
	if _, err := c.git(ctx, nil, "remote", "remove", "origin"); err != nil {
		return nil, err
	}
	if _, err := c.git(ctx, nil, "cat-file", "-e", base+"^{commit}"); err != nil {
		return nil, fmt.Errorf("base commit %s is not reachable in the clone: %w", base, err)
	}
	if _, err := c.git(ctx, nil, "checkout", "--quiet", "-B", Branch, base); err != nil {
		return nil, err
	}
	if err := c.checkPointer(); err != nil {
		return nil, err
	}
	head, err := c.Head(ctx)
	if err != nil || head != base {
		return nil, fmt.Errorf("clone HEAD %s is not the base %s: %v", head, base, err)
	}
	if out, err := c.git(ctx, nil, "status", "--porcelain", "--untracked-files=all"); err != nil || len(out) != 0 {
		return nil, fmt.Errorf("the new clone is not clean: %v %s", err, out)
	}
	return c, nil
}

// OpenClone attaches to an existing clone and checks its pointer file.
func OpenClone(work, gitdir string) (*Clone, error) {
	c := &Clone{Work: work, GitDir: gitdir}
	return c, c.checkPointer()
}

// checkPointer requires work/.git to be a regular file naming the git directory.
func (c *Clone) checkPointer() error {
	p := filepath.Join(c.Work, ".git")
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not the gitdir pointer file", p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) != "gitdir: "+c.GitDir {
		return fmt.Errorf("%s points elsewhere: %q", p, strings.TrimSpace(string(b)))
	}
	return nil
}

// git runs a git command on the clone with the hardened environment plus env.
func (c *Clone) git(ctx context.Context, env []string, args ...string) ([]byte, error) {
	full := append([]string{"--git-dir", c.GitDir, "--work-tree", c.Work, "--no-replace-objects", "--no-lazy-fetch",
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(gitEnv(), env...)
	cmd.Dir = c.Work
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Head returns the commit the branch points at.
func (c *Clone) Head(ctx context.Context) (string, error) {
	out, err := c.git(ctx, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	return strings.TrimSpace(string(out)), err
}

// TreeOf returns the tree of a commit.
func (c *Clone) TreeOf(ctx context.Context, commit string) (string, error) {
	out, err := c.git(ctx, nil, "rev-parse", "--verify", "-q", commit+"^{tree}")
	return strings.TrimSpace(string(out)), err
}

// MetadataFingerprint hashes the parts of the git directory that decide what
// the repository is: HEAD, config, refs, packed refs, info, hooks, shallow and
// alternates. The index, logs and objects change through Niten's own commits
// and are left out; a candidate is compared against the fingerprint taken
// after the coordinator's last commit.
func (c *Clone) MetadataFingerprint() (string, error) {
	h := sha256.New()
	var paths []string
	for _, name := range []string{"HEAD", "config", "packed-refs", "shallow", "objects/info/alternates", "commondir"} {
		paths = append(paths, name)
	}
	for _, dir := range []string{"refs", "info", "hooks"} {
		filepath.WalkDir(filepath.Join(c.GitDir, dir), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(c.GitDir, p)
				paths = append(paths, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	sort.Strings(paths)
	for _, rel := range paths {
		p := filepath.Join(c.GitDir, filepath.FromSlash(rel))
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%o\x00", rel, fi.Mode())
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return "", err
			}
			h.Write(b)
		} else if fi.Mode()&fs.ModeSymlink != 0 {
			t, _ := os.Readlink(p)
			h.Write([]byte(t))
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Rules are the hard policy and the plan focus used to classify a change.
type Rules struct {
	Protected   []string // protected path patterns: any change is a violation
	Instruction []string // instruction path patterns: a change needs an explicit target
	// Targets are the plan's target paths (focus, not a grant); a changed path
	// matching none of them is off-target.
	Targets []string
	// InstructionTargets are instruction paths the plan explicitly targets for
	// a change (operation other than inspect).
	InstructionTargets []string
	// Metadata is the git metadata fingerprint expected at inspection.
	Metadata string
}

// Change is one path that differs from HEAD in the snapshot.
type Change struct {
	Path          string `json:"path"`
	Status        string `json:"status"` // A, M, D or T
	OldMode       string `json:"old_mode"`
	NewMode       string `json:"new_mode"`
	SymlinkTarget string `json:"symlink_target,omitempty"`
}

// Inspection is the full state of the worktree against HEAD.
type Inspection struct {
	Head               string   `json:"head"`
	Tree               string   `json:"tree"`
	Changes            []Change `json:"changes"`
	Violations         []string `json:"violations"`
	OffTarget          []string `json:"off_target"`
	InstructionChanges []string `json:"instruction_changes"`
}

// Paths lists the changed paths.
func (ins *Inspection) Paths() []string {
	out := make([]string, 0, len(ins.Changes))
	for _, ch := range ins.Changes {
		out = append(out, ch.Path)
	}
	return out
}

// Inspect snapshots the whole worktree into a private index (the real index is
// not touched), writes its tree and classifies every change against HEAD. It
// never commits. Ignored files are not part of the snapshot.
func (c *Clone) Inspect(ctx context.Context, r Rules) (*Inspection, error) {
	ins := &Inspection{Changes: []Change{}, Violations: []string{}, OffTarget: []string{}, InstructionChanges: []string{}}
	if err := c.checkPointer(); err != nil {
		ins.Violations = append(ins.Violations, "the .git pointer file was changed: "+err.Error())
	}
	if r.Metadata != "" {
		if fp, err := c.MetadataFingerprint(); err != nil || fp != r.Metadata {
			ins.Violations = append(ins.Violations, "the git metadata changed outside the coordinator")
		}
	}
	head, err := c.Head(ctx)
	if err != nil {
		return nil, err
	}
	ins.Head = head
	idx, cleanup, err := c.tempIndex()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + idx}
	if _, err := c.git(ctx, env, "read-tree", head); err != nil {
		return nil, err
	}
	if _, err := c.git(ctx, env, "add", "--all", "--", "."); err != nil {
		return nil, err
	}
	tree, err := c.git(ctx, env, "write-tree")
	if err != nil {
		return nil, err
	}
	ins.Tree = strings.TrimSpace(string(tree))
	raw, err := c.git(ctx, env, "diff-index", "--cached", "--raw", "-z", "--no-renames", head)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(string(raw), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) != 5 {
			return nil, fmt.Errorf("unexpected diff-index record %q", fields[i])
		}
		ch := Change{Path: fields[i+1], OldMode: meta[0], NewMode: meta[1], Status: meta[4]}
		if ch.NewMode == "120000" {
			b, err := c.git(ctx, nil, "cat-file", "blob", meta[3])
			if err != nil {
				return nil, err
			}
			ch.SymlinkTarget = string(b)
		}
		ins.Changes = append(ins.Changes, ch)
		c.classify(ch, r, ins)
	}
	sort.Strings(ins.OffTarget)
	return ins, nil
}

func (c *Clone) classify(ch Change, r Rules, ins *Inspection) {
	p := ch.Path
	switch {
	case ch.NewMode == "160000" || ch.OldMode == "160000":
		ins.Violations = append(ins.Violations, p+": nested repository or submodule")
	case pathglob.MatchAny(r.Protected, p) != "":
		ins.Violations = append(ins.Violations, p+": protected path ("+pathglob.MatchAny(r.Protected, p)+")")
	case pathglob.MatchAny(r.Instruction, p) != "":
		if matchesTarget(r.InstructionTargets, p) {
			ins.InstructionChanges = append(ins.InstructionChanges, p)
		} else {
			ins.Violations = append(ins.Violations, p+": instruction path changed without an explicit plan target")
		}
	}
	if ch.NewMode == "120000" {
		t := ch.SymlinkTarget
		if strings.HasPrefix(t, "/") || strings.HasPrefix(path.Clean(path.Join(path.Dir(p), t)), "../") || path.Clean(path.Join(path.Dir(p), t)) == ".." {
			ins.Violations = append(ins.Violations, fmt.Sprintf("%s: symlink to %q leaves the repository", p, t))
		}
	}
	if !matchesTarget(r.Targets, p) {
		ins.OffTarget = append(ins.OffTarget, p)
	}
}

// matchesTarget reports whether p is a target, matches a target pattern or
// lies under a target directory.
func matchesTarget(targets []string, p string) bool {
	for _, t := range targets {
		t = strings.TrimSuffix(t, "/")
		if t == p || pathglob.Match(t, p) || strings.HasPrefix(p, t+"/") {
			return true
		}
	}
	return false
}

// tempIndex returns a fresh index path inside the git directory, outside the
// worktree and every model write root.
func (c *Clone) tempIndex() (string, func(), error) {
	f, err := os.CreateTemp(c.GitDir, "niten-index-*")
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	f.Close()
	os.Remove(name) // git creates it
	return name, func() { os.Remove(name); os.Remove(name + ".lock") }, nil
}

// Candidate is an immutable commit of the whole worktree.
type Candidate struct {
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
	Parent string `json:"parent"`
}

// Commit records an inspected snapshot as a candidate. It refuses any
// inspection with a violation (there is no partial commit of the permitted
// part), a snapshot equal to HEAD, and a HEAD that moved since the inspection.
func (c *Clone) Commit(ctx context.Context, ins *Inspection, message string, at time.Time) (Candidate, error) {
	if len(ins.Violations) > 0 {
		return Candidate{}, fmt.Errorf("refusing to commit a candidate with %d hard policy violation(s): %s", len(ins.Violations), strings.Join(ins.Violations, "; "))
	}
	head, err := c.Head(ctx)
	if err != nil {
		return Candidate{}, err
	}
	if head != ins.Head {
		return Candidate{}, fmt.Errorf("HEAD moved from %s to %s since the inspection", ins.Head, head)
	}
	headTree, err := c.TreeOf(ctx, head)
	if err != nil {
		return Candidate{}, err
	}
	if headTree == ins.Tree {
		return Candidate{}, ErrNoChanges
	}
	commit, err := c.commitTree(ctx, ins.Tree, head, message, at)
	if err != nil {
		return Candidate{}, err
	}
	if _, err := c.git(ctx, nil, "update-ref", "refs/heads/"+Branch, commit, head); err != nil {
		return Candidate{}, err
	}
	if _, err := c.git(ctx, nil, "read-tree", commit); err != nil {
		return Candidate{}, err
	}
	return Candidate{Commit: commit, Tree: ins.Tree, Parent: head}, nil
}

func (c *Clone) commitTree(ctx context.Context, tree, parent, message string, at time.Time) (string, error) {
	date := at.UTC().Format(time.RFC3339)
	env := []string{"GIT_AUTHOR_NAME=" + commitName, "GIT_AUTHOR_EMAIL=" + commitEmail, "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=" + commitName, "GIT_COMMITTER_EMAIL=" + commitEmail, "GIT_COMMITTER_DATE=" + date}
	out, err := c.git(ctx, env, "commit-tree", tree, "-p", parent, "-m", message)
	return strings.TrimSpace(string(out)), err
}

// SaveRejected keeps a rejected snapshot for analysis under
// refs/niten/rejected/<name> without moving HEAD or touching the worktree.
func (c *Clone) SaveRejected(ctx context.Context, ins *Inspection, name string, at time.Time) (string, error) {
	if name == "" || strings.ContainsAny(name, "/ \t\n:~^?*[\\") {
		return "", fmt.Errorf("invalid rejected snapshot name %q", name)
	}
	commit, err := c.commitTree(ctx, ins.Tree, ins.Head, "niten: rejected snapshot "+name, at)
	if err != nil {
		return "", err
	}
	if _, err := c.git(ctx, nil, "update-ref", "refs/niten/rejected/"+name, commit, strings.Repeat("0", 40)); err != nil {
		return "", err
	}
	return commit, nil
}

// Restore makes the worktree exactly HEAD again: tracked files are reset and
// every untracked or ignored file is removed.
func (c *Clone) Restore(ctx context.Context) error {
	if _, err := c.git(ctx, nil, "reset", "--quiet", "--hard", "HEAD"); err != nil {
		return err
	}
	_, err := c.git(ctx, nil, "clean", "-ffdxq")
	return err
}

// Materialize writes the tree of commit into dest, a new directory, through a
// private index. Nothing in dest refers back to the clone.
func (c *Clone) Materialize(ctx context.Context, commit, dest string) error {
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("copy path %q is not absolute", dest)
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("copy path %s already exists; every attempt needs new roots", dest)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	idx, cleanup, err := c.tempIndex()
	if err != nil {
		return err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + idx}
	if _, err := c.git(ctx, env, "read-tree", commit); err != nil {
		return err
	}
	cp := &Clone{Work: dest, GitDir: c.GitDir}
	if _, err := cp.git(ctx, env, "checkout-index", "--all", "--force"); err != nil {
		return err
	}
	changed, err := c.SourcesChanged(ctx, commit, dest)
	if err != nil {
		return err
	}
	if len(changed) > 0 {
		return fmt.Errorf("the materialized copy differs from %s: %v", commit, changed)
	}
	return nil
}

// SourcesChanged compares dir with the tree of commit and lists every tracked
// path that is missing, modified, has another type or another executable bit.
// Files that are not in the tree (build outputs) are not reported.
func (c *Clone) SourcesChanged(ctx context.Context, commit, dir string) ([]string, error) {
	out, err := c.git(ctx, nil, "ls-tree", "-r", "-z", commit)
	if err != nil {
		return nil, err
	}
	type entry struct{ mode, sha, path string }
	var files []entry
	var changed []string
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("unexpected ls-tree record %q", rec)
		}
		e := entry{mode: f[0], sha: f[2], path: p}
		full := filepath.Join(dir, filepath.FromSlash(p))
		fi, err := os.Lstat(full)
		switch {
		case err != nil:
			changed = append(changed, p+": missing")
		case e.mode == "120000":
			blob, berr := c.git(ctx, nil, "cat-file", "blob", e.sha)
			target, lerr := os.Readlink(full)
			if fi.Mode()&fs.ModeSymlink == 0 || berr != nil || lerr != nil || target != string(blob) {
				changed = append(changed, p+": symlink changed")
			}
		case e.mode == "100644" || e.mode == "100755":
			if !fi.Mode().IsRegular() {
				changed = append(changed, p+": not a regular file")
				continue
			}
			if (fi.Mode().Perm()&0o111 != 0) != (e.mode == "100755") {
				changed = append(changed, p+": executable bit changed")
				continue
			}
			files = append(files, e)
		default:
			changed = append(changed, p+": unsupported entry mode "+e.mode)
		}
	}
	if len(files) > 0 {
		var in bytes.Buffer
		for _, e := range files {
			if strings.ContainsAny(e.path, "\n\r") {
				return nil, fmt.Errorf("path %q cannot be hashed in batch", e.path)
			}
			in.WriteString(e.path + "\n")
		}
		cp := &Clone{Work: dir, GitDir: c.GitDir}
		cmd := exec.CommandContext(ctx, "git", "--git-dir", c.GitDir, "--work-tree", dir, "--no-replace-objects", "--no-lazy-fetch",
			"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "hash-object", "--stdin-paths")
		cmd.Env = gitEnv()
		cmd.Dir = cp.Work
		cmd.Stdin = &in
		hashes, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("git hash-object: %w", err)
		}
		sc := bufio.NewScanner(bytes.NewReader(hashes))
		for i := 0; sc.Scan(); i++ {
			if i >= len(files) {
				break
			}
			if strings.TrimSpace(sc.Text()) != files[i].sha {
				changed = append(changed, files[i].path+": content changed")
			}
		}
	}
	sort.Strings(changed)
	return changed, nil
}

func gitSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
