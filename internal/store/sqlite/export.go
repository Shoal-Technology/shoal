package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"example.com/shoal/internal/store"
)

// Export implements store.Exporter. The revision and every row are read in
// one read transaction on the store's only connection, so the dump is one
// consistent point in time.
func (s *Store) Export(ctx context.Context) (store.Dump, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return store.Dump{}, fmt.Errorf("export: begin: %w", err)
	}
	defer tx.Rollback()

	var rev int64
	if err := tx.QueryRowContext(ctx, `SELECT value FROM revision WHERE id = 1`).Scan(&rev); err != nil {
		return store.Dump{}, fmt.Errorf("export: revision: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+columns+` FROM objects ORDER BY kind, name`)
	if err != nil {
		return store.Dump{}, fmt.Errorf("export: records: %w", err)
	}
	defer rows.Close()
	var recs []store.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return store.Dump{}, fmt.Errorf("export: %w", err)
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return store.Dump{}, fmt.Errorf("export: %w", err)
	}
	return store.Dump{Revision: rev, Records: recs}, nil
}

// Import implements store.Importer. It runs in one write transaction, so a
// failure leaves the database empty.
func (s *Store) Import(ctx context.Context, d store.Dump) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("import: begin: %w", err)
	}
	defer tx.Rollback()

	var n, rev int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM objects`).Scan(&n); err != nil {
		return fmt.Errorf("import: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT value FROM revision WHERE id = 1`).Scan(&rev); err != nil {
		return fmt.Errorf("import: %w", err)
	}
	if n != 0 || rev != 0 {
		return store.ErrNotEmpty
	}
	for _, r := range d.Records {
		labels, err := encodeLabels(r.Labels)
		if err != nil {
			return fmt.Errorf("import %s %q: %w", r.Kind, r.Name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO objects (`+columns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.Kind, r.Name, r.UID, r.Generation, r.Revision, r.CreatedAt.UnixNano(), r.UpdatedAt.UnixNano(), r.Spec, r.Status, labels); err != nil {
			return fmt.Errorf("import %s %q: %w", r.Kind, r.Name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE revision SET value = ? WHERE id = 1`, d.Revision); err != nil {
		return fmt.Errorf("import: revision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("import: commit: %w", err)
	}
	return nil
}
