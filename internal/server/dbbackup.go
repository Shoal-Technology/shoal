package server

import (
	"context"
	"errors"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/kaiserern/shoal/api/v1alpha1"
	"github.com/kaiserern/shoal/internal/database"
	"github.com/kaiserern/shoal/internal/objects"
	"github.com/kaiserern/shoal/internal/store"
)

// DatabaseBackups lists the logical backups of a Database, newest first (ADR
// 0016, section 3.4).
func (c *controlService) DatabaseBackups(ctx context.Context, req *v1alpha1.DatabaseBackupsRequest) (*v1alpha1.DatabaseBackupsResponse, error) {
	if _, err := c.databaseRecord(ctx, req.GetDatabase()); err != nil {
		return nil, err
	}
	recs, err := c.s.store.List(ctx, v1alpha1.KindDatabaseBackup)
	if err != nil {
		return nil, apiError(err)
	}
	resp := &v1alpha1.DatabaseBackupsResponse{}
	for _, r := range recs {
		b := &v1alpha1.DatabaseBackupRecord{}
		if err := proto.Unmarshal(r.Spec, b); err != nil {
			return nil, apiError(err)
		}
		if b.GetDatabase() == req.GetDatabase() {
			resp.Backups = append(resp.Backups, b)
		}
	}
	slices.SortFunc(resp.Backups, func(x, y *v1alpha1.DatabaseBackupRecord) int {
		return y.GetCreatedAt().AsTime().Compare(x.GetCreatedAt().AsTime())
	})
	return resp, nil
}

// BackupDatabase records a manual backup of a Database that is Ready. The
// backup is pending until the agent on the database's node has run it.
func (c *controlService) BackupDatabase(ctx context.Context, req *v1alpha1.BackupDatabaseRequest) (*v1alpha1.BackupDatabaseResponse, error) {
	rec, err := c.databaseRecord(ctx, req.GetDatabase())
	if err != nil {
		return nil, err
	}
	d, err := objects.Database(rec)
	if err != nil {
		return nil, apiError(err)
	}
	if d.GetStatus().GetPhase() != "Ready" {
		return nil, status.Errorf(codes.FailedPrecondition, "database %s is %s: a backup needs a Ready database", req.GetDatabase(), d.GetStatus().GetPhase())
	}
	now := c.s.now()
	for range 5 {
		b := database.NewBackupRecord(req.GetDatabase(), database.ReasonManual, d.GetStatus().GetVersion(), now, database.RandomSuffix())
		spec, err := objects.Encode(b)
		if err != nil {
			return nil, apiError(err)
		}
		_, err = c.s.store.Create(ctx, store.Record{Kind: v1alpha1.KindDatabaseBackup, Name: b.GetId(), Spec: spec})
		if errors.Is(err, store.ErrExists) {
			continue
		}
		if err != nil {
			return nil, apiError(err)
		}
		c.s.log.Info("manual database backup requested", "database", req.GetDatabase(), "backup", b.GetId())
		return &v1alpha1.BackupDatabaseResponse{Backup: b}, nil
	}
	return nil, status.Error(codes.Internal, "no free backup ID")
}

// databaseRecord returns the stored Database name, or NotFound.
func (c *controlService) databaseRecord(ctx context.Context, name string) (store.Record, error) {
	rec, err := c.s.store.Get(ctx, v1alpha1.KindDatabase, name)
	if errors.Is(err, store.ErrNotFound) {
		return store.Record{}, status.Errorf(codes.NotFound, "database %q does not exist", name)
	}
	if err != nil {
		return store.Record{}, apiError(err)
	}
	return rec, nil
}
