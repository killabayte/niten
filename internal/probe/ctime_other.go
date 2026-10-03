//go:build !darwin && !linux

package probe

import "os"

func changeTime(fi os.FileInfo) (int64, uint64) { return 0, 0 }
