package raftstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/raft"

	"github.com/kaiserern/shoal/internal/store"
)

// Export implements store.Exporter. It runs on the leader only and returns
// ErrNotLeader elsewhere, so the caller can retry on the leader. The Raft
// barrier applies every entry committed so far before the copy, so the dump
// includes every acknowledged write.
func (s *Store) Export(ctx context.Context) (store.Dump, error) {
	if err := ctx.Err(); err != nil {
		return store.Dump{}, err
	}
	if !s.IsLeader() {
		return store.Dump{}, ErrNotLeader
	}
	err := s.raft.Barrier(applyTimeout).Error()
	if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
		return store.Dump{}, ErrNotLeader
	}
	if err != nil {
		return store.Dump{}, fmt.Errorf("export: barrier: %w", err)
	}
	return s.mem.Export(ctx)
}

// Import implements store.Importer by replicating one command that loads the
// dump. It waits for this node to become leader first, which for a fresh
// single-node bootstrap takes one election. It fails with store.ErrNotEmpty
// if the cluster already holds state.
func (s *Store) Import(ctx context.Context, d store.Dump) error {
	if err := s.waitLeader(ctx); err != nil {
		return err
	}
	_, err := s.apply(ctx, command{Op: opImport, Now: time.Now().UTC(), Dump: &d})
	return err
}
