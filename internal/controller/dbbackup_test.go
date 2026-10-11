package controller

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/database"
	"example.com/shoal/internal/objects"
	"example.com/shoal/internal/resource"
	"example.com/shoal/internal/store"
)

// bkDatabase applies a Database with a backup schedule (none when schedule is "").
func (e *env) bkDatabase(name, version, schedule string, retain int32) {
	e.t.Helper()
	s := &v1alpha1.DatabaseSpec{Engine: "postgres", Version: version, Size: "5Gi"}
	resource.DefaultDatabase(s)
	if schedule != "" {
		s.Backup = &v1alpha1.DatabaseBackup{Schedule: schedule, Retain: retain, Targets: []string{"volume"}}
	}
	if err := resource.ValidateDatabase(name, s); err != nil {
		e.t.Fatal(err)
	}
	spec, err := objects.Encode(s)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindDatabase, Name: name, Spec: spec, Labels: map[string]string{appLabel: dbApp}}); err != nil {
		e.t.Fatal(err)
	}
}

// bkRestoreTarget applies a new Database that restores backup from.
func (e *env) bkRestoreTarget(name, from string) {
	e.t.Helper()
	s := &v1alpha1.DatabaseSpec{Engine: "postgres", Version: "16.15", Size: "5Gi", RestoreFrom: from}
	resource.DefaultDatabase(s)
	if err := resource.ValidateDatabase(name, s); err != nil {
		e.t.Fatal(err)
	}
	spec, err := objects.Encode(s)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindDatabase, Name: name, Spec: spec, Labels: map[string]string{appLabel: dbApp}}); err != nil {
		e.t.Fatal(err)
	}
}

