package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/database"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

// Logical backups and restores of Databases (ADR 0016, sections 3.4, 3.7 and
// 3.8; pass D). The controller decides and records; the agent runs. A backup or
// restore is handed to the database's replica as its job (ReplicaSpec.job). The
// agent runs pg_dump or pg_restore with exec in that container and reports the
// result in the replica's status, and the controller then updates the record.
//
// A replica runs one job at a time. The job's ID is the backup or restore ID,
// so a job is set again, and the agent does not run it twice, if the controller
// or the agent restarts.

// retryFailedUpgrade is how long a refused minor upgrade waits before a new
// pre-upgrade backup is tried.
const retryFailedUpgrade = 10 * time.Minute

// backupRec is a stored backup record and its decoded form.
type backupRec struct {
	rec store.Record
	b   *v1alpha1.DatabaseBackupRecord
}

func decodeBackup(r store.Record) (*v1alpha1.DatabaseBackupRecord, error) {
	b := &v1alpha1.DatabaseBackupRecord{}
	if err := proto.Unmarshal(r.Spec, b); err != nil {
		return nil, fmt.Errorf("backup %s: %w", r.Name, err)
	}
	return b, nil
}

// backupsOf returns the backups of db, oldest first.
func (c *Controller) backupsOf(ctx context.Context, db string) ([]backupRec, error) {
	recs, err := c.st.List(ctx, v1alpha1.KindDatabaseBackup)
	if err != nil {
		return nil, err
	}
	var out []backupRec
	for _, r := range recs {
		b, err := decodeBackup(r)
		if err != nil {
			return nil, err
		}
		if b.GetDatabase() == db {
			out = append(out, backupRec{rec: r, b: b})
		}
	}
	slices.SortFunc(out, func(x, y backupRec) int {
		return cmp.Or(x.b.GetCreatedAt().AsTime().Compare(y.b.GetCreatedAt().AsTime()), cmp.Compare(x.b.GetId(), y.b.GetId()))
	})
	return out, nil
}

// requestBackup records a pending backup of db. The agent runs it when the
// database's replica is ready.
func (c *Controller) requestBackup(ctx context.Context, db, reason, version string) (*v1alpha1.DatabaseBackupRecord, error) {
	now := c.cfg.Now()
	for range 5 {
		b := database.NewBackupRecord(db, reason, version, now, database.RandomSuffix())
		spec, err := objects.Encode(b)
		if err != nil {
			return nil, err
		}
		_, err = c.st.Create(ctx, store.Record{Kind: v1alpha1.KindDatabaseBackup, Name: b.GetId(), Spec: spec})
		if errors.Is(err, store.ErrExists) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("record backup of %s: %w", db, err)
		}
		c.log.Info("backup requested", "database", db, "backup", b.GetId(), "reason", reason)
		return b, nil
	}
	return nil, fmt.Errorf("record backup of %s: no free ID", db)
}

// saveBackup writes the record of r.
func (c *Controller) saveBackup(ctx context.Context, r backupRec) error {
	spec, err := objects.Encode(r.b)
	if err != nil {
		return err
	}
	if _, err := c.st.Update(ctx, v1alpha1.KindDatabaseBackup, r.rec.Name, spec, r.rec.Generation); err != nil {
		return fmt.Errorf("update backup %s: %w", r.rec.Name, err)
	}
	return nil
}

// inFlight reports whether a backup is pending or running.
func inFlight(b *v1alpha1.DatabaseBackupRecord) bool {
	return b.GetStatus() == database.BackupPending || b.GetStatus() == database.BackupRunning
}

// onlyReplica returns the replica of database db when there is exactly one,
// else nil.
func onlyReplica(s *state, db string) *replica {
	reps := s.replicasOf(db)
	if len(reps) != 1 {
		return nil
	}
	return reps[0]
}

// jobSlotFree reports whether r can take a new job: it has none, or the result
// of its current one is in its status.
func jobSlotFree(r *replica) bool {
	j := r.obj.GetSpec().GetJob()
	return j == nil || r.obj.GetStatus().GetJob().GetId() == j.GetId()
}

// setJob sets the job of replica r.
func (c *Controller) setJob(ctx context.Context, r *replica, job *v1alpha1.DatabaseJob) error {
	spec := proto.Clone(r.obj.GetSpec()).(*v1alpha1.ReplicaSpec)
	spec.Job = job
	return c.updateReplicaSpec(ctx, r, spec)
}

