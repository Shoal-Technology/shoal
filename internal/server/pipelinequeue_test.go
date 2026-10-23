package server

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/pipeline"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/store/memory"
)

const testSecretValue = "s3cret-value-for-tests"

// newPipelineEnv returns a dispatcher over an in-memory store. Node r1 is a
// runner, n1 is not. The run is a protected run of the pipeline "web-ci", with
// a Secret "npm-token" holding the key "token".
func newPipelineEnv(t *testing.T, trust string, runnerLabels map[string]string) (*pipelineDispatch, store.Store) {
	t.Helper()
	ctx := t.Context()
	now := time.Now
	st := memory.New(now)
	put := func(kind, name string, spec proto.Message) {
		t.Helper()
		b, err := objects.Encode(spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Create(ctx, store.Record{Kind: kind, Name: name, Spec: b}); err != nil {
			t.Fatal(err)
		}
	}
	put(v1alpha1.KindNode, "r1", &v1alpha1.NodeSpec{Labels: runnerLabels})
	put(v1alpha1.KindNode, "n1", &v1alpha1.NodeSpec{Labels: map[string]string{}})
	put(v1alpha1.KindPipeline, "web-ci", &v1alpha1.PipelineSpec{Repo: "https://example.com/acme/web.git", Ref: "main"})
	put(v1alpha1.KindPipelineRun, "web-ci-run1", &v1alpha1.PipelineRunSpec{
		Pipeline: "web-ci", Commit: "0123456789abcdef0123456789abcdef01234567", Ref: "main", Trust: trust, Trigger: "push",
	})
	put(v1alpha1.KindSecret, "npm-token", &v1alpha1.SecretSpec{Type: "Opaque", Data: map[string]string{"token": testSecretValue}})
	d := newPipelineDispatch(st, slog.Default(), t.TempDir(), now, "10.0.0.5:5000")
	return d, st
}

func testPipelineRequest(name string, job *v1alpha1.PipelineJob) pipeline.Request {
	return pipeline.Request{Name: name, Run: "web-ci-run1", Job: job, Env: map[string]string{"NODE_ENV": "ci"}}
}

func testPipelineJob(name string) *v1alpha1.PipelineJob {
	return &v1alpha1.PipelineJob{
		Name:    name,
		Stage:   "test",
		Image:   "docker.io/library/alpine:3",
		Script:  []string{"echo ok"},
		Timeout: durationpb.New(15 * time.Minute),
	}
}

// TestPipelineJobFlow walks one job through the controller's seam and the
// runner's: start, claim, logs, a refusal from another node, and the result.
func TestPipelineJobFlow(t *testing.T) {
	ctx := t.Context()
	d, _ := newPipelineEnv(t, "protected", map[string]string{"shoal.dev/runner": "true"})

	if err := d.Start(ctx, testPipelineRequest("web-ci-run1-test", testPipelineJob("test"))); err != nil {
		t.Fatalf("start with a runner: %v", err)
	}
	if st, err := d.Status(ctx, "web-ci-run1-test"); err != nil || st.State != pipeline.Running {
		t.Fatalf("queued job status = %+v, %v; want Running", st, err)
	}
	if resp, err := d.claim(ctx, "n1"); err != nil || resp.GetJob() != "" {
		t.Fatalf("a node that is not a runner claimed %q (%v)", resp.GetJob(), err)
	}
	resp, err := d.claim(ctx, "r1")
	if err != nil || resp.GetJob() != "web-ci-run1-test" {
		t.Fatalf("claim = %+v, %v; want the job", resp, err)
	}
	if resp.GetGit().GetUrl() != "https://example.com/acme/web.git" || resp.GetGit().GetRef() == "" {
		t.Errorf("claim git = %+v, want the pipeline repository at the run's commit", resp.GetGit())
	}
	if resp.GetEnv()["NODE_ENV"] != "ci" {
		t.Errorf("claim env = %v, want the variables of the file", resp.GetEnv())
	}
	if resp.GetTimeoutSeconds() != 900 {
		t.Errorf("timeout = %d, want the job's 900 seconds", resp.GetTimeoutSeconds())
	}
	if _, err := d.report("n1", &v1alpha1.ReportJobRequest{Job: "web-ci-run1-test", Lines: []*v1alpha1.LogLine{{Line: "x"}}}); status.Code(err) != codes.NotFound {
		t.Errorf("a node that did not claim the job reported on it: %v", err)
	}
	if _, err := d.report("r1", &v1alpha1.ReportJobRequest{Job: "web-ci-run1-test", Lines: []*v1alpha1.LogLine{{Line: "hello from the job"}}}); err != nil {
		t.Fatalf("report lines: %v", err)
	}
	if lines, err := d.Logs(ctx, "web-ci-run1-test"); err != nil || len(lines) != 1 || lines[0].Text != "hello from the job" {
		t.Errorf("logs = %+v, %v; want the reported line", lines, err)
	}
	if _, err := d.report("r1", &v1alpha1.ReportJobRequest{Job: "web-ci-run1-test", Phase: "Complete"}); err != nil {
		t.Fatalf("report complete: %v", err)
	}
	if st, err := d.Status(ctx, "web-ci-run1-test"); err != nil || st.State != pipeline.Succeeded {
		t.Errorf("finished job status = %+v, %v; want Succeeded", st, err)
	}
	if _, err := d.Status(ctx, "missing"); !errors.Is(err, pipeline.ErrUnknownJob) {
		t.Errorf("unknown job error = %v, want ErrUnknownJob", err)
	}
}

// TestPipelineSecretsOnlyForProtectedRuns: a same-repo run gets no secret, so
// its job fails before any runner sees it (ADR 0022, section 7).
func TestPipelineSecretsOnlyForProtectedRuns(t *testing.T) {
	ctx := t.Context()
	d, _ := newPipelineEnv(t, "same-repo", map[string]string{"shoal.dev/runner": "true"})
	job := testPipelineJob("test")
	job.Secrets = map[string]*v1alpha1.SecretRef{"NPM_TOKEN": {Secret: "npm-token", Key: "token"}}
	if err := d.Start(ctx, testPipelineRequest("web-ci-run1-test", job)); err != nil {
		t.Fatal(err)
	}
	resp, err := d.claim(ctx, "r1")
	if err != nil || resp.GetJob() != "" {
		t.Fatalf("a same-repo run with a secret was claimed: %+v, %v", resp, err)
	}
	st, err := d.Status(ctx, "web-ci-run1-test")
	if err != nil || st.State != pipeline.Failed {
		t.Fatalf("status = %+v, %v; want Failed", st, err)
	}
}

// TestPipelineSecretsReachProtectedRunsOnly checks what a protected run's
// claim carries: the value in the environment, and the value on the redaction
// list. The secret is never in the response's other fields.
func TestPipelineSecretsReachProtectedRunsOnly(t *testing.T) {
	ctx := t.Context()
	d, _ := newPipelineEnv(t, "protected", map[string]string{"shoal.dev/runner": "true"})
	job := testPipelineJob("test")
	job.Secrets = map[string]*v1alpha1.SecretRef{"NPM_TOKEN": {Secret: "npm-token", Key: "token"}}
	if err := d.Start(ctx, testPipelineRequest("web-ci-run1-test", job)); err != nil {
		t.Fatal(err)
	}
	resp, err := d.claim(ctx, "r1")
	if err != nil || resp.GetJob() == "" {
		t.Fatalf("claim = %+v, %v; want the job", resp, err)
	}
	if resp.GetEnv()["NPM_TOKEN"] != testSecretValue {
		t.Errorf("env NPM_TOKEN = %q, want the secret's value", resp.GetEnv()["NPM_TOKEN"])
	}
	if len(resp.GetRedact()) != 1 || resp.GetRedact()[0] != testSecretValue {
		t.Errorf("redact = %v, want the secret's value", resp.GetRedact())
	}
	if resp.GetCredentials().GetPassword() != "" {
		t.Errorf("a run without a credentials Secret got credentials")
	}
}

// TestPipelineMissingSecretKeyFailsTheJob: a Secret without the named key is
// refused at claim time, and the job ends Failed.
func TestPipelineMissingSecretKeyFailsTheJob(t *testing.T) {
	ctx := t.Context()
	d, _ := newPipelineEnv(t, "protected", map[string]string{"shoal.dev/runner": "true"})
	job := testPipelineJob("test")
	job.Secrets = map[string]*v1alpha1.SecretRef{"NPM_TOKEN": {Secret: "npm-token", Key: "absent"}}
	if err := d.Start(ctx, testPipelineRequest("web-ci-run1-test", job)); err != nil {
		t.Fatal(err)
	}
	if resp, err := d.claim(ctx, "r1"); err != nil || resp.GetJob() != "" {
		t.Fatalf("claim = %+v, %v; want nothing", resp, err)
	}
	if st, _ := d.Status(ctx, "web-ci-run1-test"); st.State != pipeline.Failed {
		t.Errorf("status = %+v, want Failed", st)
	}
}

// TestPipelineTagsPlaceTheJob: a job with tags runs only on a runner whose
// labels carry every tag. With no such runner, Start refuses the job.
func TestPipelineTagsPlaceTheJob(t *testing.T) {
	ctx := t.Context()
	d, st := newPipelineEnv(t, "protected", map[string]string{"shoal.dev/runner": "true", "linux-amd64": "true"})
	// n2 is a runner without the tag: it must never take the job.
	b, err := objects.Encode(&v1alpha1.NodeSpec{Labels: map[string]string{"shoal.dev/runner": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, store.Record{Kind: v1alpha1.KindNode, Name: "n2", Spec: b}); err != nil {
		t.Fatal(err)
	}

	job := testPipelineJob("test")
	job.Tags = []string{"gpu"}
	if err := d.Start(ctx, testPipelineRequest("web-ci-run1-gpu", job)); err == nil {
		t.Fatal("start with no runner for the tags succeeded")
	}

	job.Tags = []string{"linux-amd64"}
	if err := d.Start(ctx, testPipelineRequest("web-ci-run1-test", job)); err != nil {
		t.Fatalf("start with a tagged runner: %v", err)
	}
	if resp, err := d.claim(ctx, "n2"); err != nil || resp.GetJob() != "" {
		t.Fatalf("a runner without the tag claimed the job: %+v, %v", resp, err)
	}
	if resp, err := d.claim(ctx, "r1"); err != nil || resp.GetJob() != "web-ci-run1-test" {
		t.Fatalf("tagged runner did not claim the job: %+v, %v", resp, err)
	}
}

// TestPipelineCancelAndRetry: a claimed job's cancel reaches the runner on its
// next report, a queued job is cancelled at once, and a job that ended with
// runner_error can start again under its name.
func TestPipelineCancelAndRetry(t *testing.T) {
	ctx := t.Context()
	d, _ := newPipelineEnv(t, "protected", map[string]string{"shoal.dev/runner": "true"})
	if err := d.Start(ctx, testPipelineRequest("web-ci-run1-a", testPipelineJob("a"))); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(ctx, testPipelineRequest("web-ci-run1-b", testPipelineJob("b"))); err != nil {
		t.Fatal(err)
	}
	if err := d.Cancel(ctx, "web-ci-run1-b"); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(ctx, "web-ci-run1-b"); st.State != pipeline.Failed || st.Message != "cancelled" {
		t.Errorf("cancelled queued job = %+v", st)
	}

	claimed, err := d.claim(ctx, "r1")
	if err != nil || claimed.GetJob() != "web-ci-run1-a" {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	if err := d.Cancel(ctx, "web-ci-run1-a"); err != nil {
		t.Fatal(err)
	}
	if out, err := d.report("r1", &v1alpha1.ReportJobRequest{Job: "web-ci-run1-a"}); err != nil || !out {
		t.Errorf("report after cancel = %v, %v; want the cancel flag", out, err)
	}
	if _, err := d.report("r1", &v1alpha1.ReportJobRequest{Job: "web-ci-run1-a", Phase: "Error", Reason: pipeline.ReasonRunnerError, Message: "lost"}); err != nil {
		t.Fatal(err)
	}
	st, _ := d.Status(ctx, "web-ci-run1-a")
	if st.State != pipeline.Error || st.Reason != pipeline.ReasonRunnerError {
		t.Fatalf("status = %+v, want Error with runner_error", st)
	}
	if err := d.Start(ctx, testPipelineRequest("web-ci-run1-a", testPipelineJob("a"))); err != nil {
		t.Fatalf("retry start: %v", err)
	}
	if st, _ := d.Status(ctx, "web-ci-run1-a"); st.State != pipeline.Running {
		t.Errorf("retried job status = %+v, want Running", st)
	}
}
