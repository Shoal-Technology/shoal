package resource

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/database"
)

// Database limits and defaults (ADR 0016, section 3; ADR 0017 for memory).
const (
	// DatabaseMaxNameLen keeps the owned objects' names within their limits:
	// "<name>-credentials" and the Workload name both fit in 63 and 52 chars.
	DatabaseMaxNameLen = 40

	DefaultDatabaseCPU    = "500m"
	DefaultDatabaseMemory = "384Mi"

	// MinDatabaseStorageBytes is the smallest data Volume for a database.
	MinDatabaseStorageBytes = 1 << 30
	// MinDatabaseMemoryBytes is the smallest memory limit a database runs with.
	MinDatabaseMemoryBytes = 128 << 20

	maxBackupRetain = 365
	minBackupEvery  = time.Hour
	maxBackupEvery  = 366 * 24 * time.Hour
)

// Backup targets (ADR 0016, section 3.4).
const (
	BackupTargetVolume = "volume"
	BackupTargetS3     = "s3"
)

// OwnerKey is the name of the OwnerClaim record of the object kind/name.
func OwnerKey(kind, name string) string { return kind + "/" + name }

// DefaultDatabase fills in the resources and replica count.
func DefaultDatabase(s *v1alpha1.DatabaseSpec) {
	if s.Resources == nil {
		s.Resources = &v1alpha1.Resources{}
	}
	if s.Resources.Cpu == "" {
		s.Resources.Cpu = DefaultDatabaseCPU
	}
	if s.Resources.Memory == "" {
		s.Resources.Memory = DefaultDatabaseMemory
	}
	if s.Replicas == nil {
		s.Replicas = proto.Int32(1)
	}
}

// ValidateDatabase checks a defaulted Database spec and reports every problem
// found, each prefixed with its field path.
func ValidateDatabase(name string, s *v1alpha1.DatabaseSpec) error {
	var errs []error
	bad := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}
	if !ValidName(name, DatabaseMaxNameLen) {
		bad("metadata.name", "must be a lowercase DNS label of at most %d characters", DatabaseMaxNameLen)
	}
	switch s.GetEngine() {
	case database.EnginePostgres:
	case "":
		bad("spec.engine", "required: postgres")
	default:
		bad("spec.engine", "unsupported engine %q: only postgres in this release", s.GetEngine())
	}
	if s.GetEngine() == database.EnginePostgres || s.GetEngine() == "" {
		if _, ok := database.Image(database.EnginePostgres, s.GetVersion()); !ok {
			bad("spec.version", "%q is not a supported version (supported: %s)", s.GetVersion(), strings.Join(database.Versions(database.EnginePostgres), ", "))
		}
	}
	switch size := s.GetSize(); {
	case size == "":
		bad("spec.size", "required, the storage for the data, for example 5Gi")
	default:
		if v, err := ParseMemory(size); err != nil {
			bad("spec.size", "%v", err)
		} else if v < MinDatabaseStorageBytes {
			bad("spec.size", "must be at least 1Gi")
		}
	}
	if c := s.GetResources().GetCpu(); c != "" {
		if v, err := ParseCPU(c); err != nil {
			bad("spec.resources.cpu", "%v", err)
		} else if v == 0 {
			bad("spec.resources.cpu", "must be greater than zero")
		}
	}
	if m := s.GetResources().GetMemory(); m != "" {
		if v, err := ParseMemory(m); err != nil {
			bad("spec.resources.memory", "%v", err)
		} else if v < MinDatabaseMemoryBytes {
			bad("spec.resources.memory", "must be at least 128Mi")
		}
	}
	if r := s.GetReplicas(); r != 1 {
		bad("spec.replicas", "must be 1: a database has one replica in this release (got %d)", r)
	}
	if b := s.GetBackup(); b != nil {
		validateBackup(b, bad)
	}
	return errors.Join(errs...)
}

// validateBackup checks spec.backup. Backups are stored here and run in pass D.
func validateBackup(b *v1alpha1.DatabaseBackup, bad func(field, format string, args ...any)) {
	every, err := time.ParseDuration(b.GetSchedule())
	switch {
	case b.GetSchedule() == "":
		bad("spec.backup.schedule", "required, an interval such as 24h")
	case err != nil:
		bad("spec.backup.schedule", "%v", err)
	case every < minBackupEvery || every > maxBackupEvery:
		bad("spec.backup.schedule", "must be between 1h and 366 days")
	}
	if r := b.GetRetain(); r < 1 || r > maxBackupRetain {
		bad("spec.backup.retain", "must be between 1 and %d", maxBackupRetain)
	}
	if len(b.GetTargets()) == 0 {
		bad("spec.backup.targets", "required: volume, s3 or both")
	}
	seen := map[string]bool{}
	hasS3 := false
	for i, t := range b.GetTargets() {
		f := fmt.Sprintf("spec.backup.targets[%d]", i)
		switch {
		case t != BackupTargetVolume && t != BackupTargetS3:
			bad(f, "must be volume or s3")
		case seen[t]:
			bad(f, "duplicate target %q", t)
		}
		seen[t] = true
		hasS3 = hasS3 || t == BackupTargetS3
	}
	switch {
	case hasS3 && !ValidName(b.GetS3(), 63):
		bad("spec.backup.s3", "required with the s3 target: the name of a BackupTarget")
	case !hasS3 && b.GetS3() != "":
		bad("spec.backup.s3", "set only with the s3 target")
	}
}
