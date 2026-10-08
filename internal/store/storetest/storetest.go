// Package storetest is the conformance suite for store.Store
// implementations. Every backend must pass it (ADR 0002).
package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"example.com/shoal/internal/store"
)

// Run runs the suite. newStore must return an empty store; it is closed by
// the suite.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s store.Store)
	}{
		{"CreateAndGet", testCreateAndGet},
		{"CreateExisting", testCreateExisting},
		{"GetMissing", testGetMissing},
		{"RevisionIsGlobalAndIncreasing", testRevision},
		{"UpdateCompareAndSwap", testUpdate},
		{"StatusDoesNotConflictWithSpec", testStatus},
		{"Delete", testDelete},
		{"ListSortedByKind", testList},
		{"Watch", testWatch},
		{"ConcurrentUpdates", testConcurrentUpdates},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			defer s.Close()
			tt.fn(t, s)
		})
	}
}

var ctx = context.Background()

func mustCreate(t *testing.T, s store.Store, kind, name, spec string) store.Record {
	t.Helper()
	r, err := s.Create(ctx, store.Record{Kind: kind, Name: name, Spec: []byte(spec)})
	if err != nil {
		t.Fatalf("Create(%s, %s): %v", kind, name, err)
	}
	return r
}

func testCreateAndGet(t *testing.T, s store.Store) {
	created := mustCreate(t, s, "W", "a", "spec-1")
	if created.UID == "" || created.Generation != 1 || created.Revision <= 0 || created.CreatedAt.IsZero() ||
		!created.UpdatedAt.Equal(created.CreatedAt) {
		t.Fatalf("created record has unset metadata: %+v", created)
	}
	got, err := s.Get(ctx, "W", "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != created.UID || got.Generation != 1 || got.Revision != created.Revision ||
		string(got.Spec) != "spec-1" || len(got.Status) != 0 || !got.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("Get = %+v, want %+v", got, created)
	}
}

func testCreateExisting(t *testing.T, s store.Store) {
	first := mustCreate(t, s, "W", "a", "one")
	if _, err := s.Create(ctx, store.Record{Kind: "W", Name: "a", Spec: []byte("two")}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("second Create: err = %v, want ErrExists", err)
	}
	got, _ := s.Get(ctx, "W", "a")
	if string(got.Spec) != "one" || got.UID != first.UID {
		t.Fatalf("failed Create modified the record: %+v", got)
	}
	// The same name in another kind is a different record.
	mustCreate(t, s, "N", "a", "other")
}

