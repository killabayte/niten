// Package workspace inspects and later owns the repositories Niten works on. This P1 slice
// is read-only: it fingerprints the user's source repository with Shogun's algorithm to
// detect drift from the approved planning base, and inventories the base commit's
// instruction and protected files. It never writes to the source repository.
package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/killabayte/niten/internal/pathglob"
	"github.com/killabayte/niten/internal/plan"
)

// Source is the observed state of a source repository.
type Source struct {
	Root            string `json:"root"` // canonical absolute path of the work tree top level
	Head            string `json:"head"`
	DiffSHA256      string `json:"diff_sha256"`
	UntrackedSHA256 string `json:"untracked_sha256"`
	Fingerprint     string `json:"fingerprint"`
	// TrackedChanges and Untracked describe the drift for diagnostics; Untracked holds at
	// most 20 names.
	TrackedChanges bool     `json:"tracked_changes"`
	Untracked      []string `json:"untracked,omitempty"`
}

// ErrNotRepository marks a path that is not the top level of a git work tree with a HEAD.
var ErrNotRepository = errors.New("not a git repository top level with a commit")

// Git runs git read-only in root. The environment is the caller's with every GIT_* variable
// removed except the global/system config selectors, so GIT_DIR or GIT_WORK_TREE cannot
// redirect the inspection; Shogun's own output settings are added. The file-system monitor
// is disabled, which does not change any output.
func Git(ctx context.Context, root string, args ...string) ([]byte, error) {
	full := append([]string{"-C", root, "--no-optional-locks", "-c", "core.fsmonitor=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = gitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") && k != "GIT_CONFIG_GLOBAL" && k != "GIT_CONFIG_NOSYSTEM" && k != "GIT_CONFIG_SYSTEM" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_EXTERNAL_DIFF=", "GIT_PAGER=cat", "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
}

// Canonical resolves p to an absolute path without symlinks.
func Canonical(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Within reports whether path is dir or lies below it; both must be canonical.
func Within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Inspect fingerprints the repository at root exactly as Shogun's intake does: HEAD, the
// digest of `git diff --binary` against HEAD (staged and unstaged tracked changes) and the
// digest of non-ignored untracked files. Paths in exclude (canonical absolute) and anything
// under a .git or .shogun directory are left out of the untracked set, as in Shogun.
func Inspect(ctx context.Context, root string, exclude []string) (*Source, error) {
	canon, err := Canonical(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrNotRepository, root, err)
	}
	if fi, err := os.Stat(canon); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrNotRepository, root)
	}
	if _, err := os.Stat(filepath.Join(canon, ".git")); err != nil {
		return nil, fmt.Errorf("%w: %s has no .git entry", ErrNotRepository, canon)
	}
	top, err := Git(ctx, canon, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotRepository, err)
	}
	if t, err := Canonical(strings.TrimSpace(string(top))); err != nil || t != canon {
		return nil, fmt.Errorf("%w: %s is inside the work tree %s, not its top level", ErrNotRepository, canon, strings.TrimSpace(string(top)))
	}
	head, err := Git(ctx, canon, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("%w: %s has no commit (unborn HEAD)", ErrNotRepository, canon)
	}
	s := &Source{Root: canon, Head: strings.TrimSpace(string(head))}
	diff, err := Git(ctx, canon, "diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-color", "--full-index", s.Head)
	if err != nil {
		return nil, err
	}
	s.DiffSHA256, s.TrackedChanges = digest(diff), len(diff) > 0
	untracked, err := Git(ctx, canon, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{}
	for _, p := range exclude {
		skip[p] = true
	}
	h := sha256.New()
	for _, rel := range strings.Split(string(untracked), "\x00") {
		if rel == "" || underOwn(rel) || skip[filepath.Join(canon, rel)] {
			continue
		}
		if len(s.Untracked) < 20 {
			s.Untracked = append(s.Untracked, rel)
		}
		p := filepath.Join(canon, rel)
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, fmt.Errorf("untracked %s: %w", rel, err)
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return nil, fmt.Errorf("untracked %s: %w", rel, err)
			}
			fmt.Fprintf(h, "%s\x00symlink:%s\n", rel, target)
		case fi.Mode().IsRegular():
			f, err := os.Open(p)
			if err != nil {
				return nil, fmt.Errorf("untracked %s: %w", rel, err)
			}
			fh := sha256.New()
			_, cerr := io.Copy(fh, f)
			f.Close()
			if cerr != nil {
				return nil, fmt.Errorf("untracked %s: %w", rel, cerr)
			}
			fmt.Fprintf(h, "%s\x00%s\n", rel, hex.EncodeToString(fh.Sum(nil)))
		default:
			fmt.Fprintf(h, "%s\x00special:%s\n", rel, fi.Mode().Type())
		}
	}
	s.UntrackedSHA256 = hex.EncodeToString(h.Sum(nil))
	s.Fingerprint = plan.RepoFingerprint(true, s.Head, s.DiffSHA256, s.UntrackedSHA256, "")
	return s, nil
}