// bkSetStatus sets the status of a Database.
func (e *env) bkSetStatus(name string, st *v1alpha1.DatabaseStatus) {
	e.t.Helper()
	b, err := objects.Encode(st)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.UpdateStatus(ctx, v1alpha1.KindDatabase, name, b); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) bkDB(name string) *v1alpha1.Database {
	e.t.Helper()
	d, err := objects.Database(e.record(v1alpha1.KindDatabase, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

// bkAddBackup stores a backup record.
func (e *env) bkAddBackup(db, id string, at time.Time, status, reason, version string) {
	e.t.Helper()
	b := &v1alpha1.DatabaseBackupRecord{
		Id: id, Database: db, CreatedAt: timestamppb.New(at), File: database.BackupDir + "/" + id + ".dump",
		Status: status, Reason: reason, Version: version, Sha256: "abc", Size: 10,
	}
	spec, err := objects.Encode(b)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindDatabaseBackup, Name: id, Spec: spec}); err != nil {
		e.t.Fatal(err)
	}
}

// bkSetBackupStatus changes the status of a stored backup.
func (e *env) bkSetBackupStatus(id, status, errText string) {
	e.t.Helper()
	rec := e.record(v1alpha1.KindDatabaseBackup, id)
	b, err := decodeBackup(rec)
	if err != nil {
		e.t.Fatal(err)
	}
	b.Status, b.Error = status, errText
	spec, err := objects.Encode(b)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Update(ctx, v1alpha1.KindDatabaseBackup, id, spec, rec.Generation); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) bkBackups(db string) []backupRec {
	e.t.Helper()
	recs, err := e.c.backupsOf(ctx, db)
	if err != nil {
		e.t.Fatal(err)
	}
	return recs
}

func bkIDs(recs []backupRec) []string {
	var ids []string
	for _, r := range recs {
		ids = append(ids, r.b.GetId())
	}
	return ids
}

func (e *env) bkWorkloadImage(name string) string {
	e.t.Helper()
	w, err := objects.Workload(e.record(v1alpha1.KindWorkload, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return w.GetSpec().GetImage()
}

// bkReplica stores a replica of workload on node, ready or not.
func (e *env) bkReplica(workload, name, node string, ready bool) {
	e.t.Helper()
	spec, err := objects.Encode(&v1alpha1.ReplicaSpec{Workload: workload, Node: node})
	if err != nil {
		e.t.Fatal(err)
	}
	st, err := objects.Encode(&v1alpha1.ReplicaStatus{Phase: "Running", Ready: ready})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindReplica, Name: name, Spec: spec, Status: st}); err != nil {
		e.t.Fatal(err)
	}
}

// bkReplicaJobResult sets the job result the agent reports for a replica.
func (e *env) bkReplicaJobResult(name string, res *v1alpha1.DatabaseJobResult) {
	e.t.Helper()
	rp, err := objects.Replica(e.record(v1alpha1.KindReplica, name))
	if err != nil {
		e.t.Fatal(err)
	}
	rp.Status.Job = res
	b, err := objects.Encode(rp.Status)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.UpdateStatus(ctx, v1alpha1.KindReplica, name, b); err != nil {
		e.t.Fatal(err)
	}
}

// bkPass runs the backup and restore sync over every Database, as a
// reconcile pass does.
func (e *env) bkPass() {
	e.t.Helper()
	s := &state{workloads: map[string]*v1alpha1.Workload{}, replicas: map[string]*replica{}}
	recs, err := e.st.List(ctx, v1alpha1.KindReplica)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range recs {
		rp, err := objects.Replica(r)
		if err != nil {
			e.t.Fatal(err)
		}
		s.replicas[r.Name] = &replica{rec: r, obj: rp}
	}
	dbs := map[string]*v1alpha1.Database{}
	drecs, err := e.st.List(ctx, v1alpha1.KindDatabase)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range drecs {
		d, err := objects.Database(r)
		if err != nil {
			e.t.Fatal(err)
		}
		dbs[r.Name] = d
	}
	if errs := e.c.syncDatabaseJobs(ctx, s, dbs); len(errs) > 0 {
		e.t.Fatal(errs)
	}
}

// A scheduled backup waits for its interval, and a backup in flight blocks the
// next one.
func TestScheduledBackupWaitsForItsInterval(t *testing.T) {
	e := newEnv(t)
	e.bkDatabase("blog-db", "16", "1h", 3)
	e.bkSetStatus("blog-db", &v1alpha1.DatabaseStatus{Phase: "Ready", Version: "16.15"})
	schedule := func() {
		t.Helper()
		if err := e.c.scheduleBackup(ctx, "blog-db", e.bkDB("blog-db"), e.bkBackups("blog-db")); err != nil {
			t.Fatal(err)
		}
	}

	// The first backup is due at once: none has run.
	schedule()
	recs := e.bkBackups("blog-db")
	if len(recs) != 1 || recs[0].b.GetStatus() != database.BackupPending || recs[0].b.GetReason() != database.ReasonScheduled {
		t.Fatalf("first schedule: %+v", recs)
	}
	e.bkSetBackupStatus(recs[0].b.GetId(), database.BackupOK, "")

	// The interval is measured from the last scheduled backup's creation.
	e.now = e.now.Add(30 * time.Minute)
	schedule()
	if n := len(e.bkBackups("blog-db")); n != 1 {
		t.Fatalf("a backup came before the interval: %d records", n)
	}
	e.now = e.now.Add(31 * time.Minute)
	schedule()
	recs = e.bkBackups("blog-db")
	if len(recs) != 2 {
		t.Fatalf("the interval passed but no backup was scheduled: %d records", len(recs))
	}

	// A backup in flight blocks the next one, even after the interval.
	e.now = e.now.Add(2 * time.Hour)
	schedule()
	if n := len(e.bkBackups("blog-db")); n != 2 {
		t.Fatalf("a pending backup did not block the next one: %d records", n)
	}
}

// Retention deletes the oldest ok backups beyond the count, once the new backup
// has succeeded.
func TestRetentionDeletesTheOldestBackups(t *testing.T) {
	e := newEnv(t)
	e.bkDatabase("blog-db", "16", "1h", 2)
	base := e.now.Add(-5 * time.Hour)
	for i := range 3 {
		e.bkAddBackup("blog-db", fmt.Sprintf("old-%d", i), base.Add(time.Duration(i)*time.Hour), database.BackupOK, database.ReasonScheduled, "16.15")
	}
	e.bkAddBackup("blog-db", "new-1", e.now, database.BackupRunning, database.ReasonScheduled, "16.15")
	recs := e.bkBackups("blog-db")
	var running backupRec
	for _, r := range recs {
		if r.b.GetId() == "new-1" {
			running = r
		}
	}
	prune := pruneSet(recs, 2, "new-1")
	if got := bkIDs(prune); !slices.Equal(got, []string{"old-0", "old-1"}) {
		t.Fatalf("prune set = %q, want the two oldest", got)
	}
	res := &v1alpha1.DatabaseJobResult{Id: "new-1", Ok: true, Size: 99, Sha256: "def"}
	if err := e.c.finishBackup(ctx, running, res, prune); err != nil {
		t.Fatal(err)
	}
	left := e.bkBackups("blog-db")
	if got := bkIDs(left); !slices.Equal(got, []string{"old-2", "new-1"}) {
		t.Fatalf("after retention = %q, want old-2 and new-1", got)
	}
	if last := left[len(left)-1].b; last.GetStatus() != database.BackupOK || last.GetSize() != 99 || last.GetSha256() != "def" {
		t.Fatalf("new backup record = %+v", last)
	}
}

// A failed backup is recorded as failed with its error, and prunes nothing.
func TestFailedBackupIsRecordedAndPrunesNothing(t *testing.T) {
	e := newEnv(t)
	e.bkDatabase("blog-db", "16", "1h", 1)
	e.bkAddBackup("blog-db", "old-0", e.now.Add(-time.Hour), database.BackupOK, database.ReasonScheduled, "16.15")
	e.bkAddBackup("blog-db", "new-1", e.now, database.BackupRunning, database.ReasonManual, "16.15")
	recs := e.bkBackups("blog-db")
	var running backupRec
	for _, r := range recs {
		if r.b.GetId() == "new-1" {
			running = r
		}
	}
	res := &v1alpha1.DatabaseJobResult{Id: "new-1", Ok: false, Error: "pg_dump exited 1: boom"}
	if err := e.c.finishBackup(ctx, running, res, pruneSet(recs, 1, "new-1")); err != nil {
		t.Fatal(err)
	}
	left := e.bkBackups("blog-db")
	if got := bkIDs(left); !slices.Equal(got, []string{"old-0", "new-1"}) {
		t.Fatalf("records = %q: a failed backup must not prune", got)
	}
	if b := left[1].b; b.GetStatus() != database.BackupFailed || !strings.Contains(b.GetError(), "boom") {
		t.Fatalf("failed record = %+v", b)
	}
}

// A minor upgrade takes a verified backup first: the workload keeps its old
// image while the backup runs, and a failed backup refuses the upgrade.
func TestMinorUpgradeWaitsForAVerifiedBackup(t *testing.T) {
	e := newEnv(t)
	e.database("blog-db", "16.14")
	if _, err := e.c.expandDatabase(ctx, e.bkDB("blog-db"), dbApp); err != nil {
		t.Fatal(err)
	}
	oldImage := e.bkWorkloadImage("blog-db")
	e.bkSetStatus("blog-db", &v1alpha1.DatabaseStatus{Phase: "Ready", Version: "16.14"})

	e.database("blog-db", "16.15")
	exp, err := e.c.expandDatabase(ctx, e.bkDB("blog-db"), dbApp)
	if err != nil {
		t.Fatal(err)
	}
	if exp.refusal != "" || exp.hold == "" {
		t.Fatalf("upgrade expansion = %+v, want a hold", exp)
	}
	if got := e.bkWorkloadImage("blog-db"); got != oldImage {
		t.Fatal("the workload changed while the pre-upgrade backup was pending")
	}
	recs := e.bkBackups("blog-db")
	if len(recs) != 1 || recs[0].b.GetReason() != database.ReasonUpgrade || recs[0].b.GetVersion() != "16.14" {
		t.Fatalf("pre-upgrade backup = %+v", recs)
	}

	e.bkSetBackupStatus(recs[0].b.GetId(), database.BackupOK, "")
	if exp, err = e.c.expandDatabase(ctx, e.bkDB("blog-db"), dbApp); err != nil || exp.hold != "" || exp.refusal != "" {
		t.Fatalf("verified backup: expansion %+v, err %v", exp, err)
	}
	if got := e.bkWorkloadImage("blog-db"); got == oldImage {
		t.Fatal("the workload did not move to the new minor after the verified backup")
	}

	// A failed backup refuses the upgrade. The workload stays on the old image.
	e.database("blog-db", "16.15")
	e.bkSetStatus("blog-db", &v1alpha1.DatabaseStatus{Phase: "Ready", Version: "16.14"})
	e.bkSetBackupStatus(recs[0].b.GetId(), database.BackupFailed, "disk full")
	exp, err = e.c.expandDatabase(ctx, e.bkDB("blog-db"), dbApp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(exp.refusal, "refused") || !strings.Contains(exp.refusal, "disk full") {
		t.Fatalf("failed backup: refusal = %q", exp.refusal)
	}
	if n := len(e.bkBackups("blog-db")); n != 1 {
		t.Fatalf("a refused upgrade requested %d backups before the retry window", n)
	}
	e.now = e.now.Add(retryFailedUpgrade)
	if exp, err = e.c.expandDatabase(ctx, e.bkDB("blog-db"), dbApp); err != nil || exp.hold == "" {
		t.Fatalf("after the retry window: expansion %+v, err %v", exp, err)
	}
	if n := len(e.bkBackups("blog-db")); n != 2 {
		t.Fatalf("the retry did not request a new backup: %d records", n)
	}
}

// A restore creates a new Database with the source's password, and the restore
// then runs in the source's replica once the new Database is Ready. The source
// is not written.
func TestRestoreCreatesTheNewDatabaseThenRunsPgRestore(t *testing.T) {
	e := newEnv(t)
	e.database("blog-db", "16.15")
	if _, err := e.c.expandDatabase(ctx, e.bkDB("blog-db"), dbApp); err != nil {
		t.Fatal(err)
	}
	e.bkSetStatus("blog-db", &v1alpha1.DatabaseStatus{Phase: "Ready", Version: "16.15"})
	srcPW := e.secretData(e.st, "blog-db-credentials")["password"]
	e.bkAddBackup("blog-db", "blog-db-b1", e.now.Add(-time.Hour), database.BackupOK, database.ReasonScheduled, "16.15")
	e.bkRestoreTarget("blog-db-restored", "blog-db-b1")
	e.bkReplica("blog-db", "blog-db-src", "node-1", true)

	tgt := e.bkDB("blog-db-restored")
	exp, err := e.c.expandDatabase(ctx, tgt, dbApp)
	if err != nil {
		t.Fatal(err)
	}
	if exp.refusal != "" || exp.restoreState != database.RestorePending {
		t.Fatalf("restore expansion = %+v", exp)
	}
	if pw := e.secretData(e.st, "blog-db-restored-credentials")["password"]; pw != srcPW {
		t.Fatal("the restored database does not have the source password")
	}
	if _, err := e.st.Get(ctx, v1alpha1.KindWorkload, "blog-db-restored"); err != nil {
		t.Fatalf("the new database's workload was not created: %v", err)
	}
	// The new server becomes Ready, and the restore state is recorded with it.
	e.c.writeDatabaseStatus(ctx, &state{workloads: map[string]*v1alpha1.Workload{"blog-db-restored": {Status: &v1alpha1.WorkloadStatus{ReadyReplicas: 1}}}}, tgt, exp)
	if st := e.dbStatus("blog-db-restored"); st.GetPhase() != "Ready" || st.GetRestoreState() != database.RestorePending {
		t.Fatalf("status = %+v", st)
	}

	// Pass 1: the restore job goes to the source replica.
	e.bkPass()
	rp, err := objects.Replica(e.record(v1alpha1.KindReplica, "blog-db-src"))
	if err != nil {
		t.Fatal(err)
	}
	job := rp.Spec.GetJob()
	if job.GetKind() != database.JobRestore || job.GetTarget() != "blog-db-restored.shoal" || job.GetDatabase() != "blog_db_restored" ||
		job.GetFile() != database.BackupDir+"/blog-db-b1.dump" || job.GetSha256() != "abc" || job.GetId() != database.RestoreJobID("blog-db-restored", "blog-db-b1") {
		t.Fatalf("restore job = %+v", job)
	}
	if st := e.dbStatus("blog-db-restored"); st.GetRestoreState() != database.RestoreRunning {
		t.Fatalf("restore state = %q, want running", st.GetRestoreState())
	}

	// The agent reports the restore; the state becomes done.
	e.bkReplicaJobResult("blog-db-src", &v1alpha1.DatabaseJobResult{Id: job.GetId(), Ok: true})
	e.bkPass()
	if st := e.dbStatus("blog-db-restored"); st.GetRestoreState() != database.RestoreDone {
		t.Fatalf("restore state = %q, want done", st.GetRestoreState())
	}
	if src := e.dbStatus("blog-db"); src.GetPhase() != "Ready" {
		t.Fatalf("the source database changed: %+v", src)
	}
	if pw := e.secretData(e.st, "blog-db-credentials")["password"]; pw != srcPW {
		t.Fatal("the source credentials changed")
	}
}

// A restore into a Database that already exists is refused, and so is a restore
// from a backup that is not ok.
func TestRestoreIsRefusedIntoAnExistingDatabaseOrFromABadBackup(t *testing.T) {
	e := newEnv(t)
	e.database("blog-db-restored", "16.15")
	if _, err := e.c.expandDatabase(ctx, e.bkDB("blog-db-restored"), dbApp); err != nil {
		t.Fatal(err)
	}
	e.bkSetStatus("blog-db-restored", &v1alpha1.DatabaseStatus{Phase: "Ready", Version: "16.15"})
	e.bkAddBackup("blog-db", "b-1", e.now.Add(-time.Hour), database.BackupOK, database.ReasonScheduled, "16.15")
	s, err := objects.Encode(&v1alpha1.DatabaseSpec{Engine: "postgres", Version: "16.15", Size: "5Gi", RestoreFrom: "b-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Update(ctx, v1alpha1.KindDatabase, "blog-db-restored", s, e.record(v1alpha1.KindDatabase, "blog-db-restored").Generation); err != nil {
		t.Fatal(err)
	}
	d := e.bkDB("blog-db-restored")
	if exp, err := e.c.expandDatabase(ctx, d, dbApp); err != nil || !strings.Contains(exp.refusal, "already exists") {
		t.Fatalf("existing database: expansion %+v, err %v", exp, err)
	}

	e.bkRestoreTarget("fresh-db", "missing-backup")
	if exp, err := e.c.expandDatabase(ctx, e.bkDB("fresh-db"), dbApp); err != nil || !strings.Contains(exp.refusal, "does not exist") {
		t.Fatalf("missing backup: expansion %+v, err %v", exp, err)
	}
	e.bkAddBackup("blog-db", "b-bad", e.now.Add(-time.Hour), database.BackupFailed, database.ReasonScheduled, "16.15")
	e.bkRestoreTarget("bad-db", "b-bad")
	if exp, err := e.c.expandDatabase(ctx, e.bkDB("bad-db"), dbApp); err != nil || !strings.Contains(exp.refusal, "not ok") {
		t.Fatalf("failed backup: expansion %+v, err %v", exp, err)
	}
}
