//go:build linux

package containerd

import (
	"slices"
	"testing"

	"github.com/kaiserern/shoal/internal/runtime"
)

// The runtime mount spec of a read-write volume is a bind mount with rw, and
// the read-only mounts of Secrets and Configs stay ro (ADR 0016, 1.3).
func TestBindMountsIncludeReadWriteVolume(t *testing.T) {
	got := bindMounts([]runtime.Mount{
		{Source: "/state/volumes/pg-data", Destination: "/var/lib/postgresql/data", ReadWrite: true},
		{Source: "/state/files/r1/0", Destination: "/etc/app"},
	})
	if len(got) != 2 {
		t.Fatalf("got %d mounts, want 2: %+v", len(got), got)
	}
	vol := got[0]
	if vol.Type != "bind" || vol.Source != "/state/volumes/pg-data" || vol.Destination != "/var/lib/postgresql/data" {
		t.Errorf("volume mount = %+v", vol)
	}
	want := []string{"rbind", "rw", "nosuid", "nodev"}
	if !slices.Equal(vol.Options, want) {
		t.Errorf("volume options = %v, want %v", vol.Options, want)
	}
	if file := got[1]; !slices.Equal(file.Options, []string{"rbind", "ro", "nosuid", "nodev"}) {
		t.Errorf("file mount options = %v, want read-only", file.Options)
	}
	if slices.Contains(vol.Options, "noexec") {
		t.Error("volume mount must not set noexec")
	}
}
