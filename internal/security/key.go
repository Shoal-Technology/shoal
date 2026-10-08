package security

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LoadOrCreateKey returns the 32-byte key stored at path, creating it with
// owner-only permissions on first use. It refuses a file with group or
// world access: the key decrypts every Secret.
func LoadOrCreateKey(path string) (key []byte, created bool, err error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(b) != 32 {
			return nil, false, fmt.Errorf("encryption key %s: want 32 bytes, got %d", path, len(b))
		}
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
			return nil, false, fmt.Errorf("encryption key %s is accessible by other users (mode %v); chmod 600 it", path, fi.Mode().Perm())
		}
		return b, false, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, false, fmt.Errorf("read encryption key: %w", err)
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, false, fmt.Errorf("generate encryption key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := WriteFileAtomic(path, key, 0o600); err != nil {
		return nil, false, err
	}
	return key, true, nil
}
