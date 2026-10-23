package storetest

import (
	"testing"

	"github.com/Shoal-Technology/shoal/internal/store"
)

// testRekey checks the store.Rekeyer contract: a move keeps the record's
// identity, a failed call changes nothing, and a put creates or replaces.
func testRekey(t *testing.T, s store.Store) {
	rk, ok := s.(store.Rekeyer)
	if !ok {
		t.Fatal("store does not implement store.Rekeyer")
	}
	a := mustCreate(t, s, "W", "old", "spec-a")
	mustCreate(t, s, "W", "taken", "spec-t")
	before, _ := s.List(ctx, "W")

	// Two moves to one name: the second conflicts, and the first is undone.
	err := rk.Rekey(ctx, []store.Move{
		{From: "old", Rec: store.Record{Kind: "W", Name: "fresh", UID: a.UID, Generation: a.Generation, CreatedAt: a.CreatedAt, Spec: []byte("spec-a")}},
		{From: "taken", Rec: store.Record{Kind: "W", Name: "fresh", Spec: []byte("x")}},
	}, nil)
	if err == nil {
		t.Fatal("rekey onto a taken name succeeded")
	}
	after, _ := s.List(ctx, "W")
	if len(after) != len(before) {
		t.Fatalf("failed rekey changed the store: %d records, want %d", len(after), len(before))
	}
	if _, err := s.Get(ctx, "W", "old"); err != nil {
		t.Fatalf("failed rekey removed the source: %v", err)
	}
	if _, err := s.Get(ctx, "W", "taken"); err != nil {
		t.Fatalf("failed rekey removed the other source: %v", err)
	}

	// A move of a missing record fails.
	if err := rk.Rekey(ctx, []store.Move{{From: "nope", Rec: store.Record{Kind: "W", Name: "x"}}}, nil); err == nil {
		t.Fatal("rekey of a missing record succeeded")
	}

	// A valid batch: move old to fresh, and put a marker.
	revBefore := a.Revision
	err = rk.Rekey(ctx, []store.Move{
		{From: "old", Rec: store.Record{Kind: "W", Name: "fresh", UID: a.UID, Generation: a.Generation, CreatedAt: a.CreatedAt, Spec: []byte("spec-b"), Labels: map[string]string{"k": "v"}}},
	}, []store.Record{{Kind: "M", Name: "schema", Status: []byte(`{"version":1}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "W", "old"); err == nil {
		t.Fatal("source still present after rekey")
	}
	got, err := s.Get(ctx, "W", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != a.UID || got.Generation != a.Generation || string(got.Spec) != "spec-b" || got.Labels["k"] != "v" {
		t.Fatalf("moved record lost its identity: %+v", got)
	}
	if got.Revision <= revBefore {
		t.Fatalf("revision did not advance: %d <= %d", got.Revision, revBefore)
	}
	m, err := s.Get(ctx, "M", "schema")
	if err != nil || string(m.Status) != `{"version":1}` {
		t.Fatalf("put did not create the marker: %+v, %v", m, err)
	}

	// A second put replaces the marker, keeping its UID.
	err = rk.Rekey(ctx, nil, []store.Record{{Kind: "M", Name: "schema", Status: []byte(`{"version":2}`)}})
	if err != nil {
		t.Fatal(err)
	}
	m2, _ := s.Get(ctx, "M", "schema")
	if m2.UID != m.UID || string(m2.Status) != `{"version":2}` {
		t.Fatalf("put did not replace the marker: %+v", m2)
	}
}