// pruneSet returns the ok backups that the retention of retain deletes once
// backup exclude is written: the oldest ones beyond retain (the new backup is
// one of the retained). backups is oldest first.
func pruneSet(backups []backupRec, retain int32, exclude string) []backupRec {
	if retain <= 0 {
		return nil // no backups spec: nothing is pruned
	}
	var oks []backupRec
	for _, r := range backups {
		if r.b.GetStatus() == database.BackupOK && r.b.GetId() != exclude {
			oks = append(oks, r)
		}
	}
	keep := max(int(retain)-1, 0)
	if len(oks) <= keep {
		return nil
	}
	return oks[:len(oks)-keep]
}

// backupJob is the job that writes the dump of b and deletes the prune files.
func backupJob(db string, b *v1alpha1.DatabaseBackupRecord, prune []backupRec) *v1alpha1.DatabaseJob {
	job := &v1alpha1.DatabaseJob{Id: b.GetId(), Kind: database.JobBackup, File: b.GetFile(), Database: database.DBName(tenancy.KeyName(db))}
	for _, p := range prune {
		job.Prune = append(job.Prune, p.b.GetFile())
	}
	return job
}

// syncDatabaseJobs schedules backups, hands the jobs to the databases' replicas
// and applies the results. It runs after the Databases are expanded.
func (c *Controller) syncDatabaseJobs(ctx context.Context, s *state, dbs map[string]*v1alpha1.Database) []error {
	var errs []error
	for _, name := range sortedKeys(dbs) {
		d := dbs[name]
		backups, err := c.backupsOf(ctx, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := c.scheduleBackup(ctx, name, d, backups); err != nil {
			errs = append(errs, fmt.Errorf("database %s: %w", name, err))
		}
		if err := c.dispatchBackups(ctx, s, name, d, backups); err != nil {
			errs = append(errs, fmt.Errorf("database %s: %w", name, err))
		}
		if err := c.dispatchRestore(ctx, s, name, d); err != nil {
			errs = append(errs, fmt.Errorf("database %s: %w", name, err))
		}
	}
	return errs
}

// scheduleBackup records a scheduled backup of d when its interval has passed
// since the last scheduled one and no backup is in flight (ADR 0016, 3.4).
func (c *Controller) scheduleBackup(ctx context.Context, name string, d *v1alpha1.Database, backups []backupRec) error {
	b := d.GetSpec().GetBackup()
	if b == nil || d.GetStatus().GetPhase() != "Ready" {
		return nil
	}
	every, err := time.ParseDuration(b.GetSchedule())
	if err != nil || every <= 0 {
		return nil // checked when the Database was applied
	}
	var last time.Time
	for _, r := range backups {
		if inFlight(r.b) {
			return nil
		}
		if r.b.GetReason() == database.ReasonScheduled {
			last = r.b.GetCreatedAt().AsTime() // oldest first, so this ends as the newest
		}
	}
	if !last.IsZero() && c.cfg.Now().Sub(last) < every {
		return nil
	}
	_, err = c.requestBackup(ctx, name, database.ReasonScheduled, d.GetStatus().GetVersion())
	return err
}

// dispatchBackups gives the database's replica the next pending backup, and
// applies the result of the running one once the agent has reported it.
func (c *Controller) dispatchBackups(ctx context.Context, s *state, name string, d *v1alpha1.Database, backups []backupRec) error {
	rep := onlyReplica(s, name)
	retain := d.GetSpec().GetBackup().GetRetain()
	for _, r := range backups {
		if r.b.GetStatus() != database.BackupRunning {
			continue
		}
		if rep == nil {
			return nil
		}
		if res := rep.obj.GetStatus().GetJob(); res.GetId() == r.b.GetId() {
			return c.finishBackup(ctx, r, res, pruneSet(backups, retain, r.b.GetId()))
		}
		if rep.obj.GetSpec().GetJob().GetId() != r.b.GetId() {
			// The job was lost (the replica's spec was rewritten): set it again.
			return c.setJob(ctx, rep, backupJob(name, r.b, pruneSet(backups, retain, r.b.GetId())))
		}
		return nil // still running on the agent
	}
	for _, r := range backups {
		if r.b.GetStatus() != database.BackupPending {
			continue
		}
		if rep == nil || !rep.obj.GetStatus().GetReady() || rep.obj.GetSpec().GetNode() == "" {
			return nil
		}
		if rep.obj.GetSpec().GetJob().GetId() != r.b.GetId() {
			if !jobSlotFree(rep) {
				return nil
			}
			if err := c.setJob(ctx, rep, backupJob(name, r.b, pruneSet(backups, retain, r.b.GetId()))); err != nil {
				return err
			}
		}
		r.b.Status = database.BackupRunning
		return c.saveBackup(ctx, r)
	}
	return nil
}

// finishBackup records the result of backup r. A successful backup also removes
// the records the retention chose (the agent has deleted their files).
func (c *Controller) finishBackup(ctx context.Context, r backupRec, res *v1alpha1.DatabaseJobResult, prune []backupRec) error {
	if !res.GetOk() {
		r.b.Status, r.b.Error = database.BackupFailed, res.GetError()
		c.log.Warn("backup failed", "database", r.b.GetDatabase(), "backup", r.b.GetId(), "err", res.GetError())
		return c.saveBackup(ctx, r)
	}
	r.b.Status, r.b.Size, r.b.Sha256, r.b.Error = database.BackupOK, res.GetSize(), res.GetSha256(), res.GetError()
	if err := c.saveBackup(ctx, r); err != nil {
		return err
	}
	c.log.Info("backup done", "database", r.b.GetDatabase(), "backup", r.b.GetId(), "size", r.b.GetSize())
	for _, p := range prune {
		if err := c.st.Delete(ctx, v1alpha1.KindDatabaseBackup, p.rec.Name, 0); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("prune backup %s: %w", p.rec.Name, err)
		}
		c.log.Info("backup pruned", "database", p.b.GetDatabase(), "backup", p.b.GetId())
	}
	return nil
}

