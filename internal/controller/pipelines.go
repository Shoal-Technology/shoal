package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/pipeline"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/store"
)

// Pipelines (ADR 0022, pass A). A PipelineRun is expanded once, into one Job
// per job whose rules match. Each pass then reads the runner's state for the
// running Jobs, cancels, retries or times out as asked, starts the jobs whose
// stages are done, skips the ones an earlier failure blocks, and writes the
// run's phase. Only the controller writes the status of a run and its jobs.

// pipelineRun is a PipelineRun with its Jobs, as one pass sees it.
type pipelineRun struct {
	rec  store.Record
	obj  *v1alpha1.PipelineRun
	jobs []*pipelineJob // in file order
}

// pipelineJob is a Job with its record.
type pipelineJob struct {
	rec store.Record
	obj *v1alpha1.Job
}

func (j *pipelineJob) phase() string { return j.obj.GetStatus().GetPhase() }

// logicalName is the job's name in the file, which needs refers to.
func (j *pipelineJob) logicalName() string { return j.obj.GetSpec().GetJob().GetName() }

// syncPipelines runs one pass over the PipelineRuns and returns the errors it
// skipped over.
func (c *Controller) syncPipelines(ctx context.Context) []error {
	rrecs, err := c.st.List(ctx, v1alpha1.KindPipelineRun)
	if err != nil {
		return []error{err}
	}
	jrecs, err := c.st.List(ctx, v1alpha1.KindJob)
	if err != nil {
		return []error{err}
	}
	precs, err := c.st.List(ctx, v1alpha1.KindPipeline)
	if err != nil {
		return []error{err}
	}
	var errs []error
	runs := map[string]*pipelineRun{}
	for _, r := range rrecs {
		obj, err := objects.PipelineRun(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		runs[r.Name] = &pipelineRun{rec: r, obj: obj}
	}
	for _, r := range jrecs {
		obj, err := objects.Job(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		run := runs[obj.GetSpec().GetPipelineRun()]
		if run == nil {
			if err := c.deleteIgnoringMissing(ctx, v1alpha1.KindJob, r.Name); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		run.jobs = append(run.jobs, &pipelineJob{rec: r, obj: obj})
	}
	now := c.cfg.Now()
	for _, name := range slices.Sorted(maps.Keys(runs)) {
		errs = append(errs, c.syncRun(ctx, runs[name], now)...)
	}
	return append(errs, c.syncPipelineStatus(ctx, precs, runs)...)
}

// syncRun advances one run by one pass.
func (c *Controller) syncRun(ctx context.Context, run *pipelineRun, now time.Time) []error {
	phase := run.obj.GetStatus().GetPhase()
	if resource.PipelineRunTerminal(phase) {
		return nil
	}
	if phase == "" || phase == resource.PhaseNew {
		return c.expandRun(ctx, run, now)
	}
	var errs []error
	for _, j := range run.jobs {
		if j.phase() != resource.PhaseRunning {
			continue
		}
		if err := c.checkJob(ctx, run, j, now); err != nil {
			errs = append(errs, err)
		}
	}
	if run.obj.GetSpec().GetCancelRequested() {
		return append(errs, c.cancelRun(ctx, run, now)...)
	}
	errs = append(errs, c.advanceJobs(ctx, run, now)...)
	return append(errs, c.writeRunProgress(ctx, run, now)...)
}

// expandRun creates one Job for each job of the run whose rules match, and
// moves the run to Running. A job that already exists (from an earlier pass
// that failed part way) is kept, so a retried expansion does not duplicate it.
func (c *Controller) expandRun(ctx context.Context, run *pipelineRun, now time.Time) []error {
	src := run.obj.GetSpec().GetSource()
	rctx := runContext(run.obj.GetSpec())
	existing := map[string]bool{}
	for _, j := range run.jobs {
		existing[j.rec.Name] = true
	}
	var errs []error
	matched := 0
	for _, job := range src.GetJobs() {
		when, ok := pipeline.Select(job, rctx)
		if !ok {
			continue
		}
		matched++
		name := resource.JobName(run.rec.Name, job.GetName())
		if existing[name] {
			continue
		}
		phase, msg := resource.PhaseNew, "waiting for earlier stages"
		switch when {
		case pipeline.WhenNever:
			phase, msg = resource.PhaseSkipped, "when: never"
		case pipeline.WhenManual:
			phase, msg = resource.PhaseManual, "waiting for play"
		}
		specB, err := objects.Encode(&v1alpha1.JobSpec{
			PipelineRun: run.rec.Name, Job: job, When: when,
			AllowFailure: job.GetAllowFailure(), Env: resource.JobEnv(src, job),
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		statusB, err := objects.Encode(&v1alpha1.JobStatus{Phase: phase, Message: msg})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := c.st.Create(ctx, store.Record{Kind: v1alpha1.KindJob, Name: name, Spec: specB, Status: statusB}); err != nil && !errors.Is(err, store.ErrExists) {
			errs = append(errs, fmt.Errorf("create job %s: %w", name, err))
		}
	}
	if len(errs) > 0 {
		return errs // the next pass expands the rest; existing jobs are kept
	}
	st := proto.CloneOf(run.obj.GetStatus())
	if matched == 0 {
		st.Phase, st.Message, st.FinishedAt = resource.PhaseComplete, "no job matched this run", timestamppb.New(now)
	} else {
		st.Phase, st.Message, st.StartedAt = resource.PhaseRunning, "", timestamppb.New(now)
	}
	c.log.Info("pipeline run expanded", "run", run.rec.Name, "jobs", matched)
	return c.writePipelineRunStatus(ctx, run, st)
}

// runContext is what the rules of a run are evaluated against. A tag run has
// no branch. The event of a tag run is push.
func runContext(spec *v1alpha1.PipelineRunSpec) pipeline.Context {
	ctx := pipeline.Context{Changed: spec.GetChangedPaths()}
	switch spec.GetTrigger() {
	case "tag":
		ctx.Tag, ctx.Event = true, pipeline.EventPush
	case "pull_request":
		ctx.Branch, ctx.Event = spec.GetRef(), pipeline.EventPullRequest
	case "push":
		ctx.Branch, ctx.Event = spec.GetRef(), pipeline.EventPush
	default:
		ctx.Branch, ctx.Event = spec.GetRef(), pipeline.EventManual
	}
	return ctx
}

// checkJob reads the runner's state for a running job and records what it
// found: success, failure, a retry for an infrastructure error, or a timeout.
func (c *Controller) checkJob(ctx context.Context, run *pipelineRun, j *pipelineJob, now time.Time) error {
	if c.cfg.Runner == nil {
		return c.setJobPhase(ctx, j, resource.PhaseError, noRunnerMessage, now)
	}
	st, err := c.cfg.Runner.Status(ctx, j.rec.Name)
	switch {
	case errors.Is(err, pipeline.ErrUnknownJob):
		st = pipeline.Status{State: pipeline.Error, Reason: pipeline.ReasonRunnerError, Message: "the job is gone"}
	case err != nil:
		return fmt.Errorf("status of job %s: %w", j.rec.Name, err)
	}
	switch st.State {
	case pipeline.Succeeded:
		return c.finishJob(ctx, j, resource.PhaseComplete, "", st, now)
	case pipeline.Failed:
		return c.finishJob(ctx, j, resource.PhaseFailed, orDefault(st.Message, "the script failed"), st, now)
	case pipeline.Error:
		if retryAllowed(j.obj.GetSpec().GetJob(), j.obj.GetStatus().GetAttempt(), st.Reason) {
			return c.retryJob(ctx, run, j, now)
		}
		return c.finishJob(ctx, j, resource.PhaseError, orDefault(st.Message, "the job did not finish"), st, now)
	}
	if d := timeoutOf(j.obj.GetSpec().GetJob()); d > 0 && j.obj.GetStatus().GetStartedAt() != nil &&
		now.Sub(j.obj.GetStatus().GetStartedAt().AsTime()) >= d {
		if err := c.cfg.Runner.Cancel(ctx, j.rec.Name); err != nil && !errors.Is(err, pipeline.ErrUnknownJob) {
			return fmt.Errorf("cancel timed-out job %s: %w", j.rec.Name, err)
		}
		c.log.Info("pipeline job timed out", "job", j.rec.Name, "timeout", d)
		return c.finishJob(ctx, j, resource.PhaseFailed, "timeout", st, now)
	}
	return nil
}

// retryAllowed reports whether a job that ended with an Error for reason may
// be started again. Only the reasons listed in retry.when are retried, and
// attempts stop at retry.max retries after the first try.
func retryAllowed(job *v1alpha1.PipelineJob, attempt int32, reason string) bool {
	r := job.GetRetry()
	if r == nil || reason == "" {
		return false
	}
	if attempt-1 >= r.GetMax() {
		return false
	}
	return slices.Contains(r.GetWhen(), reason)
}

// retryJob starts a job again after an infrastructure error. The job stays
// Running and counts one more attempt.
func (c *Controller) retryJob(ctx context.Context, run *pipelineRun, j *pipelineJob, now time.Time) error {
	if err := c.cfg.Runner.Start(ctx, pipelineRequestOf(run, j)); err != nil {
		return c.setJobPhase(ctx, j, resource.PhaseError, "could not restart the job: "+err.Error(), now)
	}
	c.log.Info("pipeline job retried", "job", j.rec.Name, "attempt", j.obj.GetStatus().GetAttempt()+1)
	st := proto.CloneOf(j.obj.GetStatus())
	st.Attempt++
	st.Phase, st.Message, st.StartedAt, st.Node = resource.PhaseRunning, "", timestamppb.New(now), ""
	return c.writeJobStatus(ctx, j, st)
}

// advanceJobs starts the jobs whose stages and needs are done, and skips the
// ones an earlier failure or a failed need blocks.
func (c *Controller) advanceJobs(ctx context.Context, run *pipelineRun, now time.Time) []error {
	plan := make([]planJob, 0, len(run.jobs))
	for _, j := range run.jobs {
		job := j.obj.GetSpec().GetJob()
		plan = append(plan, planJob{
			Name: job.GetName(), Stage: job.GetStage(), Needs: job.GetNeeds(),
			Phase: j.phase(), When: j.obj.GetSpec().GetWhen(), AllowFailure: j.obj.GetSpec().GetAllowFailure(),
			PlayRequested: j.obj.GetSpec().GetPlayRequested(),
		})
	}
	decisions := decide(run.obj.GetSpec().GetSource().GetStages(), plan)
	var errs []error
	for _, j := range run.jobs {
		switch d := decisions[j.logicalName()]; d.action {
		case actionStart:
			if err := c.startJob(ctx, run, j, now); err != nil {
				errs = append(errs, err)
			}
		case actionSkip:
			if err := c.setJobPhase(ctx, j, resource.PhaseSkipped, d.reason, now); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}

// startJob starts a waiting job through the runner. A start that fails leaves
// the job in Error.
func (c *Controller) startJob(ctx context.Context, run *pipelineRun, j *pipelineJob, now time.Time) error {
	if c.cfg.Runner == nil {
		return c.setJobPhase(ctx, j, resource.PhaseError, noRunnerMessage, now)
	}
	if err := c.cfg.Runner.Start(ctx, pipelineRequestOf(run, j)); err != nil {
		return c.setJobPhase(ctx, j, resource.PhaseError, "could not start the job: "+err.Error(), now)
	}
	c.log.Info("pipeline job started", "job", j.rec.Name, "run", run.rec.Name)
	st := proto.CloneOf(j.obj.GetStatus())
	st.Phase, st.Message = resource.PhaseRunning, ""
	st.StartedAt = timestamppb.New(now)
	st.Attempt = max(st.Attempt, 1)
	st.LogRef = j.rec.Name
	return c.writeJobStatus(ctx, j, st)
}

// cancelRun stops every job that has not finished and marks the run
// Cancelled once none is left running.
func (c *Controller) cancelRun(ctx context.Context, run *pipelineRun, now time.Time) []error {
	var errs []error
	for _, j := range run.jobs {
		if resource.JobTerminal(j.phase()) {
			continue
		}
		msg := "cancelled before it started"
		if j.phase() == resource.PhaseRunning {
			msg = "cancelled"
			if err := c.cancelJob(ctx, j); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := c.setJobPhase(ctx, j, resource.PhaseCancelled, msg, now); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errs
	}
	for _, j := range run.jobs {
		if !resource.JobTerminal(j.phase()) {
			return nil
		}
	}
	st := proto.CloneOf(run.obj.GetStatus())
	st.Phase, st.Message, st.FinishedAt = resource.PhaseCancelled, "cancelled", timestamppb.New(now)
	c.log.Info("pipeline run cancelled", "run", run.rec.Name)
	return c.writePipelineRunStatus(ctx, run, st)
}

// cancelJob asks the runner to stop a running job. A job the runner no longer
// knows has nothing to stop.
func (c *Controller) cancelJob(ctx context.Context, j *pipelineJob) error {
	if c.cfg.Runner == nil {
		return nil
	}
	if err := c.cfg.Runner.Cancel(ctx, j.rec.Name); err != nil && !errors.Is(err, pipeline.ErrUnknownJob) {
		return fmt.Errorf("cancel job %s: %w", j.rec.Name, err)
	}
	return nil
}

// writeRunProgress sets the run's phase from its jobs: Running while any job
// waits or runs, and a final phase once all are finished.
func (c *Controller) writeRunProgress(ctx context.Context, run *pipelineRun, now time.Time) []error {
	var waitingManual string
	unfinished := false
	var failed, infra bool
	var firstBad string
	for _, j := range run.jobs {
		p := j.phase()
		if !resource.JobTerminal(p) {
			unfinished = true
			if p == resource.PhaseManual && waitingManual == "" {
				waitingManual = j.logicalName()
			}
			continue
		}
		if blockingPhase(p) && !j.obj.GetSpec().GetAllowFailure() {
			if firstBad == "" {
				firstBad = j.logicalName()
			}
			failed = true
			infra = infra || p == resource.PhaseError
		}
	}
	if unfinished {
		msg := ""
		if waitingManual != "" {
			msg = "waiting for manual job " + waitingManual
		}
		return c.writePipelineRunStatus(ctx, run, withRunPhase(run, resource.PhaseRunning, msg))
	}
	st := proto.CloneOf(run.obj.GetStatus())
	st.FinishedAt = timestamppb.New(now)
	switch {
	case !failed:
		st.Phase, st.Message = resource.PhaseComplete, ""
	case infra:
		st.Phase, st.Message = resource.PhaseError, fmt.Sprintf("job %s could not run", firstBad)
	default:
		st.Phase, st.Message = resource.PhaseFailed, fmt.Sprintf("job %s failed", firstBad)
	}
	c.log.Info("pipeline run finished", "run", run.rec.Name, "phase", st.Phase)
	return c.writePipelineRunStatus(ctx, run, st)
}

func withRunPhase(run *pipelineRun, phase, msg string) *v1alpha1.PipelineRunStatus {
	st := proto.CloneOf(run.obj.GetStatus())
	st.Phase, st.Message = phase, msg
	return st
}

// finishJob records how a job ended, with the node and exit code the runner
// reported.
func (c *Controller) finishJob(ctx context.Context, j *pipelineJob, phase, msg string, st pipeline.Status, now time.Time) error {
	s := proto.CloneOf(j.obj.GetStatus())
	s.Phase, s.Message = phase, msg
	s.FinishedAt = timestamppb.New(now)
	if st.Node != "" {
		s.Node = st.Node
	}
	s.ExitCode = int32(st.ExitCode)
	c.log.Info("pipeline job finished", "job", j.rec.Name, "phase", phase)
	return c.writeJobStatus(ctx, j, s)
}

// setJobPhase records a phase and message. A finished phase also records when
// the job finished.
func (c *Controller) setJobPhase(ctx context.Context, j *pipelineJob, phase, msg string, now time.Time) error {
	st := proto.CloneOf(j.obj.GetStatus())
	st.Phase, st.Message = phase, msg
	if resource.JobTerminal(phase) && st.FinishedAt == nil {
		st.FinishedAt = timestamppb.New(now)
	}
	return c.writeJobStatus(ctx, j, st)
}

// writeJobStatus stores a job's status if it changed.
func (c *Controller) writeJobStatus(ctx context.Context, j *pipelineJob, st *v1alpha1.JobStatus) error {
	if proto.Equal(j.obj.GetStatus(), st) {
		return nil
	}
	enc, err := objects.Encode(st)
	if err != nil {
		return err
	}
	rec, err := c.st.UpdateStatus(ctx, v1alpha1.KindJob, j.rec.Name, enc)
	if errors.Is(err, store.ErrNotFound) {
		return nil // deleted during the pass
	}
	if err != nil {
		return fmt.Errorf("update status of job %s: %w", j.rec.Name, err)
	}
	j.rec, j.obj.Status = rec, st
	return nil
}

// writePipelineRunStatus stores a run's status if it changed.
func (c *Controller) writePipelineRunStatus(ctx context.Context, run *pipelineRun, st *v1alpha1.PipelineRunStatus) []error {
	if proto.Equal(run.obj.GetStatus(), st) {
		return nil
	}
	enc, err := objects.Encode(st)
	if err != nil {
		return []error{err}
	}
	rec, err := c.st.UpdateStatus(ctx, v1alpha1.KindPipelineRun, run.rec.Name, enc)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return []error{fmt.Errorf("update status of pipeline run %s: %w", run.rec.Name, err)}
	}
	run.rec, run.obj.Status = rec, st
	return nil
}

// syncPipelineStatus records the newest run of each Pipeline and its phase.
func (c *Controller) syncPipelineStatus(ctx context.Context, precs []store.Record, runs map[string]*pipelineRun) []error {
	newest := map[string]*pipelineRun{}
	for _, run := range runs {
		p := run.obj.GetSpec().GetPipeline()
		cur := newest[p]
		if cur == nil || run.rec.CreatedAt.After(cur.rec.CreatedAt) {
			newest[p] = run
		}
	}
	var errs []error
	for _, r := range precs {
		cur, err := objects.Pipeline(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		st := &v1alpha1.PipelineStatus{}
		if n := newest[r.Name]; n != nil {
			st.LastRun, st.LastPhase = n.rec.Name, n.obj.GetStatus().GetPhase()
		}
		if proto.Equal(cur.GetStatus(), st) {
			continue
		}
		enc, err := objects.Encode(st)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := c.st.UpdateStatus(ctx, v1alpha1.KindPipeline, r.Name, enc); err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, fmt.Errorf("update status of pipeline %s: %w", r.Name, err))
		}
	}
	return errs
}

// JobLogs returns the output the runner has for a job. Without a runner there
// is none, and a job the runner does not know has none either.
func (c *Controller) JobLogs(ctx context.Context, name string) ([]pipeline.Line, error) {
	if c.cfg.Runner == nil {
		return nil, nil
	}
	rec, err := c.st.Get(ctx, v1alpha1.KindJob, name)
	if err != nil {
		return nil, err
	}
	j, err := objects.Job(rec)
	if err != nil {
		return nil, err
	}
	ref := j.GetStatus().GetLogRef()
	if ref == "" {
		ref = rec.Name
	}
	lines, err := c.cfg.Runner.Logs(ctx, ref)
	if errors.Is(err, pipeline.ErrUnknownJob) {
		return nil, nil
	}
	return lines, err
}

const noRunnerMessage = "no pipeline runner is configured on this server"

func pipelineRequestOf(run *pipelineRun, j *pipelineJob) pipeline.Request {
	return pipeline.Request{
		Name: j.rec.Name, Run: run.rec.Name,
		Job: j.obj.GetSpec().GetJob(), Env: j.obj.GetSpec().GetEnv(),
	}
}

// timeoutOf is the job's timeout, or zero for none.
func timeoutOf(job *v1alpha1.PipelineJob) time.Duration {
	if job.GetTimeout() == nil {
		return 0
	}
	return job.GetTimeout().AsDuration()
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Actions of one pass on a waiting job.
type action int

const (
	actionWait action = iota
	actionStart
	actionSkip
)

type decision struct {
	action action
	reason string
}

// planJob is what decide needs to know about one job.
type planJob struct {
	Name, Stage   string
	Needs         []string
	Phase         string
	When          string
	AllowFailure  bool
	PlayRequested bool
}

// blockingPhase reports whether a job that ended this way stops the stages
// after it, unless the job allows failure. A Skipped job does not block:
// something earlier did.
func blockingPhase(phase string) bool {
	return phase == resource.PhaseFailed || phase == resource.PhaseError || phase == resource.PhaseCancelled
}

// waiting reports whether a job is one the pass decides about: New or Pending,
// or a manual job that was played.
func waiting(j planJob, phase string) bool {
	switch phase {
	case resource.PhaseNew, resource.PhasePending:
		return true
	case resource.PhaseManual:
		return j.PlayRequested
	}
	return false
}

// decide returns what the pass does with each waiting job (ADR 0022, section
// 3). Stages run in order. A job starts when every earlier stage has finished
// and, for on_success, no earlier job that does not allow failure has failed.
// A job also needs its needs to have completed. always starts regardless of an
// earlier failure. A job that cannot run is skipped. Phases change as decisions
// are made, so a job started here keeps the next stage waiting.
func decide(stages []string, jobs []planJob) map[string]decision {
	phase := map[string]string{}
	for _, j := range jobs {
		phase[j.Name] = j.Phase
	}
	byStage := make([][]planJob, len(stages))
	for _, j := range jobs {
		if i := slices.Index(stages, j.Stage); i >= 0 {
			byStage[i] = append(byStage[i], j)
		}
	}
	out := map[string]decision{}
	settled, blocked := true, false
	for i := range stages {
		for _, j := range byStage[i] {
			if !waiting(j, phase[j.Name]) || !settled {
				continue
			}
			needsFailed := false
			for _, n := range j.Needs {
				if p := phase[n]; p != resource.PhaseComplete && resource.JobTerminal(p) {
					needsFailed = true
				}
			}
			when := j.When
			if j.PlayRequested || when == "" {
				when = pipeline.WhenOnSuccess
			}
			var d decision
			switch {
			case when == pipeline.WhenNever:
				d = decision{action: actionSkip, reason: "when: never"}
			case needsFailed:
				d = decision{action: actionSkip, reason: "a job it needs did not complete"}
			case when == pipeline.WhenAlways:
				d = decision{action: actionStart}
			case blocked:
				d = decision{action: actionSkip, reason: "an earlier stage failed"}
			default:
				d = decision{action: actionStart}
			}
			out[j.Name] = d
			if d.action == actionStart {
				phase[j.Name] = resource.PhaseRunning
			} else {
				phase[j.Name] = resource.PhaseSkipped
			}
		}
		for _, j := range byStage[i] {
			p := phase[j.Name]
			if !resource.JobTerminal(p) {
				settled = false
			}
			if blockingPhase(p) && !j.AllowFailure {
				blocked = true
			}
		}
	}
	return out
}
