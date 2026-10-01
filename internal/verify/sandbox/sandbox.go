// Package sandbox runs coordinator-owned verification commands under an OS
// sandbox. The commands are repository tests and builds: untrusted code that
// must be able to read the verification copy and its toolchain, write only to
// the copy and a per-attempt scratch area, and reach neither the network nor
// the rest of the user's files.
//
// v0.1 ships one backend, macOS Seatbelt through the fixed launcher
// /usr/bin/sandbox-exec. There is deliberately no plain-exec fallback: when the
// backend is unavailable the caller gets an error and must not run the check.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Launcher is the only executable the Seatbelt backend will use.
const Launcher = "/usr/bin/sandbox-exec"

// Backend identifies the isolation mechanism in evidence records.
const BackendSeatbelt = "seatbelt"

var (
	// ErrUnsupportedOS is returned where no backend exists for the OS.
	ErrUnsupportedOS = errors.New("sandbox: unsupported operating system")
	// ErrUnavailable is returned when the launcher is missing, foreign or
	// fails its self-test. Verification must not proceed.
	ErrUnavailable = errors.New("sandbox: launcher unavailable")
	// ErrPolicy marks an invalid or contradictory policy.
	ErrPolicy = errors.New("sandbox: invalid policy")
	// ErrCommand marks a command that the backend refuses to run.
	ErrCommand = errors.New("sandbox: invalid command")
)

// Policy describes what a verification command may touch. All paths are made
// absolute and symlink-free by Normalize; callers should pass real directories.
type Policy struct {
	// SourceRoot is the disposable verification copy. Readable and writable.
	SourceRoot string
	// ScratchRoot holds caches, temp files and outputs. Readable and writable.
	ScratchRoot string
	// Toolchains are read-only roots of the pinned toolchain, e.g. GOROOT.
	Toolchains []string
	// ReadOnly are extra read-only roots, e.g. an immutable module cache.
	ReadOnly []string
	// DenyRead lists paths that stay unreadable even if a broader rule would
	// permit them. System secrets are denied regardless.
	DenyRead []string
}

// Command is one sandboxed invocation. Env is the complete child environment;
// nothing is inherited from the coordinator.
type Command struct {
	Argv    []string
	Dir     string
	Env     []string
	Stdout  interface{ Write([]byte) (int, error) }
	Stderr  interface{ Write([]byte) (int, error) }
	Timeout time.Duration
}

// Result is what a sandboxed run leaves behind for the evidence record.
type Result struct {
	Backend       string
	Launcher      string
	ProfilePath   string
	ProfileDigest string
	OSVersion     string
	OSBuild       string
	Started       time.Time
	Finished      time.Time
	Duration      time.Duration
	// ExitCode is the child's exit status, or -1 when it died from a signal.
	ExitCode int
	Signal   string
	TimedOut bool
	// Stragglers is true when members of the child's process group were still
	// alive after the child finished. They were killed with the group; a run
	// with stragglers must not be accepted as evidence.
	Stragglers bool
	// SurvivorPIDs lists group members found holding the roots during the
	// sweep; they were killed with the group.
	SurvivorPIDs []int
	// ForeignPIDs lists holders of the roots outside the run's process group.
	// Membership is not proven for them, so they are never killed; their
	// presence refuses the run.
	ForeignPIDs []int
}

// systemReadRoots are read-only system prefixes every process needs. The root
// directory itself must be readable: dyld opens "/" and aborts otherwise.
var systemReadRoots = []string{
	"/usr", "/bin", "/sbin", "/System", "/Library",
	"/private/etc", "/private/var/db", "/private/var/select", "/dev",
}

// systemDenyRead are denied even though they sit under a system read root.
var systemDenyRead = []string{"/Library/Keychains"}

// machServices are the only launchd services a sandboxed process may look up.
// A blanket mach-lookup would let a test reach the network through system
// daemons (for example nsurlsessiond), so the list stays minimal.
var machServices = []string{"com.apple.system.opendirectoryd.libinfo"}

