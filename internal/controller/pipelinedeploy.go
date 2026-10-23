package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/runtime"
	"github.com/Shoal-Technology/shoal/internal/store"
)

// Environments, approvals and deploy steps (ADR 0022, pass C, section 10).
//
// A deploy step runs in the controller and starts no container. It sets the
// Workload's image to the digest a build step pushed, through the same rollout
// path a build uses (ADR 0010), and ends when that rollout completes or fails.
// The step acts with a run-scoped token (section 5): the token is random, the
// job keeps only its hash, and the token stops working when the job ends or the
// run does.

// errTokenDenied is what a job token check returns when it does not allow the
// action. The reason is not returned, so a caller cannot probe the job.
var errTokenDenied = errors.New("the job token does not allow this")

// runningDeploys maps each environment to the running deploy job that holds its
// lock. A second deploy to the environment waits (resource_group, GL-35).
func runningDeploys(runs map[string]*pipelineRun) map[string]string {
	busy := map[string]string{}
	for _, run := range runs {
		for _, j := range run.jobs {
			job := j.obj.GetSpec().GetJob()
			if job.GetDeploy() != nil && j.phase() == resource.PhaseRunning {
				busy[job.GetEnvironment().GetName()] = j.rec.Name
			}
		}
	}
	return busy
}

// findRunJob returns the job of run with the given name in the file, or nil.
func findRunJob(run *pipelineRun, name string) *pipelineJob {
	for _, j := range run.jobs {
		if j.logicalName() == name {
			return j
		}
	}
	return nil
}

// recordOutputs sets the run's outputs from the build steps that completed:
// SHOAL_BUILD_<JOB>_IMAGE and SHOAL_BUILD_<JOB>_DIGEST (ADR 0022, section 4).
// Later jobs of the run start with them.
func (c *Controller) recordOutputs(ctx context.Context, run *pipelineRun) []error {
	out := map[string]string{}
	for _, j := range run.jobs {
		st := j.obj.GetStatus()
		if j.obj.GetSpec().GetJob().GetBuild() == "" || st.GetPhase() != resource.PhaseComplete || st.GetDigest() == "" {
			continue
		}
		out[resource.BuildOutputKey(j.logicalName(), "IMAGE")] = st.GetImage()
		out[resource.BuildOutputKey(j.logicalName(), "DIGEST")] = st.GetDigest()
	}
	if maps.Equal(out, run.obj.GetStatus().GetOutputs()) {
		return nil
	}
	st := proto.CloneOf(run.obj.GetStatus())
	st.Outputs = out
	return c.writePipelineRunStatus(ctx, run, st)
}

