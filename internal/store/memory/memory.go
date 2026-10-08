// Package memory is an in-memory store.Store. It passes the same
// conformance suite as the real backends and is used as the fake store in
// tests.
package memory

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"example.com/shoal/internal/store"
)

type key struct{ kind, name string }

// Store is an in-memory store.Store.
type Store struct {
	now func() time.Time
	hub store.Hub

	mu      sync.Mutex
	rev     int64
	records map[key]store.Record
}

var _ store.Store = (*Store)(nil)

// New returns an empty store. now may be nil.
func New(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{now: now, records: map[key]store.Record{}}
}

// Revision returns the current global revision; tests use it to check that
// a pass made no writes.
func (s *Store) Revision() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rev
}

func clone(r store.Record) store.Record {
	r.Spec, r.Status = bytes.Clone(r.Spec), bytes.Clone(r.Status)
	return r
}

func (s *Store) Get(_ context.Context, kind, name string) (store.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[key{kind, name}]
	if !ok {
		return store.Record{}, fmt.Errorf("%s %q: %w", kind, name, store.ErrNotFound)
	}
	return clone(r), nil
}

func (s *Store) List(_ context.Context, kind string) ([]store.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Record
	for _, k := range slices.SortedFunc(maps.Keys(s.records), func(a, b key) int {
		return cmp.Compare(a.name, b.name)
	}) {
		if k.kind == kind {
			out = append(out, clone(s.records[k]))
		}
	}
	return out, nil
}

// write applies fn under the lock with the next revision and publishes the
// event after unlocking. A failed fn doesn't consume a revision.
func (s *Store) write(fn func(rev int64, now time.Time) (store.Event, error)) error {
	s.mu.Lock()
	ev, err := fn(s.rev+1, s.now().UTC())
	if err == nil {
		s.rev++
	}
	s.mu.Unlock()
	if err == nil {
		s.hub.Publish(ev)
	}
	return err
}

func (s *Store) Create(_ context.Context, rec store.Record) (store.Record, error) {
	var out store.Record
	err := s.write(func(rev int64, now time.Time) (store.Event, error) {
		k := key{rec.Kind, rec.Name}
		if _, ok := s.records[k]; ok {
			return store.Event{}, fmt.Errorf("%s %q: %w", rec.Kind, rec.Name, store.ErrExists)
		}
		out = store.Record{
			Kind: rec.Kind, Name: rec.Name, UID: store.NewUID(), Generation: 1, Revision: rev,
			CreatedAt: now, UpdatedAt: now, Spec: bytes.Clone(rec.Spec), Status: bytes.Clone(rec.Status),
		}
		s.records[k] = out
		return store.Event{Kind: rec.Kind, Name: rec.Name, Revision: rev}, nil
	})
	return clone(out), err
}

func (s *Store) Update(_ context.Context, kind, name string, spec []byte, generation int64) (store.Record, error) {
	var out store.Record
	err := s.write(func(rev int64, now time.Time) (store.Event, error) {
		k := key{kind, name}
		cur, ok := s.records[k]
		if !ok {
			return store.Event{}, fmt.Errorf("%s %q: %w", kind, name, store.ErrNotFound)
		}
		if cur.Generation != generation {
			return store.Event{}, fmt.Errorf("%s %q: generation is %d, not %d: %w", kind, name, cur.Generation, generation, store.ErrConflict)
		}
		if !bytes.Equal(cur.Spec, spec) {
			cur.Generation++
		}
		cur.Spec, cur.Revision, cur.UpdatedAt = bytes.Clone(spec), rev, now
		s.records[k] = cur
		out = cur
		return store.Event{Kind: kind, Name: name, Revision: rev}, nil
	})
	return clone(out), err
}

func (s *Store) UpdateStatus(_ context.Context, kind, name string, status []byte) (store.Record, error) {
	var out store.Record
	err := s.write(func(rev int64, now time.Time) (store.Event, error) {
		k := key{kind, name}
		cur, ok := s.records[k]
		if !ok {
			return store.Event{}, fmt.Errorf("%s %q: %w", kind, name, store.ErrNotFound)
		}
		cur.Status, cur.Revision, cur.UpdatedAt = bytes.Clone(status), rev, now
		s.records[k] = cur
		out = cur
		return store.Event{Kind: kind, Name: name, Revision: rev}, nil
	})
	return clone(out), err
}

func (s *Store) Delete(_ context.Context, kind, name string, generation int64) error {
	return s.write(func(rev int64, _ time.Time) (store.Event, error) {
		k := key{kind, name}
		cur, ok := s.records[k]
		if !ok {
			return store.Event{}, fmt.Errorf("%s %q: %w", kind, name, store.ErrNotFound)
		}
		if generation != 0 && cur.Generation != generation {
			return store.Event{}, fmt.Errorf("%s %q: generation is %d, not %d: %w", kind, name, cur.Generation, generation, store.ErrConflict)
		}
		delete(s.records, k)
		return store.Event{Kind: kind, Name: name, Revision: rev, Deleted: true}, nil
	})
}

func (s *Store) Watch(ctx context.Context, kinds ...string) <-chan store.Event {
	return s.hub.Subscribe(ctx, kinds...)
}

func (s *Store) Close() error { return nil }
