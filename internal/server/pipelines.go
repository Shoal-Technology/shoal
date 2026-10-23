package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/pipeline"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

// Pipelines (ADR 0022, pass A). A PipelineRun is created here from the file the
// caller read at a commit; the controller expands and runs it. Pass A reads
// the file on the CLI side, so the server never clones a repository. Pass B
// adds the clone on the server or the agent.

// maxChangedPaths bounds the changed-path list of one run.
const maxChangedPaths = 10000

// StartPipelineRun creates a PipelineRun of a Pipeline. The file is parsed and
// checked here, and each Secret a job names must exist with its key. The run
// keeps a copy of the parsed file.
func (c *controlService) StartPipelineRun(ctx context.Context, req *v1alpha1.StartPipelineRunRequest) (*v1alpha1.StartPipelineRunResponse, error) {
	name := req.GetPipeline()
	switch {
	case name == "":
		return nil, status.Error(codes.InvalidArgument, "pipeline is required")
	case req.GetCommit() == "":
		return nil, status.Error(codes.InvalidArgument, "commit is required: the file is read at a commit")
	case len(req.GetFile()) == 0:
		return nil, status.Error(codes.InvalidArgument, "file is required: the contents of shoal-ci.yml at the commit")
	case len(req.GetChangedPaths()) > maxChangedPaths:
		return nil, status.Errorf(codes.InvalidArgument, "at most %d changed paths", maxChangedPaths)
	}
	rec, err := c.s.findObject(ctx, v1alpha1.KindPipeline, name, req.GetProject())
	if err != nil {
		return nil, apiError(err)
	}
	project := keyProject(rec)
	p, err := objects.Pipeline(rec)
	if err != nil {
		return nil, apiError(err)
	}
	src, err := pipeline.Parse([]byte(req.GetFile()))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%s is not valid:\n%v", pipeline.FileName, err)
	}
	if err := c.s.checkPipelineSecrets(ctx, project, src); err != nil {
		return nil, err
	}
	if err := c.s.checkPipelineEnvironments(ctx, project, src, resource.TrustProtected); err != nil {
		return nil, err
	}
	ref := req.GetRef()
	if ref == "" {
		ref = p.GetSpec().GetRef()
	}
	if ref == "" {
		ref = resource.DefaultPipelineRef
	}
	rname, err := c.s.createPipelineRun(ctx, project, name, &v1alpha1.PipelineRunSpec{
		Pipeline:     name,
		Source:       src,
		Trigger:      resource.TriggerManual,
		Commit:       req.GetCommit(),
		Ref:          ref,
		Trust:        resource.TrustProtected,
		ChangedPaths: req.GetChangedPaths(),
	})
	if err != nil {
		return nil, err
	}
	return &v1alpha1.StartPipelineRunResponse{Run: tenancy.KeyName(rname)}, nil
}

// createPipelineRun stores a new run of pipeline (a bare name in project) and
// returns the run's store key. The controller expands it.
func (s *Server) createPipelineRun(ctx context.Context, project, pipelineName string, spec *v1alpha1.PipelineRunSpec) (string, error) {
	specB, err := objects.Encode(spec)
	if err != nil {
		return "", apiError(err)
	}
	statusB, err := objects.Encode(&v1alpha1.PipelineRunStatus{Phase: resource.PhaseNew, Message: "waiting to start"})
	if err != nil {
		return "", apiError(err)
	}
	for range 5 {
		rname := resource.PipelineRunName(pipelineName, randomPipelineSuffix())
		key := tenancy.Key(project, rname)
		_, err := s.store.Create(ctx, store.Record{Kind: v1alpha1.KindPipelineRun, Name: key, Spec: specB, Status: statusB, Labels: tenancy.WithProject(nil, project)})
		if errors.Is(err, store.ErrExists) {
			continue
		}
		if err != nil {
			return "", apiError(err)
		}
		s.log.Info("pipeline run requested", "run", rname, "pipeline", pipelineName, "project", project, "commit", spec.GetCommit())
		return key, nil
	}
	return "", status.Error(codes.Internal, "no free pipeline run name")
}

