//go:build linux

package containerd

import (
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"example.com/shoal/internal/runtime"
)

// bindMounts returns the OCI bind mounts of a container's host directories.
// They are read-only unless ReadWrite is set (ADR 0016, section 1.3). The
// source is the mount's own directory, never a parent, so a container cannot
// walk into other volumes. noexec is not set, so images that run from their
// data directory keep working.
func bindMounts(ms []runtime.Mount) []specs.Mount {
	out := make([]specs.Mount, 0, len(ms))
	for _, m := range ms {
		mode := "ro"
		if m.ReadWrite {
			mode = "rw"
		}
		out = append(out, specs.Mount{
			Destination: m.Destination, Type: "bind", Source: m.Source,
			Options: []string{"rbind", mode, "nosuid", "nodev"},
		})
	}
	return out
}
