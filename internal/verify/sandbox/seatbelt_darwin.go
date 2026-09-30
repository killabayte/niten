//go:build darwin

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Seatbelt is the macOS backend. It is safe for concurrent use.
type Seatbelt struct {
	launcher   string
	profileDir string
	osVersion  string
	osBuild    string
}

// New returns the Seatbelt backend or an error wrapping ErrUnavailable. The
// profile directory must be owned by the coordinator; generated profiles are
// written there with mode 0600.
func New(profileDir string) (*Seatbelt, error) { return newSeatbelt(Launcher, profileDir) }

func newSeatbelt(launcher, profileDir string) (*Seatbelt, error) {
	if launcher != Launcher {
		return nil, fmt.Errorf("%w: launcher must be %s, got %s", ErrUnavailable, Launcher, launcher)
	}
	st, err := os.Stat(launcher)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("%w: %s is not an executable file", ErrUnavailable, launcher)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || sys.Uid != 0 {
		return nil, fmt.Errorf("%w: %s is not owned by root", ErrUnavailable, launcher)
	}
	if lst, err := os.Stat(lsofPath); err != nil || !lst.Mode().IsRegular() || lst.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("%w: %s is required to detect processes that outlive a run", ErrUnavailable, lsofPath)
	}
	if profileDir == "" || !filepath.IsAbs(profileDir) {
		return nil, fmt.Errorf("%w: profile directory must be absolute", ErrPolicy)
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	pd, err := filepath.EvalSymlinks(profileDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	s := &Seatbelt{launcher: launcher, profileDir: pd}
	s.osVersion, _ = syscall.Sysctl("kern.osproductversion")
	s.osBuild, _ = syscall.Sysctl("kern.osversion")
	return s, nil
}

// OS returns the product version and build the backend was probed on.
func (s *Seatbelt) OS() (version, build string) { return s.osVersion, s.osBuild }

// SelfTest proves that the launcher can start a trivial process under a
// default-deny profile. A failure means the sandbox cannot be used here, for
// example because of an outer sandbox that forbids nesting.
func (s *Seatbelt) SelfTest(ctx context.Context) error {
	path, _, err := s.writeProfile(selfTestProfile)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, s.launcher, "-f", path, "/usr/bin/true")
	cmd.Env = []string{}
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: self-test failed: %v: %s", ErrUnavailable, err, bytes.TrimSpace(out.Bytes()))
	}
	return nil
}

// Run executes cmd under the policy. It validates both, writes the profile,
// starts the child in its own process group, enforces the timeout and kills
// the whole group afterwards. The returned Result is filled even on non-zero
// exit; err is non-nil only when the run could not be performed as specified.
func (s *Seatbelt) Run(ctx context.Context, p Policy, cmd Command) (Result, error) {
	np, err := p.Normalize()
	if err != nil {
		return Result{}, err
	}
	if err := s.checkCommand(np, cmd); err != nil {
		return Result{}, err
	}
	for _, w := range []string{np.SourceRoot, np.ScratchRoot} {
		if s.profileDir == w || within(s.profileDir, w) {
			return Result{}, fmt.Errorf("%w: profile directory %s lies inside writable root %s", ErrPolicy, s.profileDir, w)
		}
	}
	profile := Render(np)
	path, digest, err := s.writeProfile(profile)
	if err != nil {
		return Result{}, err
	}
	res := Result{Backend: BackendSeatbelt, Launcher: s.launcher, ProfilePath: path, ProfileDigest: digest,
		OSVersion: s.osVersion, OSBuild: s.osBuild, ExitCode: -1}

	runCtx, cancel := context.WithTimeout(ctx, cmd.Timeout)
	defer cancel()
	argv := append([]string{"-f", path}, cmd.Argv...)
	c := exec.CommandContext(runCtx, s.launcher, argv...)
	c.Dir = cmd.Dir
	c.Env = cmd.Env
	c.Stdout = writerOrDiscard(cmd.Stdout)
	c.Stderr = writerOrDiscard(cmd.Stderr)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGTERM) }
	c.WaitDelay = 2 * time.Second

	res.Started = time.Now()
	if err := c.Start(); err != nil {
		return res, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	pgid := c.Process.Pid
	waitErr := c.Wait()
	res.Finished = time.Now()
	res.Duration = res.Finished.Sub(res.Started)
	res.TimedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)

	// Whatever happened, nothing from the group may outlive the run.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	if syscall.Kill(-pgid, 0) == nil {
		res.Stragglers = true
	}
	// A descendant may have left the group with setsid while keeping the
	// profile. Anything that still holds the roots is ours: kill it and say so.
	killed, sweepErr := reapRoots([]string{np.SourceRoot, np.ScratchRoot})
	if len(killed) > 0 {
		res.Stragglers = true
		res.SurvivorPIDs = killed
	}
	if sweepErr != nil {
		return res, sweepErr
	}

	if ps := c.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.Signal = ws.Signal().String()
		} else {
			res.ExitCode = ps.ExitCode()
		}
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) && !res.TimedOut {
		return res, fmt.Errorf("%w: %v", ErrUnavailable, waitErr)
	}
	return res, nil
}

func (s *Seatbelt) checkCommand(p Policy, cmd Command) error {
	if len(cmd.Argv) == 0 {
		return fmt.Errorf("%w: empty argv", ErrCommand)
	}
	if !filepath.IsAbs(cmd.Argv[0]) {
		return fmt.Errorf("%w: argv[0] %q must be an absolute path", ErrCommand, cmd.Argv[0])
	}
	if st, err := os.Stat(cmd.Argv[0]); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("%w: argv[0] %q is not an executable file", ErrCommand, cmd.Argv[0])
	}
	if cmd.Env == nil {
		return fmt.Errorf("%w: environment must be explicit (use Environment)", ErrCommand)
	}
	if cmd.Timeout <= 0 {
		return fmt.Errorf("%w: timeout must be positive", ErrCommand)
	}
	if !filepath.IsAbs(cmd.Dir) {
		return fmt.Errorf("%w: dir %q must be absolute", ErrCommand, cmd.Dir)
	}
	dir, err := filepath.EvalSymlinks(cmd.Dir)
	if err != nil {
		return fmt.Errorf("%w: dir %q: %v", ErrCommand, cmd.Dir, err)
	}
	if !(dir == p.SourceRoot || within(dir, p.SourceRoot) || dir == p.ScratchRoot || within(dir, p.ScratchRoot)) {
		return fmt.Errorf("%w: dir %s is outside the source and scratch roots", ErrCommand, dir)
	}
	return nil
}

// writeProfile stores the profile under its digest with mode 0600, atomically.
func (s *Seatbelt) writeProfile(profile string) (path, digest string, err error) {
	digest = Digest(profile)
	path = filepath.Join(s.profileDir, digest+".sb")
	if existing, rerr := os.ReadFile(path); rerr == nil {
		if string(existing) == profile {
			return path, digest, nil
		}
		return "", "", fmt.Errorf("%w: profile %s exists with different content", ErrUnavailable, path)
	}
	tmp, err := os.CreateTemp(s.profileDir, ".profile-*")
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := io.WriteString(tmp, profile); err != nil {
		tmp.Close()
		return "", "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := tmp.Close(); err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return path, digest, nil
}

func writerOrDiscard(w interface{ Write([]byte) (int, error) }) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
