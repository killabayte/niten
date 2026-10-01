//go:build darwin

package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"syscall"
	"time"

	"github.com/killabayte/niten/internal/holders"
)

// survivors returns the processes that hold the roots (see package holders,
// which fails closed on an incomplete listing). Killing the child's process
// group is not enough on its own: a descendant can leave the group through
// posix_spawn attributes while keeping the Seatbelt profile.
func survivors(roots []string) ([]int, error) { return holders.List(roots) }

// classify splits holders into members of the attempt's process group and
// everything else. The group is the only proof of membership: the sandbox
// child is started as the leader of a new group, every descendant inherits it,
// and the profile denies setsid and setpgid, so a descendant cannot leave it
// through fork. A holder outside the group is not proven to be the attempt's,
// whatever its parent or start time, and is never killed: that includes a
// process the coordinator started meanwhile and a descendant that left the
// group through posix_spawn's POSIX_SPAWN_SETSID or SETPGROUP attributes,
// which the syscall filter does not see. Its presence refuses the attempt.
// A holder that exited meanwhile is dropped; one whose group cannot be read
// counts as foreign.
func classify(pids []int, group int) (members, foreign []int) {
	for _, pid := range pids {
		pg, err := procGroup(pid)
		switch {
		case errors.Is(err, errNoProcess) || errors.Is(err, syscall.ESRCH):
		case err == nil && group > 0 && pg == group:
			members = append(members, pid)
		default:
			foreign = append(foreign, pid)
		}
	}
	return members, foreign
}

// reapRoots is the sweep shared by Run and Seal. group is the attempt's
// process group, or 0 when the attempt is over and its group is gone. Members
// of the group that still hold the roots are killed with the whole group;
// every other holder is reported, left running, and fails the sweep.
func reapRoots(roots []string, group int) (members, foreign []int, err error) {
	pids, err := survivors(roots)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: survivor scan failed: %v", ErrUnavailable, err)
	}
	left, foreign := classify(pids, group)
	members = append(members, left...)
	deadline := time.Now().Add(3 * time.Second)
	for len(left) > 0 {
		_ = syscall.Kill(-group, syscall.SIGKILL)
		if time.Now().After(deadline) {
			return members, foreign, fmt.Errorf("%w: %d member(s) of the attempt's process group still hold the roots after SIGKILL: %v", ErrUnavailable, len(left), left)
		}
		time.Sleep(50 * time.Millisecond)
		pids, err := survivors(roots)
		if err != nil {
			return members, foreign, fmt.Errorf("%w: survivor scan failed: %v", ErrUnavailable, err)
		}
		var more []int
		left, more = classify(pids, group)
		for _, pid := range more {
			if !slices.Contains(foreign, pid) {
				foreign = append(foreign, pid)
			}
		}
	}
	sort.Ints(foreign)
	if len(foreign) > 0 {
		return members, foreign, fmt.Errorf("%w: %d process(es) outside the attempt's process group hold the roots; they were not killed and the attempt is refused: %v", ErrUnavailable, len(foreign), foreign)
	}
	return members, nil, nil
}

// Seal retires the writable roots of a policy once the attempt is over. Both
// roots are renamed to sibling paths that no profile permits, so a process
// that escaped the group can no longer open anything in the trees. Run has
// already killed and waited out the attempt's process group, so any process
// still holding the renamed trees is outside it: Seal reports it, does not
// kill it, and fails, and the trees must then not be used as evidence.
// Callers read outputs from the returned paths; the old paths must not be
// used again, and a new attempt must get new root paths.
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
	_, sealed.Foreign, err = reapRoots([]string{sealed.SourceRoot, sealed.ScratchRoot}, 0)
	return sealed, err
}
