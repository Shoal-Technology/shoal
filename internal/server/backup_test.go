package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/backup"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/store/memory"
)

// newWith builds a server without serving it. Restore tests need the store
// state as New left it, before leader tasks write anything.
func newWith(ctx context.Context, dir string, c *clock, opts ...func(*Config)) (*Server, error) {
	cfg := Config{
		DataDir: dir, AdvertiseAddress: "127.0.0.1:7443", Log: discard(), Now: c.now,
		HeartbeatInterval: 100 * time.Millisecond, NodeCheckInterval: time.Hour,
	}
	for _, o := range opts {
		o(&cfg)
	}
	return New(ctx, cfg)
}

// copySecretKey copies only the secret key, which is what an operator places
// in an empty data dir before a restore.
func copySecretKey(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(from, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(to, "secret.key"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// sameRecords compares a restored dump with the records in a backup.
func sameRecords(t *testing.T, got store.Dump, want []store.Record) {
	t.Helper()
	if len(got.Records) != len(want) {
		t.Fatalf("restored %d records, backup has %d", len(got.Records), len(want))
	}
	for i, w := range want {
		g := got.Records[i]
		if g.Kind != w.Kind || g.Name != w.Name || g.UID != w.UID || g.Generation != w.Generation ||
			g.Revision != w.Revision || !g.CreatedAt.Equal(w.CreatedAt) || !g.UpdatedAt.Equal(w.UpdatedAt) ||
			!bytes.Equal(g.Spec, w.Spec) || !bytes.Equal(g.Status, w.Status) {
			t.Errorf("record %s %q differs:\n got  %+v\n want %+v", w.Kind, w.Name, g, w)
		}
	}
}

// TestBackupRestoreRaftCluster takes a backup through a follower of a
// three-node cluster (it is forwarded to the leader), restores it into a
// fresh data dir and compares every record. Secrets must stay sealed in the
// file and decrypt with the same key after the restore.
func TestBackupRestoreRaftCluster(t *testing.T) {
	ctx := context.Background()
	dir1 := t.TempDir()
	n1 := startServer(t, dir1, &clock{}, raftOpts("node-1", ""))
	dir2 := t.TempDir()
	copyCluster(t, dir1, dir2)
	n2 := startServer(t, dir2, &clock{}, raftOpts("node-2", n1.addr))
	dir3 := t.TempDir()
	copyCluster(t, dir1, dir3)
	startServer(t, dir3, &clock{}, raftOpts("node-3", n1.addr))

	eventually(t, "leader record on node-2", func() bool {
		info, err := n2.ClusterLeader(ctx)
		return err == nil && info.APIAddress == n1.addr
	})
	if _, err := n1.store.Create(ctx, store.Record{Kind: "Widget", Name: "a", Spec: []byte(`{"v":1}`), Status: []byte(`{"ok":true}`)}); err != nil {
		t.Fatal(err)
	}
	b, err := n1.store.Create(ctx, store.Record{Kind: "Widget", Name: "b", Spec: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n1.store.Update(ctx, "Widget", "b", []byte(`{"v":2}`), b.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := n1.store.Create(ctx, store.Record{Kind: v1alpha1.KindSecret, Name: "db", Spec: []byte(`{"password":"hunter2"}`)}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "secret replicated to node-2", func() bool {
		r, err := n2.store.Get(ctx, v1alpha1.KindSecret, "db")
		return err == nil && string(r.Spec) == `{"password":"hunter2"}`
	})

	resp, err := forwardingClient(t, n2.addr, adminTLS(t, dir2)).Backup(ctx, &v1alpha1.BackupRequest{},
		grpc.MaxCallRecvMsgSize(1<<26))
	if err != nil {
		t.Fatalf("backup through follower: %v", err)
	}
	file := writeFile(t, filepath.Join(t.TempDir(), "cluster.backup.json"), resp.GetBackup())
	if bytes.Contains(resp.GetBackup(), []byte("hunter2")) {
		t.Fatal("backup holds a Secret in plaintext")
	}
	want, info, err := backup.Decode(resp.GetBackup())
	if err != nil {
		t.Fatal(err)
	}
	if info.RecordCount != len(want.Records) || want.Revision != resp.GetRevision() {
		t.Fatalf("backup info %+v does not match response revision %d", info, resp.GetRevision())
	}

	dirR := t.TempDir()
	copySecretKey(t, dir1, dirR)
	s, err := newWith(ctx, dirR, &clock{}, raftOpts("node-1", ""), func(c *Config) { c.RestoreFrom = file })
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	defer s.Close()
	got, err := s.exporter.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != want.Revision {
		t.Errorf("restored revision %d, backup %d", got.Revision, want.Revision)
	}
	sameRecords(t, got, want.Records)
	sec, err := s.store.Get(ctx, v1alpha1.KindSecret, "db")
	if err != nil || string(sec.Spec) != `{"password":"hunter2"}` {
		t.Fatalf("restored secret = %q, %v", sec.Spec, err)
	}
}

// TestBackupRestoreSqliteIntoRaft is the cross-backend case: a sqlite server's
// backup restores into a raft data dir.
func TestBackupRestoreSqliteIntoRaft(t *testing.T) {
	ctx := context.Background()
	dirS := t.TempDir()
	src := startServer(t, dirS, &clock{})
	if _, err := src.store.Create(ctx, store.Record{Kind: "Widget", Name: "a", Spec: []byte(`{"v":1}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.store.Create(ctx, store.Record{Kind: v1alpha1.KindSecret, Name: "k", Spec: []byte("s3cret")}); err != nil {
		t.Fatal(err)
	}
	resp, err := src.admin(t).Backup(ctx, &v1alpha1.BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	src.stop()
	want, _, err := backup.Decode(resp.GetBackup())
	if err != nil {
		t.Fatal(err)
	}
	file := writeFile(t, filepath.Join(t.TempDir(), "sqlite.backup.json"), resp.GetBackup())

	dirR := t.TempDir()
	copySecretKey(t, dirS, dirR)
	s, err := newWith(ctx, dirR, &clock{}, raftOpts("node-1", ""), func(c *Config) { c.RestoreFrom = file })
	if err != nil {
		t.Fatalf("restore into raft: %v", err)
	}
	defer s.Close()
	got, err := s.exporter.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sameRecords(t, got, want.Records)
}

// TestRestoreRefusals covers the checks that run before anything is written:
// a damaged file, a data dir with state, a wrong key and a missing key.
func TestRestoreRefusals(t *testing.T) {
	ctx := context.Background()
	key1 := bytes.Repeat([]byte{1}, 32)
	key2 := bytes.Repeat([]byte{2}, 32)
	mem := memory.New(nil)
	enc, err := store.NewEncryptingStore(mem, key1, v1alpha1.KindSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Create(ctx, store.Record{Kind: "Widget", Name: "a", Spec: []byte("1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Create(ctx, store.Record{Kind: v1alpha1.KindSecret, Name: "k", Spec: []byte("s3cret")}); err != nil {
		t.Fatal(err)
	}
	exp, err := mem.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := backup.Encode(exp, "v", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	file := writeFile(t, filepath.Join(t.TempDir(), "b.json"), data)
	tampered := writeFile(t, filepath.Join(t.TempDir(), "t.json"),
		bytes.Replace(data, []byte(`"name": "a"`), []byte(`"name": "z"`), 1))

	restore := func(dir, file string) func(*Config) {
		return func(c *Config) { c.RestoreFrom = file }
	}
	c := &clock{}
	withKey := func(dir string, key []byte) {
		if err := os.WriteFile(filepath.Join(dir, "secret.key"), key, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("bad sha256 is refused and nothing is written", func(t *testing.T) {
		dir := t.TempDir()
		withKey(dir, key1)
		_, err := newWith(ctx, dir, c, raftOpts("node-1", ""), restore(dir, tampered))
		if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
			t.Fatalf("error = %v, want sha256 mismatch", err)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 || entries[0].Name() != "secret.key" {
			t.Fatalf("data dir changed: %v", entries)
		}
	})
	t.Run("non-empty data dir is refused", func(t *testing.T) {
		dir := t.TempDir()
		withKey(dir, key1)
		writeFile(t, filepath.Join(dir, "state.db"), []byte("x"))
		_, err := newWith(ctx, dir, c, raftOpts("node-1", ""), restore(dir, file))
		if err == nil || !strings.Contains(err.Error(), "empty data dir") {
			t.Fatalf("error = %v, want empty data dir refusal", err)
		}
	})
	t.Run("wrong secret key is refused", func(t *testing.T) {
		dir := t.TempDir()
		withKey(dir, key2)
		_, err := newWith(ctx, dir, c, raftOpts("node-1", ""), restore(dir, file))
		if err == nil || !strings.Contains(err.Error(), "different key") {
			t.Fatalf("error = %v, want wrong key refusal", err)
		}
	})
	t.Run("missing key with Secrets is refused", func(t *testing.T) {
		dir := t.TempDir()
		_, err := newWith(ctx, dir, c, raftOpts("node-1", ""), restore(dir, file))
		if err == nil || !strings.Contains(err.Error(), "copy the secret.key") {
			t.Fatalf("error = %v, want missing key refusal", err)
		}
	})
}
