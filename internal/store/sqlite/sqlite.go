// Package sqlite implements store.Store on SQLite (pure Go driver, no CGO).
//
// One table holds all records; a single-row table holds the global revision.
// Each write is one IMMEDIATE transaction that bumps the revision, so writes
// are serialized and the revision is gapless and monotonic.
package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"example.com/shoal/internal/store"
)

var schema = []string{
	`CREATE TABLE IF NOT EXISTS objects (
		kind       TEXT    NOT NULL,
		name       TEXT    NOT NULL,
		uid        TEXT    NOT NULL,
		generation INTEGER NOT NULL,
		revision   INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		spec       BLOB,
		status     BLOB,
		PRIMARY KEY (kind, name)
	) WITHOUT ROWID`,
	`CREATE TABLE IF NOT EXISTS revision (
		id    INTEGER PRIMARY KEY CHECK (id = 1),
		value INTEGER NOT NULL
	)`,
	`INSERT OR IGNORE INTO revision (id, value) VALUES (1, 0)`,
}

const columns = `kind, name, uid, generation, revision, created_at, updated_at, spec, status, labels`

// migrations add columns to databases created by earlier versions. Each
// statement runs only when its column is missing.
var migrations = []struct{ column, stmt string }{
	{"labels", `ALTER TABLE objects ADD COLUMN labels TEXT`},
}

// encodeLabels stores labels as JSON; no labels is NULL.
func encodeLabels(m map[string]string) (any, error) {
	if len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(m)
	return string(b), err
}

// Store is a SQLite-backed store.Store.
type Store struct {
	db  *sql.DB
	now func() time.Time
	hub store.Hub
}

var _ store.Store = (*Store)(nil)

// Open opens (or creates) the database at path. now may be nil.
func Open(ctx context.Context, path string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open state database %s: %w", path, err)
	}
	// One connection serializes all access. That is plenty for a small
	// cluster and rules out SQLITE_BUSY between our own connections.
	db.SetMaxOpenConns(1)
	for _, stmt := range schema {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize state database %s: %w", path, err)
		}
	}
	for _, m := range migrations {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('objects') WHERE name = ?`, m.column).Scan(&n); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate state database %s: %w", path, err)
		}
		if n == 0 {
			if _, err := db.ExecContext(ctx, m.stmt); err != nil {
				db.Close()
				return nil, fmt.Errorf("migrate state database %s: %w", path, err)
			}
		}
	}
	return &Store{db: db, now: now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type scanner interface{ Scan(...any) error }

func scanRecord(row scanner) (store.Record, error) {
	var r store.Record
	var created, updated int64
	var labels sql.NullString
	if err := row.Scan(&r.Kind, &r.Name, &r.UID, &r.Generation, &r.Revision, &created, &updated, &r.Spec, &r.Status, &labels); err != nil {
		return store.Record{}, err
	}
	r.CreatedAt, r.UpdatedAt = time.Unix(0, created).UTC(), time.Unix(0, updated).UTC()
	if labels.Valid && labels.String != "" {
		if err := json.Unmarshal([]byte(labels.String), &r.Labels); err != nil {
			return store.Record{}, fmt.Errorf("decode labels of %s %q: %w", r.Kind, r.Name, err)
		}
	}
	return r, nil
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func get(ctx context.Context, q querier, kind, name string) (store.Record, error) {
	r, err := scanRecord(q.QueryRowContext(ctx, `SELECT `+columns+` FROM objects WHERE kind = ? AND name = ?`, kind, name))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Record{}, fmt.Errorf("%s %q: %w", kind, name, store.ErrNotFound)
	}
	if err != nil {
		return store.Record{}, fmt.Errorf("get %s %q: %w", kind, name, err)
	}
	return r, nil
}

func (s *Store) Get(ctx context.Context, kind, name string) (store.Record, error) {
	return get(ctx, s.db, kind, name)
}

func (s *Store) List(ctx context.Context, kind string) ([]store.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM objects WHERE kind = ? ORDER BY name`, kind)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", kind, err)
	}
	defer rows.Close()
	var out []store.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", kind, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list %s: %w", kind, err)
	}
	return out, nil
}

// write runs fn in a transaction with the next revision and publishes the
// resulting event after commit. If fn fails, the revision bump is rolled back.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx, rev int64, now time.Time) (store.Event, error)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() // no-op after Commit
	var rev int64
	if err := tx.QueryRowContext(ctx, `UPDATE revision SET value = value + 1 WHERE id = 1 RETURNING value`).Scan(&rev); err != nil {
		return fmt.Errorf("next revision: %w", err)
	}
	ev, err := fn(tx, rev, s.now().UTC())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	s.hub.Publish(ev)
	return nil
}

