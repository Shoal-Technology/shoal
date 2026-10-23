package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/pipeline"
	"github.com/Shoal-Technology/shoal/internal/store"
)

const pipeRun = "app-r1"

// newPipelineEnv returns an env whose controller runs jobs through a fake
// runner.
func newPipelineEnv(t *testing.T) (*env, *pipeline.Fake) {
	t.Helper()
	e := newEnv(t)
	f := pipeline.NewFake()
	e.c = New(Config{Store: e.st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return e.now }, Runner: f})
	return e, f
}

// startRun stores a run of file, as the server does when the file is read.
func startRun(t *testing.T, e *env, file string, ref string, changed ...string) {
	t.Helper()
	src, err := pipeline.Parse([]byte(file))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	specB, err := objects.Encode(&v1alpha1.PipelineRunSpec{
		Pipeline: "app", Source: src, Trigger: "manual", Commit: "0123456789", Ref: ref,
		Trust: "protected", ChangedPaths: changed,
	})
	if err != nil {
		t.Fatal(err)
	}
	statusB, err := objects.Encode(&v1alpha1.PipelineRunStatus{Phase: "New"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Create(context.Background(), store.Record{Kind: v1alpha1.KindPipelineRun, Name: pipeRun, Spec: specB, Status: statusB}); err != nil {
		t.Fatal(err)
	}
}

// pass runs one pipeline pass and fails the test on an error.
func pass(t *testing.T, e *env) {
	t.Helper()
	if errs := e.c.syncPipelines(context.Background()); len(errs) > 0 {
		t.Fatalf("pass: %v", errors.Join(errs...))
	}
}

func jobName(job string) string { return pipeRun + "-" + job }

func (e *env) pjob(t *testing.T, job string) *v1alpha1.Job {
	t.Helper()
	rec, err := e.st.Get(context.Background(), v1alpha1.KindJob, jobName(job))
	if err != nil {
		t.Fatalf("job %s: %v", job, err)
	}
	j, err := objects.Job(rec)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (e *env) jobExists(job string) bool {
	_, err := e.st.Get(context.Background(), v1alpha1.KindJob, jobName(job))
	return err == nil
}

func (e *env) prun(t *testing.T) *v1alpha1.PipelineRun {
	t.Helper()
	rec, err := e.st.Get(context.Background(), v1alpha1.KindPipelineRun, pipeRun)
	if err != nil {
		t.Fatal(err)
	}
	r, err := objects.PipelineRun(rec)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) jobPhase(t *testing.T, job string) string {
	return e.pjob(t, job).GetStatus().GetPhase()
}

func (e *env) runPhase(t *testing.T) string { return e.prun(t).GetStatus().GetPhase() }

// requestRunCancel sets cancel_requested on the run, as CancelPipelineRun does.
func (e *env) requestRunCancel(t *testing.T) {
	t.Helper()
	r := e.prun(t)
	rec := e.record(v1alpha1.KindPipelineRun, pipeRun)
	r.Spec.CancelRequested = true
	enc, err := objects.Encode(r.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Update(context.Background(), v1alpha1.KindPipelineRun, pipeRun, enc, rec.Generation); err != nil {
		t.Fatal(err)
	}
}

// requestPlay sets play_requested on a job, as PlayJob does.
func (e *env) requestPlay(t *testing.T, job string) {
	t.Helper()
	j := e.pjob(t, job)
	rec := e.record(v1alpha1.KindJob, jobName(job))
	j.Spec.PlayRequested = true
	enc, err := objects.Encode(j.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Update(context.Background(), v1alpha1.KindJob, jobName(job), enc, rec.Generation); err != nil {
		t.Fatal(err)
	}
}

const twoStages = `stages: [build, test]
build:
  stage: build
  image: alpine
  script: [make]
test:
  stage: test
  image: alpine
  script: [make test]
`

func TestPipelineRunSucceeds(t *testing.T) {
	e, f := newPipelineEnv(t)
	startRun(t, e, twoStages, "main")

	pass(t, e) // expands the run into jobs
	if got := e.runPhase(t); got != "Running" {
		t.Fatalf("after expansion the run is %s, want Running", got)
	}
	pass(t, e) // build starts; test waits for its stage
	if e.jobPhase(t, "build") != "Running" || e.jobPhase(t, "test") != "New" {
		t.Fatalf("build %s, test %s: only build may start", e.jobPhase(t, "build"), e.jobPhase(t, "test"))
	}
	f.Log(jobName("build"), "compiling")
	lines, err := e.c.JobLogs(context.Background(), jobName("build"))
	if err != nil || len(lines) != 1 || lines[0].Text != "compiling" {
		t.Fatalf("job logs = %v, %v", lines, err)
	}
	f.Finish(jobName("build"), pipeline.Status{State: pipeline.Succeeded})
	pass(t, e) // build completes, test starts
	f.Finish(jobName("test"), pipeline.Status{State: pipeline.Succeeded})
	pass(t, e)

	if got := e.runPhase(t); got != "Complete" {
		t.Fatalf("run = %s, want Complete", got)
	}
	if got := strings.Join(f.Started(), ","); got != jobName("build")+","+jobName("test") {
		t.Errorf("started = %s", got)
	}
	if b := e.pjob(t, "build").GetStatus(); b.GetFinishedAt() == nil || b.GetAttempt() != 1 {
		t.Errorf("finished job status = %v", b)
	}
}

func TestFailedJobStopsLaterStages(t *testing.T) {
	e, f := newPipelineEnv(t)
	startRun(t, e, twoStages, "main")
	pass(t, e)
	pass(t, e)
	f.Finish(jobName("build"), pipeline.Status{State: pipeline.Failed, ExitCode: 1, Message: "exit status 1"})
	pass(t, e)

	if got := e.jobPhase(t, "build"); got != "Failed" {
		t.Errorf("build = %s, want Failed", got)
	}
	if got := e.jobPhase(t, "test"); got != "Skipped" {
		t.Errorf("test = %s, want Skipped", got)
	}
	if got := e.runPhase(t); got != "Failed" {
		t.Errorf("run = %s, want Failed", got)
	}
	if msg := e.prun(t).GetStatus().GetMessage(); msg != "job build failed" {
		t.Errorf("run message = %q", msg)
	}
	if len(f.Started()) != 1 {
		t.Errorf("only build may start, started %v", f.Started())
	}
}

func TestAllowFailureLetsTheRunContinue(t *testing.T) {
	e, f := newPipelineEnv(t)
	startRun(t, e, `stages: [build, test]
build:
  stage: build
  image: alpine
  script: [make]
  allow_failure: true
test:
  stage: test
  image: alpine
  script: [make test]
`, "main")
	pass(t, e)
	pass(t, e)
	f.Finish(jobName("build"), pipeline.Status{State: pipeline.Failed, ExitCode: 2})
	pass(t, e)
	if got := e.jobPhase(t, "build"); got != "Failed" {
		t.Errorf("the allowed failure still shows Failed: got %s", got)
	}
	if got := e.jobPhase(t, "test"); got != "Running" {
		t.Errorf("test = %s, want Running", got)
	}
	f.Finish(jobName("test"), pipeline.Status{State: pipeline.Succeeded})
	pass(t, e)
	if got := e.runPhase(t); got != "Complete" {
		t.Errorf("run = %s, want Complete", got)
	}
}

func TestRetryOnRunnerErrorOnly(t *testing.T) {
	e, f := newPipelineEnv(t)
	startRun(t, e, `stages: [build]
build:
  stage: build
  image: alpine
  script: [make]
  retry: { max: 2 }
`, "main")
	pass(t, e)
	pass(t, e) // attempt 1
	for range 2 {
		f.Finish(jobName("build"), pipeline.Status{State: pipeline.Error, Reason: pipeline.ReasonRunnerError, Message: "agent lost the job"})
		pass(t, e) // a retry: attempt 2, then attempt 3
	}
	if got := e.pjob(t, "build").GetStatus().GetAttempt(); got != 3 {
		t.Fatalf("attempt = %d, want 3", got)
	}
	if got := e.jobPhase(t, "build"); got != "Running" {
		t.Fatalf("build = %s, want Running on the last attempt", got)
	}
	f.Finish(jobName("build"), pipeline.Status{State: pipeline.Error, Reason: pipeline.ReasonRunnerError, Message: "agent lost the job"})
	pass(t, e) // retry max reached
	if got := e.jobPhase(t, "build"); got != "Error" {
		t.Errorf("build = %s, want Error after retry.max", got)
	}
	if got := len(f.Started()); got != 3 {
		t.Errorf("started %d times, want 3 (first try and 2 retries)", got)
	}
	if got := e.runPhase(t); got != "Error" {
		t.Errorf("run = %s, want Error", got)
	}
}

func TestNonZeroExitIsNeverRetried(t *testing.T) {
	e, f := newPipelineEnv(t)
	startRun(t, e, `stages: [build]
build:
  stage: build
  image: alpine
  script: [make]
  retry: { max: 2 }
`, "main")
	pass(t, e)
	pass(t, e)
	f.Finish(jobName("build"), pipeline.Status{State: pipeline.Failed, ExitCode: 1, Message: "exit status 1"})
	pass(t, e)
	if got := e.jobPhase(t, "build"); got != "Failed" {
		t.Errorf("build = %s, want Failed", got)
	}
	if got := len(f.Started()); got != 1 {
		t.Errorf("started %d times: a non-zero exit must not be retried", got)
	}
}

func TestTimeoutFailsTheJob(t *testing.T) {
	e, f := newPipelineEnv(t)
	startRun(t, e, `stages: [build]
build:
  stage: build
  image: alpine
  script: [make]
  timeout: 10m
`, "main")
	pass(t, e)
	pass(t, e) // started at e.now
	e.now = e.now.Add(9 * time.Minute)
	pass(t, e)
	if got := e.jobPhase(t, "build"); got != "Running" {
		t.Fatalf("before the timeout build = %s, want Running", got)
	}
	e.now = e.now.Add(2 * time.Minute)
	pass(t, e)
	j := e.pjob(t, "build")
	if j.GetStatus().GetPhase() != "Failed" || j.GetStatus().GetMessage() != "timeout" {
		t.Fatalf("build = %s %q, want Failed timeout", j.GetStatus().GetPhase(), j.GetStatus().GetMessage())
	}
	if got := strings.Join(f.Cancelled(), ","); got != jobName("build") {
		t.Errorf("the runner was asked to cancel %s, want the build", got)
	}
	if got := e.runPhase(t); got != "Failed" {
		t.Errorf("run = %s, want Failed", got)
	}
}

func TestCancelStopsRunningAndWaitingJobs(t *testing.T) {
	e, f := newPipelineEnv(t)
	startRun(t, e, twoStages, "main")
	pass(t, e)
	pass(t, e) // build running
	e.requestRunCancel(t)
	pass(t, e)

	if got := e.jobPhase(t, "build"); got != "Cancelled" {
		t.Errorf("build = %s, want Cancelled", got)
	}
	if got := e.jobPhase(t, "test"); got != "Cancelled" {
		t.Errorf("test = %s, want Cancelled (it never started)", got)
	}
	if got := strings.Join(f.Cancelled(), ","); got != jobName("build") {
		t.Errorf("cancelled %s, want only the running build", got)
	}
	if got := e.runPhase(t); got != "Cancelled" {
		t.Errorf("run = %s, want Cancelled", got)
	}
	if len(f.Started()) != 1 {
		t.Errorf("test must not start after cancel: started %v", f.Started())
	}
}

func TestManualJobWaitsForPlay(t *testing.T) {
	e, f := newPipelineEnv(t)
	startRun(t, e, `stages: [build, deploy]
build:
  stage: build
  image: alpine
  script: [make]
deploy:
  stage: deploy
  image: alpine
  script: [./deploy]
  rules:
    - if: branch == "main"
      when: manual
`, "main")
	pass(t, e)
	if got := e.jobPhase(t, "deploy"); got != "Manual" {
		t.Fatalf("deploy = %s, want Manual", got)
	}
	pass(t, e)
	f.Finish(jobName("build"), pipeline.Status{State: pipeline.Succeeded})
	pass(t, e)
	if got := e.runPhase(t); got != "Running" {
		t.Fatalf("run = %s, want Running while a manual job waits", got)
	}
	if msg := e.prun(t).GetStatus().GetMessage(); msg != "waiting for manual job deploy" {
		t.Errorf("run message = %q", msg)
	}
	e.requestPlay(t, "deploy")
	pass(t, e)
	f.Finish(jobName("deploy"), pipeline.Status{State: pipeline.Succeeded})
	pass(t, e)
	if got := e.runPhase(t); got != "Complete" {
		t.Errorf("run = %s, want Complete", got)
	}
}

func TestRulesOmitJobsAndAnEmptyRunCompletes(t *testing.T) {
	e, _ := newPipelineEnv(t)
	startRun(t, e, `stages: [build]
build:
  stage: build
  image: alpine
  script: [make]
  rules:
    - if: branch == "main"
docs:
  stage: build
  image: alpine
  script: [mkdocs]
  rules:
    - changes: ["docs/**"]
`, "main", "web/x.ts")
	pass(t, e)
	if !e.jobExists("build") {
		t.Error("build matches branch main and must exist")
	}
	if e.jobExists("docs") {
		t.Error("docs does not match docs/** and must not exist")
	}
	pass(t, e)
}

func TestRunWithNoMatchingJobCompletes(t *testing.T) {
	e, _ := newPipelineEnv(t)
	startRun(t, e, `stages: [build]
build:
  stage: build
  image: alpine
  script: [make]
  rules:
    - if: branch == "main"
`, "dev")
	pass(t, e)
	if got := e.runPhase(t); got != "Complete" {
		t.Errorf("run = %s, want Complete", got)
	}
	if msg := e.prun(t).GetStatus().GetMessage(); msg != "no job matched this run" {
		t.Errorf("message = %q", msg)
	}
}

func TestNoRunnerMakesJobsError(t *testing.T) {
	e := newEnv(t)
	e.c = New(Config{Store: e.st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return e.now }})
	startRun(t, e, twoStages, "main")
	pass(t, e) // expands
	pass(t, e) // build fails to start
	if got := e.jobPhase(t, "build"); got != "Error" {
		t.Errorf("build = %s, want Error", got)
	}
	pass(t, e) // test is skipped, and the run ends
	if got := e.jobPhase(t, "test"); got != "Skipped" {
		t.Errorf("test = %s, want Skipped", got)
	}
	if got := e.runPhase(t); got != "Error" {
		t.Errorf("run = %s, want Error", got)
	}
}

func TestSourceDoesNotChangeWithTheFile(t *testing.T) {
	// The run copies the parsed file when it is created. A later edit of the
	// file in git has no way into the run: the controller reads only the copy.
	e, _ := newPipelineEnv(t)
	startRun(t, e, twoStages, "main")
	pass(t, e)
	src := e.prun(t).GetSpec().GetSource()
	if len(src.GetJobs()) != 2 || src.GetJobs()[0].GetName() != "build" {
		t.Fatalf("source = %v", src)
	}
}

// planOf is a job view for decide.
func planOf(name, stage, phase string, when string, allowFailure bool, needs ...string) planJob {
	return planJob{Name: name, Stage: stage, Phase: phase, When: when, AllowFailure: allowFailure, Needs: needs}
}

func TestDecideStageOrder(t *testing.T) {
	stages := []string{"build", "test"}
	jobs := []planJob{
		planOf("a", "build", "New", "on_success", false),
		planOf("b", "test", "New", "on_success", false),
	}
	out := decide(stages, jobs)
	if d := out["a"]; d.action != actionStart {
		t.Errorf("a = %v, want start", d.action)
	}
	if _, ok := out["b"]; ok {
		t.Errorf("b must wait for the build stage, got %v", out["b"])
	}
}

func TestDecideBlockedAndAlways(t *testing.T) {
	stages := []string{"build", "test"}
	jobs := []planJob{
		planOf("a", "build", "Failed", "on_success", false),
		planOf("c", "test", "New", "on_success", false),
		planOf("d", "test", "New", "always", false),
	}
	out := decide(stages, jobs)
	if d := out["c"]; d.action != actionSkip || d.reason != "an earlier stage failed" {
		t.Errorf("c = %+v, want skipped by the failure", d)
	}
	if d := out["d"]; d.action != actionStart {
		t.Errorf("d (always) = %+v, want start", d)
	}
}

func TestDecideAllowedFailureDoesNotBlock(t *testing.T) {
	stages := []string{"build", "test"}
	jobs := []planJob{
		planOf("a", "build", "Failed", "on_success", true),
		planOf("c", "test", "New", "on_success", false),
	}
	if d := decide(stages, jobs)["c"]; d.action != actionStart {
		t.Errorf("c = %+v, want start: a failure that is allowed does not block", d)
	}
}

func TestDecideNeedsOfAFailedJobSkips(t *testing.T) {
	stages := []string{"build", "test"}
	jobs := []planJob{
		planOf("c", "build", "Failed", "on_success", true),
		planOf("b", "test", "New", "on_success", false, "c"),
	}
	if d := decide(stages, jobs)["b"]; d.action != actionSkip || d.reason != "a job it needs did not complete" {
		t.Errorf("b = %+v, want skipped: its need did not complete", d)
	}
}

func TestDecideNeedsCompleteAndStartsInStage(t *testing.T) {
	stages := []string{"build", "test"}
	jobs := []planJob{
		planOf("c", "build", "Complete", "on_success", false),
		planOf("b", "test", "New", "on_success", false, "c"),
	}
	if d := decide(stages, jobs)["b"]; d.action != actionStart {
		t.Errorf("b = %+v, want start", d)
	}
}

func TestDecideManualWaitsUntilPlayed(t *testing.T) {
	stages := []string{"build", "test"}
	jobs := []planJob{
		planOf("m", "build", "Manual", "manual", false),
		planOf("b", "test", "New", "on_success", false),
	}
	if out := decide(stages, jobs); len(out) != 0 {
		t.Errorf("a manual job that is not played holds the stages after it: %v", out)
	}
	jobs[0].PlayRequested = true
	jobs[0].Phase = "Manual"
	if d := decide(stages, jobs)["m"]; d.action != actionStart {
		t.Errorf("played manual job = %+v, want start", d)
	}
}

func TestDecideWhenNeverSkips(t *testing.T) {
	stages := []string{"build"}
	jobs := []planJob{planOf("n", "build", "New", "never", false)}
	if d := decide(stages, jobs)["n"]; d.action != actionSkip {
		t.Errorf("never = %+v, want skip", d)
	}
}

func TestRetryAllowedRules(t *testing.T) {
	job := &v1alpha1.PipelineJob{Retry: &v1alpha1.JobRetry{Max: 2, When: []string{pipeline.ReasonRunnerError}}}
	cases := []struct {
		attempt int32
		reason  string
		want    bool
	}{
		{1, pipeline.ReasonRunnerError, true},
		{2, pipeline.ReasonRunnerError, true},
		{3, pipeline.ReasonRunnerError, false}, // two retries already used
		{1, pipeline.ReasonNodeLost, false},    // not in when
		{1, "", false},
	}
	for _, tc := range cases {
		if got := retryAllowed(job, tc.attempt, tc.reason); got != tc.want {
			t.Errorf("retryAllowed(attempt %d, %q) = %v, want %v", tc.attempt, tc.reason, got, tc.want)
		}
	}
	if retryAllowed(&v1alpha1.PipelineJob{}, 1, pipeline.ReasonRunnerError) {
		t.Error("no retry block means no retry")
	}
}
