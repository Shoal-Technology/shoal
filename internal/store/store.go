// Package store defines the control plane's state store (ADR 0002).
//
// The store holds generic records keyed by (kind, name) with opaque spec and
// status bytes. Every write is a single-object operation that advances one
// global revision, so the contract maps 1:1 onto a replicated log (Phase 6).
package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	// ErrConflict means the record changed since it was read.
	ErrConflict = errors.New("revision conflict")
)

// Record is a stored object. Spec and Status are encoded by the caller.
type Record struct {
	Kind       string
	Name       string
	UID        string
	Generation int64 // starts at 1, incremented when Spec changes
	Revision   int64 // global revision of the last write to this record
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Spec       []byte
	Status     []byte
}

// Event reports a change to a record.
type Event struct {
	Kind     string
	Name     string
	Revision int64
	Deleted  bool
}

// Store is implemented by every state backend. All methods are safe for
// concurrent use.
type Store interface {
	// Get returns the record, or ErrNotFound.
	Get(ctx context.Context, kind, name string) (Record, error)
	// List returns all records of kind, sorted by name.
	List(ctx context.Context, kind string) ([]Record, error)
	// Create stores a new record from rec's Kind, Name, Spec and Status and
	// fills in the rest. Returns ErrExists if the name is taken.
	Create(ctx context.Context, rec Record) (Record, error)
	// Update replaces the spec if the record's generation is still
	// generation, and returns ErrConflict otherwise. The check is on the
	// generation rather than the revision so that status writes never make
	// spec writes conflict. Generation is incremented only if the spec
	// actually changed. The status is kept.
	Update(ctx context.Context, kind, name string, spec []byte, generation int64) (Record, error)
	// UpdateStatus replaces the status unconditionally: each kind's status
	// has a single writer.
	UpdateStatus(ctx context.Context, kind, name string, status []byte) (Record, error)
	// Delete removes the record if its generation is generation (0 = any).
	// Returns ErrNotFound if it doesn't exist.
	Delete(ctx context.Context, kind, name string, generation int64) error
	// Watch delivers changes to the given kinds (all kinds if none) until ctx
	// is done, then closes the channel. Delivery is best effort: events are
	// dropped when the receiver falls behind, and there is no replay. Watch
	// only triggers work early; consumers must also resync periodically.
	Watch(ctx context.Context, kinds ...string) <-chan Event
	Close() error
}

// NewUID returns a random RFC 4122 version 4 UUID.
func NewUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand panics instead)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
