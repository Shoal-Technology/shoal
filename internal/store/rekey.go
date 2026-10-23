package store

import "context"

// Move re-keys one record of kind Rec.Kind: the record stored under From is
// removed and stored again under Rec.Name, with the rest of Rec (UID,
// generation, creation time, labels and the sealed or plain spec and status)
// as given. The caller decides the new spec, so a migration can re-seal
// Secrets whose additional data includes the name.
type Move struct {
	From string
	Rec  Record
}

// Rekeyer is implemented by backends that can re-key records atomically. It
// is the primitive behind the project name migration (ADR 0023, pass B1).
type Rekeyer interface {
	// Rekey applies every move, then writes every put, all or nothing. Each
	// move's From record must exist, and its target name must be free or be
	// the From of another move in the same call. A put creates the record at
	// its key, or replaces the spec, status and labels of the record there.
	// The global revision advances once for the whole call.
	Rekey(ctx context.Context, moves []Move, puts []Record) error
}
