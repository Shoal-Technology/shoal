package agent

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/database"
	"github.com/Shoal-Technology/shoal/internal/runtime"
	"github.com/Shoal-Technology/shoal/internal/runtime/fake"
)

// dumpPW is a password that must never reach an argv.
const dumpPW = "pw-not-in-argv-9f2c"

func dbRuntime(answer func(argv []string) runtime.ExecResult) *fake.Runtime {
	rt := fake.New(nil)
	rt.AddContainer(runtime.ContainerSpec{ID: "db-1"}, runtime.ContainerStatus{State: runtime.StateRunning})
	if answer != nil {
		rt.SetExec("db-1", answer)
	}
	return rt
}

// dumpAnswers answers the checksum and size of file for a job, and succeeds for
// everything else.
func dumpAnswers(file, sum string) func(argv []string) runtime.ExecResult {
	return func(argv []string) runtime.ExecResult {
		switch argv[0] {
		case "sha256sum":
			return runtime.ExecResult{Stdout: sum + "  " + file + "\n"}
		case "stat":
			return runtime.ExecResult{Stdout: "4096\n"}
		}
		return runtime.ExecResult{}
	}
}

func containsArgv(all [][]string, want []string) bool {
	for _, a := range all {
		if slices.Equal(a, want) {
			return true
		}
	}
	return false
}

func argvsOf(rt *fake.Runtime) [][]string {
	var out [][]string
	for _, c := range rt.ExecCalls() {
		out = append(out, c.Argv)
	}
	return out
}

func backupJobFor(id string, prune ...string) *v1alpha1.DatabaseJob {
	return &v1alpha1.DatabaseJob{Id: id, Kind: database.JobBackup, File: database.BackupDir + "/" + id + ".dump", Database: "blog_db", Prune: prune}
}

func silentLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The backup connects over the local socket and the restore takes its password
// from the environment: no argv carries a password.
func TestJobArgvCarriesNoPassword(t *testing.T) {
	job := backupJobFor("b1")
	want := []string{"pg_dump", "-Fc", "-U", "app", "-d", "blog_db", "-f", database.BackupDir + "/b1.dump"}
	if got := BackupArgv(job); !slices.Equal(got, want) {
		t.Fatalf("backup argv = %q, want %q", got, want)
	}
	restore := &v1alpha1.DatabaseJob{Kind: database.JobRestore, File: database.BackupDir + "/b1.dump", Database: "blog_db_restored", Target: "blog-db-restored.shoal"}
	got := RestoreArgv(restore)
	wantRestore := []string{"pg_restore", "--clean", "--if-exists", "-h", "blog-db-restored.shoal", "-p", "5432", "-U", "app", "-d", "blog_db_restored", database.BackupDir + "/b1.dump"}
	if !slices.Equal(got, wantRestore) {
		t.Fatalf("restore argv = %q, want %q", got, wantRestore)
	}
	for _, a := range append(BackupArgv(job), got...) {
		if strings.Contains(a, "PGPASSWORD") || strings.Contains(a, dumpPW) {
			t.Fatalf("argv carries a password: %q", a)
		}
	}
}

// A backup records the size and checksum of the dump, and deletes the files the
// retention chose.
func TestBackupRecordsChecksumAndPrunes(t *testing.T) {
	ctx := context.Background()
	file := database.BackupDir + "/b1.dump"
	rt := dbRuntime(dumpAnswers(file, "abc123"))
	job := backupJobFor("b1", database.BackupDir+"/b0.dump")
	res := runJob(ctx, rt, "db-1", job, nil)
	if !res.GetOk() || res.GetSha256() != "abc123" || res.GetSize() != 4096 || res.GetError() != "" {
		t.Fatalf("result = %+v", res)
	}
	calls := argvsOf(rt)
	if !containsArgv(calls, BackupArgv(job)) {
		t.Fatalf("pg_dump not run: %q", calls)
	}
	if !containsArgv(calls, []string{"rm", "-f", database.BackupDir + "/b0.dump"}) {
		t.Fatalf("the pruned dump was not deleted: %q", calls)
	}
}

// A failed dump is reported with its stderr, and the partial file is removed.
func TestFailedDumpIsReportedAndPartialFileRemoved(t *testing.T) {
	ctx := context.Background()
	rt := dbRuntime(func(argv []string) runtime.ExecResult {
		if argv[0] == "pg_dump" {
			return runtime.ExecResult{ExitCode: 1, Stderr: "boom\n"}
		}
		return runtime.ExecResult{}
	})
	res := runJob(ctx, rt, "db-1", backupJobFor("b2"), nil)
	if res.GetOk() || !strings.Contains(res.GetError(), "pg_dump exited 1: boom") {
		t.Fatalf("result = %+v", res)
	}
	if !containsArgv(argvsOf(rt), []string{"rm", "-f", database.BackupDir + "/b2.dump"}) {
		t.Fatalf("partial dump not removed: %q", argvsOf(rt))
	}
}