func testGetMissing(t *testing.T, s store.Store) {
	if _, err := s.Get(ctx, "W", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := s.Update(ctx, "W", "nope", nil, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Update: err = %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateStatus(ctx, "W", "nope", nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateStatus: err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "W", "nope", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Delete: err = %v, want ErrNotFound", err)
	}
}

func testRevision(t *testing.T, s store.Store) {
	a := mustCreate(t, s, "W", "a", "x")
	b := mustCreate(t, s, "N", "b", "x")
	c, err := s.UpdateStatus(ctx, "W", "a", []byte("st"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Update(ctx, "N", "b", []byte("y"), b.Generation)
	if err != nil {
		t.Fatal(err)
	}
	revs := []int64{a.Revision, b.Revision, c.Revision, d.Revision}
	for i := 1; i < len(revs); i++ {
		if revs[i] <= revs[i-1] {
			t.Fatalf("revisions not strictly increasing across kinds: %v", revs)
		}
	}
	// Failed writes don't consume visible state.
	if _, err := s.Update(ctx, "N", "b", []byte("z"), 99); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	e := mustCreate(t, s, "W", "e", "x")
	if e.Revision <= d.Revision {
		t.Fatalf("revision went backwards: %d after %d", e.Revision, d.Revision)
	}
}

func testUpdate(t *testing.T, s store.Store) {
	r := mustCreate(t, s, "W", "a", "v1")
	if _, err := s.UpdateStatus(ctx, "W", "a", []byte("status")); err != nil {
		t.Fatal(err)
	}

	r2, err := s.Update(ctx, "W", "a", []byte("v2"), r.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Generation != 2 || string(r2.Spec) != "v2" || string(r2.Status) != "status" || r2.UID != r.UID {
		t.Fatalf("after spec change: %+v", r2)
	}
	if _, err := s.Update(ctx, "W", "a", []byte("v3"), r.Generation); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale generation: err = %v, want ErrConflict", err)
	}
	r3, err := s.Update(ctx, "W", "a", []byte("v2"), r2.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if r3.Generation != 2 {
		t.Fatalf("unchanged spec bumped generation to %d", r3.Generation)
	}
	got, _ := s.Get(ctx, "W", "a")
	if got.Generation != 2 || got.Revision != r3.Revision || string(got.Spec) != "v2" || string(got.Status) != "status" {
		t.Fatalf("Get after updates = %+v", got)
	}
}

func testStatus(t *testing.T, s store.Store) {
	r := mustCreate(t, s, "W", "a", "v1")
	st, err := s.UpdateStatus(ctx, "W", "a", []byte("s1"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Generation != r.Generation || string(st.Spec) != "v1" || st.Revision <= r.Revision {
		t.Fatalf("UpdateStatus = %+v", st)
	}
	// A spec writer that read the record before the status write still
	// succeeds: status writes don't change the generation.
	if _, err := s.Update(ctx, "W", "a", []byte("v2"), r.Generation); err != nil {
		t.Fatalf("Update after concurrent status write: %v", err)
	}
}

func testDelete(t *testing.T, s store.Store) {
	r := mustCreate(t, s, "W", "a", "v1")
	if err := s.Delete(ctx, "W", "a", r.Generation+1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Delete with wrong generation: err = %v, want ErrConflict", err)
	}
	if err := s.Delete(ctx, "W", "a", r.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "W", "a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get after Delete: err = %v, want ErrNotFound", err)
	}
	// Re-creating gives a new identity.
	if again := mustCreate(t, s, "W", "a", "v1"); again.UID == r.UID || again.Generation != 1 {
		t.Fatalf("re-created record = %+v", again)
	}
	if err := s.Delete(ctx, "W", "a", 0); err != nil {
		t.Fatalf("Delete with generation 0: %v", err)
	}
}

func testList(t *testing.T, s store.Store) {
	for _, n := range []string{"c", "a", "b"} {
		mustCreate(t, s, "W", n, n)
	}
	mustCreate(t, s, "N", "x", "x")
	got, err := s.List(ctx, "W")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range got {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{"a", "b", "c"}) {
		t.Fatalf("List(W) names = %v, want [a b c]", names)
	}
	if empty, err := s.List(ctx, "Nothing"); err != nil || len(empty) != 0 {
		t.Fatalf("List of empty kind = %v, %v", empty, err)
	}
}

func testWatch(t *testing.T, s store.Store) {
	wctx, cancel := context.WithCancel(ctx)
	events := s.Watch(wctx, "W")

	r := mustCreate(t, s, "W", "a", "v1")
	mustCreate(t, s, "N", "ignored", "x")
	if _, err := s.Update(ctx, "W", "a", []byte("v2"), r.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateStatus(ctx, "W", "a", []byte("s")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "W", "a", 0); err != nil {
		t.Fatal(err)
	}

	var got []string
	timeout := time.After(5 * time.Second)
	for len(got) < 4 {
		select {
		case e := <-events:
			got = append(got, fmt.Sprintf("%s/%s deleted=%v", e.Kind, e.Name, e.Deleted))
		case <-timeout:
			t.Fatalf("timed out; got %v", got)
		}
	}
	want := []string{"W/a deleted=false", "W/a deleted=false", "W/a deleted=false", "W/a deleted=true"}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}

	cancel()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("unexpected event after cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch channel not closed after cancel")
	}
}

// testConcurrentUpdates runs read-modify-write loops in parallel. With a
// correct compare-and-swap no increment is lost.
func testConcurrentUpdates(t *testing.T, s store.Store) {
	mustCreate(t, s, "W", "counter", "0")
	const workers, perWorker = 8, 10
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				for {
					r, err := s.Get(ctx, "W", "counter")
					if err != nil {
						errs <- err
						return
					}
					n, _ := strconv.Atoi(string(r.Spec))
					_, err = s.Update(ctx, "W", "counter", []byte(strconv.Itoa(n+1)), r.Generation)
					if errors.Is(err, store.ErrConflict) {
						continue
					}
					if err != nil {
						errs <- err
						return
					}
					break
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	r, err := s.Get(ctx, "W", "counter")
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Spec) != strconv.Itoa(workers*perWorker) || r.Generation != workers*perWorker+1 {
		t.Fatalf("counter = %s (generation %d), want %d", r.Spec, r.Generation, workers*perWorker)
	}
}
