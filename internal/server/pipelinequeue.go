package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/pipeline"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/store"
)

// pipelineDispatch is the server's runner for pipeline jobs (ADR 0022, pass B).
// The controller starts, polls and cancels jobs through it (pipeline.Runner).
// Runner nodes claim the queued jobs over the Agent service, report their logs
// and results, and never see a secret that the run's trust does not allow.
//
// A job with a build step does not run on a node: it starts a Build through the
// normal Build path and reports that Build's state (ADR 0022, section 4).
//
// The queue and the job states are in memory. A restart loses them, and the
// controller then finds each job unknown and sets it to Error with the reason
// runner_error, which retry.when may retry (ADR 0022, section 5). Logs are kept
// under DataDir/jobs.
type pipelineDispatch struct {
	st       store.Store
	log      *slog.Logger
	dir      string
	now      func() time.Time
	registry string  // address of the embedded registry, for the job's egress rules
	srv      *Server // starts the Builds of build steps; set once the server exists

	mu    sync.Mutex
	jobs  map[string]*pipeJob
	queue []string // names of queued jobs, oldest first
}

// pipeJob is one job the dispatch knows about.
type pipeJob struct {
	name   string
	run    string
	job    *v1alpha1.PipelineJob
	env    map[string]string // the resolved variables of the job, without secrets
	state  jobState          // jobQueued, jobClaimed (by a node or a build) or jobFinished
	node   string            // the runner that claimed it; empty for a build step
	build  string            // the Build a build step started
	cancel bool              // set by Cancel while a runner runs it
	result pipeline.Status
	endAt  time.Time
}

func newPipelineDispatch(st store.Store, log *slog.Logger, dataDir string, now func() time.Time, registry string) *pipelineDispatch {
	return &pipelineDispatch{
		st:       st,
		log:      log,
		dir:      filepath.Join(dataDir, "jobs"),
		now:      now,
		registry: registry,
		jobs:     map[string]*pipeJob{},
	}
}

// Start queues a job for a runner, or starts the Build of a build step. It
// refuses when no runner node can take the job, since the job would wait
// forever. A job that ended earlier under the same name is replaced (a retry).
func (d *pipelineDispatch) Start(ctx context.Context, req pipeline.Request) error {
	d.mu.Lock()
	if j, ok := d.jobs[req.Name]; ok {
		state := j.state
		d.mu.Unlock()
		if state == jobFinished {
			d.forget(req.Name)
		} else {
			return errors.New("job already running")
		}
	} else {
		d.mu.Unlock()
	}
	if req.Job.GetBuild() != "" {
		return d.startBuildStep(ctx, req)
	}
	ok, err := d.hasRunner(ctx, req.Job.GetTags())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no runner node for tags %v: label a node shoal.dev/runner=true", req.Job.GetTags())
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune()
	d.jobs[req.Name] = &pipeJob{name: req.Name, run: req.Run, job: req.Job, env: req.Env, state: jobQueued}
	d.queue = append(d.queue, req.Name)
	return nil
}

// forget drops a finished job so that its name can start again.
func (d *pipelineDispatch) forget(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.jobs, name)
	d.dequeue(name)
}

// Status reports a job. Queued and claimed jobs are Running.
func (d *pipelineDispatch) Status(ctx context.Context, name string) (pipeline.Status, error) {
	d.mu.Lock()
	j, ok := d.jobs[name]
	if !ok {
		d.mu.Unlock()
		return pipeline.Status{}, pipeline.ErrUnknownJob
	}
	if j.state == jobFinished {
		d.mu.Unlock()
		return j.result, nil
	}
	buildName := j.build
	d.mu.Unlock()
	if buildName != "" {
		return d.buildStatus(ctx, buildName)
	}
	return pipeline.Status{State: pipeline.Running}, nil
}

// Cancel stops a queued job at once, and asks a claimed job's runner to stop on
// its next report. A build step cancels its Build. A finished job is left alone.
func (d *pipelineDispatch) Cancel(ctx context.Context, name string) error {
	d.mu.Lock()
	j, ok := d.jobs[name]
	if !ok {
		d.mu.Unlock()
		return pipeline.ErrUnknownJob
	}
	if j.build != "" {
		buildName, finished := j.build, j.state == jobFinished
		d.mu.Unlock()
		if finished {
			return nil
		}
		_, err := (&controlService{s: d.srv}).CancelBuild(ctx, &v1alpha1.CancelBuildRequest{Build: buildName})
		if status.Code(err) == codes.FailedPrecondition {
			return nil // the Build already ended
		}
		return err
	}
	defer d.mu.Unlock()
	switch j.state {
	case jobQueued:
		d.dequeue(name)
		d.finish(j, pipeline.Status{State: pipeline.Failed, Message: "cancelled"})
	case jobClaimed:
		j.cancel = true
	}
	return nil
}

