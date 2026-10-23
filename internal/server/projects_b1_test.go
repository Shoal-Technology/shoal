package server

import (
	"context"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/projmigrate"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/store/sqlite"
	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

// mustCreateIn stores one record of kind named name in project, with spec
// and status encoded as the server encodes them.
func mustCreateIn(t *testing.T, ts *testServer, kind, name, project string, spec, status proto.Message) {
	t.Helper()
	sb, err := objects.Encode(spec)
	if err != nil {
		t.Fatal(err)
	}
	var stb []byte
	if status != nil {
		stb, err = objects.Encode(status)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ts.store.Create(context.Background(), store.Record{
		Kind: kind, Name: tenancy.Key(project, name), Spec: sb, Status: stb, Labels: tenancy.WithProject(nil, project),
	}); err != nil {
		t.Fatalf("create %s %s in %s: %v", kind, name, project, err)
	}
}

// TestSameNameInTwoProjects checks that one name in two projects is two
// objects, and that a lookup without a project names the ambiguity.
func TestSameNameInTwoProjects(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	p := projectClient{t: t, admin: ts.admin(t), ctx: context.Background()}
	ctx := p.ctx
	p.mustApply(projDoc(v1alpha1.KindProject, "team-a", "", ""))
	p.mustApply(projDoc(v1alpha1.KindWorkload, "web", "team-a", workloadBody(1, "100m")))
	p.mustApply(projDoc(v1alpha1.KindWorkload, "web", "", workloadBody(1, "100m")))

	_, err := p.admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: "web"})
	projWantStatus(t, err, codes.FailedPrecondition, "several projects")
	for _, proj := range []string{tenancy.DefaultProject, "team-a"} {
		w, err := p.admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: "web", Project: proj})
		if err != nil || w.GetWorkload().GetMetadata().GetName() != "web" || w.GetWorkload().GetMetadata().GetProject() != proj {
			t.Fatalf("web in %s: %v, %v", proj, w, err)
		}
	}
	// Each object has its own store key, the bare name for default and
	// name.project for team-a (ADR 0023, pass B1).
	for _, key := range []string{"web", "web.team-a"} {
		if _, err := ts.store.Get(ctx, v1alpha1.KindWorkload, key); err != nil {
			t.Fatalf("store key %q: %v", key, err)
		}
	}
}

