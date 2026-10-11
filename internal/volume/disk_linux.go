//go:build linux

package volume

import (
	"path/filepath"
	"syscall"
)

// DiskBytes returns the total size of the file system that holds path, or 0
// if it cannot be read. The node reports it as its disk capacity (ADR 0016,
// section 1.2): the scheduler counts Volume sizes against it.
func DiskBytes(path string) int64 {
	for p := path; ; p = filepath.Dir(p) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err == nil {
			return int64(st.Blocks) * st.Bsize
		}
		if filepath.Dir(p) == p {
			return 0
		}
	}
}
