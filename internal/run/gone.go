package run

import (
	"os"
	"syscall"
)

// deleted reports that an open file was unlinked: its link count is 0. A
// re-created file can reuse the inode number (ext4 does), so comparing
// inodes alone misses a replaced store; the open handle does not.
func deleted(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return true
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Nlink == 0
}
