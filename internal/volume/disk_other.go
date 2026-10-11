//go:build !linux

package volume

// DiskBytes is 0 on systems without statfs support here: the node reports no
// disk capacity, so it takes no new Volumes.
func DiskBytes(string) int64 { return 0 }