// Logs returns the lines of a job. A build step's lines are its Build's.
func (d *pipelineDispatch) Logs(ctx context.Context, name string) ([]pipeline.Line, error) {
	d.mu.Lock()
	j, known := d.jobs[name]
	buildName := ""
	if known {
		buildName = j.build
	}
	d.mu.Unlock()
	if buildName != "" {
		lines, err := d.srv.builds.Logs(ctx, buildName)
		if err != nil {
			return nil, err
		}
		out := make([]pipeline.Line, 0, len(lines))
		for _, l := range lines {
			out = append(out, pipeline.Line{Time: l.Time, Text: l.Text})
		}
		return out, nil
	}
	data, err := os.ReadFile(d.logPath(name))
	if errors.Is(err, os.ErrNotExist) {
		if !known {
			return nil, pipeline.ErrUnknownJob
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read job logs: %w", err)
	}
	var out []pipeline.Line
	for _, l := range strings.Split(string(data), "\n") {
		t, text, ok := strings.Cut(l, "\t")
		if !ok {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, t)
		if err != nil {
			continue
		}
		out = append(out, pipeline.Line{Time: ts, Text: text})
	}
	return out, nil
}

// claim hands the oldest queued job that matches node's labels to node, when
// node is a runner. A job the server refuses to run is finished as Failed and
// the next one is tried.
func (d *pipelineDispatch) claim(ctx context.Context, node string) (*v1alpha1.ClaimJobResponse, error) {
	rec, err := d.st.Get(ctx, v1alpha1.KindNode, node)
	if err != nil {
		return nil, apiError(err)
	}
	n, err := objects.Node(rec)
	if err != nil {
		return nil, apiError(err)
	}
	labels := n.GetSpec().GetLabels()
	if !resource.IsRunner(labels) {
		return &v1alpha1.ClaimJobResponse{}, nil
	}
	for {
		j := d.takeQueued(node, labels)
		if j == nil {
			return &v1alpha1.ClaimJobResponse{}, nil
		}
		resp, err := d.prepare(ctx, j)
		if err != nil {
			d.mu.Lock()
			if j.state == jobClaimed && j.node == node {
				d.finish(j, pipeline.Status{State: pipeline.Failed, Message: err.Error()})
			}
			d.mu.Unlock()
			d.log.Warn("pipeline job failed before it started", "job", j.name, "err", err)
			continue
		}
		d.log.Info("pipeline job claimed", "job", j.name, "node", node)
		return resp, nil
	}
}

// takeQueued removes the oldest queued job that node may run from the queue,
// and marks it claimed by node.
func (d *pipelineDispatch) takeQueued(node string, labels map[string]string) *pipeJob {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, name := range d.queue {
		j := d.jobs[name]
		if j == nil || j.state != jobQueued {
			continue
		}
		if !resource.NodeMatchesTags(labels, j.job.GetTags()) {
			continue
		}
		d.queue = slices.Delete(d.queue, i, i+1)
		j.state, j.node = jobClaimed, node
		return j
	}
	return nil
}

// prepare builds what a runner needs to run a claimed job. It reads the
// secrets of the job, but only when the run's trust allows them (ADR 0022,
// section 7). A refusal is an error and the job is not run.
func (d *pipelineDispatch) prepare(ctx context.Context, j *pipeJob) (*v1alpha1.ClaimJobResponse, error) {
	runRec, err := d.st.Get(ctx, v1alpha1.KindPipelineRun, j.run)
	if err != nil {
		return nil, fmt.Errorf("pipeline run %s: %w", j.run, err)
	}
	run, err := objects.PipelineRun(runRec)
	if err != nil {
		return nil, err
	}
	pipeRec, err := d.st.Get(ctx, v1alpha1.KindPipeline, run.GetSpec().GetPipeline())
	if err != nil {
		return nil, fmt.Errorf("pipeline %s: %w", run.GetSpec().GetPipeline(), err)
	}
	pipe, err := objects.Pipeline(pipeRec)
	if err != nil {
		return nil, err
	}
	env := maps.Clone(j.env)
	if env == nil {
		env = map[string]string{}
	}
	var redact []string
	names := slices.Sorted(maps.Keys(j.job.GetSecrets()))
	if len(names) > 0 && run.GetSpec().GetTrust() != resource.TrustProtected {
		return nil, fmt.Errorf("secrets are not given to a %s run (ADR 0022, section 7)", run.GetSpec().GetTrust())
	}
	for _, envName := range names {
		ref := j.job.GetSecrets()[envName]
		rec, err := d.st.Get(ctx, v1alpha1.KindSecret, ref.GetSecret())
		if err != nil {
			return nil, fmt.Errorf("secret %q: %w", ref.GetSecret(), err)
		}
		sec, err := objects.Secret(rec)
		if err != nil {
			return nil, err
		}
		v, ok := sec.GetSpec().GetData()[ref.GetKey()]
		if !ok {
			return nil, fmt.Errorf("secret %q has no key %q", ref.GetSecret(), ref.GetKey())
		}
		env[envName] = v
		redact = append(redact, v)
	}
	creds, err := gitCredentials(ctx, d.st, pipe.GetSpec().GetCredentialsSecret())
	if err != nil {
		return nil, err
	}
	timeout := int64(resource.DefaultJobTimeoutSeconds)
	if j.job.GetTimeout() != nil {
		timeout = int64(j.job.GetTimeout().AsDuration().Seconds())
	}
	timeout = min(max(timeout, 1), resource.MaxJobTimeoutSeconds)
	return &v1alpha1.ClaimJobResponse{
		Job:             j.name,
		Run:             j.run,
		Image:           j.job.GetImage(),
		Script:          j.job.GetScript(),
		Env:             env,
		TimeoutSeconds:  timeout,
		Git:             &v1alpha1.GitSource{Url: pipe.GetSpec().GetRepo(), Ref: run.GetSpec().GetCommit()},
		Credentials:     creds,
		RegistryAddress: d.registry,
		Redact:          redact,
		CpuMillis:       resource.DefaultJobCPUMillis,
		MemoryBytes:     resource.DefaultJobMemoryBytes,
		PidsLimit:       resource.DefaultJobPids,
	}, nil
}

// report stores a runner's log lines and, with a phase, the result of a claimed
// job. It returns whether the job has been cancelled.
func (d *pipelineDispatch) report(node string, req *v1alpha1.ReportJobRequest) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, ok := d.jobs[req.GetJob()]
	if !ok || j.state != jobClaimed || j.node != node {
		return false, status.Error(codes.NotFound, "job is not claimed by this node")
	}
	if err := d.appendLines(j.name, req.GetLines()); err != nil {
		return false, status.Error(codes.Internal, err.Error())
	}
	switch req.GetPhase() {
	case "":
	case "Complete":
		d.finish(j, pipeline.Status{State: pipeline.Succeeded, Node: node})
	case "Failed":
		d.finish(j, pipeline.Status{State: pipeline.Failed, ExitCode: int(req.GetExitCode()), Message: req.GetMessage(), Node: node})
	case "Error":
		reason := req.GetReason()
		if reason == "" {
			reason = pipeline.ReasonRunnerError
		}
		d.finish(j, pipeline.Status{State: pipeline.Error, Reason: reason, Message: req.GetMessage(), Node: node})
	default:
		return false, status.Errorf(codes.InvalidArgument, "unknown job phase %q", req.GetPhase())
	}
	return j.cancel, nil
}

