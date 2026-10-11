package controller

import (
	"bytes"
	"strings"
	"testing"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/database"
	"example.com/shoal/internal/objects"
	"example.com/shoal/internal/resource"
	"example.com/shoal/internal/store"
)

// dbApp is the app label of the Databases the tests apply.
const dbApp = "web"

// database applies a Database the way the API does (defaulted, validated),
// labelled with the app dbApp.
func (e *env) database(name, version string) {
	e.t.Helper()
	e.databaseLabelled(name, version, map[string]string{appLabel: dbApp})
}

func (e *env) databaseLabelled(name, version string, labels map[string]string) {
	e.t.Helper()
	s := &v1alpha1.DatabaseSpec{Engine: "postgres", Version: version, Size: "5Gi"}
	resource.DefaultDatabase(s)
	if err := resource.ValidateDatabase(name, s); err != nil {
		e.t.Fatal(err)
	}
	spec, err := objects.Encode(s)
	if err != nil {
		e.t.Fatal(err)
	}
	if r, err := e.st.Get(ctx, v1alpha1.KindDatabase, name); err == nil {
		if _, err := e.st.Update(ctx, v1alpha1.KindDatabase, name, spec, r.Generation); err != nil {
			e.t.Fatal(err)
		}
		return
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindDatabase, Name: name, Spec: spec, Labels: labels}); err != nil {
		e.t.Fatal(err)
	}
}

// record returns the stored record of kind/name, or fails the test.
func (e *env) record(kind, name string) store.Record {
	e.t.Helper()
	r, err := e.st.Get(ctx, kind, name)
	if err != nil {
		e.t.Fatalf("%s %s: %v", kind, name, err)
	}
	return r
}

