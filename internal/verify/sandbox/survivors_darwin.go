//go:build darwin

package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// lsofPath is the fixed system tool used to find processes that still hold
// the sandbox roots. Killing the child's process group is not enough: a
// descendant that calls setsid leaves the group while keeping the Seatbelt
// profile, so it can go on writing inside the roots after Run returns.
const lsofPath = "/usr/sbin/lsof"

// Sealed describes the roots after Seal: their new paths and the processes
// that still held them and were killed.
type Sealed struct {
	SourceRoot  string
	ScratchRoot string
	Killed      []int
}

// survivors returns the PIDs of the caller's other processes that have an
// open file, cwd, root or mapped binary under any of the roots.
func survivors(roots []string) ([]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, lsofPath, "-w", "-n", "-P", "-F", "pn", "-u", strconv.Itoa(os.Getuid()))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		// lsof exits 1 when some entries could not be listed; the listing it
		// did produce is still usable. Anything else is a real failure.
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || out.Len() == 0 {
			return nil, fmt.Errorf("%s: %v: %s", lsofPath, err, bytes.TrimSpace(errb.Bytes()))
		}
	}
	self := os.Getpid()
	seen := map[int]bool{}
	var pids []int
	cur := 0
	for _, line := range strings.Split(out.String(), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			cur, _ = strconv.Atoi(line[1:])
		case 'n':
			if cur == 0 || cur == self || seen[cur] {
				continue
			}
			name := line[1:]
			for _, r := range roots {
				if name == r || strings.HasPrefix(name, r+"/") {
					seen[cur] = true
					pids = append(pids, cur)
					break
				}
			}
		}
	}
	sort.Ints(pids)
	return pids, nil
}

// killSurvivors sends SIGKILL to pids and waits until nothing holds the roots
// any more. It returns the PIDs that still hold them after the grace period.
func killSurvivors(roots []string, pids []int) ([]int, error) {
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		left, err := survivors(roots)
		if err != nil {
			return nil, err
		}
		if len(left) == 0 || time.Now().After(deadline) {
			return left, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// reapRoots is the post-run sweep shared by Run and Seal: find processes that
// still hold the roots, kill them and report them.
func reapRoots(roots []string) (killed []int, err error) {
	pids, err := survivors(roots)
	if err != nil {
		return nil, fmt.Errorf("%w: survivor scan failed: %v", ErrUnavailable, err)
	}
	if len(pids) == 0 {
		return nil, nil
	}
	left, err := killSurvivors(roots, pids)
	if err != nil {
		return pids, fmt.Errorf("%w: survivor scan failed: %v", ErrUnavailable, err)
	}
	if len(left) > 0 {
		return pids, fmt.Errorf("%w: %d process(es) still hold the roots after SIGKILL: %v", ErrUnavailable, len(left), left)
	}
	return pids, nil
}

// Seal retires the writable roots of a policy once the attempt is over. Both
// roots are renamed to sibling paths that no profile permits, so a process
// that escaped the group kill can no longer write into the trees, and every
// process still holding them is killed. Callers read outputs from the
// returned paths; the old paths must not be used again.
func (s *Seatbelt) Seal(p Policy) (Sealed, error) {
	np, err := p.Normalize()
	if err != nil {
		return Sealed{}, err
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Sealed{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	suffix := ".sealed-" + hex.EncodeToString(nonce[:])
	sealed := Sealed{SourceRoot: np.SourceRoot + suffix, ScratchRoot: np.ScratchRoot + suffix}
	if err := os.Rename(np.SourceRoot, sealed.SourceRoot); err != nil {
		return Sealed{}, fmt.Errorf("%w: seal source root: %v", ErrUnavailable, err)
	}
	if err := os.Rename(np.ScratchRoot, sealed.ScratchRoot); err != nil {
		return sealed, fmt.Errorf("%w: seal scratch root: %v", ErrUnavailable, err)
	}
	killed, err := reapRoots([]string{sealed.SourceRoot, sealed.ScratchRoot})
	sealed.Killed = killed
	return sealed, err
}
