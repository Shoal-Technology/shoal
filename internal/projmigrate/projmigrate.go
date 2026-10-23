// Package projmigrate moves a store to project-qualified keys (ADR 0023, pass
// B1). Before it runs, a project-scoped object in project P is keyed by its
// bare name, which is unique across the cluster. After it runs, the key is
// tenancy.Key(P, name), so two projects can each hold an object with the same
// name.
//
// The migration is versioned by a StoreMeta record. Build is a pure function
// of the records: it reports the moves that would bring the store to the
// current version, or an error when the state is partial. Run takes a backup,
// applies the whole plan in one atomic store.Rekeyer call, and checks the
// result. Running it again changes nothing.
package projmigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

// Version is the store schema version this code writes. Version 1 keys
// project-scoped records by tenancy.Key.
const Version = 1

// MetaKind and MetaName name the record that holds the schema version.
const (
	MetaKind = "StoreMeta"
	MetaName = "schema"
)

// ErrPartial means the store is in a state no version of this code writes:
// some records are keyed for a version and others are not, or the store is
// newer than this server. Nothing is changed. The operator restores a backup
// taken before the migration, or runs the server version that wrote it.
var ErrPartial = errors.New("partial project migration")

type marker struct {
	Version int `json:"version"`
}

// Plan is what Build found to do. From is the version the store holds (0
// when it has no StoreMeta record).
type Plan struct {
	From  int
	Moves []store.Move
	Puts  []store.Record
}

// Empty reports whether there is nothing to do.
func (p Plan) Empty() bool { return len(p.Moves) == 0 && len(p.Puts) == 0 }

// storedVersion returns the version recorded in recs, or 0 if there is none.
func storedVersion(recs []store.Record) (int, error) {
	for _, r := range recs {
		if r.Kind != MetaKind || r.Name != MetaName {
			continue
		}
		var m marker
		if err := json.Unmarshal(r.Status, &m); err != nil {
			return 0, fmt.Errorf("%w: the %s record is not readable: %v", ErrPartial, MetaKind, err)
		}
		return m.Version, nil
	}
	return 0, nil
}

