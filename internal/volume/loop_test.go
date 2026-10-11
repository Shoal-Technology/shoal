package volume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeRunner records the commands the loop backend would run. reply decides
// the output and error of each one.
type fakeRunner struct {
	calls [][]string
	reply func(call []string) (string, error)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	if f.reply != nil {
		return f.reply(call)
	}
	return "", nil
}

// newTestLoop returns a loop manager whose mount table is *mounts and whose
// chown is recorded only.
func newTestLoop(t *testing.T, run *fakeRunner, mounts *string) *loopManager {
	t.Helper()
	m := newLoop(t.TempDir(), quiet, run)
	m.mountinfo = func() ([]byte, error) { return []byte(*mounts), nil }
	m.chown = func(string, int, int) error { return nil }
	return m
}

// mountLine is a mountinfo line for a mount at dir.
func mountLine(dir string) string {
	return fmt.Sprintf("36 35 7:0 / %s rw,relatime - ext4 /dev/loop7 rw\n", dir)
}

func defaultLoopReply(call []string) (string, error) {
	if call[0] == "losetup" && len(call) > 1 && call[1] == "--find" {
		return "/dev/loop7\n", nil
	}
	return "", nil
}

func TestLoopEnsureFormatsAttachesAndMounts(t *testing.T) {
	run := &fakeRunner{reply: defaultLoopReply}
	mounts := ""
	m := newTestLoop(t, run, &mounts)
	dir, err := m.Ensure(ctx, testSpec("pg-data", RetentionRetain))
	if err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(m.root, "pg-data.img")
	want := [][]string{
		{"mkfs.ext4", "-q", "-F", img},
		{"losetup", "--find", "--show", img},
		{"mount", "-o", "nosuid,nodev", "/dev/loop7", dir},
	}
	if !reflect.DeepEqual(run.calls, want) {
		t.Errorf("commands:\n got %q\nwant %q", run.calls, want)
	}
	st, err := os.Stat(img)
	if err != nil || st.Size() != 16<<20 {
		t.Errorf("image size = %v, %v; want 16Mi", st, err)
	}
}

func TestLoopEnsureDoesNothingWhenMounted(t *testing.T) {
	run := &fakeRunner{reply: defaultLoopReply}
	mounts := ""
	m := newTestLoop(t, run, &mounts)
	dir, err := m.Ensure(ctx, testSpec("pg-data", RetentionRetain))
	if err != nil {
		t.Fatal(err)
	}
	before := len(run.calls)
	mounts = mountLine(dir)
	if _, err := m.Ensure(ctx, testSpec("pg-data", RetentionRetain)); err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != before {
		t.Errorf("Ensure of a mounted volume ran %q", run.calls[before:])
	}
}

// After an agent restart the image exists but is not mounted: Ensure attaches
// and mounts it again, without formatting it.
func TestLoopEnsureRemountsAfterRestart(t *testing.T) {
	run := &fakeRunner{reply: defaultLoopReply}
	mounts := ""
	m := newTestLoop(t, run, &mounts)
	if _, err := m.Ensure(ctx, testSpec("pg-data", RetentionRetain)); err != nil {
		t.Fatal(err)
	}
	run.calls = nil // the agent restarts: mounts are gone, the image stays
	if _, err := m.Ensure(ctx, testSpec("pg-data", RetentionRetain)); err != nil {
		t.Fatal(err)
	}
	for _, c := range run.calls {
		if c[0] == "mkfs.ext4" {
			t.Errorf("re-formatted an existing volume: %q", run.calls)
		}
	}
	if len(run.calls) != 2 || run.calls[0][0] != "losetup" || run.calls[1][0] != "mount" {
		t.Errorf("remount commands = %q", run.calls)
	}
}

func TestLoopMountFailureDetachesTheDevice(t *testing.T) {
	run := &fakeRunner{reply: func(call []string) (string, error) {
		if call[0] == "mount" {
			return "", errors.New("mount: busy")
		}
		return defaultLoopReply(call)
	}}
	mounts := ""
	m := newTestLoop(t, run, &mounts)
	if _, err := m.Ensure(ctx, testSpec("pg-data", RetentionRetain)); err == nil {
		t.Fatal("Ensure succeeded though mount failed")
	}
	last := run.calls[len(run.calls)-1]
	if !reflect.DeepEqual(last, []string{"losetup", "-d", "/dev/loop7"}) {
		t.Errorf("last command = %q, want the device detached", last)
	}
}