func (s *Store) Create(ctx context.Context, rec store.Record) (store.Record, error) {
	var out store.Record
	err := s.write(ctx, func(tx *sql.Tx, rev int64, now time.Time) (store.Event, error) {
		out = store.Record{
			Kind: rec.Kind, Name: rec.Name, UID: store.NewUID(), Generation: 1, Revision: rev,
			CreatedAt: now, UpdatedAt: now, Spec: rec.Spec, Status: rec.Status, Labels: maps.Clone(rec.Labels),
		}
		labels, err := encodeLabels(out.Labels)
		if err != nil {
			return store.Event{}, fmt.Errorf("create %s %q: %w", rec.Kind, rec.Name, err)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO objects (`+columns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (kind, name) DO NOTHING`,
			out.Kind, out.Name, out.UID, out.Generation, out.Revision, now.UnixNano(), now.UnixNano(), out.Spec, out.Status, labels)
		if err != nil {
			return store.Event{}, fmt.Errorf("create %s %q: %w", rec.Kind, rec.Name, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return store.Event{}, fmt.Errorf("create %s %q: %w", rec.Kind, rec.Name, err)
		} else if n == 0 {
			return store.Event{}, fmt.Errorf("%s %q: %w", rec.Kind, rec.Name, store.ErrExists)
		}
		return store.Event{Kind: out.Kind, Name: out.Name, Revision: rev}, nil
	})
	if err != nil {
		return store.Record{}, err
	}
	return out, nil
}

func (s *Store) Update(ctx context.Context, kind, name string, spec []byte, generation int64) (store.Record, error) {
	var out store.Record
	err := s.write(ctx, func(tx *sql.Tx, rev int64, now time.Time) (store.Event, error) {
		cur, err := get(ctx, tx, kind, name)
		if err != nil {
			return store.Event{}, err
		}
		if cur.Generation != generation {
			return store.Event{}, fmt.Errorf("%s %q: generation is %d, not %d: %w", kind, name, cur.Generation, generation, store.ErrConflict)
		}
		out = cur
		if !bytes.Equal(cur.Spec, spec) {
			out.Generation++
		}
		out.Spec, out.Revision, out.UpdatedAt = spec, rev, now
		if _, err := tx.ExecContext(ctx, `UPDATE objects SET spec = ?, generation = ?, revision = ?, updated_at = ? WHERE kind = ? AND name = ?`,
			out.Spec, out.Generation, rev, now.UnixNano(), kind, name); err != nil {
			return store.Event{}, fmt.Errorf("update %s %q: %w", kind, name, err)
		}
		return store.Event{Kind: kind, Name: name, Revision: rev}, nil
	})
	if err != nil {
		return store.Record{}, err
	}
	return out, nil
}

func (s *Store) SetLabels(ctx context.Context, kind, name string, labels map[string]string, generation int64) (store.Record, error) {
	var out store.Record
	err := s.write(ctx, func(tx *sql.Tx, rev int64, now time.Time) (store.Event, error) {
		cur, err := get(ctx, tx, kind, name)
		if err != nil {
			return store.Event{}, err
		}
		if cur.Generation != generation {
			return store.Event{}, fmt.Errorf("%s %q: generation is %d, not %d: %w", kind, name, cur.Generation, generation, store.ErrConflict)
		}
		enc, err := encodeLabels(labels)
		if err != nil {
			return store.Event{}, fmt.Errorf("set labels of %s %q: %w", kind, name, err)
		}
		out = cur
		out.Labels, out.Revision, out.UpdatedAt = maps.Clone(labels), rev, now
		if _, err := tx.ExecContext(ctx, `UPDATE objects SET labels = ?, revision = ?, updated_at = ? WHERE kind = ? AND name = ?`,
			enc, rev, now.UnixNano(), kind, name); err != nil {
			return store.Event{}, fmt.Errorf("set labels of %s %q: %w", kind, name, err)
		}
		return store.Event{Kind: kind, Name: name, Revision: rev}, nil
	})
	if err != nil {
		return store.Record{}, err
	}
	return out, nil
}

func (s *Store) UpdateStatus(ctx context.Context, kind, name string, status []byte) (store.Record, error) {
	var out store.Record
	err := s.write(ctx, func(tx *sql.Tx, rev int64, now time.Time) (store.Event, error) {
		cur, err := get(ctx, tx, kind, name)
		if err != nil {
			return store.Event{}, err
		}
		out = cur
		out.Status, out.Revision, out.UpdatedAt = status, rev, now
		if _, err := tx.ExecContext(ctx, `UPDATE objects SET status = ?, revision = ?, updated_at = ? WHERE kind = ? AND name = ?`,
			status, rev, now.UnixNano(), kind, name); err != nil {
			return store.Event{}, fmt.Errorf("update status of %s %q: %w", kind, name, err)
		}
		return store.Event{Kind: kind, Name: name, Revision: rev}, nil
	})
	if err != nil {
		return store.Record{}, err
	}
	return out, nil
}

func (s *Store) Delete(ctx context.Context, kind, name string, generation int64) error {
	return s.write(ctx, func(tx *sql.Tx, rev int64, _ time.Time) (store.Event, error) {
		cur, err := get(ctx, tx, kind, name)
		if err != nil {
			return store.Event{}, err
		}
		if generation != 0 && cur.Generation != generation {
			return store.Event{}, fmt.Errorf("%s %q: generation is %d, not %d: %w", kind, name, cur.Generation, generation, store.ErrConflict)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM objects WHERE kind = ? AND name = ?`, kind, name); err != nil {
			return store.Event{}, fmt.Errorf("delete %s %q: %w", kind, name, err)
		}
		return store.Event{Kind: kind, Name: name, Revision: rev, Deleted: true}, nil
	})
}

func (s *Store) Watch(ctx context.Context, kinds ...string) <-chan store.Event {
	return s.hub.Subscribe(ctx, kinds...)
}
