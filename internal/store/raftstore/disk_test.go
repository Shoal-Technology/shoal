package raftstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/shoal/internal/store"
)

// TestDiskRestart writes through a snapshot and then more records, restarts
// the node from disk, and checks that all records survive with their
// metadata. The snapshot restore and the log replay after it are both
// exercised.
func TestDiskRestart(t *testing.T) {
	ctx := context.Background()
	cfg := DiskConfig{DataDir: t.TempDir(), NodeID: "n1", BindAddr: "127.0.0.1:0"}

	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	waitLeader(t, []*Store{s})

	want := map[string]store.Record{}
	write := func(from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			name := fmt.Sprintf("r%02d", i)
			rec, err := s.Create(ctx, store.Record{Kind: "W", Name: name, Spec: []byte("v")})
			if err != nil {
				t.Fatal(err)
			}
			want[name] = rec
		}
	}
	write(0, 50)
	if err := s.raft.Snapshot().Error(); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(cfg.DataDir, "snapshots")); len(entries) == 0 {
		t.Fatal("no snapshot written to disk")
	}
	write(50, 55)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	waitLeader(t, []*Store{s})
	// Leadership doesn't mean the FSM has applied the log tail replayed
	// from disk yet. A barrier waits until everything committed is applied.
	if err := s.raft.Barrier(10 * time.Second).Error(); err != nil {
		t.Fatalf("barrier: %v", err)
	}

	got, err := s.List(ctx, "W")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 55 {
		t.Fatalf("after restart: %d records, want 55", len(got))
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