// Build returns the plan that brings recs (every record of the store) to
// Version. key is the secret key, used to re-seal Secrets whose key changes.
// encKinds are the kinds whose specs are sealed.
func Build(recs []store.Record, key []byte, encKinds []string) (Plan, error) {
	from, err := storedVersion(recs)
	if err != nil {
		return Plan{}, err
	}
	if from > Version {
		return Plan{}, fmt.Errorf("%w: the store is version %d, newer than this server (%d)", ErrPartial, from, Version)
	}
	// Every name of every kind, and the project each project-scoped name is
	// in. Before version 1 a bare name is unique cluster-wide, so each bare
	// name maps to one project.
	byName := map[string]map[string]store.Record{}
	projectOf := map[string]map[string]string{}
	for _, r := range recs {
		if byName[r.Kind] == nil {
			byName[r.Kind] = map[string]store.Record{}
			projectOf[r.Kind] = map[string]string{}
		}
		byName[r.Kind][r.Name] = r
		if tenancy.ProjectScoped(r.Kind) {
			projectOf[r.Kind][r.Name] = tenancy.ProjectOf(r.Labels)
		}
	}
	// lookup returns the project of the object called name of kind, if it is
	// project-scoped and not in the default project. Default objects keep
	// their keys, so their references do not change.
	lookup := func(kind, name string) (string, bool) {
		p, ok := projectOf[kind][name]
		if !ok || p == tenancy.DefaultProject {
			return "", false
		}
		return p, true
	}

	var movedFrom []store.Record
	for _, r := range recs {
		if !tenancy.ProjectScoped(r.Kind) {
			continue
		}
		p := tenancy.ProjectOf(r.Labels)
		qualified := p != tenancy.DefaultProject && strings.HasSuffix(r.Name, "."+p)
		switch {
		case p == tenancy.DefaultProject && strings.Contains(r.Name, "."):
			return Plan{}, fmt.Errorf("%w: %s %q is in the default project but its name has a dot", ErrPartial, r.Kind, r.Name)
		case from == 0 && qualified:
			return Plan{}, fmt.Errorf("%w: %s %q is keyed by its project (%q) but the store has no version record", ErrPartial, r.Kind, r.Name, p)
		case from >= Version && p != tenancy.DefaultProject && !qualified:
			return Plan{}, fmt.Errorf("%w: %s %q is in project %q but keyed by its bare name, while the store is version %d", ErrPartial, r.Kind, r.Name, p, from)
		case from == 0 && p != tenancy.DefaultProject:
			movedFrom = append(movedFrom, r)
		}
	}

	var moves []store.Move
	moving := map[string]bool{}
	for _, r := range movedFrom {
		moving[r.Kind+"\x00"+r.Name] = true
	}
	for _, r := range movedFrom {
		to := tenancy.Key(tenancy.ProjectOf(r.Labels), r.Name)
		if _, taken := byName[r.Kind][to]; taken && !moving[r.Kind+"\x00"+to] {
			return Plan{}, fmt.Errorf("%w: %s %q cannot move to %q: a record with that key exists", ErrPartial, r.Kind, r.Name, to)
		}
		rec := r
		rec.Name = to
		if slicesContain(encKinds, r.Kind) {
			sealed, err := store.Reseal(key, r.Kind, r.Name, to, r.Spec)
			if err != nil {
				return Plan{}, fmt.Errorf("re-seal %s %q: %w", r.Kind, r.Name, err)
			}
			rec.Spec = sealed
		}
		moves = append(moves, store.Move{From: r.Name, Rec: rec})
	}

	// References held by cluster-wide records name project-scoped objects by
	// bare name. Those are rewritten to keys. References between project
	// objects stay bare: they resolve in the referrer's project.
	var puts []store.Record
	if from == 0 {
		var err error
		moves, puts, err = rewriteRefs(recs, lookup, moves)
		if err != nil {
			return Plan{}, err
		}
	}
	if len(moves) > 0 || from != Version {
		body, err := json.Marshal(marker{Version: Version})
		if err != nil {
			return Plan{}, err
		}
		puts = append(puts, store.Record{Kind: MetaKind, Name: MetaName, Status: body})
	}
	return Plan{From: from, Moves: moves, Puts: puts}, nil
}

// rewriteRefs returns the moves and puts that rewrite the references of the
// cluster-wide records. A record that is moved is not also put: its spec
// goes through its move. lookup gives the project of a bare name.
func rewriteRefs(recs []store.Record, lookup func(kind, name string) (string, bool), moves []store.Move) ([]store.Move, []store.Record, error) {
	moved := map[string]store.Move{}
	for _, m := range moves {
		moved[m.Rec.Kind+"\x00"+m.From] = m
	}
	var puts []store.Record
	for _, r := range recs {
		m, isMoved := moved[r.Kind+"\x00"+r.Name]
		cur := r
		if isMoved {
			cur = m.Rec
		}
		newName, newSpec, changed, err := refsOf(cur, lookup)
		if err != nil {
			return nil, nil, err
		}
		if !changed {
			continue
		}
		if isMoved {
			// The spec change rides on the move; the new name stays the key.
			idx := -1
			for i := range moves {
				if moves[i].From == r.Name && moves[i].Rec.Kind == r.Kind {
					idx = i
				}
			}
			moves[idx].Rec.Spec = newSpec
			continue
		}
		rec := r
		rec.Spec = newSpec
		if newName != "" && newName != r.Name {
			rec.Name = newName
			moves = append(moves, store.Move{From: r.Name, Rec: rec})
			continue
		}
		puts = append(puts, rec)
	}
	return moves, puts, nil
}

