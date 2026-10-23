package controller

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/pipeline"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/store"
)

// newDigest is the digest a build pushes in these tests. rolloutDigest is the
// one the environment starts at.
const newDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// deployFile builds one image and deploys it to production.
const deployFile = `stages: [build, deploy]
build-web:
  stage: build
  build: web
deploy-production:
  stage: deploy
  needs: [build-web]
  environment:
    name: production
  deploy:
    workload: web
    from: build-web
`

// twoDeploysFile has two deploys to one environment, which must not overlap.
const twoDeploysFile = `stages: [build, deploy]
build-web:
  stage: build
  build: web
deploy-a:
  stage: deploy
  needs: [build-web]
  environment:
    name: production
  deploy:
    workload: web
    from: build-web
deploy-b:
  stage: deploy
  needs: [build-web]
  environment:
    name: production
  deploy:
    workload: web
    from: build-web
`

// outputFile runs a script job after a build, which reads the build's output.
const outputFile = `stages: [build, test]
build-web:
  stage: build
  build: web
test-web:
  stage: test
  image: alpine
  needs: [build-web]
  script: [make test]
`

// startPipeRun stores a run of file with the given trust, as the server does.
func startPipeRun(t *testing.T, e *env, file, trust string) {
	t.Helper()
	src, err := pipeline.Parse([]byte(file))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	specB, err := objects.Encode(&v1alpha1.PipelineRunSpec{
		Pipeline: "app", Source: src, Trigger: "manual", Commit: "0123456789", Ref: "main", Trust: trust,
	})
	if err != nil {
		t.Fatal(err)
	}
	statusB, err := objects.Encode(&v1alpha1.PipelineRunStatus{Phase: "New"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindPipelineRun, Name: pipeRun, Spec: specB, Status: statusB}); err != nil {
		t.Fatal(err)
	}
}

// environment creates an Environment the way the API applies one.
func (e *env) environment(name string, s *v1alpha1.EnvironmentSpec) {
	e.t.Helper()
	resource.DefaultEnvironment(s)
	if err := resource.ValidateEnvironment(name, s); err != nil {
		e.t.Fatal(err)
	}
	spec, err := objects.Encode(s)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindEnvironment, Name: name, Spec: spec}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) envObj(name string) *v1alpha1.Environment {
	e.t.Helper()
	rec, err := e.st.Get(ctx, v1alpha1.KindEnvironment, name)
	if err != nil {
		e.t.Fatal(err)
	}
	obj, err := objects.Environment(rec)
	if err != nil {
		e.t.Fatal(err)
	}
	return obj
}

// setEnvStatus stores an environment's status, as the controller does.
func (e *env) setEnvStatus(name string, st *v1alpha1.EnvironmentStatus) {
	e.t.Helper()
	enc, err := objects.Encode(st)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.UpdateStatus(ctx, v1alpha1.KindEnvironment, name, enc); err != nil {
		e.t.Fatal(err)
	}
}

// buildRecord creates the Build a build step started, with the repository it
// pushes to.
func (e *env) buildRecord(name, repo string) {
	e.t.Helper()
	spec, err := objects.Encode(&v1alpha1.BuildSpec{
		Config: "web", Source: &v1alpha1.BuildConfigSpec{Output: &v1alpha1.BuildOutput{Repository: repo}},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindBuild, Name: name, Spec: spec}); err != nil {
		e.t.Fatal(err)
	}
}

// setRollout records a rollout result on a Workload, as the rollout code does.
func (e *env) setRollout(name string, gen int64, ro *v1alpha1.RolloutStatus) {
	e.t.Helper()
	enc, err := objects.Encode(&v1alpha1.WorkloadStatus{ObservedGeneration: gen, Rollout: ro})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.UpdateStatus(ctx, v1alpha1.KindWorkload, name, enc); err != nil {
		e.t.Fatal(err)
	}
}