// checkPipelineSecrets checks that each Secret a job names exists and holds the
// key. The value is not read into the response and not stored in the run.
func (s *Server) checkPipelineSecrets(ctx context.Context, project string, src *v1alpha1.PipelineSource) error {
	var errs []error
	for _, job := range src.GetJobs() {
		for _, env := range slices.Sorted(maps.Keys(job.GetSecrets())) {
			ref := job.GetSecrets()[env]
			rec, err := s.store.Get(ctx, v1alpha1.KindSecret, tenancy.Key(project, ref.GetSecret()))
			if errors.Is(err, store.ErrNotFound) {
				errs = append(errs, fmt.Errorf("job %s: %s names Secret %q, which does not exist", job.GetName(), env, ref.GetSecret()))
				continue
			}
			if err != nil {
				return apiError(err)
			}
			sec, err := objects.Secret(rec)
			if err != nil {
				return apiError(err)
			}
			if _, ok := sec.GetSpec().GetData()[ref.GetKey()]; !ok {
				errs = append(errs, fmt.Errorf("job %s: Secret %q has no key %q", job.GetName(), ref.GetSecret(), ref.GetKey()))
			}
		}
	}
	if len(errs) > 0 {
		return status.Error(codes.FailedPrecondition, errors.Join(errs...).Error())
	}
	return nil
}

// checkPipelineEnvironments checks the environments a file's jobs name, for a
// run of trust. A deploy step names a Workload that its environment lists (ADR
// 0022, section 10). Only a protected run needs the check: any other run creates
// these jobs Skipped (the controller does that).
func (s *Server) checkPipelineEnvironments(ctx context.Context, project string, src *v1alpha1.PipelineSource, trust string) error {
	if trust != resource.TrustProtected {
		return nil
	}
	var errs []error
	for _, job := range src.GetJobs() {
		d, envName := job.GetDeploy(), job.GetEnvironment().GetName()
		if d != nil && envName == "" {
			errs = append(errs, fmt.Errorf("job %s: a deploy step needs an environment", job.GetName()))
			continue
		}
		if envName == "" {
			continue
		}
		rec, err := s.store.Get(ctx, v1alpha1.KindEnvironment, tenancy.Key(project, envName))
		if errors.Is(err, store.ErrNotFound) {
			errs = append(errs, fmt.Errorf("job %s: environment %q does not exist", job.GetName(), envName))
			continue
		}
		if err != nil {
			return apiError(err)
		}
		e, err := objects.Environment(rec)
		if err != nil {
			return apiError(err)
		}
		if d != nil && !slices.Contains(e.GetSpec().GetWorkloads(), d.GetWorkload()) {
			errs = append(errs, fmt.Errorf("job %s: workload %q is not one of the workloads of environment %q", job.GetName(), d.GetWorkload(), envName))
		}
	}
	if len(errs) > 0 {
		return status.Error(codes.FailedPrecondition, errors.Join(errs...).Error())
	}
	return nil
}

// maxApprovalComment bounds the comment of an approval or rejection.
const maxApprovalComment = 500

// ApproveJob records an approval of a job that waits at an environment's gate
// (ADR 0022, section 10). The approver is the caller. The controller starts the
// job once the environment's approvals are recorded.
func (c *controlService) ApproveJob(ctx context.Context, req *v1alpha1.ApproveJobRequest) (*v1alpha1.ApproveJobResponse, error) {
	if err := c.decideJob(ctx, req.GetJob(), req.GetProject(), resource.DecisionApproved, req.GetComment()); err != nil {
		return nil, err
	}
	return &v1alpha1.ApproveJobResponse{}, nil
}

// RejectJob records a rejection of a job at an environment's gate. The job ends
// Cancelled.
func (c *controlService) RejectJob(ctx context.Context, req *v1alpha1.RejectJobRequest) (*v1alpha1.RejectJobResponse, error) {
	if err := c.decideJob(ctx, req.GetJob(), req.GetProject(), resource.DecisionRejected, req.GetComment()); err != nil {
		return nil, err
	}
	return &v1alpha1.RejectJobResponse{}, nil
}