// Normalize resolves and validates the policy. It returns a copy whose paths
// are absolute, cleaned and free of symlinks, or an error wrapping ErrPolicy.
func (p Policy) Normalize() (Policy, error) {
	var n Policy
	var err error
	if n.SourceRoot, err = realDir("SourceRoot", p.SourceRoot); err != nil {
		return Policy{}, err
	}
	if n.ScratchRoot, err = realDir("ScratchRoot", p.ScratchRoot); err != nil {
		return Policy{}, err
	}
	if n.SourceRoot == n.ScratchRoot || within(n.SourceRoot, n.ScratchRoot) || within(n.ScratchRoot, n.SourceRoot) {
		return Policy{}, fmt.Errorf("%w: SourceRoot and ScratchRoot must be disjoint", ErrPolicy)
	}
	for _, w := range []string{n.SourceRoot, n.ScratchRoot} {
		for _, sys := range append(append([]string{"/"}, systemReadRoots...), systemDenyRead...) {
			if w == sys || (sys != "/" && within(w, sys)) {
				return Policy{}, fmt.Errorf("%w: writable root %s lies inside system path %s", ErrPolicy, w, sys)
			}
		}
	}
	for _, t := range p.Toolchains {
		r, err := realDir("Toolchains", t)
		if err != nil {
			return Policy{}, err
		}
		n.Toolchains = append(n.Toolchains, r)
	}
	for _, t := range p.ReadOnly {
		r, err := realDir("ReadOnly", t)
		if err != nil {
			return Policy{}, err
		}
		n.ReadOnly = append(n.ReadOnly, r)
	}
	for _, d := range p.DenyRead {
		if !filepath.IsAbs(d) {
			return Policy{}, fmt.Errorf("%w: DenyRead %q is not absolute", ErrPolicy, d)
		}
		d = filepath.Clean(d)
		// A deny path need not exist yet; resolve it when it does so that the
		// overlap checks compare real paths.
		if r, rerr := filepath.EvalSymlinks(d); rerr == nil {
			d = r
		}
		if err := safePath(d); err != nil {
			return Policy{}, err
		}
		for _, w := range []string{n.SourceRoot, n.ScratchRoot} {
			if w == d || within(w, d) || within(d, w) {
				return Policy{}, fmt.Errorf("%w: DenyRead %s overlaps writable root %s", ErrPolicy, d, w)
			}
		}
		n.DenyRead = append(n.DenyRead, d)
	}
	for _, ro := range append(append([]string{}, n.Toolchains...), n.ReadOnly...) {
		for _, w := range []string{n.SourceRoot, n.ScratchRoot} {
			if ro == w || within(ro, w) || within(w, ro) {
				return Policy{}, fmt.Errorf("%w: read-only root %s overlaps writable root %s", ErrPolicy, ro, w)
			}
		}
	}
	sort.Strings(n.Toolchains)
	sort.Strings(n.ReadOnly)
	sort.Strings(n.DenyRead)
	return n, nil
}

func realDir(field, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: %s is empty", ErrPolicy, field)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%w: %s %q is not absolute", ErrPolicy, field, p)
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("%w: %s %q: %v", ErrPolicy, field, p, err)
	}
	st, err := os.Stat(r)
	if err != nil {
		return "", fmt.Errorf("%w: %s %q: %v", ErrPolicy, field, p, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%w: %s %q is not a directory", ErrPolicy, field, p)
	}
	if err := safePath(r); err != nil {
		return "", err
	}
	return r, nil
}

// safePath rejects characters that cannot be embedded in an SBPL string.
func safePath(p string) error {
	for _, c := range p {
		if c == '"' || c == '\\' || c < 0x20 || c == 0x7f {
			return fmt.Errorf("%w: path %q contains an unsupported character", ErrPolicy, p)
		}
	}
	return nil
}

// within reports whether child is strictly inside parent. Both must be clean
// absolute paths.
func within(child, parent string) bool {
	if parent == "/" {
		return child != "/"
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}
