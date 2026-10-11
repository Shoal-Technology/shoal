package backup

import (
	"testing"
	"time"

	"example.com/shoal/internal/store"
)

// FuzzDecode checks that arbitrary backup bytes never panic Decode, and that
// any accepted file survives an Encode and Decode round trip with the same
// records and revision.
func FuzzDecode(f *testing.F) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	valid, _, err := Encode(store.Dump{
		Revision: 7,
		Records: []store.Record{{
			Kind: "Workload", Name: "web", UID: "u1", Generation: 2, Revision: 7,
			CreatedAt: now, UpdatedAt: now, Spec: []byte(`{"image":"nginx"}`), Status: []byte(`{}`),
		}},
	}, "test", now)
	if err != nil {
		f.Fatal(err)
	}
	empty, _, err := Encode(store.Dump{}, "test", now)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add(empty)
	f.Add([]byte(""))
	f.Add([]byte("{}"))
	f.Add([]byte("null"))
	f.Add([]byte(`{"format":"shoal-backup/v1","recordCount":1,"records":[null]}`))
	f.Add([]byte(`{"format":"shoal-backup/v1","records":[{"kind":"A","name":"x"},{"kind":"A","name":"x"}]}`))
	f.Add([]byte(`{"format":"shoal-backup/v1","createdAt":"not a time"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		d, info, err := Decode(data)
		if err != nil {
			return
		}
		out, _, err := Encode(d, info.ServerVersion, info.CreatedAt)
		if err != nil {
			t.Fatalf("accepted backup cannot be re-encoded: %v", err)
		}
		d2, info2, err := Decode(out)
		if err != nil {
			t.Fatalf("re-encoded backup is refused: %v", err)
		}
		if d2.Revision != d.Revision || len(d2.Records) != len(d.Records) || info2.RecordCount != info.RecordCount {
			t.Fatalf("round trip changed the dump: revision %d->%d, records %d->%d",
				d.Revision, d2.Revision, len(d.Records), len(d2.Records))
		}
	})
}
