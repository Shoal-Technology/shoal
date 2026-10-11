package resource

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

func validDatabase() *v1alpha1.DatabaseSpec {
	return &v1alpha1.DatabaseSpec{Engine: "postgres", Version: "16.15", Size: "5Gi"}
}

// checkDatabase defaults and validates s the way the API does.
func checkDatabase(name string, s *v1alpha1.DatabaseSpec) error {
	DefaultDatabase(s)
	return ValidateDatabase(name, s)
}

func TestDatabaseDefaults(t *testing.T) {
	s := validDatabase()
	DefaultDatabase(s)
	if s.GetResources().GetMemory() != "384Mi" || s.GetResources().GetCpu() != "500m" {
		t.Errorf("resources = %v, want 500m and 384Mi (ADR 0017)", s.GetResources())
	}
	if s.GetReplicas() != 1 {
		t.Errorf("replicas = %d, want 1", s.GetReplicas())
	}
	if err := ValidateDatabase("app-db", s); err != nil {
		t.Fatalf("defaulted spec refused: %v", err)
	}
}

func TestDatabaseValidation(t *testing.T) {
	backup := func(b *v1alpha1.DatabaseBackup) func(*v1alpha1.DatabaseSpec) {
		return func(s *v1alpha1.DatabaseSpec) { s.Backup = b }
	}
	tests := []struct {
		name    string
		db      string
		mutate  func(*v1alpha1.DatabaseSpec)
		message string // expected in the error; "" means valid
	}{
		{name: "valid", db: "app-db"},
		{name: "bad name", db: "App_DB", message: "metadata.name"},
		{name: "name too long", db: strings.Repeat("a", DatabaseMaxNameLen+1), message: "metadata.name"},
		{name: "engine missing", db: "app-db", mutate: func(s *v1alpha1.DatabaseSpec) { s.Engine = "" }, message: "spec.engine: required"},
		{name: "redis not yet", db: "app-db", mutate: func(s *v1alpha1.DatabaseSpec) { s.Engine = "redis" }, message: "unsupported engine"},
		{name: "unknown version", db: "app-db", mutate: func(s *v1alpha1.DatabaseSpec) { s.Version = "16.99" }, message: "spec.version"},
		{name: "unknown major", db: "app-db", mutate: func(s *v1alpha1.DatabaseSpec) { s.Version = "18" }, message: "spec.version"},
		{name: "size missing", db: "app-db", mutate: func(s *v1alpha1.DatabaseSpec) { s.Size = "" }, message: "spec.size: required"},
		{name: "size below 1Gi", db: "app-db", mutate: func(s *v1alpha1.DatabaseSpec) { s.Size = "512Mi" }, message: "at least 1Gi"},
		{name: "memory below 128Mi", db: "app-db", mutate: func(s *v1alpha1.DatabaseSpec) { s.Resources = &v1alpha1.Resources{Memory: "64Mi"} }, message: "at least 128Mi"},
		{name: "two replicas", db: "app-db", mutate: func(s *v1alpha1.DatabaseSpec) { s.Replicas = proto.Int32(2) }, message: "must be 1"},
		{name: "backup valid", db: "app-db", mutate: backup(&v1alpha1.DatabaseBackup{Schedule: "24h", Retain: 7, Targets: []string{"volume"}})},
		{name: "backup s3 valid", db: "app-db", mutate: backup(&v1alpha1.DatabaseBackup{Schedule: "6h", Retain: 3, Targets: []string{"volume", "s3"}, S3: "offsite"})},
		{name: "schedule too short", db: "app-db", mutate: backup(&v1alpha1.DatabaseBackup{Schedule: "30m", Retain: 1, Targets: []string{"volume"}}), message: "between 1h and 366 days"},
		{name: "schedule not a duration", db: "app-db", mutate: backup(&v1alpha1.DatabaseBackup{Schedule: "daily", Retain: 1, Targets: []string{"volume"}}), message: "spec.backup.schedule"},
		{name: "retain zero", db: "app-db", mutate: backup(&v1alpha1.DatabaseBackup{Schedule: "24h", Retain: 0, Targets: []string{"volume"}}), message: "spec.backup.retain"},
		{name: "no targets", db: "app-db", mutate: backup(&v1alpha1.DatabaseBackup{Schedule: "24h", Retain: 1}), message: "spec.backup.targets: required"},
		{name: "unknown target", db: "app-db", mutate: backup(&v1alpha1.DatabaseBackup{Schedule: "24h", Retain: 1, Targets: []string{"ftp"}}), message: "must be volume or s3"},
		{name: "duplicate target", db: "app-db", mutate: backup(&v1alpha1.DatabaseBackup{Schedule: "24h", Retain: 1, Targets: []string{"volume", "volume"}}), message: "duplicate target"},
		{name: "s3 target without name", db: "app-db", mutate: backup(&v1alpha1.DatabaseBackup{Schedule: "24h", Retain: 1, Targets: []string{"s3"}}), message: "spec.backup.s3: required"},
		{name: "s3 name without target", db: "app-db", mutate: backup(&v1alpha1.DatabaseBackup{Schedule: "24h", Retain: 1, Targets: []string{"volume"}, S3: "offsite"}), message: "set only with the s3 target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validDatabase()
			if tt.mutate != nil {
				tt.mutate(s)
			}
			err := checkDatabase(tt.db, s)
			switch {
			case tt.message == "" && err != nil:
				t.Fatalf("valid spec refused: %v", err)
			case tt.message != "" && (err == nil || !strings.Contains(err.Error(), tt.message)):
				t.Fatalf("error = %v, want containing %q", err, tt.message)
			}
		})
	}
}

// A Database manifest decodes and prepares like the other kinds.
func TestDecodeDatabase(t *testing.T) {
	objs, err := Decode([]byte(`
kind: Database
metadata:
  name: app-db
spec:
  engine: postgres
  version: "16.15"
  size: 5Gi
  resources:
    memory: 512Mi
  backup:
    schedule: 24h
    retain: 7
    targets: [volume]
`))
	if err != nil {
		t.Fatal(err)
	}
	kind, name, spec, err := Prepare(objs[0])
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if kind != v1alpha1.KindDatabase || name != "app-db" {
		t.Fatalf("kind %q name %q", kind, name)
	}
	s := spec.(*v1alpha1.DatabaseSpec)
	if s.GetResources().GetMemory() != "512Mi" || s.GetResources().GetCpu() != "500m" || s.GetBackup().GetRetain() != 7 {
		t.Errorf("prepared spec = %v", s)
	}
}
