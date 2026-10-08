package store

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
)

// sealedVersion is the first byte of every encrypted spec, so the format
// can change later without guessing.
const sealedVersion = 1

// EncryptingStore encrypts the specs of chosen kinds (Secrets) with
// AES-256-GCM before they reach the wrapped store, and decrypts them on
// read (ADR 0002). Statuses are never encrypted. The record's kind and name
// are bound to the ciphertext as additional data, so a sealed spec can't be
// moved to another record.
//
// Sealed format: version (1 byte) | key ID (4 bytes) | nonce (12) | ciphertext.
type EncryptingStore struct {
	Store
	aead  cipher.AEAD
	keyID [4]byte
	kinds []string
}

// NewEncryptingStore wraps s; key must be 32 bytes.
func NewEncryptingStore(s Store, key []byte, kinds ...string) (*EncryptingStore, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("encryption key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encryption key: %w", err)
	}
	e := &EncryptingStore{Store: s, aead: aead, kinds: kinds}
	sum := sha256.Sum256(key)
	copy(e.keyID[:], sum[:4])
	return e, nil
}

func (e *EncryptingStore) encrypted(kind string) bool { return slices.Contains(e.kinds, kind) }

func aad(kind, name string) []byte { return []byte(kind + "/" + name) }

func (e *EncryptingStore) seal(kind, name string, plain []byte) []byte {
	out := make([]byte, 0, 1+4+e.aead.NonceSize()+len(plain)+e.aead.Overhead())
	out = append(out, sealedVersion)
	out = append(out, e.keyID[:]...)
	nonce := make([]byte, e.aead.NonceSize())
	_, _ = rand.Read(nonce) // never fails (crypto/rand panics instead)
	out = append(out, nonce...)
	return e.aead.Seal(out, nonce, plain, aad(kind, name))
}

// ErrWrongKey means a record was encrypted with a different key.
var ErrWrongKey = errors.New("encrypted with a different key (check --encryption-key-file)")

func (e *EncryptingStore) open(r Record) (Record, error) {
	if !e.encrypted(r.Kind) || len(r.Spec) == 0 {
		return r, nil
	}
	ns := e.aead.NonceSize()
	b := r.Spec
	if len(b) < 1+4+ns || b[0] != sealedVersion {
		return Record{}, fmt.Errorf("%s %q: spec is not in the encrypted format", r.Kind, r.Name)
	}
	if [4]byte(b[1:5]) != e.keyID {
		return Record{}, fmt.Errorf("%s %q: %w", r.Kind, r.Name, ErrWrongKey)
	}
	plain, err := e.aead.Open(nil, b[5:5+ns], b[5+ns:], aad(r.Kind, r.Name))
	if err != nil {
		return Record{}, fmt.Errorf("%s %q: decrypt: %w", r.Kind, r.Name, err)
	}
	r.Spec = plain
	return r, nil
}

func (e *EncryptingStore) Get(ctx context.Context, kind, name string) (Record, error) {
	r, err := e.Store.Get(ctx, kind, name)
	if err != nil {
		return Record{}, err
	}
	return e.open(r)
}

func (e *EncryptingStore) List(ctx context.Context, kind string) ([]Record, error) {
	recs, err := e.Store.List(ctx, kind)
	if err != nil || !e.encrypted(kind) {
		return recs, err
	}
	for i := range recs {
		if recs[i], err = e.open(recs[i]); err != nil {
			return nil, err
		}
	}
	return recs, nil
}

func (e *EncryptingStore) Create(ctx context.Context, rec Record) (Record, error) {
	plain := rec.Spec
	if e.encrypted(rec.Kind) {
		rec.Spec = e.seal(rec.Kind, rec.Name, rec.Spec)
	}
	out, err := e.Store.Create(ctx, rec)
	out.Spec = plain
	return out, err
}

// Update encrypts like Create. Every encryption differs, so for an
// unchanged spec it passes the stored ciphertext on: the wrapped store then
// sees equal bytes and keeps the generation, as the Store contract requires.
func (e *EncryptingStore) Update(ctx context.Context, kind, name string, spec []byte, generation int64) (Record, error) {
	plain := spec
	if e.encrypted(kind) {
		cur, err := e.Store.Get(ctx, kind, name)
		if err != nil {
			return Record{}, err
		}
		if opened, err := e.open(cur); err == nil && bytes.Equal(opened.Spec, plain) {
			spec = cur.Spec
		} else {
			spec = e.seal(kind, name, plain)
		}
	}
	out, err := e.Store.Update(ctx, kind, name, spec, generation)
	out.Spec = plain
	return out, err
}

func (e *EncryptingStore) UpdateStatus(ctx context.Context, kind, name string, status []byte) (Record, error) {
	r, err := e.Store.UpdateStatus(ctx, kind, name, status)
	if err != nil {
		return Record{}, err
	}
	return e.open(r)
}
