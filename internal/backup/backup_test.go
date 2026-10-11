package backup

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/internal/store"
)

var now = time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)

func sampleDump() store.Dump {
	return store.Dump{Revision: 7, Records: []store.Record{
		{Kind: "Widget", Name: "a", UID: "u-a", Generation: 2, Revision: 5,
			CreatedAt: now, UpdatedAt: now.Add(time.Second), Spec: []byte(`{"v":2}`), Status: []byte(`{"ok":true}`)},
		{Kind: "Widget", Name: "b", UID: "u-b", Generation: 1, Revision: 7,
			CreatedAt: now, UpdatedAt: now, Spec: []byte{}, Status: nil},
	}}
}

func TestRoundTrip(t *testing.T) {
	d := sampleDump()
	data, info, err := Encode(d, "v-test", now)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != Format || info.RecordCount != 2 || info.Revision != 7 || info.ServerVersion != "v-test" {
		t.Fatalf("info = %+v", info)
	}
	got, dinfo, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if dinfo.SHA256 != info.SHA256 || got.Revision != 7 || len(got.Records) != 2 {
		t.Fatalf("decoded %+v, info %+v", got, dinfo)
	}
	for i, r := range got.Records {
		w := d.Records[i]
		if r.Kind != w.Kind || r.Name != w.Name || r.UID != w.UID || r.Generation != w.Generation ||
			r.Revision != w.Revision || !r.CreatedAt.Equal(w.CreatedAt) || !r.UpdatedAt.Equal(w.UpdatedAt) ||
			!bytes.Equal(r.Spec, w.Spec) || !bytes.Equal(r.Status, w.Status) {
			t.Errorf("record %d: got %+v, want %+v", i, r, w)
		}
	}
}

func TestDecodeRefusesTampering(t *testing.T) {
	data, _, err := Encode(sampleDump(), "v", now)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, old, new, want string
	}{
		{"edited record", `"name": "a"`, `"name": "z"`, "sha256 mismatch"},
		{"other format", Format, "shoal-backup/v2", "unsupported backup format"},
		{"count changed", `"recordCount": 2`, `"recordCount": 3`, "record count"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(string(data), c.old) {
				t.Fatalf("fixture does not contain %q", c.old)
			}
			bad := strings.Replace(string(data), c.old, c.new, 1)
			_, _, err := Decode([]byte(bad))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Decode error = %v, want %q", err, c.want)
			}
		})
	}
	if _, _, err := Decode([]byte("not json")); err == nil {
		t.Fatal("Decode accepted non-JSON")
	}
}

func TestDecodeRefusesDuplicateRecords(t *testing.T) {
	d := sampleDump()
	d.Records[1].Name = "a"
	data, _, err := Encode(d, "v", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decode(data); err == nil || !strings.Contains(err.Error(), "duplicate record") {
		t.Fatalf("Decode error = %v, want duplicate record", err)
	}
}
