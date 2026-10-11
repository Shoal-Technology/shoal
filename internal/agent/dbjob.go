package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/database"
	"example.com/shoal/internal/runtime"
)

// Database jobs (ADR 0016, sections 3.4 and 3.7, pass D). The controller puts
// a job in a database replica's spec; the agent runs it with exec in that
// container and reports the result in the replica's status. Nothing is run
// unless the container is running and ready.
const (
	jobBackupTimeout  = 2 * time.Hour
	jobRestoreTimeout = 2 * time.Hour
	jobStepTimeout    = time.Minute
	// jobErrorLimit caps the stderr text kept in a result.
	jobErrorLimit = 500
)

// jobRunner runs one job at a time per replica and keeps the result of each
// replica's latest job until the controller sends another one.
type jobRunner struct {
	log *slog.Logger

	mu      sync.Mutex
	running map[string]string                      // replica -> job ID in progress
	done    map[string]*v1alpha1.DatabaseJobResult // replica -> latest result
}

func newJobRunner(log *slog.Logger) *jobRunner {
	return &jobRunner{log: log, running: map[string]string{}, done: map[string]*v1alpha1.DatabaseJobResult{}}
}

// result returns the finished result of job in replica, or nil while it runs
// or was never started.
func (j *jobRunner) result(replica string, job *v1alpha1.DatabaseJob) *v1alpha1.DatabaseJobResult {
	j.mu.Lock()
	defer j.mu.Unlock()
	r := j.done[replica]
	if r == nil || r.GetId() != job.GetId() {
		return nil
	}
	return proto.Clone(r).(*v1alpha1.DatabaseJobResult)
}

// start runs job in the container replica in the background, unless the job
// is already running or has finished. env is the container's environment as
// the assignment gives it: a restore reads the source password from it.
func (j *jobRunner) start(ctx context.Context, rt runtime.Runtime, replica string, job *v1alpha1.DatabaseJob, env []string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running[replica] == job.GetId() {
		return
	}
	if r := j.done[replica]; r != nil && r.GetId() == job.GetId() {
		return
	}
	if _, busy := j.running[replica]; busy {
		return // one job per replica: the previous one is still running
	}
	j.running[replica] = job.GetId()
	go func() {
		res := runJob(ctx, rt, replica, job, env)
		j.mu.Lock()
		defer j.mu.Unlock()
		delete(j.running, replica)
		j.done[replica] = res
		if res.GetOk() {
			j.log.Info("database job done", "replica", replica, "job", job.GetId(), "kind", job.GetKind())
		} else {
			j.log.Warn("database job failed", "replica", replica, "job", job.GetId(), "kind", job.GetKind(), "err", res.GetError())
		}
	}()
}

// runJob runs one job and returns its result. A failure is in the result, not
// returned: the controller decides what it means.
func runJob(ctx context.Context, rt runtime.Runtime, replica string, job *v1alpha1.DatabaseJob, env []string) *v1alpha1.DatabaseJobResult {
	res := &v1alpha1.DatabaseJobResult{Id: job.GetId()}
	var err error
	switch job.GetKind() {
	case database.JobBackup:
		err = runBackup(ctx, rt, replica, job, res)
	case database.JobRestore:
		err = runRestore(ctx, rt, replica, job, env)
	default:
		err = fmt.Errorf("unknown job kind %q", job.GetKind())
	}
	if err != nil {
		res.Ok = false
		res.Error = errorText(err)
		return res
	}
	res.Ok = true
	return res
}

// runBackup writes the dump (pg_dump -Fc over the local socket, which needs no
// password), records its size and SHA-256, and then deletes the files the
// retention chose. A file that cannot be deleted is reported, not fatal.
func runBackup(ctx context.Context, rt runtime.Runtime, replica string, job *v1alpha1.DatabaseJob, res *v1alpha1.DatabaseJobResult) error {
	file := job.GetFile()
	if !database.IsDumpPath(file) {
		return fmt.Errorf("backup file %q is not in %s", file, database.BackupDir)
	}
	if _, err := execStep(ctx, rt, replica, jobStepTimeout, []string{"mkdir", "-p", database.BackupDir}); err != nil {
		return err
	}
	argv := BackupArgv(job)
	if _, err := execStep(ctx, rt, replica, jobBackupTimeout, argv); err != nil {
		// Do not leave a partial dump behind for the next backup to find.
		_, _ = rt.Exec(ctx, replica, []string{"rm", "-f", file}, jobStepTimeout)
		return err
	}
	out, err := execStep(ctx, rt, replica, jobStepTimeout, []string{"sha256sum", file})
	if err != nil {
		return err
	}
	res.Sha256 = firstField(out)
	out, err = execStep(ctx, rt, replica, jobStepTimeout, []string{"stat", "-c", "%s", file})
	if err != nil {
		return err
	}
	size, err := strconv.ParseInt(firstField(out), 10, 64)
	if err != nil {
		return fmt.Errorf("stat %s: size %q: %w", file, out, err)
	}
	res.Size = size

	var pruneErrs []string
	for _, p := range job.GetPrune() {
		if !database.IsDumpPath(p) {
			pruneErrs = append(pruneErrs, fmt.Sprintf("%q is not a dump", p))
			continue
		}
		if _, err := execStep(ctx, rt, replica, jobStepTimeout, []string{"rm", "-f", p}); err != nil {
			pruneErrs = append(pruneErrs, err.Error())
		}
	}
	if len(pruneErrs) > 0 {
		res.Error = errorText(fmt.Errorf("prune: %s", strings.Join(pruneErrs, "; ")))
	}
	return nil
}

