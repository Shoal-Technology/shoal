// Package resource parses user-facing resource quantities such as "200m" CPU
// or "128Mi" memory into integer units used internally.
package resource

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseCPU parses a CPU quantity into millicores.
// Accepted forms: "200m" (millicores), "1", "0.5", "1.25" (cores, at most 3 decimals).
func ParseCPU(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("invalid cpu quantity %q: empty", s)
	}
	if milli, ok := strings.CutSuffix(s, "m"); ok {
		v, err := strconv.ParseInt(milli, 10, 64)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("invalid cpu quantity %q: want non-negative integer millicores", s)
		}
		return v, nil
	}

	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 3 {
		return 0, fmt.Errorf("invalid cpu quantity %q: at most 3 decimal places (use millicores)", s)
	}
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w < 0 || w > math.MaxInt64/1000 {
		return 0, fmt.Errorf("invalid cpu quantity %q", s)
	}
	var f int64
	if frac != "" {
		f, err = strconv.ParseInt(frac+strings.Repeat("0", 3-len(frac)), 10, 64)
		if err != nil || f < 0 {
			return 0, fmt.Errorf("invalid cpu quantity %q", s)
		}
	}
	return w*1000 + f, nil
}

var memorySuffixes = []struct {
	suffix string
	mult   float64
}{
	// Binary suffixes first so "Mi" is not mistaken for "M".
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
	{"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
}

// ParseMemory parses a memory quantity into bytes.
// Accepted forms: "1024" (bytes), "128Mi", "1.5Gi", "500M", "2G".
func ParseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("invalid memory quantity %q: empty", s)
	}
	num, mult := s, 1.0
	for _, m := range memorySuffixes {
		if n, ok := strings.CutSuffix(s, m.suffix); ok {
			num, mult = n, m.mult
			break
		}
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, fmt.Errorf("invalid memory quantity %q", s)
	}
	bytes := math.Round(v * mult)
	if bytes > math.MaxInt64/2 {
		return 0, fmt.Errorf("invalid memory quantity %q: too large", s)
	}
	return int64(bytes), nil
}