// editJobStatus changes a job's status directly, for the states a test needs.
func (e *env) editJobStatus(t *testing.T, job string, mutate func(*v1alpha1.JobStatus)) {
	t.Helper()
	st := proto.CloneOf(e.pjob(t, job).GetStatus())
	mutate(st)
	enc, err := objects.Encode(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpdateStatus(ctx, v1alpha1.KindJob, jobName(job), enc); err != nil {
		t.Fatal(err)
	}
}

// decide records an approval or a rejection on a job's spec, as ApproveJob and
// RejectJob do.
func (e *env) decide(t *testing.T, job, by, decision, comment string) {
	t.Helper()
	j := e.pjob(t, job)
	rec := e.record(v1alpha1.KindJob, jobName(job))
	j.Spec.Approvals = append(j.Spec.Approvals, &v1alpha1.JobApproval{
		Decision: decision, By: by, At: timestamppb.New(e.now), Comment: comment,
	})
	enc, err := objects.Encode(j.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Update(ctx, v1alpha1.KindJob, jobName(job), enc, rec.Generation); err != nil {
		t.Fatal(err)
	}
}

// buildDone finishes the build step of a file in the fake runner, then runs the
// passes that record its digest.
func buildDone(t *testing.T, e *env, f *pipeline.Fake, digest string) {
	t.Helper()
	pass(t, e) // expands the run
	pass(t, e) // the build starts
	f.Finish(jobName("build-web"), pipeline.Status{State: pipeline.Succeeded, Build: "app-bld1", Image: "app:1", Digest: digest})
	pass(t, e) // the build completes and its outputs are recorded
}

// workloadImage is the image the Workload web runs.
func (e *env) workloadImage() string { return e.workloadObj("web").GetSpec().GetImage() }

func TestDeploySetsWorkloadToBuildDigest(t *testing.T) {
	e, f := newPipelineEnv(t)
	e.workload("web", 1)
	e.environment("production", &v1alpha1.EnvironmentSpec{Workloads: []string{"web"}})
	e.buildRecord("app-bld1", "app")
	startPipeRun(t, e, deployFile, "protected")

	buildDone(t, e, f, newDigest)
	if got, want := e.prun(t).GetStatus().GetOutputs()[resource.BuildOutputKey("build-web", "DIGEST")], newDigest; got != want {
		t.Fatalf("run output digest = %q, want %q", got, want)
	}
	pass(t, e) // the deploy starts
	if got := e.jobPhase(t, "deploy-production"); got != resource.PhaseRunning {
		t.Fatalf("deploy is %s, want Running", got)
	}
	if got, want := e.workloadImage(), "shoal.local/app@"+newDigest; got != want {
		t.Fatalf("workload image = %q, want %q", got, want)
	}

	gen := e.workloadObj("web").GetMetadata().GetGeneration()
	e.setRollout("web", gen, &v1alpha1.RolloutStatus{Phase: "Complete", StableGeneration: gen})
	pass(t, e)
	if got := e.jobPhase(t, "deploy-production"); got != resource.PhaseComplete {
		t.Fatalf("deploy is %s after the rollout completed, want Complete", got)
	}
	env := e.envObj("production")
	if env.GetStatus().GetDigest() != newDigest {
		t.Fatalf("environment digest = %q, want %q", env.GetStatus().GetDigest(), newDigest)
	}
	if h := env.GetStatus().GetDeployments(); len(h) != 1 || h[0].GetPhase() != resource.PhaseComplete || h[0].GetJob() != "deploy-production" {
		t.Fatalf("deployments = %v, want one Complete deploy of deploy-production", h)
	}
	if got := e.runPhase(t); got != resource.PhaseComplete {
		t.Fatalf("run is %s, want Complete", got)
	}
}

func TestDeployRollbackFailsJobAndKeepsEnvironmentDigest(t *testing.T) {
	e, f := newPipelineEnv(t)
	e.workload("web", 1)
	e.environment("production", &v1alpha1.EnvironmentSpec{Workloads: []string{"web"}})
	e.setEnvStatus("production", &v1alpha1.EnvironmentStatus{Digest: rolloutDigest})
	e.buildRecord("app-bld1", "app")
	startPipeRun(t, e, deployFile, "protected")

	buildDone(t, e, f, newDigest)
	pass(t, e)
	gen := e.workloadObj("web").GetMetadata().GetGeneration()
	e.setRollout("web", gen, &v1alpha1.RolloutStatus{
		Phase: "RolledBack", FailedGeneration: gen, StableGeneration: gen - 1, Message: "replica crash-looped",
	})
	pass(t, e)

	if got := e.jobPhase(t, "deploy-production"); got != resource.PhaseFailed {
		t.Fatalf("deploy is %s after a rollback, want Failed", got)
	}
	env := e.envObj("production")
	if env.GetStatus().GetDigest() != rolloutDigest || env.GetStatus().GetPreviousDigest() != "" {
		t.Fatalf("environment digest = %q (previous %q), want it unchanged at %q",
			env.GetStatus().GetDigest(), env.GetStatus().GetPreviousDigest(), rolloutDigest)
	}
	if h := env.GetStatus().GetDeployments(); len(h) != 1 || h[0].GetPhase() != resource.PhaseFailed {
		t.Fatalf("deployments = %v, want one Failed deploy", h)
	}
}

func TestDeploysOfOneEnvironmentDoNotOverlap(t *testing.T) {
	e, f := newPipelineEnv(t)
	e.workload("web", 1)
	e.environment("production", &v1alpha1.EnvironmentSpec{Workloads: []string{"web"}})
	e.buildRecord("app-bld1", "app")
	startPipeRun(t, e, twoDeploysFile, "protected")

	buildDone(t, e, f, newDigest)
	pass(t, e) // deploy-a starts, deploy-b waits for the lock
	if got := e.jobPhase(t, "deploy-a"); got != resource.PhaseRunning {
		t.Fatalf("deploy-a is %s, want Running", got)
	}
	if got := e.jobPhase(t, "deploy-b"); got != resource.PhasePending {
		t.Fatalf("deploy-b is %s while deploy-a runs, want Pending", got)
	}

	gen := e.workloadObj("web").GetMetadata().GetGeneration()
	e.setRollout("web", gen, &v1alpha1.RolloutStatus{Phase: "Complete", StableGeneration: gen})
	pass(t, e) // deploy-a completes
	if got := e.jobPhase(t, "deploy-a"); got != resource.PhaseComplete {
		t.Fatalf("deploy-a is %s, want Complete", got)
	}
	pass(t, e) // deploy-b starts and its rollout is already complete
	pass(t, e)
	if got := e.jobPhase(t, "deploy-b"); got != resource.PhaseComplete {
		t.Fatalf("deploy-b is %s, want Complete once the lock is free", got)
	}
	if h := e.envObj("production").GetStatus().GetDeployments(); len(h) != 2 {
		t.Fatalf("history has %d deploys, want 2", len(h))
	}
}

func TestDeployWorkloadMustBeInEnvironment(t *testing.T) {
	e, f := newPipelineEnv(t)
	e.workload("web", 1)
	e.environment("production", &v1alpha1.EnvironmentSpec{Workloads: []string{"api"}})
	e.buildRecord("app-bld1", "app")
	startPipeRun(t, e, deployFile, "protected")

	buildDone(t, e, f, newDigest)
	pass(t, e)
	if got := e.jobPhase(t, "deploy-production"); got != resource.PhaseFailed {
		t.Fatalf("deploy is %s, want Failed", got)
	}
	if msg := e.pjob(t, "deploy-production").GetStatus().GetMessage(); !strings.Contains(msg, "not one of the workloads") {
		t.Fatalf("message = %q", msg)
	}
	if got := e.workloadImage(); got == "shoal.local/app@"+newDigest {
		t.Fatalf("workload was set to the digest of a deploy it may not make")
	}
}

func TestDeployOnlyInProtectedRun(t *testing.T) {
	e, f := newPipelineEnv(t)
	e.workload("web", 1)
	e.environment("production", &v1alpha1.EnvironmentSpec{Workloads: []string{"web"}})
	e.buildRecord("app-bld1", "app")
	startPipeRun(t, e, deployFile, "same-repo")

	pass(t, e)
	pass(t, e)
	f.Finish(jobName("build-web"), pipeline.Status{State: pipeline.Succeeded, Build: "app-bld1", Image: "app:1", Digest: newDigest})
	pass(t, e)
	pass(t, e)
	if got := e.jobPhase(t, "deploy-production"); got != resource.PhaseSkipped {
		t.Fatalf("deploy in a same-repo run is %s, want Skipped", got)
	}
	if got := e.workloadImage(); got == "shoal.local/app@"+newDigest {
		t.Fatalf("a same-repo run changed the workload")
	}
}

// protectedProduction is an environment that needs approval before a deploy.
func protectedProduction(e *env, required int32) {
	e.environment("production", &v1alpha1.EnvironmentSpec{Protected: true, RequiredApprovals: required, Workloads: []string{"web"}})
}

func TestApprovalGateApproveStartsDeploy(t *testing.T) {
	e, f := newPipelineEnv(t)
	e.workload("web", 1)
	protectedProduction(e, 0)
	e.buildRecord("app-bld1", "app")
	startPipeRun(t, e, deployFile, "protected")

	buildDone(t, e, f, newDigest)
	pass(t, e)
	j := e.pjob(t, "deploy-production")
	if j.GetStatus().GetPhase() != resource.PhaseManual || j.GetStatus().GetMessage() != "waiting for approval (0 of 1)" {
		t.Fatalf("deploy is %s (%q), want Manual waiting for approval", j.GetStatus().GetPhase(), j.GetStatus().GetMessage())
	}
	if j.GetStatus().GetWaitingSince() == nil {
		t.Fatalf("waiting_since is not set")
	}
	if got := e.workloadImage(); got == "shoal.local/app@"+newDigest {
		t.Fatalf("the workload changed before approval")
	}

	e.decide(t, "deploy-production", "admin", resource.DecisionApproved, "looks good")
	pass(t, e)
	j = e.pjob(t, "deploy-production")
	if j.GetStatus().GetPhase() != resource.PhaseRunning {
		t.Fatalf("deploy is %s after approval, want Running", j.GetStatus().GetPhase())
	}
	if as := j.GetStatus().GetApprovals(); len(as) != 1 || as[0].GetBy() != "admin" || as[0].GetComment() != "looks good" || as[0].GetAt() == nil {
		t.Fatalf("approval record = %v, want one by admin with a time", as)
	}
	if got, want := e.workloadImage(), "shoal.local/app@"+newDigest; got != want {
		t.Fatalf("workload image = %q, want %q", got, want)
	}
	gen := e.workloadObj("web").GetMetadata().GetGeneration()
	e.setRollout("web", gen, &v1alpha1.RolloutStatus{Phase: "Complete", StableGeneration: gen})
	pass(t, e)
	if h := e.envObj("production").GetStatus().GetDeployments(); len(h) != 1 || len(h[0].GetApprovedBy()) != 1 || h[0].GetApprovedBy()[0] != "admin" {
		t.Fatalf("deployment history = %v, want one deploy approved by admin", h)
	}
}

func TestApprovalGateRejectCancelsJob(t *testing.T) {
	e, f := newPipelineEnv(t)
	e.workload("web", 1)
	protectedProduction(e, 0)
	e.buildRecord("app-bld1", "app")
	startPipeRun(t, e, deployFile, "protected")

	buildDone(t, e, f, newDigest)
	pass(t, e)
	e.decide(t, "deploy-production", "ops", resource.DecisionRejected, "freeze")
	pass(t, e)

	j := e.pjob(t, "deploy-production")
	if j.GetStatus().GetPhase() != resource.PhaseCancelled {
		t.Fatalf("deploy is %s after rejection, want Cancelled", j.GetStatus().GetPhase())
	}
	if msg := j.GetStatus().GetMessage(); !strings.Contains(msg, "rejected by ops: freeze") {
		t.Fatalf("message = %q", msg)
	}
	if got := e.workloadImage(); got == "shoal.local/app@"+newDigest {
		t.Fatalf("a rejected deploy changed the workload")
	}
	if got := e.runPhase(t); got != resource.PhaseFailed {
		t.Fatalf("run is %s after a rejected deploy, want Failed", got)
	}
}

func TestApprovalGateTimesOut(t *testing.T) {
	e, f := newPipelineEnv(t)
	e.workload("web", 1)
	protectedProduction(e, 0)
	e.buildRecord("app-bld1", "app")
	startPipeRun(t, e, deployFile, "protected")

	buildDone(t, e, f, newDigest)
	pass(t, e)
	e.now = e.now.Add(resource.ApprovalTimeout - time.Minute)
	pass(t, e)
	if got := e.jobPhase(t, "deploy-production"); got != resource.PhaseManual {
		t.Fatalf("deploy is %s before the timeout, want Manual", got)
	}
	e.now = e.now.Add(2 * time.Minute)
	pass(t, e)
	j := e.pjob(t, "deploy-production")
	if j.GetStatus().GetPhase() != resource.PhaseFailed || j.GetStatus().GetMessage() != "approval timed out" {
		t.Fatalf("deploy is %s (%q), want Failed on the approval timeout", j.GetStatus().GetPhase(), j.GetStatus().GetMessage())
	}
	if got := e.workloadImage(); got == "shoal.local/app@"+newDigest {
		t.Fatalf("a timed-out deploy changed the workload")
	}
}

func TestApprovalGateNeedsEveryRequiredApprover(t *testing.T) {
	e, f := newPipelineEnv(t)
	e.workload("web", 1)
	protectedProduction(e, 2)
	e.buildRecord("app-bld1", "app")
	startPipeRun(t, e, deployFile, "protected")

	buildDone(t, e, f, newDigest)
	e.decide(t, "deploy-production", "admin", resource.DecisionApproved, "")
	e.decide(t, "deploy-production", "admin", resource.DecisionApproved, "") // the same approver counts once
	pass(t, e)
	if j := e.pjob(t, "deploy-production"); j.GetStatus().GetPhase() != resource.PhaseManual || j.GetStatus().GetMessage() != "waiting for approval (1 of 2)" {
		t.Fatalf("deploy is %s (%q) with one approver, want Manual (1 of 2)", j.GetStatus().GetPhase(), j.GetStatus().GetMessage())
	}
	e.decide(t, "deploy-production", "ops", resource.DecisionApproved, "")
	pass(t, e)
	if got := e.jobPhase(t, "deploy-production"); got != resource.PhaseRunning {
		t.Fatalf("deploy is %s with two approvers, want Running", got)
	}
}

func TestJobTokenScope(t *testing.T) {
	e, f := newPipelineEnv(t)
	e.workload("web", 1)
	e.environment("production", &v1alpha1.EnvironmentSpec{Workloads: []string{"web"}})
	e.buildRecord("app-bld1", "app")
	startPipeRun(t, e, deployFile, "protected")
	buildDone(t, e, f, newDigest)
	pass(t, e) // the deploy starts

	// A token the test knows, in place of the random one the deploy got.
	const token = "known-deploy-token"
	e.editJobStatus(t, "deploy-production", func(st *v1alpha1.JobStatus) { st.TokenHash = tokenHash(token) })
	job := jobName("deploy-production")
	authorize := func(action, run, tok, workload string) error {
		return e.c.authorizeDeploy(ctx, action, run, job, tok, workload)
	}

	if err := authorize("deploy", pipeRun, token, "web"); err != nil {
		t.Fatalf("the job's own token, for its own workload: %v", err)
	}
	denied := map[string]error{
		"wrong token":       authorize("deploy", pipeRun, "guessed", "web"),
		"empty token":       authorize("deploy", pipeRun, "", "web"),
		"other workload":    authorize("deploy", pipeRun, token, "api"),
		"other action":      authorize("read", pipeRun, token, "web"),
		"other run":         authorize("deploy", "app-r2", token, "web"),
		"admin-like action": authorize("admin", pipeRun, token, "web"),
	}
	for name, err := range denied {
		if err == nil {
			t.Errorf("%s: allowed, want denied", name)
		}
	}

	// The job ends: the rollout completes and the token is revoked.
	gen := e.workloadObj("web").GetMetadata().GetGeneration()
	e.setRollout("web", gen, &v1alpha1.RolloutStatus{Phase: "Complete", StableGeneration: gen})
	pass(t, e)
	if err := authorize("deploy", pipeRun, token, "web"); err == nil {
		t.Fatalf("token allowed after its job ended")
	}
}

func TestJobTokenRevokedWhenRunEnds(t *testing.T) {
	e, f := newPipelineEnv(t)
	e.workload("web", 1)
	e.environment("production", &v1alpha1.EnvironmentSpec{Workloads: []string{"web"}})
	e.buildRecord("app-bld1", "app")
	startPipeRun(t, e, deployFile, "protected")
	buildDone(t, e, f, newDigest)
	pass(t, e)

	const token = "still-running-token"
	e.editJobStatus(t, "deploy-production", func(st *v1alpha1.JobStatus) { st.TokenHash = tokenHash(token) })
	job := jobName("deploy-production")
	if err := e.c.authorizeDeploy(ctx, "deploy", pipeRun, job, token, "web"); err != nil {
		t.Fatalf("before the run ends: %v", err)
	}

	// The run is cancelled: its jobs stop, and the run's tokens are revoked.
	e.requestRunCancel(t)
	pass(t, e)
	if got := e.runPhase(t); got != resource.PhaseCancelled {
		t.Fatalf("run is %s, want Cancelled", got)
	}
	if err := e.c.authorizeDeploy(ctx, "deploy", pipeRun, job, token, "web"); err == nil {
		t.Fatalf("token allowed after its run ended")
	}
	if got := e.pjob(t, "deploy-production").GetStatus().GetTokenHash(); got != "" {
		t.Fatalf("token hash still stored after the run ended")
	}
}

func TestBuildOutputsReachLaterJobs(t *testing.T) {
	e, f := newPipelineEnv(t)
	e.buildRecord("app-bld1", "app")
	startPipeRun(t, e, outputFile, "protected")
	buildDone(t, e, f, newDigest)
	pass(t, e) // the test job starts with the build's outputs

	env := f.Env(jobName("test-web"))
	if env == nil {
		t.Fatalf("test-web was not started")
	}
	if got := env[resource.BuildOutputKey("build-web", "IMAGE")]; got != "app:1" {
		t.Fatalf("SHOAL_BUILD_BUILD_WEB_IMAGE = %q, want app:1", got)
	}
	if got := env[resource.BuildOutputKey("build-web", "DIGEST")]; got != newDigest {
		t.Fatalf("SHOAL_BUILD_BUILD_WEB_DIGEST = %q, want %q", got, newDigest)
	}
}

func TestEnvironmentHistoryKeepsLastDeploys(t *testing.T) {
	e, _ := newPipelineEnv(t)
	e.environment("production", &v1alpha1.EnvironmentSpec{Workloads: []string{"web"}})
	for i := range 12 {
		entry := &v1alpha1.EnvironmentDeployment{
			Job: "deploy", Phase: resource.PhaseComplete, Digest: "sha256:" + strings.Repeat(string(rune('a'+i)), 64),
		}
		if err := e.c.recordDeployment(ctx, "production", entry); err != nil {
			t.Fatal(err)
		}
	}
	st := e.envObj("production").GetStatus()
	if len(st.GetDeployments()) != resource.EnvironmentHistory {
		t.Fatalf("history has %d deploys, want %d", len(st.GetDeployments()), resource.EnvironmentHistory)
	}
	if st.GetDeployments()[0].GetDigest() != "sha256:"+strings.Repeat("l", 64) {
		t.Fatalf("history is not newest first")
	}
	if st.GetDigest() != "sha256:"+strings.Repeat("l", 64) || st.GetPreviousDigest() != "sha256:"+strings.Repeat("k", 64) {
		t.Fatalf("digest %q, previous %q: want the last two deploys", st.GetDigest(), st.GetPreviousDigest())
	}
}