// BackupArgv is the argv of the dump of a backup job. It carries no password:
// the backup connects over the local socket, which the database accepts for
// its own user.
func BackupArgv(job *v1alpha1.DatabaseJob) []string {
	return []string{"pg_dump", "-Fc", "-U", database.User, "-d", job.GetDatabase(), "-f", job.GetFile()}
}

// RestoreArgv is the argv of a restore job. The password is not in it: the
// restore reads PGPASSWORD from the exec environment (runtime.WithEnv).
func RestoreArgv(job *v1alpha1.DatabaseJob) []string {
	return []string{
		"pg_restore", "--clean", "--if-exists",
		"-h", job.GetTarget(), "-p", strconv.Itoa(database.Port), "-U", database.User,
		"-d", job.GetDatabase(), job.GetFile(),
	}
}

// runRestore loads the dump into the target database. It first checks the
// dump against the SHA-256 the backup recorded, and refuses a mismatch.
func runRestore(ctx context.Context, rt runtime.Runtime, replica string, job *v1alpha1.DatabaseJob, env []string) error {
	file := job.GetFile()
	if !database.IsDumpPath(file) {
		return fmt.Errorf("restore file %q is not in %s", file, database.BackupDir)
	}
	pw, ok := envValue(env, "POSTGRES_PASSWORD")
	if !ok || pw == "" {
		return fmt.Errorf("restore: the replica has no POSTGRES_PASSWORD")
	}
	out, err := execStep(ctx, rt, replica, jobStepTimeout, []string{"sha256sum", file})
	if err != nil {
		return err
	}
	if got := firstField(out); got != job.GetSha256() {
		return fmt.Errorf("restore: checksum of %s is %s, the backup recorded %s", file, got, job.GetSha256())
	}
	_, err = execStep(ctx, rt, replica, jobRestoreTimeout, RestoreArgv(job), runtime.WithEnv("PGPASSWORD="+pw))
	return err
}

// execStep runs one command in the replica. A non-zero exit is an error that
// carries the command's name and its stderr.
func execStep(ctx context.Context, rt runtime.Runtime, replica string, timeout time.Duration, argv []string, opts ...runtime.ExecOption) (string, error) {
	res, err := rt.Exec(ctx, replica, argv, timeout, opts...)
	if err != nil {
		return "", fmt.Errorf("%s: %w", argv[0], err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%s exited %d: %s", argv[0], res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// firstField returns the first word of s, or "".
func firstField(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// envValue returns the value of key in env (KEY=VALUE entries).
func envValue(env []string, key string) (string, bool) {
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// errorText is err as a result message: short, one line, with no password.
func errorText(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > jobErrorLimit {
		s = s[:jobErrorLimit] + "..."
	}
	return s
}

// run starts the job of each ready replica that has one, and attaches the
// results of finished jobs to the statuses. It returns the statuses.
func (j *jobRunner) run(ctx context.Context, rt runtime.Runtime, desired []Assignment, statuses []ReplicaStatus) []ReplicaStatus {
	byReplica := map[string]*Assignment{}
	for i := range desired {
		byReplica[desired[i].ReplicaID] = &desired[i]
	}
	for i := range statuses {
		a := byReplica[statuses[i].ReplicaID]
		if a == nil || a.Job == nil {
			continue
		}
		if statuses[i].Ready {
			j.start(ctx, rt, a.ReplicaID, a.Job, a.Spec.Env)
		}
		statuses[i].Job = j.result(a.ReplicaID, a.Job)
	}
	return statuses
}
