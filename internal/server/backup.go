package server

import (
	"context"
	"errors"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/backup"
	"github.com/Shoal-Technology/shoal/internal/store/raftstore"
)

// Backup returns a point-in-time copy of every record in the shoal-backup/v1
// format. It is a unary call: the payload is control-plane records only (no
// logs or images), and the client raises its receive limit for it. On raft,
// only the leader answers; a follower returns the leader's address and the
// CLI retries there. Admin only, like every Control call.
func (c *controlService) Backup(ctx context.Context, _ *v1alpha1.BackupRequest) (*v1alpha1.BackupResponse, error) {
	s := c.s
	if s.exporter == nil {
		return nil, apiError(errors.New("this store backend cannot export a backup"))
	}
	d, err := s.exporter.Export(ctx)
	if errors.Is(err, raftstore.ErrNotLeader) {
		return nil, s.notLeader(ctx)
	}
	if err != nil {
		return nil, apiError(err)
	}
	data, info, err := backup.Encode(d, s.cfg.Version, s.cfg.Now())
	if err != nil {
		return nil, apiError(err)
	}
	s.log.Info("backup taken", "peer", peerAddr(ctx), "revision", d.Revision, "records", info.RecordCount, "sha256", info.SHA256)
	return &v1alpha1.BackupResponse{Backup: data, Revision: d.Revision, RecordCount: uint64(info.RecordCount)}, nil
}
