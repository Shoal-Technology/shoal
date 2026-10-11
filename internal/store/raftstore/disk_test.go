package raftstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/Shoal-Technology/shoal/internal/store"
)

// openReady opens a durable node and waits until it is ready to serve.
func openReady(t *testing.T, cfg DiskConfig) *Store {
	t.Helper()
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.WaitReady(ctx); err != nil {
		_ = s.Close()
		t.Fatalf("WaitReady: %v", err)
	}
	return s
}

// createRecords writes records [from, to) and returns them by name.
func createRecords(t *testing.T, s *Store, from, to int, into map[string]store.Record) {
	t.Helper()
	for i := from; i < to; i++ {
		name := fmt.Sprintf("r%02d", i)
		rec, err := s.Create(context.Background(), store.Record{Kind: "W", Name: name, Spec: []byte("v")})
		if err != nil {
			t.Fatal(err)
		}
		into[name] = rec
	}
}

// checkRecords asserts that s holds exactly want, with the same metadata.
func checkRecords(t *testing.T, s *Store, want map[string]store.Record) {
	t.Helper()
	ctx := context.Background()
	got, err := s.List(ctx, "W")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("after restart: %d records, want %d", len(got), len(want))
	}
	for name, w := range want {
		g, err := s.Get(ctx, "W", name)
		if err != nil {
			t.Fatalf("Get %s: %v", name, err)
		}
		if g.UID != w.UID || g.Generation != w.Generation || g.Revision != w.Revision {
			t.Fatalf("%s after restart = uid %s gen %d rev %d, want uid %s gen %d rev %d",
				name, g.UID, g.Generation, g.Revision, w.UID, w.Generation, w.Revision)
		}
	}
}

// TestDiskRestart writes through a snapshot and then more records, restarts
// the node from disk, and checks that all records survive with their
// metadata. The snapshot restore and the log replay after it are both
// exercised.
func TestDiskRestart(t *testing.T) {
	cfg := DiskConfig{DataDir: t.TempDir(), NodeID: "n1", BindAddr: "127.0.0.1:0"}
	want := map[string]store.Record{}

	s := openReady(t, cfg)
	createRecords(t, s, 0, 50, want)
	if err := s.raft.Snapshot().Error(); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(cfg.DataDir, "snapshots")); len(entries) == 0 {
		t.Fatal("no snapshot written to disk")
	}
	createRecords(t, s, 50, 55, want)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openReady(t, cfg)
	defer s.Close()
	checkRecords(t, s, want)
}

// TestDiskSnapshotOnlyRestart truncates the log right after a snapshot
// (TrailingLogs 0), so the restart can only recover the records from the
// snapshot file, not from log replay.
func TestDiskSnapshotOnlyRestart(t *testing.T) {
	rc := raft.DefaultConfig()
	rc.TrailingLogs = 0
	cfg := DiskConfig{DataDir: t.TempDir(), NodeID: "n1", BindAddr: "127.0.0.1:0", Raft: rc}
	want := map[string]store.Record{}

	s := openReady(t, cfg)
	createRecords(t, s, 0, 20, want)
	if err := s.raft.Snapshot().Error(); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openReady(t, cfg)
	defer s.Close()
	checkRecords(t, s, want)
}
