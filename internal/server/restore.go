package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Shoal-Technology/shoal/internal/backup"
	"github.com/Shoal-Technology/shoal/internal/security"
	"github.com/Shoal-Technology/shoal/internal/store"
)

// restorePlan is a backup that passed every check that needs no writes. The
// restore itself runs once the store is open.
type restorePlan struct {
	file string
	dump store.Dump
	info backup.Info
}

// planRestore checks everything before New writes anything: the data dir
// must hold no state, the file must verify, and the secret key must open
// every Secret in the backup. A failure here leaves the data dir untouched.
func planRestore(cfg Config) (*restorePlan, error) {
	if cfg.RaftJoinAddr != "" || cfg.RaftJoinBundle != "" {
		return nil, errors.New("restore creates a new cluster and cannot be combined with --raft-join or --raft-join-bundle")
	}
	if err := checkRestoreDataDir(cfg.DataDir, cfg.EncryptionKeyFile); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(cfg.RestoreFrom)
	if err != nil {
		return nil, fmt.Errorf("read backup: %w", err)
	}
	dump, info, err := backup.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("backup %s: %w", cfg.RestoreFrom, err)
	}
	hasSecrets := false
	for _, r := range dump.Records {
		if isEncryptedKind(r.Kind) {
			hasSecrets = true
			break
		}
	}
	key, err := loadExistingKey(cfg.EncryptionKeyFile)
	switch {
	case errors.Is(err, fs.ErrNotExist) && hasSecrets:
		return nil, fmt.Errorf("backup %s holds Secrets: copy the secret.key it was taken with to %s before restoring (a new key cannot read them)", cfg.RestoreFrom, cfg.EncryptionKeyFile)
	case errors.Is(err, fs.ErrNotExist):
		// No Secrets and no key yet: a new key is created as on any start.
	case err != nil:
		return nil, err
	default:
		if err := store.CheckEncrypted(key, dump.Records, encryptedKinds...); err != nil {
			return nil, fmt.Errorf("backup %s: %w: restore needs the secret.key the backup was taken with", cfg.RestoreFrom, err)
		}
	}
	return &restorePlan{file: cfg.RestoreFrom, dump: dump, info: info}, nil
}

// loadExistingKey loads the secret key only if the file exists. It never
// creates one, so a missing key is reported as fs.ErrNotExist.
func loadExistingKey(path string) ([]byte, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	key, _, err := security.LoadOrCreateKey(path)
	return key, err
}

// checkRestoreDataDir refuses a data dir that holds state. The only entry
// allowed is the secret key, which the operator places there on purpose.
func checkRestoreDataDir(dataDir, keyFile string) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return fmt.Errorf("read data dir: %w", err)
	}
	absKey, err := filepath.Abs(keyFile)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p, err := filepath.Abs(filepath.Join(dataDir, e.Name()))
		if err != nil {
			return err
		}
		if p == absKey {
			continue
		}
		return fmt.Errorf("restore needs an empty data dir, but %s holds %q: use a new --data-dir", dataDir, e.Name())
	}
	return nil
}

// restoreInto loads the backup into an empty store. It runs once, before the
// node serves. If it fails part way, the data dir is no longer empty and
// must be deleted before a retry.
func restoreInto(ctx context.Context, inner store.Store, p *restorePlan, log *slog.Logger) error {
	imp, ok := inner.(store.Importer)
	if !ok {
		return errors.New("restore: this store backend cannot import a backup")
	}
	if err := imp.Import(ctx, p.dump); err != nil {
		return fmt.Errorf("restore %s: %w (the data dir may now hold part of the backup: delete it before retrying)", p.file, err)
	}
	log.Info("restored backup", "file", p.file, "records", len(p.dump.Records),
		"revision", p.dump.Revision, "backupServerVersion", p.info.ServerVersion, "createdAt", p.info.CreatedAt)
	return nil
}
