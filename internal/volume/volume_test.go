package volume

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

var (
	ctx   = context.Background()
	quiet = slog.New(slog.NewTextHandler(io.Discard, nil))
)

type chownCall struct {
	path     string
	uid, gid int
}

// newTestDir returns a dir manager on a temporary root. Chown is recorded
// rather than applied, so the tests run without root.
func newTestDir(t *testing.T) (*dirManager, *[]chownCall) {
	t.Helper()
	m := newDir(t.TempDir(), quiet)
	var calls []chownCall
	m.chown = func(p string, uid, gid int) error {
		calls = append(calls, chownCall{p, uid, gid})
		return nil
	}
	return m, &calls
}

func testSpec(name, retention string) Spec {
	return Spec{Name: name, Bytes: 16 << 20, UID: 999, GID: 999, Mode: 0o700, Retention: retention}
}

// retainedEntries lists what is under <root>/.retained.
func retainedEntries(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, retainedDir))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestDirEnsureCreatesOwnedDirectoryOnce(t *testing.T) {
	m, calls := newTestDir(t)
	dir, err := m.Ensure(ctx, testSpec("pg-data", RetentionRetain))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(m.root, "pg-data"); dir != want {
		t.Errorf("host path = %s, want %s", dir, want)
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		t.Fatalf("volume directory: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, want 0700", st.Mode().Perm())
	}
	if len(*calls) != 1 || (*calls)[0] != (chownCall{dir, 999, 999}) {
		t.Fatalf("chown calls = %v", *calls)
	}
	// Owner and mode are fixed at create: a second Ensure does not chown.
	if _, err := m.Ensure(ctx, testSpec("pg-data", RetentionRetain)); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Errorf("second Ensure chowned again: %v", *calls)
	}
}

func TestDirRetireRetainMovesDataAndPurgeRemovesIt(t *testing.T) {
	m, _ := newTestDir(t)
	dir, err := m.Ensure(ctx, testSpec("pg-data", RetentionRetain))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("16"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Retire(ctx, "pg-data"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("volume directory still in place: %v", err)
	}
	if known, _ := m.Known(); len(known) != 0 {
		t.Errorf("Known after retire = %v, want none", known)
	}
	entries := retainedEntries(t, m.root)
	if len(entries) == 0 {
		t.Fatal("nothing retained")
	}
	data, err := os.ReadFile(filepath.Join(m.root, retainedDir, entries[0], "PG_VERSION"))
	if err != nil || string(data) != "16" {
		t.Fatalf("retained data = %q, %v", data, err)
	}
	removed, err := Purge(m.root, "pg-data")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) == 0 || len(retainedEntries(t, m.root)) != 0 {
		t.Errorf("purge removed %v, left %v", removed, retainedEntries(t, m.root))
	}
}

func TestDirRetireDeleteRemovesDataAndRecord(t *testing.T) {
	m, _ := newTestDir(t)
	dir, err := m.Ensure(ctx, testSpec("cache", RetentionDelete))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Retire(ctx, "cache"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("directory still there: %v", err)
	}
	if got := retainedEntries(t, m.root); len(got) != 0 {
		t.Errorf("Delete retained %v", got)
	}
}

// A volume with no recorded retention keeps its data: the safe default.
func TestRetireWithoutRecordRetains(t *testing.T) {
	m, _ := newTestDir(t)
	dir := m.dirPath("orphan")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.Retire(ctx, "orphan"); err != nil {
		t.Fatal(err)
	}
	if got := retainedEntries(t, m.root); len(got) != 1 {
		t.Errorf("retained = %v, want the orphan's data", got)
	}
}

func TestPurgeMatchesOnlyTheNamedVolume(t *testing.T) {
	root := t.TempDir()
	ret := filepath.Join(root, retainedDir)
	if err := os.MkdirAll(ret, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"pg-data-1", "pg-1", "pg-data-2.meta.json", "pg-x-3", "pg-7.img"} {
		if err := os.WriteFile(filepath.Join(ret, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := Purge(root, "pg")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(removed)
	if want := []string{"pg-1", "pg-7.img"}; !slices.Equal(removed, want) {
		t.Errorf("purged %v, want %v", removed, want)
	}
	if _, err := os.Stat(filepath.Join(ret, "pg-data-1")); err != nil {
		t.Errorf("purge removed another volume's data: %v", err)
	}
}

func TestPurgeRejectsBadNames(t *testing.T) {
	for _, n := range []string{"", "../x", "a/b", "Upper"} {
		if _, err := Purge(t.TempDir(), n); err == nil {
			t.Errorf("Purge(%q) accepted", n)
		}
	}
}

func TestEnsureRejectsBadNames(t *testing.T) {
	m, _ := newTestDir(t)
	for _, n := range []string{"../escape", "a/b", "", "Pg"} {
		if _, err := m.Ensure(ctx, testSpec(n, RetentionRetain)); err == nil {
			t.Errorf("Ensure(%q) accepted", n)
		}
	}
}

func TestKnownListsVolumesWithRecords(t *testing.T) {
	m, _ := newTestDir(t)
	for _, n := range []string{"a", "b"} {
		if _, err := m.Ensure(ctx, testSpec(n, RetentionRetain)); err != nil {
			t.Fatal(err)
		}
	}
	known, err := m.Known()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(known)
	if !slices.Equal(known, []string{"a", "b"}) {
		t.Errorf("Known = %v", known)
	}
}

func TestDiskBytesReportsTheFileSystem(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("disk capacity is reported on Linux only")
	}
	if DiskBytes(t.TempDir()) <= 0 {
		t.Error("DiskBytes of a temp dir is 0")
	}
	if DiskBytes(filepath.Join(t.TempDir(), "missing", "deeper")) <= 0 {
		t.Error("DiskBytes walks up to an existing parent")
	}
}

func TestNewManagerRejectsUnknownBackend(t *testing.T) {
	if _, err := NewManager(t.TempDir(), "zfs", quiet); err == nil {
		t.Error("unknown backend accepted")
	}
	m, err := NewManager(t.TempDir(), BackendDir, quiet)
	if err != nil || m.Backend() != BackendDir {
		t.Errorf("dir backend: %v, %v", m, err)
	}
}