// startBuildStep starts the Build a build step names, pinned to the run's
// commit, and tracks it. The Build runs through the normal Build path, so the
// same builder node and registry checks apply (ADR 0021).
func (d *pipelineDispatch) startBuildStep(ctx context.Context, req pipeline.Request) error {
	runRec, err := d.st.Get(ctx, v1alpha1.KindPipelineRun, req.Run)
	if err != nil {
		return fmt.Errorf("pipeline run %s: %w", req.Run, err)
	}
	run, err := objects.PipelineRun(runRec)
	if err != nil {
		return err
	}
	cfgRec, err := d.st.Get(ctx, v1alpha1.KindBuildConfig, req.Job.GetBuild())
	if err != nil {
		return fmt.Errorf("build step %s: BuildConfig %q: %w", req.Job.GetName(), req.Job.GetBuild(), err)
	}
	cfg, err := objects.BuildConfig(cfgRec)
	if err != nil {
		return err
	}
	src := proto.CloneOf(cfg.GetSpec())
	if src.Git == nil {
		return fmt.Errorf("BuildConfig %q has no git source", req.Job.GetBuild())
	}
	src.Git.Ref = run.GetSpec().GetCommit()
	bname, err := d.srv.createBuild(ctx, req.Job.GetBuild(), src, resource.BuildTriggerPipeline)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.jobs[req.Name] = &pipeJob{name: req.Name, run: req.Run, job: req.Job, env: req.Env, state: jobClaimed, build: bname}
	d.log.Info("pipeline build step started", "job", req.Name, "build", bname)
	return nil
}

