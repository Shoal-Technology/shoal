package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
)

// RecordFormat versions the install record file.
const RecordFormat = "shoal-init/v1"

// Record is what init changed on this host. It lives in
// /var/lib/shoal/init.json and tells uninstall what init created. A containerd
// that existed before init has ContainerdOwned false and is never removed.
type Record struct {
	Format            string   `json:"format"`
	ContainerdOwned   bool     `json:"containerd_owned,omitempty"`
	ContainerdVersion string   `json:"containerd_version,omitempty"`
	CNIVersion        string   `json:"cni_version,omitempty"`
	Owned             []string `json:"owned,omitempty"`
	CAHash            string   `json:"ca_hash,omitempty"`
	Server            string   `json:"server,omitempty"`
	Joined            bool     `json:"joined,omitempty"`
	EdgeLabeled       bool     `json:"edge_labeled,omitempty"`
	SwapFile          bool     `json:"swap_file,omitempty"`
}

// Bundle is the join file written by `shoal token create --bundle` (format
// shoal-join/v1). It holds a single-use token, so it is a secret.
type Bundle struct {
	Format    string `json:"format"`
	Server    string `json:"server"`
	Token     string `json:"token"`
	CAHash    string `json:"ca_hash"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// BundleFormat is the only join bundle format this version reads.
const BundleFormat = "shoal-join/v1"

// ParseBundle checks and decodes a join bundle.
func ParseBundle(data []byte) (*Bundle, error) {
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("join bundle: %w", err)
	}
	if b.Format != BundleFormat {
		return nil, fmt.Errorf("join bundle: format %q, want %q", b.Format, BundleFormat)
	}
	if b.Server == "" || b.Token == "" || b.CAHash == "" {
		return nil, errors.New("join bundle: server, token and ca_hash are required")
	}
	return &b, nil
}

func loadRecord(ctx context.Context, h Host) (Record, bool, error) {
	data, err := h.ReadFile(pathRecord)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{Format: RecordFormat}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, false, fmt.Errorf("%s: %w", pathRecord, err)
	}
	return r, true, nil
}

func saveRecord(h Host, r Record) error {
	r.Format = RecordFormat
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return h.WriteFile(pathRecord, append(data, '\n'), 0o600)
}

var sizeRe = regexp.MustCompile(`^(\d+)([MmGg])$`)

// parseSizeMiB reads a size such as 512M or 1G (the swap flag).
func parseSizeMiB(s string) (uint64, error) {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("size %q: want a number with M or G, e.g. 1G", s)
	}
	n, _ := strconv.ParseUint(m[1], 10, 64)
	if strings.EqualFold(m[2], "G") {
		return n * 1024, nil
	}
	return n, nil
}

// swapSizeArg is the size as fallocate expects it.
func swapSizeArg(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }
