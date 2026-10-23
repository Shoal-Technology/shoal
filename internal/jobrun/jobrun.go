// Package jobrun runs one pipeline job in a container on a runner node
// (ADR 0022, pass B). The agent calls Run with the job the server claimed.
// Run clones the repository on the host, starts a locked-down container, and
// returns how the job ended.
package jobrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/build/buildkit"
	"github.com/Shoal-Technology/shoal/internal/pipeline"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/runtime"
)

// Wire names of the phases Run ends with. They match the server's job states.
const (
	PhaseComplete = "Complete"
	PhaseFailed   = "Failed"
	PhaseError    = "Error"
)

// LabelJob marks a job's container with its Job. The node's reconciler leaves
// such containers alone: they are not replicas.
const LabelJob = "shoal.dev/job"

const (
	// workspaceDest is where the job's writable workspace appears in the
	// container. The repository is cloned to workspaceDest + "/repo".
	workspaceDest = "/workspace"
	repoDest      = workspaceDest + "/repo"
	// ctlDest is the read-only control directory: the script and the gate.
	ctlDest = "/shoal-ctl"
	gate    = "gate"
	script  = "run.sh"
	// gateWait is how many seconds the job waits for the gate.
	gateWait = 300
	// logName is the job log inside the job directory.
	logName = "job.log"

	stopTimeout = 10 * time.Second
	// logDrain is how long Run waits for the last log lines after the exit.
	logDrain  = 10 * time.Second
	pollEvery = 500 * time.Millisecond
)

// Job is one pipeline job for a runner node.
type Job struct {
	// Name is the Job record's name. The container is named after it.
	Name string
	// Dir is the job's private directory. The caller creates it with mode
	// 0700 and removes it when Run returns.
	Dir    string
	Image  string
	Script []string
	// Env holds the variables of the job, with the values of its secrets for
	// a protected run (ADR 0022, section 6).
	Env map[string]string
	// Git is the repository at the run's commit, and Credentials its clone
	// credentials. Credentials are removed before the script runs.
	Git         *v1alpha1.GitSource
	Credentials *v1alpha1.BuildCredentials
	// Registry is the embedded registry's address. Empty adds no exception.
	Registry string
	// Timeout bounds the run. Zero means resource.DefaultJobTimeoutSeconds.
	Timeout time.Duration
	// Limits. Zero means the resource defaults.
	CPUMillis, MemoryBytes, PidsLimit int64
}

// Outcome is how a job ended.
type Outcome struct {
	Phase    string // PhaseComplete, PhaseFailed or PhaseError
	ExitCode int
	// Reason is set with PhaseError: runner_error (ADR 0022, section 5).
	Reason  string
	Message string
}

// ContainerID is the container of a job.
func ContainerID(job string) string { return "shoal-job-" + job }

// LogFile is the job log inside a job directory.
func LogFile(dir string) string { return filepath.Join(dir, logName) }

// isolateFn firewalls the container's network namespace. Tests replace it.
var isolateFn = buildkit.Isolate

// Run runs one job and returns how it ended. It never returns an error: a
// failure to run is an Outcome with PhaseError. The container is removed
// before Run returns, whatever the outcome.
func Run(parent context.Context, rt runtime.Runtime, job Job) Outcome {
	if err := validate(job); err != nil {
		return runnerError(err)
	}
	logf, err := os.OpenFile(LogFile(job.Dir), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return runnerError(fmt.Errorf("open job log: %w", err))
	}
	defer logf.Close()

	timeout := job.Timeout
	if timeout <= 0 {
		timeout = resource.DefaultJobTimeoutSeconds * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	ws := filepath.Join(job.Dir, "workspace")
	ctl := filepath.Join(job.Dir, "ctl")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return runnerError(err)
	}
	if err := os.MkdirAll(ctl, 0o755); err != nil {
		return runnerError(err)
	}
	// The clone is the host's work: it needs the credentials, and a failed
	// clone (a bad ref, a refused login) is the job's failure, not the node's.
	bk := buildkit.Job{Dir: job.Dir, Source: &v1alpha1.BuildConfigSpec{Git: job.Git}, Credentials: job.Credentials}
	if _, err := buildkit.CloneRepo(ctx, bk, filepath.Join(ws, "repo"), logf); err != nil {
		return failed(-1, "clone: "+err.Error())
	}
	if err := os.RemoveAll(filepath.Join(job.Dir, "git")); err != nil {
		return runnerError(fmt.Errorf("remove git credentials: %w", err))
	}
	if err := openTree(ws); err != nil {
		return runnerError(err)
	}
	if err := os.WriteFile(filepath.Join(ctl, script), []byte(scriptText(job.Script)), 0o644); err != nil {
		return runnerError(err)
	}

	id := ContainerID(job.Name)
	_ = rt.Remove(context.Background(), id) // a leftover from an earlier attempt
	defer cleanup(rt, id)
	if err := rt.EnsureImage(ctx, job.Image, nil); err != nil {
		return runnerError(fmt.Errorf("pull %s: %w", job.Image, err))
	}
	if err := rt.Create(ctx, Spec(job, id)); err != nil {
		return runnerError(fmt.Errorf("create the job container: %w", err))
	}
	if err := rt.Start(ctx, id); err != nil {
		return runnerError(fmt.Errorf("start the job container: %w", err))
	}
	// Fail closed: the namespace is firewalled before the script can run.
	if err := isolateFn(ctx, rt, id, job.Registry); err != nil {
		return runnerError(fmt.Errorf("isolate the job network: %w", err))
	}
	if err := os.WriteFile(filepath.Join(ctl, gate), nil, 0o644); err != nil {
		return runnerError(fmt.Errorf("open the job gate: %w", err))
	}

	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		_ = rt.Logs(ctx, id, runtime.LogOptions{Follow: true, Tail: -1}, func(e runtime.LogEntry) error {
			_, err := io.WriteString(logf, e.Line+"\n")
			return err
		})
	}()
	code, err := waitExit(ctx, rt, id)
	if err != nil {
		// Why the wait ended is read before anything else cancels ctx.
		byParent := parent.Err() != nil
		byDeadline := errors.Is(ctx.Err(), context.DeadlineExceeded)
		stop(rt, id)
		cancel() // ends the log stream, which may still be open
		<-logDone
		switch {
		case byParent:
			return cancelled()
		case byDeadline:
			return timedOut()
		}
		return runnerError(fmt.Errorf("wait for the job container: %w", err))
	}
	select {
	case <-logDone:
	case <-time.After(logDrain):
	}
	if code == 0 {
		return Outcome{Phase: PhaseComplete}
	}
	return failed(code, fmt.Sprintf("the script failed (exit %d)", code))
}

