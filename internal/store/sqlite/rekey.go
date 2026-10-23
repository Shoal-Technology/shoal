package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Shoal-Technology/shoal/internal/store"
)

// Rekey implements store.Rekeyer in one IMMEDIATE transaction: either every
// move and put is committed, or none is, and the revision moves once.
func (s *Store) Rekey(ctx context.Context, moves []store.Move, puts []store.Record) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rekey: begin: %w", err)
	}
	defer tx.Rollback() // no-op after Commit

	var rev int64
	if err := tx.QueryRowContext(ctx, `UPDATE revision SET value = value + 1 WHERE id = 1 RETURNING value`).Scan(&rev); err != nil {
		return fmt.Errorf("rekey: next revision: %w", err)
	}
	now := s.now().UTC()
	var evs []store.Event
	for _, m := range moves {
		if m.From == m.Rec.Name {
			return fmt.Errorf("rekey %s %q: the new name is the old one", m.Rec.Kind, m.From)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM objects WHERE kind = ? AND name = ?`, m.Rec.Kind, m.From)
		if err != nil {
			return fmt.Errorf("rekey %s %q: %w", m.Rec.Kind, m.From, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("rekey %s %q: %w", m.Rec.Kind, m.From, err)
		} else if n != 1 {
			return fmt.Errorf("rekey %s %q: %w", m.Rec.Kind, m.From, store.ErrNotFound)
		}
		evs = append(evs, store.Event{Kind: m.Rec.Kind, Name: m.From, Revision: rev, Deleted: true})
	}
	for _, m := range moves {
		taken, err := exists(ctx, tx, m.Rec.Kind, m.Rec.Name)
		if err != nil {
			return fmt.Errorf("rekey %s %q: %w", m.Rec.Kind, m.Rec.Name, err)
		}
		if taken {
			return fmt.Errorf("rekey %s %q: %w", m.Rec.Kind, m.Rec.Name, store.ErrExists)
		}
		labels, err := encodeLabels(m.Rec.Labels)
		if err != nil {
			return fmt.Errorf("rekey %s %q: %w", m.Rec.Kind, m.Rec.Name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO objects (`+columns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.Rec.Kind, m.Rec.Name, m.Rec.UID, m.Rec.Generation, rev, m.Rec.CreatedAt.UnixNano(), now.UnixNano(), m.Rec.Spec, m.Rec.Status, labels); err != nil {
			return fmt.Errorf("rekey %s %q: %w", m.Rec.Kind, m.Rec.Name, err)
		}
		evs = append(evs, store.Event{Kind: m.Rec.Kind, Name: m.Rec.Name, Revision: rev})
	}
	for _, p := range puts {
		labels, err := encodeLabels(p.Labels)
		if err != nil {
			return fmt.Errorf("put %s %q: %w", p.Kind, p.Name, err)
		}
		taken, err := exists(ctx, tx, p.Kind, p.Name)
		if err != nil {
			return fmt.Errorf("put %s %q: %w", p.Kind, p.Name, err)
		}
		if taken {
			_, err = tx.ExecContext(ctx, `UPDATE objects SET spec = ?, status = ?, labels = ?, revision = ?, updated_at = ? WHERE kind = ? AND name = ?`,
				p.Spec, p.Status, labels, rev, now.UnixNano(), p.Kind, p.Name)
		} else {
			uid := p.UID
			if uid == "" {
				uid = store.NewUID()
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO objects (`+columns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				p.Kind, p.Name, uid, 1, rev, now.UnixNano(), now.UnixNano(), p.Spec, p.Status, labels)
		}
		if err != nil {
			return fmt.Errorf("put %s %q: %w", p.Kind, p.Name, err)
		}
		evs = append(evs, store.Event{Kind: p.Kind, Name: p.Name, Revision: rev})
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rekey: commit: %w", err)
	}
	for _, ev := range evs {
		s.hub.Publish(ev)
	}
	return nil
}

// exists reports whether a record of kind called name is stored.
func exists(ctx context.Context, tx *sql.Tx, kind, name string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM objects WHERE kind = ? AND name = ?`, kind, name).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}
