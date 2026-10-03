//go:build linux

package probe

import (
	"os"
	"syscall"
)

func changeTime(fi os.FileInfo) (int64, uint64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ctim.Nano(), st.Ino
	}
	return 0, 0
}
