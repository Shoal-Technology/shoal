package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kaiserern/shoal/internal/store"
	"github.com/kaiserern/shoal/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), nil)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestReopenKeepsDataAndRevision(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(ctx, store.Record{Kind: "W", Name: "a", Spec: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Get(ctx, "W", "a")
	if err != nil || got.UID != r.UID || string(got.Spec) != "x" {
		t.Fatalf("after reopen: %+v, %v", got, err)
	}
	next, err := s.Create(ctx, store.Record{Kind: "W", Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision <= r.Revision {
		t.Fatalf("revision restarted after reopen: %d <= %d", next.Revision, r.Revision)
	}
}