func underOwn(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == ".git" || part == ".shogun" {
			return true
		}
	}
	return false
}

// Dirty reports tracked changes or untracked files.
func (s *Source) Dirty() bool {
	return s.DiffSHA256 != plan.EmptySHA256 || s.UntrackedSHA256 != plan.EmptySHA256
}

// BaseFile is one entry of the base commit that the policy cares about.
type BaseFile struct {
	Path          string `json:"path"`
	Mode          string `json:"mode"`
	Blob          string `json:"blob"`
	SHA256        string `json:"sha256,omitempty"` // content digest of a regular file
	SymlinkTarget string `json:"symlink_target,omitempty"`
	Pattern       string `json:"pattern"` // the policy pattern that matched
	Content       []byte `json:"-"`       // regular-file content, for instruction copies
}

// Base is the inventory of the pinned base commit.
type Base struct {
	Commit       string     `json:"commit"`
	Tree         string     `json:"tree"`
	Instructions []BaseFile `json:"instructions"`
	Protected    []BaseFile `json:"protected"`
	// Unsupported lists features v0.1 does not execute: submodules and Git LFS.
	Unsupported []string `json:"unsupported,omitempty"`
}

// maxInstruction bounds one instruction file copied from the base commit.
const maxInstruction = 1 << 20

// InventoryBase lists the base commit's instruction and protected entries (from the
// commit, not the working tree) and detects submodules and Git LFS attributes.
func InventoryBase(ctx context.Context, root, commit string, instruction, protected []string) (*Base, error) {
	tree, err := Git(ctx, root, "rev-parse", "--verify", "-q", commit+"^{tree}")
	if err != nil {
		return nil, err
	}
	b := &Base{Commit: commit, Tree: strings.TrimSpace(string(tree)), Instructions: []BaseFile{}, Protected: []BaseFile{}}
	out, err := Git(ctx, root, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return nil, err
	}
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("unexpected ls-tree record %q", rec)
		}
		mode, typ, blob := f[0], f[1], f[2]
		if mode == "160000" || typ == "commit" {
			b.Unsupported = append(b.Unsupported, "submodule "+p)
			continue
		}
		if filepath.Base(p) == ".gitattributes" && typ == "blob" {
			data, err := Git(ctx, root, "cat-file", "blob", blob)
			if err != nil {
				return nil, err
			}
			if bytes.Contains(data, []byte("filter=lfs")) {
				b.Unsupported = append(b.Unsupported, "Git LFS attributes in "+p)
			}
		}
		pi := pathglob.MatchAny(instruction, p)
		pp := pathglob.MatchAny(protected, p)
		if pi == "" && pp == "" {
			continue
		}
		bf := BaseFile{Path: p, Mode: mode, Blob: blob}
		if mode == "120000" || mode == "100644" || mode == "100755" {
			data, err := Git(ctx, root, "cat-file", "blob", blob)
			if err != nil {
				return nil, err
			}
			if mode == "120000" {
				bf.SymlinkTarget = string(data)
			} else {
				bf.SHA256 = digest(data)
				if pi != "" {
					if len(data) > maxInstruction {
						return nil, fmt.Errorf("instruction file %s is larger than %d bytes", p, maxInstruction)
					}
					bf.Content = data
				}
			}
		}
		if pi != "" {
			bf.Pattern = pi
			b.Instructions = append(b.Instructions, bf)
		}
		if pp != "" {
			bf.Pattern, bf.Content = pp, nil
			b.Protected = append(b.Protected, bf)
		}
	}
	sort.Slice(b.Instructions, func(i, j int) bool { return b.Instructions[i].Path < b.Instructions[j].Path })
	sort.Slice(b.Protected, func(i, j int) bool { return b.Protected[i].Path < b.Protected[j].Path })
	return b, nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
