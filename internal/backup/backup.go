// Package backup reads and writes the cluster backup file, format
// shoal-backup/v1.
//
// The file is JSON. The records are the store's records as they are stored:
// Secret specs stay encrypted with the cluster's secret key, so a backup is
// as sensitive as the key is. Restoring needs that same secret.key.
//
// records_sha256 is the SHA-256 of the compact JSON encoding of the records
// array. Decode recomputes it, so a damaged or edited file is refused.
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kaiserern/shoal/internal/store"
)

// Format names the layout written by Encode and accepted by Decode.
const Format = "shoal-backup/v1"

// Info describes a backup file.
type Info struct {
	Format        string
	CreatedAt     time.Time
	ServerVersion string
	Revision      int64
	RecordCount   int
	SHA256        string
}

type file struct {
	Format        string    `json:"format"`
	CreatedAt     time.Time `json:"createdAt"`
	ServerVersion string    `json:"serverVersion"`
	Revision      int64     `json:"revision"`
	RecordCount   int       `json:"recordCount"`
	RecordsSHA256 string    `json:"recordsSha256"`
	Records       []record  `json:"records"`
}

type record struct {
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	UID        string    `json:"uid"`
	Generation int64     `json:"generation"`
	Revision   int64     `json:"revision"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Spec       []byte    `json:"spec"`
	Status     []byte    `json:"status"`
}

// Encode returns the backup file for d, indented for reading.
func Encode(d store.Dump, serverVersion string, now time.Time) ([]byte, Info, error) {
	recs := make([]record, 0, len(d.Records))
	for _, r := range d.Records {
		recs = append(recs, record{
			Kind: r.Kind, Name: r.Name, UID: r.UID, Generation: r.Generation, Revision: r.Revision,
			CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(), Spec: r.Spec, Status: r.Status,
		})
	}
	sum, err := recordsSum(recs)
	if err != nil {
		return nil, Info{}, err
	}
	f := file{
		Format: Format, CreatedAt: now.UTC(), ServerVersion: serverVersion,
		Revision: d.Revision, RecordCount: len(recs), RecordsSHA256: sum, Records: recs,
	}
	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, Info{}, fmt.Errorf("encode backup: %w", err)
	}
	out = append(out, '\n')
	return out, Info{
		Format: Format, CreatedAt: f.CreatedAt, ServerVersion: serverVersion,
		Revision: d.Revision, RecordCount: len(recs), SHA256: sum,
	}, nil
}

// Decode checks the format, the record count, the SHA-256 and that no
// (kind, name) appears twice, then returns the dump.
func Decode(data []byte) (store.Dump, Info, error) {
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return store.Dump{}, Info{}, fmt.Errorf("not a backup file: %w", err)
	}
	if f.Format != Format {
		return store.Dump{}, Info{}, fmt.Errorf("unsupported backup format %q (want %q)", f.Format, Format)
	}
	sum, err := recordsSum(f.Records)
	if err != nil {
		return store.Dump{}, Info{}, err
	}
	if sum != f.RecordsSHA256 {
		return store.Dump{}, Info{}, fmt.Errorf("records sha256 mismatch (file says %s, records hash to %s): the backup is damaged or was edited", f.RecordsSHA256, sum)
	}
	if f.RecordCount != len(f.Records) {
		return store.Dump{}, Info{}, fmt.Errorf("record count %d does not match %d records", f.RecordCount, len(f.Records))
	}
	type key struct{ kind, name string }
	seen := make(map[key]bool, len(f.Records))
	recs := make([]store.Record, 0, len(f.Records))
	for _, r := range f.Records {
		k := key{r.Kind, r.Name}
		if seen[k] {
			return store.Dump{}, Info{}, fmt.Errorf("duplicate record %s %q", r.Kind, r.Name)
		}
		seen[k] = true
		recs = append(recs, store.Record{
			Kind: r.Kind, Name: r.Name, UID: r.UID, Generation: r.Generation, Revision: r.Revision,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Spec: r.Spec, Status: r.Status,
		})
	}
	return store.Dump{Revision: f.Revision, Records: recs}, Info{
		Format: f.Format, CreatedAt: f.CreatedAt, ServerVersion: f.ServerVersion,
		Revision: f.Revision, RecordCount: f.RecordCount, SHA256: sum,
	}, nil
}

// recordsSum hashes the compact encoding of the records. Encode and Decode
// both hash a []record, so the sums match when the content does.
func recordsSum(recs []record) (string, error) {
	if recs == nil {
		recs = []record{}
	}
	body, err := json.Marshal(recs)
	if err != nil {
		return "", fmt.Errorf("hash records: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}