// decideApprovals reads the approvals and rejections of the gated jobs that
// wait. A rejection cancels the job. Enough approvals mark the job approved for
// this pass. A wait longer than resource.ApprovalTimeout fails it.
func (c *Controller) decideApprovals(ctx context.Context, run *pipelineRun, now time.Time) []error {
	var errs []error
	for _, j := range run.jobs {
		j.approved = false
		job := j.obj.GetSpec().GetJob()
		if j.phase() != resource.PhaseManual || job.GetEnvironment() == nil {
			continue
		}
		env, err := c.loadEnvironment(ctx, job.GetEnvironment().GetName())
		if errors.Is(err, store.ErrNotFound) {
			continue // startJob reports the missing environment
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		approvals := j.obj.GetSpec().GetApprovals()
		st := proto.CloneOf(j.obj.GetStatus())
		st.Approvals = approvals
		if rej := resource.Rejection(approvals); rej != nil {
			msg := "rejected by " + rej.GetBy()
			if rej.GetComment() != "" {
				msg += ": " + rej.GetComment()
			}
			st.Phase, st.Message, st.FinishedAt = resource.PhaseCancelled, msg, timestamppb.New(now)
			if err := c.writeJobStatus(ctx, j, st); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		need := resource.ApprovalsNeeded(resource.JobGated(job, env.GetSpec()), env.GetSpec())
		have := len(resource.ApprovedBy(approvals))
		if need > 0 && have >= need {
			j.approved = true
			continue
		}
		since := j.obj.GetStatus().GetWaitingSince()
		if since != nil && now.Sub(since.AsTime()) >= resource.ApprovalTimeout {
			st.Phase, st.Message, st.FinishedAt = resource.PhaseFailed, "approval timed out", timestamppb.New(now)
			if err := c.writeJobStatus(ctx, j, st); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		// A job that has passed the gate once shows how many approvals it has.
		if since != nil && need > 0 {
			st.Message = approvalWaitMessage(have, need)
			if err := c.writeJobStatus(ctx, j, st); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}

// approvalWaitMessage is what a gated job says while it waits for approvals.
func approvalWaitMessage(have, need int) string {
	return fmt.Sprintf("waiting for approval (%d of %d)", have, need)
}

// waitForApproval holds a gated job in Manual until it has its approvals.
func (c *Controller) waitForApproval(ctx context.Context, j *pipelineJob, have, need int, now time.Time) error {
	st := proto.CloneOf(j.obj.GetStatus())
	st.Phase = resource.PhaseManual
	st.Message = approvalWaitMessage(have, need)
	st.Approvals = j.obj.GetSpec().GetApprovals()
	if st.WaitingSince == nil {
		st.WaitingSince = timestamppb.New(now)
	}
	return c.writeJobStatus(ctx, j, st)
}

// loadEnvironment reads an Environment record.
func (c *Controller) loadEnvironment(ctx context.Context, name string) (*v1alpha1.Environment, error) {
	rec, err := c.st.Get(ctx, v1alpha1.KindEnvironment, name)
	if err != nil {
		return nil, err
	}
	return objects.Environment(rec)
}

// startDeploy starts a deploy job: it checks the environment and the lock, takes
// a token, authorises the deploy with it, and sets the Workload's image. The
// rollout is then watched by checkDeploy.
func (c *Controller) startDeploy(ctx context.Context, run *pipelineRun, j *pipelineJob, env *v1alpha1.EnvironmentSpec, busy map[string]string, now time.Time) error {
	job := j.obj.GetSpec().GetJob()
	d := job.GetDeploy()
	envName := job.GetEnvironment().GetName()
	if env == nil {
		return c.setJobPhase(ctx, j, resource.PhaseError, "a deploy step needs an environment", now)
	}
	if !slices.Contains(env.GetWorkloads(), d.GetWorkload()) {
		return c.setJobPhase(ctx, j, resource.PhaseFailed,
			fmt.Sprintf("workload %s is not one of the workloads of environment %s", d.GetWorkload(), envName), now)
	}
	if holder, ok := busy[envName]; ok && holder != j.rec.Name {
		return c.setJobPhase(ctx, j, resource.PhasePending,
			fmt.Sprintf("waiting for environment %s: job %s is deploying", envName, holder), now)
	}
	image, digest, err := c.deployImage(ctx, run, d.GetFrom())
	if err != nil {
		return c.setJobPhase(ctx, j, resource.PhaseFailed, err.Error(), now)
	}
	token, hash, err := newDeployToken()
	if err != nil {
		return fmt.Errorf("deploy token: %w", err)
	}
	st := proto.CloneOf(j.obj.GetStatus())
	st.Phase, st.Message = resource.PhaseRunning, ""
	st.StartedAt = timestamppb.New(now)
	st.Attempt = max(st.Attempt, 1)
	st.Image, st.Digest, st.TokenHash = image, digest, hash
	st.Approvals, st.WaitingSince, st.WorkloadGeneration = j.obj.GetSpec().GetApprovals(), nil, 0
	if err := c.writeJobStatus(ctx, j, st); err != nil {
		return err
	}
	busy[envName] = j.rec.Name
	if err := c.authorizeDeploy(ctx, resource.ActionDeploy, run.rec.Name, j.rec.Name, token, d.GetWorkload()); err != nil {
		return c.finishDeploy(ctx, j, resource.PhaseFailed, err.Error(), now)
	}
	c.log.Info("deploy started", "job", j.rec.Name, "workload", d.GetWorkload(), "image", image)
	return c.setDeployImage(ctx, j, now)
}

// deployImage is the image a deploy job sets: the registry and the repository
// of the build step named by from, pinned to the digest that build pushed.
func (c *Controller) deployImage(ctx context.Context, run *pipelineRun, from string) (string, string, error) {
	src := findRunJob(run, from)
	if src == nil || src.obj.GetSpec().GetJob().GetBuild() == "" {
		return "", "", fmt.Errorf("deploy.from %q is not a build job of this run", from)
	}
	st := src.obj.GetStatus()
	if st.GetPhase() != resource.PhaseComplete || st.GetBuild() == "" || !digestRef.MatchString(st.GetDigest()) {
		return "", "", fmt.Errorf("build job %s has no digest to deploy: its build did not complete", from)
	}
	rec, err := c.st.Get(ctx, v1alpha1.KindBuild, st.GetBuild())
	if err != nil {
		return "", "", fmt.Errorf("build %s of job %s: %w", st.GetBuild(), from, err)
	}
	b, err := objects.Build(rec)
	if err != nil {
		return "", "", err
	}
	repo := b.GetSpec().GetSource().GetOutput().GetRepository()
	if repo == "" {
		return "", "", fmt.Errorf("build %s has no output repository", st.GetBuild())
	}
	return runtime.RegistryAlias + "/" + repo + "@" + st.GetDigest(), st.GetDigest(), nil
}

// setDeployImage sets the deploy's Workload to the job's image and records the
// generation the rollout must reach. Setting it again is harmless, so a pass
// that failed part way retries it.
func (c *Controller) setDeployImage(ctx context.Context, j *pipelineJob, now time.Time) error {
	d := j.obj.GetSpec().GetJob().GetDeploy()
	gen, err := c.setWorkloadImage(ctx, d.GetWorkload(), j.obj.GetStatus().GetImage())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return c.finishDeploy(ctx, j, resource.PhaseFailed, fmt.Sprintf("workload %s not found", d.GetWorkload()), now)
	case err != nil:
		return err
	}
	st := proto.CloneOf(j.obj.GetStatus())
	st.WorkloadGeneration = gen
	return c.writeJobStatus(ctx, j, st)
}

// checkDeploy watches a running deploy job until its rollout completes, fails,
// is superseded, or runs out of time.
func (c *Controller) checkDeploy(ctx context.Context, run *pipelineRun, j *pipelineJob, now time.Time) error {
	job := j.obj.GetSpec().GetJob()
	d := job.GetDeploy()
	st := j.obj.GetStatus()
	if st.GetWorkloadGeneration() == 0 {
		return c.setDeployImage(ctx, j, now)
	}
	wrec, err := c.st.Get(ctx, v1alpha1.KindWorkload, d.GetWorkload())
	if errors.Is(err, store.ErrNotFound) {
		return c.finishDeploy(ctx, j, resource.PhaseFailed, fmt.Sprintf("workload %s not found", d.GetWorkload()), now)
	}
	if err != nil {
		return err
	}
	w, err := objects.Workload(wrec)
	if err != nil {
		return err
	}
	gen := st.GetWorkloadGeneration()
	ro := w.GetStatus().GetRollout()
	switch {
	case ro.GetPhase() == RolloutComplete && ro.GetStableGeneration() >= gen && w.GetStatus().GetObservedGeneration() >= gen:
		return c.finishDeploy(ctx, j, resource.PhaseComplete, "", now)
	case w.GetMetadata().GetGeneration() > gen:
		return c.finishDeploy(ctx, j, resource.PhaseFailed,
			fmt.Sprintf("workload %s changed again before the rollout finished", d.GetWorkload()), now)
	case ro.GetFailedGeneration() == gen && (ro.GetPhase() == RolloutRolledBack || ro.GetPhase() == RolloutFailed):
		msg := "the rollout failed"
		if ro.GetPhase() == RolloutRolledBack {
			msg = "the rollout failed and was rolled back"
		}
		if ro.GetMessage() != "" {
			msg += ": " + ro.GetMessage()
		}
		return c.finishDeploy(ctx, j, resource.PhaseFailed, msg, now)
	}
	deadline := timeoutOf(job)
	if deadline == 0 {
		deadline = resource.DefaultJobTimeoutSeconds * time.Second
	}
	if st.GetStartedAt() != nil && now.Sub(st.GetStartedAt().AsTime()) >= deadline {
		return c.finishDeploy(ctx, j, resource.PhaseFailed, "timeout", now)
	}
	return nil
}

// finishDeploy ends a deploy job, revokes its token and records the deploy in
// the environment's history.
func (c *Controller) finishDeploy(ctx context.Context, j *pipelineJob, phase, msg string, now time.Time) error {
	job := j.obj.GetSpec().GetJob()
	st := proto.CloneOf(j.obj.GetStatus())
	st.Phase, st.Message, st.FinishedAt, st.TokenHash = phase, msg, timestamppb.New(now), ""
	if err := c.writeJobStatus(ctx, j, st); err != nil {
		return err
	}
	c.log.Info("deploy finished", "job", j.rec.Name, "phase", phase)
	return c.recordDeployment(ctx, job.GetEnvironment().GetName(), &v1alpha1.EnvironmentDeployment{
		Run: j.obj.GetSpec().GetPipelineRun(), Job: job.GetName(), Workload: job.GetDeploy().GetWorkload(),
		Image: st.GetImage(), Digest: st.GetDigest(), Phase: phase, Message: msg,
		ApprovedBy: resource.ApprovedBy(j.obj.GetSpec().GetApprovals()), At: timestamppb.New(now),
	})
}

// recordDeployment adds a deploy to an environment's history, keeping the last
// resource.EnvironmentHistory. A completed deploy moves the environment's digest
// on and keeps the one before it as the rollback target.
func (c *Controller) recordDeployment(ctx context.Context, envName string, entry *v1alpha1.EnvironmentDeployment) error {
	e, err := c.loadEnvironment(ctx, envName)
	if errors.Is(err, store.ErrNotFound) {
		return nil // deleted during the deploy
	}
	if err != nil {
		return err
	}
	st := proto.CloneOf(e.GetStatus())
	if st == nil {
		st = &v1alpha1.EnvironmentStatus{}
	}
	if entry.GetPhase() == resource.PhaseComplete && entry.GetDigest() != "" && entry.GetDigest() != st.GetDigest() {
		st.PreviousDigest, st.Digest = st.GetDigest(), entry.GetDigest()
	}
	st.Deployments = append([]*v1alpha1.EnvironmentDeployment{entry}, st.GetDeployments()...)
	if len(st.Deployments) > resource.EnvironmentHistory {
		st.Deployments = st.Deployments[:resource.EnvironmentHistory]
	}
	enc, err := objects.Encode(st)
	if err != nil {
		return err
	}
	if _, err := c.st.UpdateStatus(ctx, v1alpha1.KindEnvironment, envName, enc); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("update status of environment %s: %w", envName, err)
	}
	return nil
}

// newDeployToken returns a new random token and its hash. Only the hash is
// stored.
func newDeployToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(b)
	return token, tokenHash(token), nil
}

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// authorizeDeploy reports whether token allows action on workload for job
// jobName of run runName. The token must be the one the job stored, the job
// must belong to the run, the run must not have ended, the action must be
// deploy, and the workload must be the one the job deploys (ADR 0022, section
// 5). Any other request gets errTokenDenied, with no reason.
func (c *Controller) authorizeDeploy(ctx context.Context, action, runName, jobName, token, workload string) error {
	if action != resource.ActionDeploy || token == "" {
		return errTokenDenied
	}
	jrec, err := c.st.Get(ctx, v1alpha1.KindJob, jobName)
	if err != nil {
		return errTokenDenied
	}
	j, err := objects.Job(jrec)
	if err != nil || j.GetSpec().GetPipelineRun() != runName {
		return errTokenDenied
	}
	rrec, err := c.st.Get(ctx, v1alpha1.KindPipelineRun, runName)
	if err != nil {
		return errTokenDenied
	}
	r, err := objects.PipelineRun(rrec)
	if err != nil || resource.PipelineRunTerminal(r.GetStatus().GetPhase()) {
		return errTokenDenied
	}
	stored := j.GetStatus().GetTokenHash()
	if stored == "" || subtle.ConstantTimeCompare([]byte(tokenHash(token)), []byte(stored)) != 1 {
		return errTokenDenied
	}
	if d := j.GetSpec().GetJob().GetDeploy(); d == nil || d.GetWorkload() != workload {
		return errTokenDenied
	}
	return nil
}

// revokeRunTokens clears the token hash of every job of a run that has ended.
func (c *Controller) revokeRunTokens(ctx context.Context, run *pipelineRun) []error {
	var errs []error
	for _, j := range run.jobs {
		if j.obj.GetStatus().GetTokenHash() == "" {
			continue
		}
		st := proto.CloneOf(j.obj.GetStatus())
		st.TokenHash = ""
		if err := c.writeJobStatus(ctx, j, st); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
