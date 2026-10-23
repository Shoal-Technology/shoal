//go:build linux

package containerd

import (
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/containerd/containerd/v2/pkg/oci"

	"github.com/Shoal-Technology/shoal/internal/runtime"
)

// hardeningOpts returns the isolation settings of a container that asks for
// them (ADR 0022, section 5): a read-only root filesystem, no capabilities and
// a process limit. A container that does not ask keeps containerd's defaults.
// These options never clear no_new_privs: that is privilegeOpts' job, and only
// the build daemon asks for it.
func hardeningOpts(spec runtime.ContainerSpec) []oci.SpecOpts {
	var opts []oci.SpecOpts
	if spec.ReadOnlyRootfs {
		opts = append(opts, oci.WithRootFSReadonly())
	}
	if spec.DropAllCapabilities {
		opts = append(opts, oci.WithCapabilities(nil))
	}
	if spec.PidsLimit > 0 {
		opts = append(opts, oci.WithPidsLimit(spec.PidsLimit))
	}
	return opts
}

// tmpfsMounts returns one in-memory, empty, writable mount per path.
func tmpfsMounts(paths []string) []specs.Mount {
	var out []specs.Mount
	for _, p := range paths {
		out = append(out, specs.Mount{
			Destination: p, Type: "tmpfs", Source: "tmpfs",
			Options: []string{"nosuid", "nodev", "noexec", "mode=1777"},
		})
	}
	return out
}
