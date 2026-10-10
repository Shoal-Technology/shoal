package store

import (
	"context"
	"errors"
)

// Dump is a point-in-time copy of every record and of the global revision.
// It is what a backup carries and what a restore loads.
type Dump struct {
	Revision int64
	Records  []Record
}

// ErrNotEmpty means Import was called on a store that already holds records
// or a revision. Restores only target empty stores.
var ErrNotEmpty = errors.New("store is not empty")

// Exporter is implemented by backends that can produce a consistent Dump.
// Specs of encrypted kinds are returned as stored (sealed), so a backup
// never holds plaintext Secrets.
type Exporter interface {
	// Export returns every record and the revision as of one instant.
	Export(ctx context.Context) (Dump, error)
}

// Importer is implemented by backends that can load a Dump into an empty
// store, keeping UIDs, generations, revisions and timestamps.
type Importer interface {
	// Import fails with ErrNotEmpty unless the store is empty.
	Import(ctx context.Context, d Dump) error
}