// restoreSource returns the backup id, which must exist and be ok. Otherwise it
// returns the refusal to show.
func (c *Controller) restoreSource(ctx context.Context, id string) (*v1alpha1.DatabaseBackupRecord, string, error) {
	rec, err := c.st.Get(ctx, v1alpha1.KindDatabaseBackup, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Sprintf("restore refused: backup %q does not exist", id), nil
	}
	if err != nil {
		return nil, "", err
	}
	b, err := decodeBackup(rec)
	if err != nil {
		return nil, "", err
	}
	if b.GetStatus() != database.BackupOK {
		return nil, fmt.Sprintf("restore refused: backup %s is %s, not ok", id, b.GetStatus()), nil
	}
	return b, "", nil
}

// dispatchRestore runs the restore of a new Database once it is Ready: the
// job runs in the source database's replica, which connects to the new server
// with the source password, which the new Database was created with. The source
// is never written: pg_restore --clean only touches the new Database.
func (c *Controller) dispatchRestore(ctx context.Context, s *state, target string, d *v1alpha1.Database) error {
	from := d.GetSpec().GetRestoreFrom()
	state := d.GetStatus().GetRestoreState()
	if from == "" || (state != database.RestorePending && state != database.RestoreRunning) {
		return nil
	}
	if state == database.RestorePending && d.GetStatus().GetPhase() != "Ready" {
		return nil
	}
	src, refusal, err := c.restoreSource(ctx, from)
	if err != nil {
		return err
	}
	if refusal != "" {
		return c.setRestoreState(ctx, target, database.RestoreFailed, refusal)
	}
	rep := onlyReplica(s, src.GetDatabase())
	if rep == nil || !rep.obj.GetStatus().GetReady() {
		return nil // the source must be running to run the restore
	}
	jobID := database.RestoreJobID(target, from)
	if state == database.RestoreRunning {
		res := rep.obj.GetStatus().GetJob()
		switch {
		case res.GetId() == jobID && res.GetOk():
			return c.setRestoreState(ctx, target, database.RestoreDone, "")
		case res.GetId() == jobID:
			return c.setRestoreState(ctx, target, database.RestoreFailed, "restore from "+from+" failed: "+res.GetError())
		case rep.obj.GetSpec().GetJob().GetId() != jobID && jobSlotFree(rep):
			return c.setJob(ctx, rep, restoreJob(target, src))
		}
		return nil
	}
	if rep.obj.GetSpec().GetJob().GetId() != jobID {
		if !jobSlotFree(rep) {
			return nil
		}
		if err := c.setJob(ctx, rep, restoreJob(target, src)); err != nil {
			return err
		}
	}
	return c.setRestoreState(ctx, target, database.RestoreRunning, "")
}

