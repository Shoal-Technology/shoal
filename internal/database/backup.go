package database

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"example.com/shoal/api/v1alpha1"
)

// Logical backups of Postgres Databases (ADR 0016, sections 3.4 and 3.7, pass
// D). The dump is written inside the database container, into BackupDir on the
// data Volume (next to pgdata, not inside it), so the agent runs it with exec.
const (
	// BackupDir is the directory of the dump files in the container.
	BackupDir = "/var/lib/postgresql/data/backups"
	// User is the database user the controller creates (POSTGRES_USER).
	User = "app"
	// Port is the port of the database server.
	Port = 5432
)

// Backup statuses, as stored in DatabaseBackupRecord.status.
const (
	BackupPending = "pending"
	BackupRunning = "running"
	BackupOK      = "ok"
	BackupFailed  = "failed"
)

// Backup reasons, as stored in DatabaseBackupRecord.reason.
const (
	ReasonScheduled = "scheduled"
	ReasonManual    = "manual"
	ReasonUpgrade   = "upgrade"
)

// Job kinds, as stored in DatabaseJob.kind.
const (
	JobBackup  = "backup"
	JobRestore = "restore"
)

// Restore states, as stored in DatabaseStatus.restore_state.
const (
	RestorePending = "pending"
	RestoreRunning = "running"
	RestoreDone    = "done"
	RestoreFailed  = "failed"
)

// DBName is the name of the database inside a Database's server (POSTGRES_DB):
// the Database name with - replaced by _.
func DBName(db string) string {
	return strings.ReplaceAll(db, "-", "_")
}

// NewBackupRecord returns a pending backup of db, taken now from engine
// version version. suffix makes the ID unique; use RandomSuffix.
func NewBackupRecord(db, reason, version string, now time.Time, suffix string) *v1alpha1.DatabaseBackupRecord {
	id := fmt.Sprintf("%s-%s-%s", db, now.UTC().Format("20060102t150405"), suffix)
	return &v1alpha1.DatabaseBackupRecord{
		Id:        id,
		Database:  db,
		CreatedAt: timestamppb.New(now),
		File:      BackupDir + "/" + id + ".dump",
		Status:    BackupPending,
		Reason:    reason,
		Version:   version,
	}
}

// RestoreJobID is the job ID of the restore of backup into target. It is fixed
// per pair, so the agent runs it once.
func RestoreJobID(target, backup string) string {
	return "restore-" + target + "-" + backup
}

// RandomSuffix returns 10 random hex digits for an ID.
func RandomSuffix() string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

// IsDumpPath reports whether file names a file directly in BackupDir. The
// agent refuses any other path, so a job cannot read or delete elsewhere.
func IsDumpPath(file string) bool {
	name, ok := strings.CutPrefix(file, BackupDir+"/")
	return ok && name != "" && !strings.Contains(name, "/")
}
