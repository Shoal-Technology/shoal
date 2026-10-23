package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Shoal-Technology/shoal/internal/backup"
	"github.com/Shoal-Technology/shoal/internal/projmigrate"
	"github.com/Shoal-Technology/shoal/internal/security"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/store/raftstore"
)

// migrateProjects brings the store to the project key layout (ADR 0023,
// pass B1). It runs before the API serves, and again when this node becomes
// leader, and does nothing once the store is at the current version. On a
// follower it waits: the leader migrates the store, and the follower applies
// that change through the log.
func (s *Server) migrateProjects(ctx context.Context) error {
	err := projmigrate.Run(ctx, s.raw, s.encKey, encryptedKinds, s.backupBeforeMigration, s.log)
	if errors.Is(err, raftstore.ErrNotLeader) {
		s.log.Info("project migration left to the leader")
		return nil
	}
	return err
}

// backupBeforeMigration writes a shoal-backup/v1 file of the store as it is
// before the migration, under DataDir/backups. The file holds the Secrets
// sealed, as Backup does. A failure stops the migration.
func (s *Server) backupBeforeMigration(d store.Dump) error {
	data, info, err := backup.Encode(d, s.cfg.Version, s.cfg.Now())
	if err != nil {
		return fmt.Errorf("encode backup: %w", err)
	}
	dir := filepath.Join(s.cfg.DataDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, fmt.Sprintf("pre-projects-%s.shoalbackup", s.cfg.Now().UTC().Format("20060102T150405Z")))
	if err := security.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	s.log.Info("backup taken before migrating the store", "file", path, "records", info.RecordCount, "sha256", info.SHA256)
	return nil
}
