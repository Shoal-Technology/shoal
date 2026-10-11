//go:build !unix

package nodestat

import "errors"

// Disk is not available on this platform; the disk metric is skipped.
func Disk(string) (total, free uint64, err error) {
	return 0, 0, errors.New("nodestat: disk usage not supported on this platform")
}