// refsOf returns the new name and spec of a cluster-wide record that refers
// to project-scoped objects, and whether either changed. Unknown kinds are
// returned unchanged.
func refsOf(r store.Record, lookup func(kind, name string) (string, bool)) (string, []byte, bool, error) {
	switch r.Kind {
	case v1alpha1.KindReplica:
		var spec v1alpha1.ReplicaSpec
		if err := proto.Unmarshal(r.Spec, &spec); err != nil {
			return "", nil, false, fmt.Errorf("replica %q: %w", r.Name, err)
		}
		p, ok := lookup(v1alpha1.KindWorkload, spec.GetWorkload())
		if !ok {
			return "", nil, false, nil
		}
		spec.Workload = tenancy.Key(p, spec.GetWorkload())
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(&spec)
		return "", b, true, err

	case v1alpha1.KindVolumeClaim:
		newName := r.Name
		if p, ok := lookup(v1alpha1.KindVolume, r.Name); ok {
			newName = tenancy.Key(p, r.Name)
		}
		spec := r.Spec
		if w := string(r.Spec); w != "" {
			if p, ok := lookup(v1alpha1.KindWorkload, w); ok {
				spec = []byte(tenancy.Key(p, w))
			}
		}
		return newName, spec, newName != r.Name || string(spec) != string(r.Spec), nil

	case v1alpha1.KindRouteClaim:
		// The claim is keyed by host and path, which are global. Its spec is
		// the Route's bare name.
		if p, ok := lookup(v1alpha1.KindRoute, string(r.Spec)); ok {
			spec := []byte(tenancy.Key(p, string(r.Spec)))
			return "", spec, true, nil
		}
		return "", nil, false, nil

	case v1alpha1.KindOwnerClaim:
		// Name is "<kind>/<name>" of the owned object; the spec is the
		// Database's bare name.
		kind, name, ok := strings.Cut(r.Name, "/")
		newName := r.Name
		if ok {
			if p, found := lookup(kind, name); found {
				newName = kind + "/" + tenancy.Key(p, name)
			}
		}
		spec := r.Spec
		if p, found := lookup(v1alpha1.KindDatabase, string(r.Spec)); found {
			spec = []byte(tenancy.Key(p, string(r.Spec)))
		}
		return newName, spec, newName != r.Name || string(spec) != string(r.Spec), nil

	case v1alpha1.KindDatabaseBackup:
		var rec v1alpha1.DatabaseBackupRecord
		if err := proto.Unmarshal(r.Spec, &rec); err != nil {
			return "", nil, false, fmt.Errorf("database backup %q: %w", r.Name, err)
		}
		p, ok := lookup(v1alpha1.KindDatabase, rec.GetDatabase())
		if !ok {
			return "", nil, false, nil
		}
		rec.Database = tenancy.Key(p, rec.GetDatabase())
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(&rec)
		return "", b, true, err
	}
	return "", nil, false, nil
}

func slicesContain(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Run brings st to Version. st must implement store.Exporter and
// store.Rekeyer. A store that is not leader returns the Export error
// unchanged, so the caller can wait for leadership and try again. Backup is
// called with the dump before anything changes, and only when a migration
// will run on a store that has records.
func Run(ctx context.Context, st store.Store, key []byte, encKinds []string, backup func(store.Dump) error, log *slog.Logger) error {
	ex, ok := st.(store.Exporter)
	if !ok {
		return errors.New("project migration: this store backend cannot export")
	}
	rk, ok := st.(store.Rekeyer)
	if !ok {
		return errors.New("project migration: this store backend cannot re-key records")
	}
	d, err := ex.Export(ctx)
	if err != nil {
		return err
	}
	plan, err := Build(d.Records, key, encKinds)
	if err != nil {
		return fmt.Errorf("project migration refused, nothing changed: %w (restore a backup taken before the migration, or run the server version that wrote the store)", err)
	}
	if plan.Empty() {
		return nil
	}
	if len(d.Records) > 0 && backup != nil {
		if err := backup(d); err != nil {
			return fmt.Errorf("project migration: backup before migrating: %w (nothing changed)", err)
		}
	}
	if err := rk.Rekey(ctx, plan.Moves, plan.Puts); err != nil {
		return fmt.Errorf("project migration: %w (nothing changed)", err)
	}
	// Check the result: a second Build must find nothing to do.
	d, err = ex.Export(ctx)
	if err != nil {
		return fmt.Errorf("project migration: verify: %w", err)
	}
	again, err := Build(d.Records, key, encKinds)
	if err != nil || !again.Empty() {
		return fmt.Errorf("project migration: verify failed after the change (%v): restore the backup taken before the migration", err)
	}
	if log != nil {
		log.Info("migrated the store to project keys", "fromVersion", plan.From, "toVersion", Version,
			"renamed", len(plan.Moves), "rewritten", len(plan.Puts)-1)
	}
	return nil
}
