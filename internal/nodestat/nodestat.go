// Package nodestat reads host memory and disk usage for the agent's metrics.
// Memory comes from /proc/meminfo, so it is available on Linux only; callers
// skip the metric when it returns an error.
package nodestat

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// Memory returns total and available memory in bytes.
func Memory() (total, available uint64, err error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	return parseMeminfo(string(b))
}

func parseMeminfo(s string) (total, available uint64, err error) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		var dst *uint64
		switch f[0] {
		case "MemTotal:":
			dst = &total
		case "MemAvailable:":
			dst = &available
		default:
			continue
		}
		kb, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return 0, 0, err
		}
		*dst = kb * 1024
	}
	if total == 0 {
		return 0, 0, errors.New("nodestat: MemTotal not found in meminfo")
	}
	return total, available, nil
}