// A job never touches a file outside the backup directory.
func TestJobsRefuseFilesOutsideTheBackupDir(t *testing.T) {
	ctx := context.Background()
	rt := dbRuntime(nil)
	bad := &v1alpha1.DatabaseJob{Id: "x", Kind: database.JobBackup, File: "/etc/passwd", Database: "blog_db"}
	if res := runJob(ctx, rt, "db-1", bad, nil); res.GetOk() {
		t.Fatalf("a backup to /etc/passwd ran: %+v", res)
	}
	if n := len(rt.ExecCalls()); n != 0 {
		t.Fatalf("%d execs for a refused path", n)
	}
	file := database.BackupDir + "/b3.dump"
	rt = dbRuntime(dumpAnswers(file, "abc"))
	job := backupJobFor("b3", "/etc/passwd")
	res := runJob(ctx, rt, "db-1", job, nil)
	if !res.GetOk() || !strings.Contains(res.GetError(), "is not a dump") {
		t.Fatalf("prune of a non-dump: %+v", res)
	}
	for _, a := range argvsOf(rt) {
		if a[0] == "rm" {
			t.Fatalf("rm ran on %q", a)
		}
	}
}

// A restore loads only a dump whose checksum is the one the backup recorded,
// and passes the password in PGPASSWORD.
func TestRestoreChecksDumpAndPassesPasswordInEnv(t *testing.T) {
	ctx := context.Background()
	file := database.BackupDir + "/b1.dump"
	env := []string{"POSTGRES_USER=app", "POSTGRES_PASSWORD=" + dumpPW}
	job := &v1alpha1.DatabaseJob{Id: "restore-x-b1", Kind: database.JobRestore, File: file, Database: "blog_db_restored", Target: "blog-db-restored.shoal", Sha256: "abc123"}

	rt := dbRuntime(dumpAnswers(file, "ffff"))
	if res := runJob(ctx, rt, "db-1", job, env); res.GetOk() || !strings.Contains(res.GetError(), "checksum") {
		t.Fatalf("mismatched dump: %+v", res)
	}
	for _, a := range argvsOf(rt) {
		if a[0] == "pg_restore" {
			t.Fatal("a mismatched dump was loaded")
		}
	}

	rt = dbRuntime(dumpAnswers(file, "abc123"))
	if res := runJob(ctx, rt, "db-1", job, env); !res.GetOk() {
		t.Fatalf("restore: %+v", res)
	}
	calls := rt.ExecCalls()
	last := calls[len(calls)-1]
	if last.Argv[0] != "pg_restore" || !slices.Equal(last.Argv, RestoreArgv(job)) {
		t.Fatalf("last exec = %q", last.Argv)
	}
	if !slices.Contains(last.Env, "PGPASSWORD="+dumpPW) {
		t.Fatalf("PGPASSWORD missing from the exec environment: %q", last.Env)
	}
	for _, a := range last.Argv {
		if strings.Contains(a, dumpPW) {
			t.Fatalf("password in argv: %q", last.Argv)
		}
	}

	rt = dbRuntime(dumpAnswers(file, "abc123"))
	if res := runJob(ctx, rt, "db-1", job, []string{"POSTGRES_USER=app"}); res.GetOk() {
		t.Fatal("a restore without a password ran")
	}
	for _, a := range argvsOf(rt) {
		if a[0] == "pg_restore" {
			t.Fatal("pg_restore ran without a password")
		}
	}
}

// A job starts once per replica and its result stays until another job comes.
func TestJobRunnerRunsAJobOnce(t *testing.T) {
	ctx := context.Background()
	file := database.BackupDir + "/b1.dump"
	rt := dbRuntime(dumpAnswers(file, "abc123"))
	j := newJobRunner(silentLog())
	job := backupJobFor("b1")
	j.start(ctx, rt, "db-1", job, nil)
	j.start(ctx, rt, "db-1", job, nil)
	var res *v1alpha1.DatabaseJobResult
	for deadline := time.Now().Add(5 * time.Second); res == nil && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		res = j.result("db-1", job)
	}
	if res == nil || !res.GetOk() {
		t.Fatalf("no result: %+v", res)
	}
	j.start(ctx, rt, "db-1", job, nil)
	dumps := 0
	for _, a := range argvsOf(rt) {
		if a[0] == "pg_dump" {
			dumps++
		}
	}
	if dumps != 1 {
		t.Fatalf("pg_dump ran %d times, want once", dumps)
	}
	if j.result("db-1", backupJobFor("b2")) != nil {
		t.Fatal("a result was returned for another job")
	}
}