// A failed format must not leave an image that the next Ensure would mount
// unformatted.
func TestLoopFailedFormatRemovesTheImage(t *testing.T) {
	run := &fakeRunner{reply: func(call []string) (string, error) {
		if call[0] == "mkfs.ext4" {
			return "", errors.New("mkfs failed")
		}
		return "", nil
	}}
	mounts := ""
	m := newTestLoop(t, run, &mounts)
	if _, err := m.Ensure(ctx, testSpec("pg-data", RetentionRetain)); err == nil {
		t.Fatal("Ensure succeeded though mkfs failed")
	}
	if _, err := os.Stat(filepath.Join(m.root, "pg-data.img")); !os.IsNotExist(err) {
		t.Errorf("image left behind: %v", err)
	}
}

func TestLoopRetireRetainUnmountsDetachesAndKeepsImage(t *testing.T) {
	run := &fakeRunner{reply: func(call []string) (string, error) {
		if call[0] == "losetup" && call[1] == "-j" {
			return "/dev/loop7: [0039]:1234 (" + call[2] + ")", nil
		}
		return defaultLoopReply(call)
	}}
	mounts := ""
	m := newTestLoop(t, run, &mounts)
	dir, err := m.Ensure(ctx, testSpec("pg-data", RetentionRetain))
	if err != nil {
		t.Fatal(err)
	}
	mounts = mountLine(dir)
	run.calls = nil
	if err := m.Retire(ctx, "pg-data"); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(m.root, "pg-data.img")
	want := [][]string{
		{"umount", dir},
		{"losetup", "-j", img},
		{"losetup", "-d", "/dev/loop7"},
	}
	if !reflect.DeepEqual(run.calls, want) {
		t.Errorf("commands:\n got %q\nwant %q", run.calls, want)
	}
	retained := retainedEntries(t, m.root)
	var images int
	for _, n := range retained {
		if strings.HasSuffix(n, ".img") {
			images++
		}
	}
	if images != 1 {
		t.Errorf("retained = %v, want one image", retained)
	}
	if _, err := os.Stat(img); !os.IsNotExist(err) {
		t.Errorf("image still in the volume root: %v", err)
	}
}

func TestLoopRetireDeleteRemovesImage(t *testing.T) {
	run := &fakeRunner{reply: func(call []string) (string, error) {
		if call[0] == "losetup" && call[1] == "-j" {
			return "/dev/loop7: [0039]:1234 (" + call[2] + ")", nil
		}
		return defaultLoopReply(call)
	}}
	mounts := ""
	m := newTestLoop(t, run, &mounts)
	if _, err := m.Ensure(ctx, testSpec("cache", RetentionDelete)); err != nil {
		t.Fatal(err)
	}
	if err := m.Retire(ctx, "cache"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.root, "cache.img")); !os.IsNotExist(err) {
		t.Errorf("image not removed: %v", err)
	}
	if got := retainedEntries(t, m.root); len(got) != 0 {
		t.Errorf("Delete retained %v", got)
	}
}

func TestMountinfoHasFindsEscapedMountPoints(t *testing.T) {
	data := []byte("1 0 7:0 / /var/lib/shoal/volumes/a\\040b rw - ext4 /dev/loop1 rw\n2 1 7:1 / /other rw - ext4 /dev/loop2 rw\n")
	if !mountinfoHas(data, "/var/lib/shoal/volumes/a b") {
		t.Error("escaped space not decoded")
	}
	if mountinfoHas(data, "/var/lib/shoal/volumes/a") {
		t.Error("prefix matched a longer mount point")
	}
	if mountinfoHas([]byte("garbage"), "/x") {
		t.Error("short line matched")
	}
}

func TestLoopMissingReportsTools(t *testing.T) {
	// The check itself depends on the host; it must only name real gaps.
	for _, m := range loopMissing() {
		if m == "" {
			t.Error("empty missing entry")
		}
	}
}
