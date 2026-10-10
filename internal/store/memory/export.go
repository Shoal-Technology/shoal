package memory

import (
	"bytes"
	"cmp"
	"context"
	"slices"

	"example.com/shoal/internal/store"
)

// Export implements store.Exporter. Records are sorted by kind and name so
// the same state always yields the same dump.
func (s *Store) Export(ctx context.Context) (store.Dump, error) {
	if err := ctx.Err(); err != nil {
		return store.Dump{}, err
	}
	rev, recs := s.Dump()
	slices.SortFunc(recs, func(a, b store.Record) int {
		if c := cmp.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return store.Dump{Revision: rev, Records: recs}, nil
}

// Import implements store.Importer. The check and the load happen under one
// lock, so the store is either loaded or left untouched.
func (s *Store) Import(ctx context.Context, d store.Dump) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rev != 0 || len(s.records) != 0 {
		return store.ErrNotEmpty
	}
	s.rev = d.Revision
	for _, r := range d.Records {
		r.Spec, r.Status = bytes.Clone(r.Spec), bytes.Clone(r.Status)
		s.records[key{r.Kind, r.Name}] = r
	}
	return nil
}