// buildStatus maps a Build's phase to a job state.
func (d *pipelineDispatch) buildStatus(ctx context.Context, name string) (pipeline.Status, error) {
	rec, err := d.st.Get(ctx, v1alpha1.KindBuild, name)
	if errors.Is(err, store.ErrNotFound) {
		return pipeline.Status{State: pipeline.Failed, Message: "the build of this step was deleted"}, nil
	}
	if err != nil {
		return pipeline.Status{}, apiError(err)
	}
	b, err := objects.Build(rec)
	if err != nil {
		return pipeline.Status{}, err
	}
	switch phase := b.GetStatus().GetPhase(); phase {
	case resource.BuildComplete:
		return pipeline.Status{State: pipeline.Succeeded}, nil
	case resource.BuildFailed, resource.BuildError, resource.BuildCancelled:
		return pipeline.Status{State: pipeline.Failed, Message: fmt.Sprintf("build %s %s: %s", name, phase, b.GetStatus().GetMessage())}, nil
	}
	return pipeline.Status{State: pipeline.Running}, nil
}

// hasRunner reports whether a runner node exists that may run a job with tags.
func (d *pipelineDispatch) hasRunner(ctx context.Context, tags []string) (bool, error) {
	recs, err := d.st.List(ctx, v1alpha1.KindNode)
	if err != nil {
		return false, apiError(err)
	}
	for _, r := range recs {
		n, err := objects.Node(r)
		if err != nil {
			continue
		}
		labels := n.GetSpec().GetLabels()
		if resource.IsRunner(labels) && resource.NodeMatchesTags(labels, tags) {
			return true, nil
		}
	}
	return false, nil
}

// gitCredentials reads the git credentials of a pipeline from its Secret. The
// values go to the runner only. An empty secret name means a public repository.
func gitCredentials(ctx context.Context, st store.Store, secret string) (*v1alpha1.BuildCredentials, error) {
	if secret == "" {
		return &v1alpha1.BuildCredentials{}, nil
	}
	rec, err := st.Get(ctx, v1alpha1.KindSecret, secret)
	if err != nil {
		return nil, fmt.Errorf("secret %q: %w", secret, err)
	}
	sec, err := objects.Secret(rec)
	if err != nil {
		return nil, err
	}
	data := sec.GetSpec().GetData()
	return &v1alpha1.BuildCredentials{
		Type:          sec.GetSpec().GetType(),
		Username:      data["username"],
		Password:      data["password"],
		SshPrivateKey: data[resource.SSHPrivateKeyKey],
		KnownHosts:    data[resource.KnownHostsKey],
	}, nil
}

// finish records the result of a job. d.mu must be held.
func (d *pipelineDispatch) finish(j *pipeJob, st pipeline.Status) {
	j.state = jobFinished
	j.result = st
	j.endAt = d.now()
	d.log.Info("pipeline job finished", "job", j.name, "state", st.State, "message", st.Message)
}

// dequeue removes name from the queue. d.mu must be held.
func (d *pipelineDispatch) dequeue(name string) {
	d.queue = slices.DeleteFunc(d.queue, func(n string) bool { return n == name })
}

// prune forgets finished jobs older than finishedJobKeep. d.mu must be held.
func (d *pipelineDispatch) prune() {
	cutoff := d.now().Add(-finishedJobKeep)
	for name, j := range d.jobs {
		if j.state == jobFinished && j.endAt.Before(cutoff) {
			delete(d.jobs, name)
		}
	}
}

// appendLines writes log lines to the job's log file. d.mu must be held.
func (d *pipelineDispatch) appendLines(name string, lines []*v1alpha1.LogLine) error {
	if len(lines) == 0 {
		return nil
	}
	if err := os.MkdirAll(d.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(d.logPath(name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, l := range lines {
		ts := l.GetTime().AsTime()
		if l.GetTime() == nil {
			ts = d.now()
		}
		text := strings.ReplaceAll(l.GetLine(), "\n", " ")
		if _, err := fmt.Fprintf(f, "%s\t%s\n", ts.Format(time.RFC3339Nano), text); err != nil {
			return err
		}
	}
	return nil
}

func (d *pipelineDispatch) logPath(name string) string {
	return filepath.Join(d.dir, name+".log")
}