// restoreJob is the job that loads backup src into the database target.
func restoreJob(target string, src *v1alpha1.DatabaseBackupRecord) *v1alpha1.DatabaseJob {
	return &v1alpha1.DatabaseJob{
		Id:       database.RestoreJobID(target, src.GetId()),
		Kind:     database.JobRestore,
		File:     src.GetFile(),
		Database: database.DBName(tenancy.KeyName(target)),
		Target:   target + "." + v1alpha1.DNSDomain,
		Sha256:   src.GetSha256(),
	}
}

// setRestoreState records the restore state of the Database target. A failed
// restore also sets the status message; another state clears it.
func (c *Controller) setRestoreState(ctx context.Context, target, state, message string) error {
	rec, err := c.st.Get(ctx, v1alpha1.KindDatabase, target)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	d, err := objects.Database(rec)
	if err != nil {
		return err
	}
	st := &v1alpha1.DatabaseStatus{}
	if d.GetStatus() != nil {
		proto.Merge(st, d.GetStatus())
	}
	if st.GetRestoreState() == state && (state != database.RestoreFailed || st.GetMessage() == message) {
		return nil
	}
	if state == database.RestoreFailed || st.GetRestoreState() == database.RestoreFailed {
		st.Message = message
	}
	st.RestoreState = state
	b, err := objects.Encode(st)
	if err != nil {
		return err
	}
	if _, err := c.st.UpdateStatus(ctx, v1alpha1.KindDatabase, target, b); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("update restore state of %s: %w", target, err)
	}
	c.log.Info("restore state", "database", target, "state", state)
	return nil
}

// upgradeGate decides whether db may move from its running version cur to
// version (ADR 0016, section 3.8). A minor upgrade first takes a backup of cur
// and waits for it to verify. It returns a refusal when that backup failed
// (retried after retryFailedUpgrade), or a hold while it is not done.
func (c *Controller) upgradeGate(ctx context.Context, db, cur, version string) (refusal, hold string, err error) {
	backups, err := c.backupsOf(ctx, db)
	if err != nil {
		return "", "", err
	}
	var latest *backupRec
	for i := range backups {
		if backups[i].b.GetReason() == database.ReasonUpgrade && backups[i].b.GetVersion() == cur {
			latest = &backups[i]
		}
	}
	request := func() (string, string, error) {
		if _, err := c.requestBackup(ctx, db, database.ReasonUpgrade, cur); err != nil {
			return "", "", err
		}
		return "", fmt.Sprintf("upgrade to %s waits for a backup of %s", version, cur), nil
	}
	if latest == nil {
		return request()
	}
	id := latest.b.GetId()
	switch latest.b.GetStatus() {
	case database.BackupOK:
		return "", "", nil
	case database.BackupFailed:
		if c.cfg.Now().Sub(latest.b.GetCreatedAt().AsTime()) >= retryFailedUpgrade {
			return request()
		}
		return fmt.Sprintf("upgrade to %s refused: the pre-upgrade backup %s failed: %s", version, id, latest.b.GetError()), "", nil
	default:
		return "", fmt.Sprintf("upgrade to %s waits for the pre-upgrade backup %s", version, id), nil
	}
}

// newDatabaseCredentials returns the credentials of a new Database. A restored
// Database takes the password of its source, so the restore can connect with
// the source's password (ADR 0016, 3.7). Any other gets a new random one.
func (c *Controller) newDatabaseCredentials(ctx context.Context, name, dbName string, source *v1alpha1.DatabaseBackupRecord) ([]byte, error) {
	if source == nil {
		return newCredentials(name, dbName)
	}
	sec, err := c.st.Get(ctx, v1alpha1.KindSecret, tenancy.Key(tenancy.KeyProject(source.GetDatabase()), tenancy.KeyName(source.GetDatabase())+dbCredsSuffix))
	if err != nil {
		return nil, fmt.Errorf("restore: read the credentials of %s: %w", source.GetDatabase(), err)
	}
	var s v1alpha1.SecretSpec
	if err := proto.Unmarshal(sec.Spec, &s); err != nil {
		return nil, fmt.Errorf("restore: decode the credentials of %s: %w", source.GetDatabase(), err)
	}
	pw := s.GetData()["password"]
	if pw == "" || strings.ContainsAny(pw, " \n") {
		return nil, fmt.Errorf("restore: the credentials of %s have no usable password", source.GetDatabase())
	}
	return encodeCredentials(name, dbName, pw)
}