// Spec is the container of a job (ADR 0022, section 5). It is the baseline of
// every job: not privileged, no capabilities, no_new_privs left on (only the
// build daemon clears it), a read-only root filesystem with a writable
// workspace and /tmp, mounts only from the job's directory, and CPU, memory and
// process limits. The container gets its own network namespace from the
// runtime, and Run firewalls it before the script starts.
func Spec(job Job, id string) runtime.ContainerSpec {
	gatePath := ctlDest + "/" + gate
	wrapper := fmt.Sprintf(`i=0; while [ ! -e %s ]; do i=$((i+1)); [ "$i" -gt %d ] && exit 1; sleep 1; done; exec sh %s/%s`,
		gatePath, gateWait, ctlDest, script)
	cpu, mem, pids := job.CPUMillis, job.MemoryBytes, job.PidsLimit
	if cpu <= 0 {
		cpu = resource.DefaultJobCPUMillis
	}
	if mem <= 0 {
		mem = resource.DefaultJobMemoryBytes
	}
	if pids <= 0 {
		pids = resource.DefaultJobPids
	}
	return runtime.ContainerSpec{
		ID:      id,
		Image:   job.Image,
		Command: []string{"sh", "-c", wrapper},
		Env:     envList(job.Env),
		Mounts: []runtime.Mount{
			{Source: filepath.Join(job.Dir, "workspace"), Destination: workspaceDest, ReadWrite: true},
			{Source: filepath.Join(job.Dir, "ctl"), Destination: ctlDest},
		},
		Tmpfs:               []string{"/tmp"},
		Labels:              map[string]string{LabelJob: job.Name},
		CPUMillis:           cpu,
		MemoryBytes:         mem,
		PidsLimit:           pids,
		ReadOnlyRootfs:      true,
		DropAllCapabilities: true,
	}
}

func validate(job Job) error {
	switch {
	case job.Name == "":
		return errors.New("jobrun: job name is required")
	case job.Image == "":
		return errors.New("jobrun: the job has no image")
	case len(job.Script) == 0:
		return errors.New("jobrun: the job has no script")
	case job.Git.GetUrl() == "":
		return errors.New("jobrun: the run has no git URL")
	case job.Dir == "":
		return errors.New("jobrun: no job directory")
	}
	return nil
}

// scriptText is the file the container runs: the repository is the working
// directory, and the first failing command ends the script.
func scriptText(lines []string) string {
	return "set -e\ncd " + repoDest + "\n" + strings.Join(lines, "\n") + "\n"
}

// envList returns the variables as KEY=VALUE entries, sorted by name.
func envList(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// waitExit polls the container until it exits and returns its exit code.
func waitExit(ctx context.Context, rt runtime.Runtime, id string) (int, error) {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		st, err := rt.Status(ctx, id)
		if err != nil {
			return 0, err
		}
		if st.State == runtime.StateExited {
			return st.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-t.C:
		}
	}
}

// stop ends the container's process. It runs after ctx may be done.
func stop(rt runtime.Runtime, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout+30*time.Second)
	defer cancel()
	_ = rt.Stop(ctx, id, stopTimeout)
}

// cleanup stops and removes the job's container.
func cleanup(rt runtime.Runtime, id string) {
	stop(rt, id)
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout+30*time.Second)
	defer cancel()
	_ = rt.Remove(ctx, id)
}

// openTree makes the cloned tree writable by the job's user, which may not be
// root. Only the job's own directory is changed.
func openTree(dir string) error {
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.Type()&os.ModeSymlink != 0:
			return nil
		case d.IsDir():
			return os.Chmod(p, 0o777)
		default:
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.Chmod(p, info.Mode().Perm()|0o666)
		}
	})
}

func runnerError(err error) Outcome {
	return Outcome{Phase: PhaseError, Reason: pipeline.ReasonRunnerError, Message: err.Error()}
}

func failed(code int, msg string) Outcome {
	return Outcome{Phase: PhaseFailed, ExitCode: code, Message: msg}
}

func timedOut() Outcome { return Outcome{Phase: PhaseFailed, ExitCode: -1, Message: "timeout"} }

func cancelled() Outcome { return Outcome{Phase: PhaseFailed, ExitCode: -1, Message: "cancelled"} }
