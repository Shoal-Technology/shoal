package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/kaiserern/shoal/api/v1alpha1"
	"github.com/kaiserern/shoal/internal/resource"
	"github.com/kaiserern/shoal/internal/store"
)

// ownerOf returns the Database that owns the object kind/name, or "" when no
// Database owns it (ADR 0016, pass C). The controller writes the OwnerClaim
// records; the server only reads them to refuse user edits.
func (s *Server) ownerOf(ctx context.Context, kind, name string) (string, error) {
	rec, err := s.store.Get(ctx, v1alpha1.KindOwnerClaim, resource.OwnerKey(kind, name))
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(rec.Spec), nil
}

// ownedMessage explains a refused change to an object a Database owns.
func ownedMessage(kind, name, owner string) string {
	return fmt.Sprintf("%s %q is managed by database %q: change the Database instead", kind, name, owner)
}
