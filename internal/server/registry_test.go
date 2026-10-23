package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/registry"
	"github.com/Shoal-Technology/shoal/internal/store"
)

// TestImageTagRows checks the ImageTag records written for registry tags:
// one row per repository and tag, moved on re-push, and written only when the
// digest changes (ADR 0020, section 4).
func TestImageTagRows(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	ctx := t.Context()
	d1 := "sha256:" + strings.Repeat("a", 64)
	d2 := "sha256:" + strings.Repeat("b", 64)
	record := func(tag, digest string) {
		t.Helper()
		if err := ts.recordImageTag(ctx, registry.Tag{Repository: "team/app", Tag: tag, Digest: digest, MediaType: "m"}); err != nil {
			t.Fatalf("record %s: %v", tag, err)
		}
	}
	record("v1", d1)
	record("v1", d1) // unchanged: the row is not rewritten
	record("latest", d1)
	record("v1", d2) // moved

	got, err := ts.listImageTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []registry.Tag{
		{Repository: "team/app", Tag: "latest", Digest: d1, MediaType: "m"},
		{Repository: "team/app", Tag: "v1", Digest: d2, MediaType: "m"},
	}
	if len(got) != len(want) {
		t.Fatalf("tags = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tag %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	rec, err := ts.store.Get(ctx, v1alpha1.KindImageTag, "team/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Generation != 2 {
		t.Fatalf("generation of team/app:v1 = %d, want 2 (create, then one move)", rec.Generation)
	}
	if err := ts.recordImageTag(ctx, registry.Tag{Repository: "team/app", Tag: "bad"}); err == nil {
		t.Fatal("a tag without a digest was recorded")
	}
	if ok, err := ts.registryNodeExists(ctx, "missing"); err != nil || ok {
		t.Fatalf("registryNodeExists(missing) = %v, %v", ok, err)
	}
	if _, err := ts.store.Get(ctx, v1alpha1.KindImageTag, "nope:none"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing tag: %v", err)
	}
}
