//go:build darwin

package probe

import (
	"os"
	"syscall"
)

// changeTime is the inode change time: no user process can set it, so an
// edit that restores a file's content still changes it.
func changeTime(fi os.FileInfo) (int64, uint64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ctimespec.Nano(), st.Ino
	}
	return 0, 0
}