// decideJob records an approval or a rejection of a gated job. Only the caller
// that the job is asked of may decide; with the admin key that is the admin
// (ADR 0023 B2 adds the project role check here).
func (c *controlService) decideJob(ctx context.Context, name, project, decision, comment string) error {
	if name == "" {
		return status.Error(codes.InvalidArgument, "job is required")
	}
	if len(comment) > maxApprovalComment {
		return status.Errorf(codes.InvalidArgument, "comment is at most %d bytes", maxApprovalComment)
	}
	rec, err := c.s.findObject(ctx, v1alpha1.KindJob, name, project)
	if err != nil {
		return apiError(err)
	}
	j, err := objects.Job(rec)
	if err != nil {
		return apiError(err)
	}
	if j.GetSpec().GetJob().GetEnvironment() == nil {
		return status.Errorf(codes.FailedPrecondition, "job %s has no environment, so there is no approval to give", name)
	}
	if phase := j.GetStatus().GetPhase(); phase != resource.PhaseManual {
		return status.Errorf(codes.FailedPrecondition, "job %s is %s: only a job waiting at an approval gate can be decided", name, phase)
	}
	if j.GetSpec().GetWhen() == pipeline.WhenManual && !j.GetSpec().GetPlayRequested() {
		return status.Errorf(codes.FailedPrecondition, "job %s waits to be played: play it before it is approved", name)
	}
	by := callerName(ctx)
	approvals := j.GetSpec().GetApprovals()
	if rej := resource.Rejection(approvals); rej != nil {
		return status.Errorf(codes.FailedPrecondition, "job %s was rejected by %s and cannot be decided again", name, rej.GetBy())
	}
	if decision == resource.DecisionApproved && slices.ContainsFunc(approvals, func(a *v1alpha1.JobApproval) bool {
		return a.GetBy() == by && a.GetDecision() == resource.DecisionApproved
	}) {
		return nil // this caller already approved it
	}
	j.Spec.Approvals = append(approvals, &v1alpha1.JobApproval{
		Decision: decision, By: by, At: timestamppb.New(c.s.now()), Comment: comment,
	})
	if err := c.updateSpec(ctx, v1alpha1.KindJob, rec.Name, j.Spec, rec.Generation); err != nil {
		return err
	}
	c.s.log.Info("pipeline job decided", "job", name, "decision", decision, "by", by)
	return nil
}

// callerName is the name of the authenticated caller, for the record of a
// decision.
func callerName(ctx context.Context) string {
	if c, ok := callerFrom(ctx); ok && c.id.Name != "" {
		return c.id.Name
	}
	return "unknown"
}

// CancelPipelineRun asks a run that has not finished to stop. The controller
// performs the stop.
func (c *controlService) CancelPipelineRun(ctx context.Context, req *v1alpha1.CancelPipelineRunRequest) (*v1alpha1.CancelPipelineRunResponse, error) {
	name := req.GetRun()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "run is required")
	}
	rec, err := c.s.findObject(ctx, v1alpha1.KindPipelineRun, name, req.GetProject())
	if err != nil {
		return nil, apiError(err)
	}
	r, err := objects.PipelineRun(rec)
	if err != nil {
		return nil, apiError(err)
	}
	if phase := r.GetStatus().GetPhase(); resource.PipelineRunTerminal(phase) {
		return nil, status.Errorf(codes.FailedPrecondition, "pipeline run %s is %s: it cannot be cancelled", name, phase)
	}
	if r.GetSpec().GetCancelRequested() {
		return &v1alpha1.CancelPipelineRunResponse{}, nil
	}
	r.Spec.CancelRequested = true
	if err := c.updateSpec(ctx, v1alpha1.KindPipelineRun, rec.Name, r.Spec, rec.Generation); err != nil {
		return nil, err
	}
	c.s.log.Info("pipeline run cancel requested", "run", name)
	return &v1alpha1.CancelPipelineRunResponse{}, nil
}

