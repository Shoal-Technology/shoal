package containerd

import (
	"context"
	"reflect"
	"testing"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/Shoal-Technology/shoal/internal/runtime"
)

// TestNoNewPrivilegesIsClearedOnlyForBuildDaemons starts from the spec
// containerd generates by default, which sets no_new_privs, and applies the
// options of each kind of container. Only a build daemon clears the flag, and
// it gains nothing else: the capability sets and the privileged mode stay as
// they are (ADR 0021).
func TestNoNewPrivilegesIsClearedOnlyForBuildDaemons(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "shoal-test")
	apply := func(spec runtime.ContainerSpec) (*oci.Spec, error) {
		c := &containers.Container{ID: spec.ID}
		s, err := oci.GenerateSpec(ctx, nil, c)
		if err != nil {
			return nil, err
		}
		for _, opt := range privilegeOpts(spec) {
			if err := opt(ctx, nil, c, s); err != nil {
				return nil, err
			}
		}
		return s, nil
	}

	defaults, err := oci.GenerateSpec(ctx, nil, &containers.Container{ID: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if !defaults.Process.NoNewPrivileges {
		t.Fatal("containerd's default spec does not set no_new_privs")
	}

	workload, err := apply(runtime.ContainerSpec{ID: "web", Image: "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	if !workload.Process.NoNewPrivileges {
		t.Fatal("a workload spec has no_new_privs cleared")
	}

	daemon, err := apply(runtime.ContainerSpec{ID: "shoal-build-web-abc12", AllowPrivilegeEscalation: true})
	if err != nil {
		t.Fatal(err)
	}
	if daemon.Process.NoNewPrivileges {
		t.Fatal("a build daemon spec keeps no_new_privs set")
	}
	// Privileged mode is not a flag in the OCI spec: it shows as extra
	// capabilities, host devices, unmasked paths or writable system paths.
	// None of them may differ from the defaults.
	if !reflect.DeepEqual(daemon.Process.Capabilities, defaults.Process.Capabilities) {
		t.Fatal("a build daemon spec changes its capability sets")
	}
	if !reflect.DeepEqual(daemon.Linux.Devices, defaults.Linux.Devices) {
		t.Fatal("a build daemon spec changes the devices of its container")
	}
	if !reflect.DeepEqual(daemon.Linux.MaskedPaths, defaults.Linux.MaskedPaths) ||
		!reflect.DeepEqual(daemon.Linux.ReadonlyPaths, defaults.Linux.ReadonlyPaths) {
		t.Fatal("a build daemon spec unmasks or unlocks system paths")
	}
	if daemon.Process.SelinuxLabel != defaults.Process.SelinuxLabel {
		t.Fatal("a build daemon spec changes its SELinux label")
	}
}

// TestBuildDaemonUsesItsOwnNetworkNamespace checks that the daemon's spec is
// pointed at the namespace pinned for its container, never left to the host's
// (ADR 0021). The firewall is applied to that namespace.
func TestBuildDaemonUsesItsOwnNetworkNamespace(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "shoal-test")
	s, err := oci.GenerateSpec(ctx, nil, &containers.Container{ID: "shoal-build-web-abc12"})
	if err != nil {
		t.Fatal(err)
	}
	const pinned = "/var/lib/shoal/netns/shoal-build-web-abc12"
	if !setNetNS(s, pinned) {
		t.Fatal("pinning the network namespace did not change the spec")
	}
	var got string
	count := 0
	for _, ns := range s.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace {
			got = ns.Path
			count++
		}
	}
	if count != 1 || got != pinned {
		t.Fatalf("network namespaces %d, path %q; want one, at %q", count, got, pinned)
	}
	if setNetNS(s, pinned) {
		t.Fatal("pinning the same namespace twice changed the spec")
	}
}
