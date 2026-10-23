//go:build linux

package containerd

import (
	"context"
	"slices"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/Shoal-Technology/shoal/internal/runtime"
)

// applyOpts runs spec options on a spec the way containerd does, without a
// daemon: each option only edits the spec.
func applyOpts(t *testing.T, s *specs.Spec, opts []oci.SpecOpts) {
	t.Helper()
	for _, o := range opts {
		if err := o(context.Background(), nil, nil, s); err != nil {
			t.Fatal(err)
		}
	}
}

func jobLikeSpec() *specs.Spec {
	return &specs.Spec{
		Root: &specs.Root{Path: "rootfs"},
		Process: &specs.Process{
			NoNewPrivileges: true,
			Capabilities: &specs.LinuxCapabilities{
				Bounding:    []string{"CAP_CHOWN", "CAP_NET_RAW"},
				Effective:   []string{"CAP_CHOWN", "CAP_NET_RAW"},
				Permitted:   []string{"CAP_CHOWN", "CAP_NET_RAW"},
				Inheritable: []string{"CAP_CHOWN", "CAP_NET_RAW"},
			},
		},
		Linux: &specs.Linux{},
	}
}

// TestHardeningDropsCapabilitiesAndLocksTheRootfs is the job baseline on the
// spec: no capabilities, a read-only root, a process limit, and no_new_privs
// left on.
func TestHardeningDropsCapabilitiesAndLocksTheRootfs(t *testing.T) {
	s := jobLikeSpec()
	applyOpts(t, s, hardeningOpts(runtime.ContainerSpec{ReadOnlyRootfs: true, DropAllCapabilities: true, PidsLimit: 64}))

	if !s.Root.Readonly {
		t.Error("the root filesystem is writable")
	}
	c := s.Process.Capabilities
	for name, set := range map[string][]string{"bounding": c.Bounding, "effective": c.Effective, "permitted": c.Permitted, "inheritable": c.Inheritable} {
		if len(set) != 0 {
			t.Errorf("%s capabilities = %v, want none", name, set)
		}
	}
	if !s.Process.NoNewPrivileges {
		t.Error("no_new_privs is off for a job")
	}
	if s.Linux.Resources == nil || s.Linux.Resources.Pids == nil || s.Linux.Resources.Pids.Limit == nil || *s.Linux.Resources.Pids.Limit != 64 {
		t.Errorf("pids limit = %+v, want 64", s.Linux.Resources)
	}
}

// TestHardeningIsEmptyByDefault: an ordinary workload keeps containerd's
// defaults, so existing containers are unchanged.
func TestHardeningIsEmptyByDefault(t *testing.T) {
	if n := len(hardeningOpts(runtime.ContainerSpec{})); n != 0 {
		t.Errorf("hardeningOpts for a plain container has %d options, want none", n)
	}
}

// TestNewPrivilegesStaysOnForJobs: only the build daemon clears no_new_privs.
func TestNewPrivilegesStaysOnForJobs(t *testing.T) {
	s := jobLikeSpec()
	applyOpts(t, s, privilegeOpts(runtime.ContainerSpec{ReadOnlyRootfs: true, DropAllCapabilities: true}))
	if !s.Process.NoNewPrivileges {
		t.Error("a job spec without AllowPrivilegeEscalation cleared no_new_privs")
	}
}

func TestTmpfsMountsAreWritableAndNotExecutable(t *testing.T) {
	m := tmpfsMounts([]string{"/tmp"})
	if len(m) != 1 || m[0].Destination != "/tmp" || m[0].Type != "tmpfs" {
		t.Fatalf("mounts = %+v, want one tmpfs at /tmp", m)
	}
	for _, opt := range []string{"nosuid", "nodev", "noexec"} {
		if !slices.Contains(m[0].Options, opt) {
			t.Errorf("tmpfs options = %v, want %s", m[0].Options, opt)
		}
	}
}
