package projmigrate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/store/memory"
	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

var (
	ctx     = context.Background()
	testKey = make([]byte, 32)
	encKind = []string{v1alpha1.KindSecret}
)

// labels returns the labels of a record in project p, as the server writes them.
func labels(p string) map[string]string { return tenancy.WithProject(nil, p) }

func mustCreate(t *testing.T, s store.Store, kind, name, p string, spec []byte) {
	t.Helper()
	if _, err := s.Create(ctx, store.Record{Kind: kind, Name: name, Spec: spec, Labels: labels(p)}); err != nil {
		t.Fatalf("create %s %q: %v", kind, name, err)
	}
}

func mustProto(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// preMigration fills a store the way pass A left it: bare names, cluster
// unique, with projects in labels, and every reference by bare name.
func preMigration(t *testing.T) *memory.Store {
	t.Helper()
	s := memory.New(nil)
	mustCreate(t, s, v1alpha1.KindWorkload, "api", tenancy.DefaultProject, mustProto(t, &v1alpha1.WorkloadSpec{Image: "api"}))
	mustCreate(t, s, v1alpha1.KindWorkload, "web", "team-a", mustProto(t, &v1alpha1.WorkloadSpec{Image: "web"}))
	mustCreate(t, s, v1alpha1.KindVolume, "data", "team-a", []byte("vol"))
	mustCreate(t, s, v1alpha1.KindRoute, "site", "team-a", mustProto(t, &v1alpha1.RouteSpec{Host: "site.example.com"}))
	mustCreate(t, s, v1alpha1.KindDatabase, "db", "team-a", []byte("db"))
	// A Secret is sealed under the name it is stored with.
	enc, err := store.NewEncryptingStore(s, testKey, v1alpha1.KindSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Create(ctx, store.Record{Kind: v1alpha1.KindSecret, Name: "creds", Spec: []byte("hunter2"), Labels: labels("team-a")}); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, v1alpha1.KindReplica, "web-abc", tenancy.DefaultProject,
		mustProto(t, &v1alpha1.ReplicaSpec{Workload: "web"}))
	mustCreate(t, s, v1alpha1.KindReplica, "api-xyz", tenancy.DefaultProject,
		mustProto(t, &v1alpha1.ReplicaSpec{Workload: "api"}))
	mustCreate(t, s, v1alpha1.KindVolumeClaim, "data", tenancy.DefaultProject, []byte("web"))
	mustCreate(t, s, v1alpha1.KindRouteClaim, "example.com/", tenancy.DefaultProject, []byte("site"))
	mustCreate(t, s, v1alpha1.KindOwnerClaim, "Workload/web", tenancy.DefaultProject, []byte("db"))
	mustCreate(t, s, v1alpha1.KindDatabaseBackup, "b1", tenancy.DefaultProject,
		mustProto(t, &v1alpha1.DatabaseBackupRecord{Id: "b1", Database: "db"}))
	return s
}

func get(t *testing.T, s store.Store, kind, name string) store.Record {
	t.Helper()
	r, err := s.Get(ctx, kind, name)
	if err != nil {
		t.Fatalf("get %s %q: %v", kind, name, err)
	}
	return r
}

func TestFreshStoreOnlyWritesVersion(t *testing.T) {
	s := memory.New(nil)
	if err := Run(ctx, s, testKey, encKind, nil, nil); err != nil {
		t.Fatal(err)
	}
	m := get(t, s, MetaKind, MetaName)
	if string(m.Status) != `{"version":1}` {
		t.Fatalf("marker = %s", m.Status)
	}
	rev := s.Revision()
	if err := Run(ctx, s, testKey, encKind, nil, nil); err != nil {
		t.Fatal(err)
	}
	if s.Revision() != rev {
		t.Fatalf("second run wrote to the store: revision %d -> %d", rev, s.Revision())
	}
}

func TestMigratesRenamesAndRewritesReferences(t *testing.T) {
	s := preMigration(t)
	backups := 0
	if err := Run(ctx, s, testKey, encKind, func(store.Dump) error { backups++; return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if backups != 1 {
		t.Fatalf("backups = %d, want 1", backups)
	}
	// Default objects keep their keys; project objects move.
	get(t, s, v1alpha1.KindWorkload, "api")
	web := get(t, s, v1alpha1.KindWorkload, "web.team-a")
	if tenancy.ProjectOf(web.Labels) != "team-a" || specImage(t, web.Spec) != "web" {
		t.Fatalf("moved workload: %+v", web)
	}
	if _, err := s.Get(ctx, v1alpha1.KindWorkload, "web"); err == nil {
		t.Fatal("bare key of a project workload still present")
	}
	get(t, s, v1alpha1.KindVolume, "data.team-a")
	get(t, s, v1alpha1.KindRoute, "site.team-a")
	get(t, s, v1alpha1.KindDatabase, "db.team-a")

	// Cluster-wide references now name the keys.
	var rs v1alpha1.ReplicaSpec
	if err := proto.Unmarshal(get(t, s, v1alpha1.KindReplica, "web-abc").Spec, &rs); err != nil || rs.GetWorkload() != "web.team-a" {
		t.Fatalf("replica of web: %v %q", err, rs.GetWorkload())
	}
	if err := proto.Unmarshal(get(t, s, v1alpha1.KindReplica, "api-xyz").Spec, &rs); err != nil || rs.GetWorkload() != "api" {
		t.Fatalf("replica of api changed: %v %q", err, rs.GetWorkload())
	}
	vc := get(t, s, v1alpha1.KindVolumeClaim, "data.team-a")
	if string(vc.Spec) != "web.team-a" {
		t.Fatalf("volume claim spec = %q", vc.Spec)
	}
	if string(get(t, s, v1alpha1.KindRouteClaim, "example.com/").Spec) != "site.team-a" {
		t.Fatal("route claim not rewritten")
	}
	oc := get(t, s, v1alpha1.KindOwnerClaim, "Workload/web.team-a")
	if string(oc.Spec) != "db.team-a" {
		t.Fatalf("owner claim spec = %q", oc.Spec)
	}
	var br v1alpha1.DatabaseBackupRecord
	if err := proto.Unmarshal(get(t, s, v1alpha1.KindDatabaseBackup, "b1").Spec, &br); err != nil || br.GetDatabase() != "db.team-a" {
		t.Fatalf("backup record: %v %q", err, br.GetDatabase())
	}

	// The Secret was re-sealed under its new key: it opens through the store
	// and not under the old name.
	enc, _ := store.NewEncryptingStore(s, testKey, v1alpha1.KindSecret)
	sec, err := enc.Get(ctx, v1alpha1.KindSecret, "creds.team-a")
	if err != nil || string(sec.Spec) != "hunter2" {
		t.Fatalf("re-sealed secret: %q, %v", sec.Spec, err)
	}

	mk := get(t, s, MetaKind, MetaName)
	var m marker
	if err := json.Unmarshal(mk.Status, &m); err != nil || m.Version != Version {
		t.Fatalf("marker: %s", mk.Status)
	}

	// A second run finds nothing to do.
	rev := s.Revision()
	if err := Run(ctx, s, testKey, encKind, nil, nil); err != nil {
		t.Fatal(err)
	}
	if s.Revision() != rev {
		t.Fatal("migrated store changed on the second run")
	}
}

func TestSameNameInTwoProjects(t *testing.T) {
	s := memory.New(nil)
	mustCreate(t, s, v1alpha1.KindWorkload, "web", tenancy.DefaultProject, []byte("a"))
	mustCreate(t, s, v1alpha1.KindWorkload, tenancy.Key("team-a", "web"), "team-a", []byte("b"))
	// The version record, as a migrated store has it.
	if err := s.Rekey(ctx, nil, []store.Record{{Kind: MetaKind, Name: MetaName, Status: []byte(`{"version":1}`)}}); err != nil {
		t.Fatal(err)
	}
	d, _ := s.Export(ctx)
	plan, err := Build(d.Records, testKey, encKind)
	if err != nil || !plan.Empty() {
		t.Fatalf("two projects with one name: plan %+v, err %v", plan, err)
	}
	if string(get(t, s, v1alpha1.KindWorkload, "web").Spec) != "a" || string(get(t, s, v1alpha1.KindWorkload, "web.team-a").Spec) != "b" {
		t.Fatal("the two objects are not separate")
	}
}

func TestCrashBeforeCommitLeavesStoreUnchanged(t *testing.T) {
	inner := preMigration(t)
	before, _ := inner.Export(ctx)
	crash := &crashingStore{Store: inner}
	if err := Run(ctx, crash, testKey, encKind, func(store.Dump) error { return nil }, nil); err == nil {
		t.Fatal("run succeeded through a crash")
	}
	after, _ := inner.Export(ctx)
	if after.Revision != before.Revision || len(after.Records) != len(before.Records) {
		t.Fatalf("crash changed the store: revision %d -> %d", before.Revision, after.Revision)
	}
	if _, err := inner.Get(ctx, MetaKind, MetaName); err == nil {
		t.Fatal("version record written by a crashed migration")
	}
	// The restart completes the migration.
	if err := Run(ctx, inner, testKey, encKind, nil, nil); err != nil {
		t.Fatal(err)
	}
	get(t, inner, v1alpha1.KindWorkload, "web.team-a")
}

// crashingStore fails the commit of every Rekey, as a crash before commit
// would: nothing is written.
type crashingStore struct{ store.Store }

func (c *crashingStore) Export(ctx context.Context) (store.Dump, error) {
	return c.Store.(store.Exporter).Export(ctx)
}

func (c *crashingStore) Rekey(context.Context, []store.Move, []store.Record) error {
	return errors.New("crash before commit")
}

func TestBackupFailureChangesNothing(t *testing.T) {
	s := preMigration(t)
	rev := s.Revision()
	err := Run(ctx, s, testKey, encKind, func(store.Dump) error { return errors.New("disk full") }, nil)
	if err == nil {
		t.Fatal("run succeeded without a backup")
	}
	if s.Revision() != rev {
		t.Fatal("store changed although the backup failed")
	}
}

func TestRefusesPartialState(t *testing.T) {
	cases := map[string]func(t *testing.T, s *memory.Store){
		"qualified key with no version record": func(t *testing.T, s *memory.Store) {
			mustCreate(t, s, v1alpha1.KindWorkload, "web.team-a", "team-a", []byte("x"))
		},
		"version 1 but a bare project key": func(t *testing.T, s *memory.Store) {
			mustCreate(t, s, v1alpha1.KindWorkload, "web", "team-a", []byte("x"))
			_ = s.Rekey(ctx, nil, []store.Record{{Kind: MetaKind, Name: MetaName, Status: []byte(`{"version":1}`)}})
		},
		"default name with a dot": func(t *testing.T, s *memory.Store) {
			mustCreate(t, s, v1alpha1.KindWorkload, "web.team-a", tenancy.DefaultProject, []byte("x"))
		},
		"store newer than the server": func(t *testing.T, s *memory.Store) {
			_ = s.Rekey(ctx, nil, []store.Record{{Kind: MetaKind, Name: MetaName, Status: []byte(`{"version":2}`)}})
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			s := memory.New(nil)
			setup(t, s)
			rev := s.Revision()
			err := Run(ctx, s, testKey, encKind, nil, nil)
			if !errors.Is(err, ErrPartial) {
				t.Fatalf("err = %v, want ErrPartial", err)
			}
			if s.Revision() != rev {
				t.Fatal("a refused migration changed the store")
			}
		})
	}
}

// specImage returns the image of an encoded Workload spec.
func specImage(t *testing.T, b []byte) string {
	t.Helper()
	var ws v1alpha1.WorkloadSpec
	if err := proto.Unmarshal(b, &ws); err != nil {
		t.Fatal(err)
	}
	return ws.GetImage()
}

// TestRefusesCrossProjectReferences checks that pass-A data holding a
// reference to another project is not migrated: the refusal lists the
// reference, changes nothing, and takes no backup.
func TestRefusesCrossProjectReferences(t *testing.T) {
	s := memory.New(nil)
	mustCreate(t, s, v1alpha1.KindService, "api", tenancy.DefaultProject, mustProto(t, &v1alpha1.ServiceSpec{Selector: "web"}))
	mustCreate(t, s, v1alpha1.KindRoute, "site", "team-a", mustProto(t, &v1alpha1.RouteSpec{Host: "site.example.com", Service: "api"}))
	rev := s.Revision()
	backups := 0
	err := Run(ctx, s, testKey, encKind, func(store.Dump) error { backups++; return nil }, nil)
	if !errors.Is(err, ErrCrossProject) {
		t.Fatalf("err = %v, want ErrCrossProject", err)
	}
	want := `Route "site" in project "team-a" references Service "api", which is in project "default"`
	if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "remedy:") {
		t.Fatalf("refusal does not name the reference and the remedy: %v", err)
	}
	if backups != 0 {
		t.Fatalf("a backup was taken for a refused migration")
	}
	if s.Revision() != rev {
		t.Fatal("a refused migration changed the store")
	}
	if _, err := s.Get(ctx, MetaKind, MetaName); err == nil {
		t.Fatal("version record written by a refused migration")
	}
}

// TestSameProjectReferencesMigrate checks that a reference within one project
// is not a cross-project reference, even when another project holds the name.
func TestSameProjectReferencesMigrate(t *testing.T) {
	s := memory.New(nil)
	mustCreate(t, s, v1alpha1.KindService, "api", tenancy.DefaultProject, mustProto(t, &v1alpha1.ServiceSpec{Selector: "web"}))
	mustCreate(t, s, v1alpha1.KindService, "edge", "team-a", mustProto(t, &v1alpha1.ServiceSpec{Selector: "web"}))
	mustCreate(t, s, v1alpha1.KindRoute, "site", "team-a", mustProto(t, &v1alpha1.RouteSpec{Host: "site.example.com", Service: "edge"}))
	if err := Run(ctx, s, testKey, encKind, nil, nil); err != nil {
		t.Fatal(err)
	}
	get(t, s, v1alpha1.KindRoute, "site.team-a")
}
