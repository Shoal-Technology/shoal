package raftstore

import (
	"bytes"
	"context"
	"io"
	"testing"

	"example.com/shoal/internal/store"
	"example.com/shoal/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s, err := New(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

// bufSink is an in-memory raft.SnapshotSink.
type bufSink struct{ bytes.Buffer }

func (*bufSink) ID() string    { return "test" }
func (*bufSink) Cancel() error { return nil }
func (*bufSink) Close() error  { return nil }

func TestSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	a, err := s.Create(ctx, store.Record{Kind: "W", Name: "a", Spec: []byte("v1")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateStatus(ctx, "W", "a", []byte("up")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, store.Record{Kind: "N", Name: "b", Spec: []byte("x")}); err != nil {
		t.Fatal(err)
	}

	snap, err := s.fsm.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var sink bufSink
	if err := snap.Persist(&sink); err != nil {
		t.Fatal(err)
	}
	snap.Release()

	restored := newFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatal(err)
	}
	r := &Store{fsm: restored, mem: restored.mem}

	got, err := r.Get(ctx, "W", "a")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := s.Get(ctx, "W", "a")
	if got.UID != a.UID || got.UID != want.UID || got.Generation != want.Generation ||
		got.Revision != want.Revision || !got.CreatedAt.Equal(want.CreatedAt) ||
		string(got.Spec) != "v1" || string(got.Status) != "up" {
		t.Fatalf("restored W/a = %+v, want %+v", got, want)
	}
	if _, err := r.Get(ctx, "N", "b"); err != nil {
		t.Fatalf("restored N/b: %v", err)
	}
	if rev := restored.mem.Revision(); rev != s.mem.Revision() {
		t.Fatalf("restored revision = %d, want %d", rev, s.mem.Revision())
	}
}
