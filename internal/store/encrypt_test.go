package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/kaiserern/shoal/internal/store"
	"github.com/kaiserern/shoal/internal/store/memory"
	"github.com/kaiserern/shoal/internal/store/storetest"
)

var key = bytes.Repeat([]byte{7}, 32)

func TestEncryptingStoreConformance(t *testing.T) {
	// The wrapper must still be a correct Store, for encrypted kinds too
	// ("W" is the kind the suite uses most).
	storetest.Run(t, func(t *testing.T) store.Store {
		e, err := store.NewEncryptingStore(memory.New(nil), key, "W")
		if err != nil {
			t.Fatal(err)
		}
		return e
	})
}

func TestSecretsAreEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	raw := memory.New(nil)
	e, err := store.NewEncryptingStore(raw, key, "Secret")
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("password=hunter2")
	if _, err := e.Create(ctx, store.Record{Kind: "Secret", Name: "db", Spec: secret}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Create(ctx, store.Record{Kind: "Config", Name: "app", Spec: []byte("plain")}); err != nil {
		t.Fatal(err)
	}

	stored, _ := raw.Get(ctx, "Secret", "db")
	if bytes.Contains(stored.Spec, []byte("hunter2")) || stored.Spec[0] != 1 {
		t.Fatalf("secret stored in the clear: %q", stored.Spec)
	}
	if got, _ := raw.Get(ctx, "Config", "app"); string(got.Spec) != "plain" {
		t.Fatalf("config must not be encrypted: %q", got.Spec)
	}
	got, err := e.Get(ctx, "Secret", "db")
	if err != nil || !bytes.Equal(got.Spec, secret) {
		t.Fatalf("decrypted = %q, %v", got.Spec, err)
	}

	// A sealed spec can't be moved to another record...
	if _, err := raw.Create(ctx, store.Record{Kind: "Secret", Name: "copy", Spec: stored.Spec}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Get(ctx, "Secret", "copy"); err == nil {
		t.Fatal("ciphertext moved to another record was accepted")
	}
	// ...or read with another key.
	other, _ := store.NewEncryptingStore(raw, bytes.Repeat([]byte{8}, 32), "Secret")
	if _, err := other.Get(ctx, "Secret", "db"); !errors.Is(err, store.ErrWrongKey) {
		t.Fatalf("wrong key: err = %v, want ErrWrongKey", err)
	}
	if _, err := store.NewEncryptingStore(raw, []byte("short"), "Secret"); err == nil {
		t.Fatal("short key accepted")
	}
}
