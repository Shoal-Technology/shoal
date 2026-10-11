package storetest

import (
	"bytes"
	"errors"
	"testing"

	"github.com/Shoal-Technology/shoal/internal/store"
)

// runExport checks Export and Import on backends that implement them: a dump
// loads into an empty store with every field intact, the revision carries
// on from the dump, and a second Import is refused.
func runExport(t *testing.T, newStore func(t *testing.T) store.Store) {
	src := newStore(t)
	defer src.Close()
	ex, ok := src.(store.Exporter)
	if !ok {
		t.Skip("backend does not export")
	}
	a := mustCreate(t, src, "Widget", "a", `{"v":1}`)
	b := mustCreate(t, src, "Widget", "b", `{"v":1}`)
	if _, err := src.Update(ctx, "Widget", "b", []byte(`{"v":2}`), b.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := src.UpdateStatus(ctx, "Widget", a.Name, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, src, "Gadget", "g", "x")

	d, err := ex.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Records) != 3 {
		t.Fatalf("export has %d records, want 3", len(d.Records))
	}

	dst := newStore(t)
	defer dst.Close()
	im, ok := dst.(store.Importer)
	if !ok {
		t.Fatal("backend exports but cannot import")
	}
	if err := im.Import(ctx, d); err != nil {
		t.Fatalf("import: %v", err)
	}
	got, err := dst.(store.Exporter).Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != d.Revision {
		t.Errorf("revision after import = %d, want %d", got.Revision, d.Revision)
	}
	if len(got.Records) != len(d.Records) {
		t.Fatalf("records after import = %d, want %d", len(got.Records), len(d.Records))
	}
	for i := range d.Records {
		if !sameRecord(got.Records[i], d.Records[i]) {
			t.Errorf("record %d after import: got %+v, want %+v", i, got.Records[i], d.Records[i])
		}
	}

	if err := im.Import(ctx, d); !errors.Is(err, store.ErrNotEmpty) {
		t.Errorf("second import error = %v, want ErrNotEmpty", err)
	}
	r, err := dst.Create(ctx, store.Record{Kind: "Widget", Name: "new", Spec: []byte("n")})
	if err != nil {
		t.Fatal(err)
	}
	if r.Revision != d.Revision+1 {
		t.Errorf("first write after import has revision %d, want %d", r.Revision, d.Revision+1)
	}
}

func sameRecord(a, b store.Record) bool {
	return a.Kind == b.Kind && a.Name == b.Name && a.UID == b.UID &&
		a.Generation == b.Generation && a.Revision == b.Revision &&
		a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt) &&
		bytes.Equal(a.Spec, b.Spec) && bytes.Equal(a.Status, b.Status)
}
