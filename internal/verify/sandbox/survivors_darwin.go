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
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// lsofPath is the fixed system tool used to find processes that still hold
// the sandbox roots. Killing the child's process group is not enough: a
// descendant that calls setsid leaves the group while keeping the Seatbelt
// profile, so it can go on writing inside the roots after Run returns. It is a
// variable only so tests can substitute a failing tool.
var lsofPath = "/usr/sbin/lsof"

// Sealed describes the roots after Seal: their new paths, the processes of the
// attempt that still held them and were killed, and holders the attempt did not
// start, which were left running.
type Sealed struct {
	SourceRoot  string
	ScratchRoot string
	Killed      []int
	Foreign     []int
}

// survivors returns the PIDs of the caller's other processes that have an
// open file, cwd, root or mapped binary under any of the roots.
func survivors(roots []string) ([]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, lsofPath, "-w", "-n", "-P", "-F", "pn", "-u", strconv.Itoa(os.Getuid()))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	// The listing covers every process of the user, the caller included, so a
	// complete run always exits 0. Any other exit means some entries could not
	// be listed; a holder may be among them, and an open descriptor keeps
	// writing into a tree even after Seal renames it, so the scan fails closed.
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: incomplete listing: %v: %s", lsofPath, err, bytes.TrimSpace(errb.Bytes()))
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

// classify splits holders into the attempt's own processes and the others. A
// holder whose ancestry cannot be read is not the attempt's: it is refused,
// never killed. A holder that exited meanwhile is dropped.
func classify(pids []int, since time.Time) (mine, foreign []int) {
	for _, pid := range pids {
		ok, err := owned(pid, since)
		switch {
		case errors.Is(err, errNoProcess) || errors.Is(err, syscall.ESRCH):
		case err != nil || !ok:
			foreign = append(foreign, pid)
		default:
			mine = append(mine, pid)
		}
	}
	return mine, foreign
}

// reapRoots is the post-run sweep shared by Run and Seal. Processes that hold
// the roots and belong to the attempt that started at since are killed and
// reported. Processes the attempt did not start (an editor or a shell of the
// user, a monitoring tool) are never killed: they are reported and the sweep
// fails, so the attempt is refused instead.
func reapRoots(roots []string, since time.Time) (killed, foreign []int, err error) {
	pids, err := survivors(roots)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: survivor scan failed: %v", ErrUnavailable, err)
	}
	mine, foreign := classify(pids, since)
	deadline := time.Now().Add(3 * time.Second)
	for len(mine) > 0 {
		for _, pid := range mine {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			if !slices.Contains(killed, pid) {
				killed = append(killed, pid)
			}
		}
		if time.Now().After(deadline) {
			return killed, foreign, fmt.Errorf("%w: %d process(es) of the attempt still hold the roots after SIGKILL: %v", ErrUnavailable, len(mine), mine)
		}
		time.Sleep(50 * time.Millisecond)
		left, err := survivors(roots)
		if err != nil {
			return killed, foreign, fmt.Errorf("%w: survivor scan failed: %v", ErrUnavailable, err)
		}
		var more []int
		mine, more = classify(left, since)
		for _, pid := range more {
			if !slices.Contains(foreign, pid) {
				foreign = append(foreign, pid)
			}
		}
	}
	sort.Ints(killed)
	sort.Ints(foreign)
	if len(foreign) > 0 {
		return killed, foreign, fmt.Errorf("%w: %d process(es) this attempt did not start hold the roots; they were left running and the attempt is refused: %v", ErrUnavailable, len(foreign), foreign)
	}
	return killed, nil, nil
}

// Seal retires the writable roots of a policy once the attempt that started at
// since is over (Result.Started of its first Run on these roots). Both roots
// are renamed to sibling paths that no profile permits, so a process that
// escaped the group kill can no longer open anything in the trees; the
// attempt's processes still holding them are killed, and holders it did not
// start make Seal fail without being killed. Callers read outputs from the
// returned paths; the old paths must not be used again.
func (s *Seatbelt) Seal(p Policy, since time.Time) (Sealed, error) {
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
	sealed.Killed, sealed.Foreign, err = reapRoots([]string{sealed.SourceRoot, sealed.ScratchRoot}, since)
	return sealed, err
}