func (e *env) dbStatus(name string) *v1alpha1.DatabaseStatus {
	e.t.Helper()
	d, err := objects.Database(e.record(v1alpha1.KindDatabase, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return d.Status
}

// secretData returns the decoded data of the credentials Secret, read through st
// (so an encrypting store decrypts it).
func (e *env) secretData(st store.Store, name string) map[string]string {
	e.t.Helper()
	r, err := st.Get(ctx, v1alpha1.KindSecret, name)
	if err != nil {
		e.t.Fatal(err)
	}
	s, err := objects.Secret(r)
	if err != nil {
		e.t.Fatal(err)
	}
	return s.Spec.Data
}

func (e *env) dbWorkload(name string) *v1alpha1.Workload {
	e.t.Helper()
	w, err := objects.Workload(e.record(v1alpha1.KindWorkload, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

func (e *env) hasRecord(kind, name string) bool {
	_, err := e.st.Get(ctx, kind, name)
	return err == nil
}

const dbCredentials = "app-db-credentials"

// A Database expands into exactly four owned objects, each claimed by it.
func TestDatabaseExpandsIntoOwnedObjects(t *testing.T) {
	e := newEnv(t)
	e.database("app-db", "16.15")
	e.reconcile()

	vol, err := objects.Volume(e.record(v1alpha1.KindVolume, "app-db-data"))
	if err != nil {
		t.Fatal(err)
	}
	if vol.Spec.GetSize() != "5Gi" || vol.Spec.GetOwner().GetUid() != 999 || vol.Spec.GetOwner().GetGid() != 999 ||
		vol.Spec.GetMode() != "0700" || vol.Spec.GetRetention() != resource.RetentionRetain || vol.Spec.GetAccessMode() != "RWO" {
		t.Errorf("volume spec = %v", vol.Spec)
	}

	w := e.dbWorkload("app-db")
	image, _ := database.Image("postgres", "16.15")
	if w.Spec.GetImage() != image || w.Spec.GetReplicas() != 1 {
		t.Errorf("workload image %q replicas %d, want %q and 1", w.Spec.GetImage(), w.Spec.GetReplicas(), image)
	}
	if mounts := w.Spec.GetVolumes(); len(mounts) != 1 || mounts[0].GetName() != "app-db-data" || mounts[0].GetMountPath() != "/var/lib/postgresql/data" || mounts[0].GetReadOnly() {
		t.Errorf("volume mounts = %v", mounts)
	}
	env := map[string]*v1alpha1.EnvVar{}
	for _, v := range w.Spec.GetEnv() {
		env[v.GetName()] = v
	}
	for name, key := range map[string]string{"POSTGRES_USER": "username", "POSTGRES_PASSWORD": "password", "POSTGRES_DB": "database"} {
		ref := env[name].GetValueFrom().GetSecret()
		if ref.GetName() != dbCredentials || ref.GetKey() != key {
			t.Errorf("%s from %v, want secret %s key %s", name, ref, dbCredentials, key)
		}
	}
	if env["PGDATA"].GetValue() != "/var/lib/postgresql/data/pgdata" {
		t.Errorf("PGDATA = %q", env["PGDATA"].GetValue())
	}
	if w.Spec.GetResources().GetCpu() != "500m" || w.Spec.GetResources().GetMemory() != "384Mi" {
		t.Errorf("resources = %v", w.Spec.GetResources())
	}
	if hc := w.Spec.GetHealthCheck(); strings.Join(hc.GetExec().GetCommand(), " ") != "pg_isready -U app -d app_db" {
		t.Errorf("health check = %v, want pg_isready", hc)
	}
	if p := w.Spec.GetPorts(); len(p) != 1 || p[0].GetContainerPort() != 5432 {
		t.Errorf("ports = %v", p)
	}

	svc, err := objects.Service(e.record(v1alpha1.KindService, "app-db"))
	if err != nil {
		t.Fatal(err)
	}
	if svc.Spec.GetSelector() != "app-db" || len(svc.Spec.GetPorts()) != 1 || svc.Spec.GetPorts()[0].GetPort() != 5432 {
		t.Errorf("service spec = %v", svc.Spec)
	}

	for _, kind := range []string{v1alpha1.KindVolume, v1alpha1.KindWorkload, v1alpha1.KindService, v1alpha1.KindSecret} {
		name := "app-db"
		if kind == v1alpha1.KindVolume {
			name = "app-db-data"
		} else if kind == v1alpha1.KindSecret {
			name = dbCredentials
		}
		r := e.record(kind, name)
		if r.Labels["app"] != dbApp {
			t.Errorf("%s labels = %v, want app=%s copied from the Database", kind, r.Labels, dbApp)
		}
		if owner := e.record(v1alpha1.KindOwnerClaim, kind+"/"+name); string(owner.Spec) != "app-db" {
			t.Errorf("claim of %s/%s = %q", kind, name, owner.Spec)
		}
	}
	// Exactly one object of each owned kind.
	for _, kind := range []string{v1alpha1.KindVolume, v1alpha1.KindWorkload, v1alpha1.KindService, v1alpha1.KindSecret} {
		if recs, _ := e.st.List(ctx, kind); len(recs) != 1 {
			t.Errorf("%d %s objects, want 1", len(recs), kind)
		}
	}
	if st := e.dbStatus("app-db"); st.GetPhase() != "Provisioning" || st.GetEndpoint() != "app-db.shoal:5432" || st.GetVersion() != "16.15" {
		t.Errorf("status = %v", st)
	}
}

// The password is generated once and never changes on reconcile or upgrade.
func TestCredentialsCreatedOnceAndStable(t *testing.T) {
	e := newEnv(t)
	e.database("app-db", "16.14")
	e.reconcile()
	e.reconcile()
	before := e.secretData(e.st, dbCredentials)
	if len(before["password"]) < 40 {
		t.Fatalf("password %q is too short", before["password"])
	}
	e.reconcile()
	rev := e.st.Revision()
	e.reconcile()
	if got := e.st.Revision(); got != rev {
		t.Errorf("a converged pass wrote to the store (revision %d -> %d)", rev, got)
	}
	e.database("app-db", "16.15")
	e.reconcile()
	after := e.secretData(e.st, dbCredentials)
	if after["password"] != before["password"] || after["url"] != before["url"] {
		t.Error("credentials changed across reconciles or an upgrade")
	}
	wantURL := "postgres://app:" + before["password"] + "@app-db.shoal:5432/app_db?sslmode=disable"
	if before["url"] != wantURL || before["host"] != "app-db.shoal" || before["port"] != "5432" || before["database"] != "app_db" || before["username"] != "app" {
		t.Errorf("credentials = %v", before)
	}
}

// The credentials Secret is encrypted at rest: the raw store holds no
// plaintext password, and the store's own read returns it.
func TestCredentialsStoredEncrypted(t *testing.T) {
	e := newEnv(t)
	key := bytes.Repeat([]byte{7}, 32)
	enc, err := store.NewEncryptingStore(e.st, key, v1alpha1.KindSecret)
	if err != nil {
		t.Fatal(err)
	}
	e.withStore(enc)
	e.database("app-db", "16.15")
	e.reconcile()

	pw := e.secretData(enc, dbCredentials)["password"]
	if pw == "" {
		t.Fatal("no password through the encrypting store")
	}
	raw := e.record(v1alpha1.KindSecret, dbCredentials)
	if bytes.Contains(raw.Spec, []byte(pw)) || bytes.Contains(raw.Spec, []byte("postgres://")) {
		t.Fatal("the raw store holds the credentials in plaintext")
	}
}

// A minor version change updates the image in place, and keeps the volume.
func TestMinorUpgradeUpdatesImage(t *testing.T) {
	e := newEnv(t)
	e.database("app-db", "16.14")
	e.reconcile()
	volBefore := e.record(v1alpha1.KindVolume, "app-db-data").Generation

	e.database("app-db", "16.15")
	e.reconcile()
	want, _ := database.Image("postgres", "16.15")
	if img := e.dbWorkload("app-db").Spec.GetImage(); img != want {
		t.Errorf("image = %q, want %q", img, want)
	}
	if st := e.dbStatus("app-db"); st.GetVersion() != "16.15" {
		t.Errorf("status version = %q, want 16.15", st.GetVersion())
	}
	if g := e.record(v1alpha1.KindVolume, "app-db-data").Generation; g != volBefore {
		t.Errorf("the data volume changed on a minor upgrade (generation %d -> %d)", volBefore, g)
	}
}

// A major version change is refused with a clear status, and the workload keeps
// its image (ADR 0016, section 3.8).
func TestMajorUpgradeRefused(t *testing.T) {
	e := newEnv(t)
	e.database("app-db", "16.15")
	e.reconcile()
	image := e.dbWorkload("app-db").Spec.GetImage()

	e.database("app-db", "17.11")
	e.reconcile()
	st := e.dbStatus("app-db")
	if st.GetPhase() != "Failed" || !strings.Contains(st.GetMessage(), "major upgrade from 16.15 to 17.11 is refused") {
		t.Errorf("status = %v, want Failed with the refusal", st)
	}
	if got := e.dbWorkload("app-db").Spec.GetImage(); got != image {
		t.Errorf("workload image changed to %q by a refused upgrade", got)
	}
}

// Deleting the Database removes its objects and their claims. The Volume is
// removed as an object while its retention (Retain) keeps the data on disk.
func TestDeleteCascadesAndRetainsVolume(t *testing.T) {
	e := newEnv(t)
	e.node("a", v1alpha1.NodeReady, 8000, 8192, 0)
	e.database("app-db", "16.15")
	e.reconcile()
	if vol, _ := objects.Volume(e.record(v1alpha1.KindVolume, "app-db-data")); vol.Spec.GetRetention() != resource.RetentionRetain {
		t.Fatalf("retention = %q, want Retain by default", vol.Spec.GetRetention())
	}

	if err := e.st.Delete(ctx, v1alpha1.KindDatabase, "app-db", 0); err != nil {
		t.Fatal(err)
	}
	// A Volume waits for its Workload's claim, so the cascade takes a few passes.
	for range 4 {
		e.reconcile()
	}
	for _, kind := range []string{v1alpha1.KindVolume, v1alpha1.KindWorkload, v1alpha1.KindService, v1alpha1.KindSecret, v1alpha1.KindOwnerClaim, v1alpha1.KindVolumeClaim} {
		if recs, _ := e.st.List(ctx, kind); len(recs) != 0 {
			t.Errorf("%d %s records left after the cascade", len(recs), kind)
		}
	}
	if n := len(e.replicas()); n != 0 {
		t.Errorf("%d replicas left after the cascade", n)
	}
}

// A user Workload with the Database's name is not adopted: the Database is
// refused and the user's object is untouched.
func TestNameOfUnownedObjectRefused(t *testing.T) {
	e := newEnv(t)
	e.workload("app-db", 1)
	e.database("app-db", "16.15")
	e.reconcile()
	if st := e.dbStatus("app-db"); st.GetPhase() != "Failed" || !strings.Contains(st.GetMessage(), "not owned by database") {
		t.Errorf("status = %v, want Failed: not owned", st)
	}
	if img := e.dbWorkload("app-db").Spec.GetImage(); img != "nginx:1.27" {
		t.Errorf("the user's workload was changed to %q", img)
	}
	if e.hasRecord(v1alpha1.KindOwnerClaim, "Workload/app-db") {
		t.Error("a claim was taken on the user's workload")
	}
}

// A major version alone resolves to the newest pinned minor, which is what the
// app spec emits (database: postgres defaults to version 16).
func TestMajorVersionResolvesToNewestMinor(t *testing.T) {
	e := newEnv(t)
	e.database("app-db", "16")
	e.reconcile()
	want, _ := database.Image("postgres", "16.15")
	if img := e.dbWorkload("app-db").Spec.GetImage(); img != want {
		t.Errorf("image = %q, want %q", img, want)
	}
	if st := e.dbStatus("app-db"); st.GetVersion() != "16.15" {
		t.Errorf("status version = %q, want 16.15", st.GetVersion())
	}
}

// Without an app label on the Database, owned objects get no labels.
func TestOwnedObjectsCopyOnlyAnAppLabel(t *testing.T) {
	e := newEnv(t)
	e.databaseLabelled("app-db", "16.15", nil)
	e.reconcile()
	for _, kind := range []string{v1alpha1.KindWorkload, v1alpha1.KindService} {
		if r := e.record(kind, "app-db"); len(r.Labels) != 0 {
			t.Errorf("%s labels = %v, want none", kind, r.Labels)
		}
	}
}

// The Database is Ready once its Workload has a ready replica, and not before.
func TestStatusReadyWhenWorkloadReady(t *testing.T) {
	e := newEnv(t)
	e.nodeDisk("a", v1alpha1.NodeReady, 100, 0) // the data Volume first-binds here
	e.database("app-db", "16.15")
	// Pass 1 creates the Workload, pass 2 schedules its replica (the scheduler
	// only sees Workloads that were there at the start of a pass).
	e.reconcile()
	e.reconcile()
	if st := e.dbStatus("app-db"); st.GetPhase() != "Provisioning" {
		t.Fatalf("phase = %q before the replica is ready, want Provisioning", st.GetPhase())
	}
	e.readyAll()
	e.reconcile() // the Workload's status counts the ready replica
	e.reconcile() // the Database reads it
	if st := e.dbStatus("app-db"); st.GetPhase() != "Ready" || st.GetMessage() != "" {
		t.Errorf("status = %v, want Ready", st)
	}
}
