// Package holders finds the processes that hold a directory tree: an open
// file, a working directory, a root or a mapped binary under it. It uses the
// fixed system lsof and fails closed: an incomplete listing is an error,
// never an empty answer, because a missed holder may keep writing.
package holders

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

var lsofPath = "/usr/sbin/lsof"

// SetLsofForTest replaces the lsof binary and returns a restore function.
func SetLsofForTest(path string) (restore func()) {
	old := lsofPath
	lsofPath = path
	return func() { lsofPath = old }
}

// List returns the PIDs of the caller's other processes that hold anything
// under the given absolute roots. The listing covers every process of the
// user, the caller included, so a complete run always exits 0; any other exit
// means some entries could not be listed and is an error.
func List(roots []string) ([]int, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, lsofPath, "-w", "-n", "-P", "-F", "pn", "-u", strconv.Itoa(os.Getuid()))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
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

// Available reports whether the lsof binary can be used.
func Available() error {
	st, err := os.Stat(lsofPath)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", lsofPath)
	}
	return nil
}