// TestScopedRPCsResolveTheProject checks the RPCs that take an object name
// and a project: a name in one project does not reach another project's
// object, and a name that two projects hold is refused without a project.
func TestScopedRPCsResolveTheProject(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	p := projectClient{t: t, admin: ts.admin(t), ctx: context.Background()}
	ctx := p.ctx
	p.mustApply(projDoc(v1alpha1.KindProject, "team-a", "", ""))
	p.mustApply(projDoc(v1alpha1.KindWorkload, "web", "team-a", workloadBody(1, "100m")))
	p.mustApply(projDoc(v1alpha1.KindWorkload, "web", "", workloadBody(1, "100m")))

	// RestartWorkload: an unqualified name is ambiguous; with a project it
	// restarts that project's Workload only.
	_, err := p.admin.RestartWorkload(ctx, &v1alpha1.RestartWorkloadRequest{Name: "web"})
	projWantStatus(t, err, codes.FailedPrecondition, "several projects")
	if _, err := p.admin.RestartWorkload(ctx, &v1alpha1.RestartWorkloadRequest{Name: "web", Project: "team-a"}); err != nil {
		t.Fatal(err)
	}
	ta, _ := p.admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: "web", Project: "team-a"})
	def, _ := p.admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: "web", Project: tenancy.DefaultProject})
	if ta.GetWorkload().GetSpec().GetRestartedAt() == nil || def.GetWorkload().GetSpec().GetRestartedAt() != nil {
		t.Fatalf("restart reached the wrong project: team-a %v, default %v", ta.GetWorkload().GetSpec().GetRestartedAt(), def.GetWorkload().GetSpec().GetRestartedAt())
	}
	_, err = p.admin.RestartWorkload(ctx, &v1alpha1.RestartWorkloadRequest{Name: "web", Project: "ghost"})
	projWantStatus(t, err, codes.NotFound, "not found in project")

	// Logs: team-a's web has no replicas, and the call says so, scoped.
	stream, err := p.admin.Logs(ctx, &v1alpha1.LogsRequest{Workload: "web", Project: "team-a", Tail: 1})
	if err == nil {
		_, err = stream.Recv()
	}
	projWantStatus(t, err, codes.NotFound, `workload "web" has no replicas`)
	stream, err = p.admin.Logs(ctx, &v1alpha1.LogsRequest{Workload: "web", Project: "ghost", Tail: 1})
	if err == nil {
		_, err = stream.Recv()
	}
	projWantStatus(t, err, codes.NotFound, `workload "web" has no replicas`)

	// BuildLogs and CancelBuild: a Build in team-a is reached only through team-a.
	mustCreateIn(t, ts, v1alpha1.KindBuild, "ci-x", "team-a", &v1alpha1.BuildSpec{Config: "ci"}, &v1alpha1.BuildStatus{Phase: resource.BuildNew})
	if _, err := p.admin.BuildLogs(ctx, &v1alpha1.BuildLogsRequest{Build: "ci-x", Project: "team-a"}); err != nil {
		t.Fatalf("BuildLogs in team-a: %v", err)
	}
	_, err = p.admin.BuildLogs(ctx, &v1alpha1.BuildLogsRequest{Build: "ci-x", Project: tenancy.DefaultProject})
	projWantStatus(t, err, codes.NotFound, "not found in project")
	if _, err := p.admin.CancelBuild(ctx, &v1alpha1.CancelBuildRequest{Build: "ci-x", Project: "team-a"}); err != nil {
		t.Fatalf("CancelBuild in team-a: %v", err)
	}
	_, err = p.admin.CancelBuild(ctx, &v1alpha1.CancelBuildRequest{Build: "ci-x", Project: tenancy.DefaultProject})
	projWantStatus(t, err, codes.NotFound, "not found in project")

	// Pipeline runs and jobs: cancel, play and logs, scoped the same way.
	mustCreateIn(t, ts, v1alpha1.KindPipelineRun, "run-1", "team-a", &v1alpha1.PipelineRunSpec{Pipeline: "p"}, &v1alpha1.PipelineRunStatus{Phase: resource.PhaseNew})
	mustCreateIn(t, ts, v1alpha1.KindJob, "run-1-build", "team-a", &v1alpha1.JobSpec{PipelineRun: "run-1"}, &v1alpha1.JobStatus{Phase: resource.PhaseManual})
	if _, err := p.admin.CancelPipelineRun(ctx, &v1alpha1.CancelPipelineRunRequest{Run: "run-1", Project: "team-a"}); err != nil {
		t.Fatalf("CancelPipelineRun in team-a: %v", err)
	}
	_, err = p.admin.CancelPipelineRun(ctx, &v1alpha1.CancelPipelineRunRequest{Run: "run-1", Project: tenancy.DefaultProject})
	projWantStatus(t, err, codes.NotFound, "not found in project")
	if _, err := p.admin.PlayJob(ctx, &v1alpha1.PlayJobRequest{Job: "run-1-build", Project: "team-a"}); err != nil {
		t.Fatalf("PlayJob in team-a: %v", err)
	}
	if _, err := p.admin.JobLogs(ctx, &v1alpha1.JobLogsRequest{Job: "run-1-build", Project: "team-a"}); err != nil {
		t.Fatalf("JobLogs in team-a: %v", err)
	}
	_, err = p.admin.JobLogs(ctx, &v1alpha1.JobLogsRequest{Job: "run-1-build", Project: tenancy.DefaultProject})
	projWantStatus(t, err, codes.NotFound, "not found in project")

	// Database backups: the backup records the Database by its key, so
	// the list is found through the same project.
	mustCreateIn(t, ts, v1alpha1.KindDatabase, "db", "team-a", &v1alpha1.DatabaseSpec{Engine: "postgres", Version: "16"}, &v1alpha1.DatabaseStatus{Phase: "Ready"})
	if _, err := p.admin.BackupDatabase(ctx, &v1alpha1.BackupDatabaseRequest{Database: "db", Project: "team-a"}); err != nil {
		t.Fatalf("BackupDatabase in team-a: %v", err)
	}
	list, err := p.admin.DatabaseBackups(ctx, &v1alpha1.DatabaseBackupsRequest{Database: "db", Project: "team-a"})
	if err != nil || len(list.GetBackups()) != 1 || list.GetBackups()[0].GetDatabase() != "db.team-a" {
		t.Fatalf("DatabaseBackups in team-a: %v, %v", list, err)
	}
	_, err = p.admin.DatabaseBackups(ctx, &v1alpha1.DatabaseBackupsRequest{Database: "db", Project: tenancy.DefaultProject})
	projWantStatus(t, err, codes.NotFound, "does not exist")
}

