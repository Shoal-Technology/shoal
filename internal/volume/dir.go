package volume

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// dirManager keeps each volume as a plain directory. It has no size limit: the
// Volume's size is advisory on this backend, and the node reports quota none.
// Data is not crash-consistent beyond what the file system gives.
type dirManager struct {
	base
}

func newDir(root string, log *slog.Logger) *dirManager {
	return &dirManager{base: newBase(root, BackendDir, log)}
}

func (m *dirManager) Ensure(ctx context.Context, s Spec) (string, error) {
	if err := checkName(s.Name); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return "", err
	}
	dir := m.dirPath(s.Name)
	_, err := os.Stat(dir)
	created := errors.Is(err, fs.ErrNotExist)
	if err != nil && !created {
		return "", err
	}
	if created {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return "", err
		}
	}
	// Owner and mode are applied once, at the first Ensure (ADR 0016, 1.7).
	if _, metaErr := m.readMeta(s.Name); created || errors.Is(metaErr, fs.ErrNotExist) {
		if err := m.chown(dir, s.UID, s.GID); err != nil {
			return "", fmt.Errorf("chown volume %s: %w", s.Name, err)
		}
		if err := os.Chmod(dir, s.Mode); err != nil {
			return "", fmt.Errorf("chmod volume %s: %w", s.Name, err)
		}
	}
	if err := m.writeMeta(s.Name, s.Retention, BackendDir); err != nil {
		return "", err
	}
	return dir, nil
}

func (m *dirManager) Retire(_ context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	dir := m.dirPath(name)
	retention := m.retention(name)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return m.moveMeta(name, "") // only the record is left
	}
	switch retention {
	case RetentionDelete:
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		return m.moveMeta(name, "")
	default:
		dst, err := m.retainedName(name)
		if err != nil {
			return err
		}
		if err := os.Rename(dir, dst); err != nil {
			return fmt.Errorf("retain volume %s: %w", name, err)
		}
		m.log.Info("volume data retained", "volume", name, "path", filepath.Base(dst))
		return m.moveMeta(name, dst)
	}
}
