package install

import (
	"context"
	"fmt"
	"io/fs"
)

// Host is everything the install plan touches on the machine. The linux
// implementation is in realhost_linux.go; tests use a fake. Paths are logical
// absolute paths such as /usr/local/bin/shoal. A host created with a root
// directory prefixes every path with it (--root-dir, for tests).
type Host interface {
	// Facts reads the preflight facts. It must not change anything.
	Facts(ctx context.Context) (Facts, error)
	// ReadFile returns an error wrapping fs.ErrNotExist when the file is absent.
	ReadFile(path string) ([]byte, error)
	// Exists reports whether a file or directory is present.
	Exists(path string) bool
	// WriteFile replaces path atomically (temp file and rename), creating its directory.
	WriteFile(path string, data []byte, mode fs.FileMode) error
	// Remove deletes a file; a missing file is not an error.
	Remove(path string) error
	// RemoveAll deletes a directory tree; a missing path is not an error.
	RemoveAll(path string) error
	MkdirAll(path string, mode fs.FileMode) error
	// Chown gives path to the named user and group.
	Chown(path, user string) error
	// Fetch downloads url into memory. Dry runs never call it.
	Fetch(ctx context.Context, url string) ([]byte, error)
	// Run runs a command and returns its combined output. A non-zero exit is an error.
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// Facts are the read-only observations preflight and the plan base their
// decisions on. SHOAL_INIT_FAKE_HOST=FILE replaces all of them with the JSON in FILE.
type Facts struct {
	Root              bool              `json:"root"`
	Hostname          string            `json:"hostname"`
	OS                string            `json:"os"`
	Arch              string            `json:"arch"`
	Kernel            string            `json:"kernel"`
	SystemdRunning    bool              `json:"systemd_running"`
	Cgroup2           bool              `json:"cgroup2"`
	OverlayFS         bool              `json:"overlayfs"`
	BrNetfilter       bool              `json:"br_netfilter_loadable"`
	BrNetfilterLoaded bool              `json:"br_netfilter_loaded"`
	Sysctl            map[string]string `json:"sysctl,omitempty"`
	Tools             map[string]bool   `json:"tools,omitempty"`
	LoopControl       bool              `json:"loop_control"`
	MemTotalMiB       uint64            `json:"mem_total_mib"`
	SwapMiB           uint64            `json:"swap_mib"`
	DiskFreeMiB       uint64            `json:"disk_free_mib"`
	PortOwners        map[int]string    `json:"port_owners,omitempty"`
	ContainerdVersion string            `json:"containerd_version,omitempty"`
	ContainerdSocket  bool              `json:"containerd_socket"`
	UFW               string            `json:"ufw,omitempty"`
	IPv4              string            `json:"ipv4,omitempty"`
}

// ExitError carries the process exit code that `shoal init` and
// `shoal uninstall` return: 1 a step failed, 2 preflight failed (nothing
// changed), 3 partly applied (re-run to continue).
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode lets the command entry point pick the process exit status.
func (e *ExitError) ExitCode() int { return e.Code }

func errUnsupportedArch(arch string) error {
	return fmt.Errorf("unsupported architecture %q (want amd64 or arm64)", arch)
}
