package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/Shoal-Technology/shoal/internal/store"
)

// Rekey implements store.Rekeyer. Everything is checked before anything
// changes, so a failed call leaves the store as it was.
func (s *Store) Rekey(ctx context.Context, moves []store.Move, puts []store.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.writeAll(func(rev int64, now time.Time) ([]store.Event, error) {
		from := map[key]bool{}
		for _, m := range moves {
			k := key{m.Rec.Kind, m.From}
			if _, ok := s.records[k]; !ok {
				return nil, fmt.Errorf("rekey %s %q: %w", m.Rec.Kind, m.From, store.ErrNotFound)
			}
			if m.From == m.Rec.Name {
				return nil, fmt.Errorf("rekey %s %q: the new name is the old one", m.Rec.Kind, m.From)
			}
			from[k] = true
		}
		targets := map[key]bool{}
		for _, m := range moves {
			to := key{m.Rec.Kind, m.Rec.Name}
			_, taken := s.records[to]
			if (taken && !from[to]) || targets[to] {
				return nil, fmt.Errorf("rekey %s %q: %w", m.Rec.Kind, m.Rec.Name, store.ErrExists)
			}
			targets[to] = true
		}
		var evs []store.Event
		for _, m := range moves {
			delete(s.records, key{m.Rec.Kind, m.From})
			evs = append(evs, store.Event{Kind: m.Rec.Kind, Name: m.From, Revision: rev, Deleted: true})
		}
		for _, m := range moves {
			r := clone(m.Rec)
			r.Revision, r.UpdatedAt = rev, now
			s.records[key{r.Kind, r.Name}] = r
			evs = append(evs, store.Event{Kind: r.Kind, Name: r.Name, Revision: rev})
		}
		for _, p := range puts {
			k := key{p.Kind, p.Name}
			r := clone(p)
			if old, ok := s.records[k]; ok {
				r.UID, r.CreatedAt, r.Generation = old.UID, old.CreatedAt, old.Generation
			} else {
				if r.UID == "" {
					r.UID = store.NewUID()
				}
				r.CreatedAt, r.Generation = now, 1
			}
			r.Revision, r.UpdatedAt = rev, now
			s.records[k] = r
			evs = append(evs, store.Event{Kind: r.Kind, Name: r.Name, Revision: rev})
		}
		return evs, nil
	})
}