// PlayJob starts a manual job. The controller starts it once its stage is
// reached, as for any job.
func (c *controlService) PlayJob(ctx context.Context, req *v1alpha1.PlayJobRequest) (*v1alpha1.PlayJobResponse, error) {
	name := req.GetJob()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "job is required")
	}
	rec, err := c.s.findObject(ctx, v1alpha1.KindJob, name, req.GetProject())
	if err != nil {
		return nil, apiError(err)
	}
	j, err := objects.Job(rec)
	if err != nil {
		return nil, apiError(err)
	}
	if phase := j.GetStatus().GetPhase(); phase != resource.PhaseManual {
		return nil, status.Errorf(codes.FailedPrecondition, "job %s is %s: only a manual job can be played", name, phase)
	}
	if j.GetSpec().GetPlayRequested() {
		return &v1alpha1.PlayJobResponse{}, nil
	}
	j.Spec.PlayRequested = true
	if err := c.updateSpec(ctx, v1alpha1.KindJob, rec.Name, j.Spec, rec.Generation); err != nil {
		return nil, err
	}
	c.s.log.Info("pipeline job play requested", "job", name)
	return &v1alpha1.PlayJobResponse{}, nil
}

// updateSpec stores a new spec of a pipeline object, keeping its generation
// check.
func (c *controlService) updateSpec(ctx context.Context, kind, name string, spec proto.Message, generation int64) error {
	enc, err := objects.Encode(spec)
	if err != nil {
		return apiError(err)
	}
	if _, err := c.s.store.Update(ctx, kind, name, enc, generation); err != nil {
		return apiError(err)
	}
	return nil
}

// JobLogs returns the output of a job so far, oldest line first.
func (c *controlService) JobLogs(ctx context.Context, req *v1alpha1.JobLogsRequest) (*v1alpha1.JobLogsResponse, error) {
	name := req.GetJob()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "job is required")
	}
	rec, err := c.s.findObject(ctx, v1alpha1.KindJob, name, req.GetProject())
	if err != nil {
		return nil, apiError(err)
	}
	lines, err := c.s.ctrl.JobLogs(ctx, rec.Name)
	if err != nil {
		return nil, apiError(err)
	}
	out := &v1alpha1.JobLogsResponse{}
	for _, l := range lines {
		out.Lines = append(out.Lines, &v1alpha1.LogLine{Time: timestamppb.New(l.Time), Line: l.Text})
	}
	return out, nil
}

// checkPipelineDeletable refuses to delete a run or a job that has not
// finished: the runner would keep it going. Cancel it first.
func (s *Server) checkPipelineDeletable(ctx context.Context, kind, name string) error {
	rec, err := s.store.Get(ctx, kind, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil // the delete itself reports it
	}
	if err != nil {
		return apiError(err)
	}
	// A job that waits for play is not running, so it can be deleted.
	var phase string
	if kind == v1alpha1.KindPipelineRun {
		r, err := objects.PipelineRun(rec)
		if err != nil {
			return apiError(err)
		}
		if !resource.PipelineRunTerminal(r.GetStatus().GetPhase()) {
			phase = phaseOrNew(r.GetStatus().GetPhase())
		}
	} else {
		j, err := objects.Job(rec)
		if err != nil {
			return apiError(err)
		}
		if p := j.GetStatus().GetPhase(); !resource.JobTerminal(p) && p != resource.PhaseManual {
			phase = phaseOrNew(p)
		}
	}
	if phase != "" {
		return status.Errorf(codes.FailedPrecondition, "%s %s is %s: cancel it first", kind, name, phase)
	}
	return nil
}

// randomPipelineSuffix returns the random part of a run's name.
func randomPipelineSuffix() string {
	b := make([]byte, resource.PipelineRunSuffixLen)
	maxN := big.NewInt(int64(len(buildSuffixAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, maxN)
		if err != nil {
			panic(err) // crypto/rand never fails on supported platforms
		}
		b[i] = buildSuffixAlphabet[n.Int64()]
	}
	return string(b)
}

// phaseOrNew is the phase, or New when the controller has not set one yet.
func phaseOrNew(phase string) string {
	if phase == "" {
		return resource.PhaseNew
	}
	return phase
}