// TestStartMigratesAPreProjectStore starts a server over a store as pass A
// left it (bare names for a project's objects, no version record), and checks
// that the start migrates it and keeps a backup of the old state.
func TestStartMigratesAPreProjectStore(t *testing.T) {
	dir := t.TempDir()
	ts := startServer(t, dir, &clock{})
	p := projectClient{t: t, admin: ts.admin(t), ctx: context.Background()}
	p.mustApply(projDoc(v1alpha1.KindProject, "team-a", "", ""))
	ts.stop()

	// Rewrite the state the way pass A left it, directly in the database.
	ctx := context.Background()
	db, err := sqlite.Open(ctx, dir+"/state.db", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(ctx, projmigrate.MetaKind, projmigrate.MetaName, 0); err != nil {
		t.Fatal(err)
	}
	mustPut := func(name, project string) {
		spec, err := objects.Encode(&v1alpha1.WorkloadSpec{Image: "nginx:1.27", Replicas: proto.Int32(1)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Create(ctx, store.Record{Kind: v1alpha1.KindWorkload, Name: name, Spec: spec, Labels: tenancy.WithProject(nil, project)}); err != nil {
			t.Fatal(err)
		}
	}
	mustPut("api", tenancy.DefaultProject)
	mustPut("web", "team-a")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	ts2 := startServer(t, dir, &clock{})
	if _, err := ts2.store.Get(ctx, v1alpha1.KindWorkload, "web.team-a"); err != nil {
		t.Fatalf("web was not migrated: %v", err)
	}
	if _, err := ts2.store.Get(ctx, v1alpha1.KindWorkload, "web"); err == nil {
		t.Fatal("web kept its bare key")
	}
	if _, err := ts2.store.Get(ctx, v1alpha1.KindWorkload, "api"); err != nil {
		t.Fatalf("default workload changed: %v", err)
	}
	backups, err := filepathGlob(dir + "/backups/pre-projects-*.shoalbackup")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backup before migration: %v, %v", backups, err)
	}
}

// filepathGlob is filepath.Glob, kept here so the test reads as one file.
func filepathGlob(pattern string) ([]string, error) { return filepath.Glob(pattern) }

// TestCrossProjectReferencesRefused checks that an apply refuses each kind of
// reference to an object in another project, with both projects named, and
// accepts the same reference within one project (ADR 0023, pass B1).
func TestCrossProjectReferencesRefused(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	p := projectClient{t: t, admin: ts.admin(t), ctx: context.Background()}
	p.mustApply(projDoc(v1alpha1.KindProject, "team-a", "", ""))
	// The targets live in the default project.
	p.mustApply(projDoc(v1alpha1.KindWorkload, "web", "", workloadBody(1, "100m")))
	p.mustApply(projDoc(v1alpha1.KindService, "api", "", "  selector: web\n  ports:\n  - port: 80\n    targetPort: 8080\n    protocol: tcp\n"))
	p.mustApply(projDoc(v1alpha1.KindSecret, "creds", "", "  data:\n    token: c2VjcmV0\n"))
	p.mustApply(projDoc(v1alpha1.KindConfig, "settings", "", "  data:\n    level: info\n"))
	p.mustApply(projDoc(v1alpha1.KindVolume, "data", "", "  size: 1Gi\n"))

	image := "  image: nginx:1.27\n"
	cases := []struct {
		name, kind, spec, want string
	}{
		{"route to service", v1alpha1.KindRoute, "  host: site.example.com\n  service: api\n  port: 80\n",
			`Route "site" in project "team-a" references Service "api", which is in project "default"`},
		{"service to workload", v1alpha1.KindService, "  selector: web\n  ports:\n  - port: 80\n    targetPort: 8080\n    protocol: tcp\n",
			`Service "api2" in project "team-a" references Workload "web", which is in project "default"`},
		{"workload env to secret", v1alpha1.KindWorkload, image + "  env:\n  - name: TOKEN\n    valueFrom:\n      secret:\n        name: creds\n        key: token\n",
			`Workload "w" in project "team-a" references Secret "creds", which is in project "default"`},
		{"workload file to config", v1alpha1.KindWorkload, image + "  files:\n  - path: /etc/app.conf\n    config: settings\n",
			`Workload "w" in project "team-a" references Config "settings", which is in project "default"`},
		{"workload to volume", v1alpha1.KindWorkload, image + "  volumes:\n  - name: data\n    mountPath: /data\n",
			`Workload "w" in project "team-a" references Volume "data", which is in project "default"`},
		{"workload pull secret", v1alpha1.KindWorkload, image + "  imagePullSecrets:\n  - creds\n",
			`Workload "w" in project "team-a" references Secret "creds", which is in project "default"`},
		{"pipeline to secret", v1alpha1.KindPipeline, "  repo: https://example.com/acme/app.git\n  credentialsSecret: creds\n",
			`Pipeline "p" in project "team-a" references Secret "creds", which is in project "default"`},
	}
	for _, tc := range cases {
		name := map[string]string{
			v1alpha1.KindRoute: "site", v1alpha1.KindService: "api2", v1alpha1.KindWorkload: "w", v1alpha1.KindPipeline: "p",
		}[tc.kind]
		_, err := p.apply(projDoc(tc.kind, name, "team-a", tc.spec))
		projWantStatus(t, err, codes.InvalidArgument, tc.want)
		_ = tc.name
	}

	// The same references within team-a are accepted.
	p.mustApply(projDoc(v1alpha1.KindSecret, "creds", "team-a", "  data:\n    token: c2VjcmV0\n"))
	p.mustApply(projDoc(v1alpha1.KindWorkload, "w", "team-a", image+"  env:\n  - name: TOKEN\n    valueFrom:\n      secret:\n        name: creds\n        key: token\n"))
}
