package jobrun

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/pipeline"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/runtime"
	"github.com/Shoal-Technology/shoal/internal/runtime/fake"
)

// withIsolation replaces the egress firewall for one test.
func withIsolation(t *testing.T, fn func(ctx context.Context, rt runtime.Runtime, id, registry string) error) {
	t.Helper()
	prev := isolateFn
	isolateFn = fn
	t.Cleanup(func() { isolateFn = prev })
}

func allowAll(context.Context, runtime.Runtime, string, string) error { return nil }

// repo is a local git repository with one commit, cloned by the job.
func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %v: %s", err, out)
		}
	}
	return dir
}

func newJob(t *testing.T) Job {
	t.Helper()
	return Job{
		Name:   "run-test-job",
		Dir:    t.TempDir(),
		Image:  "docker.io/library/alpine:3",
		Script: []string{"echo hello", "make test"},
		Env:    map[string]string{"B": "2", "A": "1"},
		Git:    &v1alpha1.GitSource{Url: repo(t)},
	}
}

// newFake returns a runtime with the job's image present.
func newFake(job Job) *fake.Runtime {
	rt := fake.New(time.Now)
	rt.EnsureImage(context.Background(), job.Image, nil)
	return rt
}

// exitWhenRunning waits for the job's container to run, then sets its logs
// and exit code, as a node would report them.
func exitWhenRunning(rt *fake.Runtime, id string, lines []string, code int) {
	go func() {
		for range 5000 {
			st, err := rt.Status(context.Background(), id)
			if err == nil && st.State == runtime.StateRunning {
				if len(lines) > 0 {
					var entries []runtime.LogEntry
					for _, l := range lines {
						entries = append(entries, runtime.LogEntry{Stream: "stdout", Line: l})
					}
					rt.SetLogs(id, entries)
				}
				rt.Exit(id, code)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
}

func TestSpecIsolationBaseline(t *testing.T) {
	job := newJob(t)
	spec := Spec(job, ContainerID(job.Name))
	if spec.AllowPrivilegeEscalation {
		t.Error("a job clears no_new_privs: only the build daemon may")
	}
	if !spec.DropAllCapabilities {
		t.Error("a job keeps capabilities")
	}
	if !spec.ReadOnlyRootfs {
		t.Error("a job has a writable root filesystem")
	}
	if len(spec.Tmpfs) != 1 || spec.Tmpfs[0] != "/tmp" {
		t.Errorf("tmpfs = %v, want [/tmp]", spec.Tmpfs)
	}
	if spec.Labels[LabelJob] != job.Name {
		t.Errorf("label %s = %q, want %q", LabelJob, spec.Labels[LabelJob], job.Name)
	}
	for _, m := range spec.Mounts {
		if !strings.HasPrefix(m.Source, job.Dir+string(filepath.Separator)) {
			t.Errorf("mount %s comes from %s, outside the job directory", m.Destination, m.Source)
		}
	}
	if len(spec.Mounts) != 2 {
		t.Fatalf("mounts = %d, want the workspace and the control directory", len(spec.Mounts))
	}
	if ws := spec.Mounts[0]; ws.Destination != workspaceDest || !ws.ReadWrite {
		t.Errorf("workspace mount = %+v, want writable at %s", ws, workspaceDest)
	}
	if ctl := spec.Mounts[1]; ctl.Destination != ctlDest || ctl.ReadWrite {
		t.Errorf("control mount = %+v, want read-only at %s", ctl, ctlDest)
	}
	if spec.Command[0] != "sh" || !strings.Contains(spec.Command[2], ctlDest+"/gate") {
		t.Errorf("command does not wait for the gate: %v", spec.Command)
	}
}

func TestSpecDefaultLimits(t *testing.T) {
	spec := Spec(newJob(t), "c")
	if spec.CPUMillis != resource.DefaultJobCPUMillis || spec.MemoryBytes != resource.DefaultJobMemoryBytes || spec.PidsLimit != resource.DefaultJobPids {
		t.Errorf("limits = %d/%d/%d, want the defaults", spec.CPUMillis, spec.MemoryBytes, spec.PidsLimit)
	}
	job := newJob(t)
	job.CPUMillis, job.MemoryBytes, job.PidsLimit = 250, 1<<20, 64
	spec = Spec(job, "c")
	if spec.CPUMillis != 250 || spec.MemoryBytes != 1<<20 || spec.PidsLimit != 64 {
		t.Errorf("limits = %d/%d/%d, want the job's", spec.CPUMillis, spec.MemoryBytes, spec.PidsLimit)
	}
}

func TestSpecEnvIsSorted(t *testing.T) {
	spec := Spec(newJob(t), "c")
	if len(spec.Env) != 2 || spec.Env[0] != "A=1" || spec.Env[1] != "B=2" {
		t.Errorf("env = %v, want [A=1 B=2]", spec.Env)
	}
}

func TestScriptRunsInTheRepositoryAndStopsOnError(t *testing.T) {
	text := scriptText([]string{"echo hello", "make test"})
	if !strings.HasPrefix(text, "set -e\ncd "+repoDest+"\n") {
		t.Errorf("script = %q, want set -e and a cd to the repository first", text)
	}
	if !strings.HasSuffix(text, "make test\n") {
		t.Errorf("script = %q, want the lines in order", text)
	}
}

func TestRunCompletes(t *testing.T) {
	withIsolation(t, allowAll)
	job := newJob(t)
	rt := newFake(job)
	id := ContainerID(job.Name)
	// The fake returns the lines it holds when the log is read, so they are
	// set before the job starts.
	rt.SetLogs(id, []runtime.LogEntry{{Stream: "stdout", Line: "hello"}})
	exitWhenRunning(rt, id, nil, 0)

	out := Run(context.Background(), rt, job)
	if out.Phase != PhaseComplete {
		t.Fatalf("outcome = %+v, want Complete", out)
	}
	logs, err := os.ReadFile(LogFile(job.Dir))
	if err != nil || !strings.Contains(string(logs), "hello") {
		t.Errorf("job log = %q (%v), want the container's line", logs, err)
	}
	if _, err := rt.Status(context.Background(), id); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("container still exists after the job: %v", err)
	}
	ran, err := os.ReadFile(filepath.Join(job.Dir, "ctl", "run.sh"))
	if err != nil || !strings.Contains(string(ran), "make test") {
		t.Errorf("script file = %q (%v)", ran, err)
	}
}

func TestRunReportsExitCode(t *testing.T) {
	withIsolation(t, allowAll)
	job := newJob(t)
	rt := newFake(job)
	exitWhenRunning(rt, ContainerID(job.Name), nil, 3)

	out := Run(context.Background(), rt, job)
	if out.Phase != PhaseFailed || out.ExitCode != 3 {
		t.Errorf("outcome = %+v, want Failed with exit 3", out)
	}
}

func TestRunTimesOut(t *testing.T) {
	withIsolation(t, allowAll)
	job := newJob(t)
	job.Timeout = 200 * time.Millisecond
	rt := newFake(job)

	out := Run(context.Background(), rt, job)
	if out.Phase != PhaseFailed || out.Message != "timeout" {
		t.Errorf("outcome = %+v, want Failed with message timeout", out)
	}
	if _, err := rt.Status(context.Background(), ContainerID(job.Name)); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("container still exists after a timeout: %v", err)
	}
}

func TestRunCancelStopsTheJob(t *testing.T) {
	withIsolation(t, allowAll)
	job := newJob(t)
	rt := newFake(job)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for range 5000 {
			if st, err := rt.Status(context.Background(), ContainerID(job.Name)); err == nil && st.State == runtime.StateRunning {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	out := Run(ctx, rt, job)
	if out.Phase != PhaseFailed || out.Message != "cancelled" {
		t.Errorf("outcome = %+v, want Failed with message cancelled", out)
	}
	if _, err := rt.Status(context.Background(), ContainerID(job.Name)); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("container still exists after a cancel: %v", err)
	}
}

func TestRunFailsClosedWhenIsolationFails(t *testing.T) {
	withIsolation(t, func(context.Context, runtime.Runtime, string, string) error {
		return errors.New("iptables refused")
	})
	job := newJob(t)
	rt := newFake(job)

	out := Run(context.Background(), rt, job)
	if out.Phase != PhaseError || out.Reason != pipeline.ReasonRunnerError {
		t.Fatalf("outcome = %+v, want Error with runner_error", out)
	}
	if _, err := os.Stat(filepath.Join(job.Dir, "ctl", "gate")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the gate was written after a failed isolation: %v", err)
	}
	if _, err := rt.Status(context.Background(), ContainerID(job.Name)); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("container still exists after a failed isolation: %v", err)
	}
}

func TestRunCloneFailureIsTheJobsFailure(t *testing.T) {
	withIsolation(t, allowAll)
	job := newJob(t)
	job.Git = &v1alpha1.GitSource{Url: filepath.Join(t.TempDir(), "missing")}
	rt := newFake(job)

	out := Run(context.Background(), rt, job)
	if out.Phase != PhaseFailed || !strings.HasPrefix(out.Message, "clone:") {
		t.Errorf("outcome = %+v, want Failed with a clone message", out)
	}
}

func TestRunRemovesCredentialsBeforeTheScript(t *testing.T) {
	withIsolation(t, allowAll)
	job := newJob(t)
	job.Credentials = &v1alpha1.BuildCredentials{Type: resource.SecretTypeBasicAuth, Username: "u", Password: "p"}
	rt := newFake(job)
	exitWhenRunning(rt, ContainerID(job.Name), nil, 0)

	if out := Run(context.Background(), rt, job); out.Phase != PhaseComplete {
		t.Fatalf("outcome = %+v, want Complete", out)
	}
	if _, err := os.Stat(filepath.Join(job.Dir, "git")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("git credentials are still on the node: %v", err)
	}
}

func TestRunRefusesAJobWithoutAScript(t *testing.T) {
	job := newJob(t)
	job.Script = nil
	out := Run(context.Background(), newFake(job), job)
	if out.Phase != PhaseError {
		t.Errorf("outcome = %+v, want Error for a job without a script", out)
	}
}
