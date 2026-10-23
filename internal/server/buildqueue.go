package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/build"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

// finishedJobKeep is how long a finished job stays in memory. The controller
// reads a result within a pass or two. The logs stay on disk.
const finishedJobKeep = time.Hour

// buildDispatch is the server's build executor (ADR 0021, pass B). The
// controller starts, polls and cancels Builds through it (build.Executor).
// Builder nodes claim the queued Builds over the Agent service, report their
// logs and results, and fetch their push certificates (agentbuild.go).
//
// The queue and the job states are in memory. A server restart loses them,
// and the controller then finds the Build unknown and sets it to Error, which
// ADR 0021 allows for a lost job. Logs are kept under DataDir/builds.
type buildDispatch struct {
	st       store.Store
	log      *slog.Logger
	dir      string
	now      func() time.Time
	registry string // address builders push to; empty when the registry is off

	mu    sync.Mutex
	jobs  map[string]*dispatchJob
	queue []string // names of queued jobs, oldest first
}

type jobState int

const (
	jobQueued   jobState = iota // waiting for a builder
	jobClaimed                  // a builder runs it
	jobFinished                 // result is in dispatchJob.result
)

type dispatchJob struct {
	name   string
	source *v1alpha1.BuildConfigSpec
	state  jobState
	node   string // the builder that claimed it
	cancel bool   // set by Cancel while the job runs; the builder learns it on its next report
	result build.Status
	endAt  time.Time
}

func newBuildDispatch(st store.Store, log *slog.Logger, dataDir string, now func() time.Time, registry string) *buildDispatch {
	return &buildDispatch{
		st:       st,
		log:      log,
		dir:      filepath.Join(dataDir, "builds"),
		now:      now,
		registry: registry,
		jobs:     map[string]*dispatchJob{},
	}
}

// Start queues a Build for a builder node. It refuses when the registry is off
// (a builder can only push there) or when no node is a builder, since the
// Build would wait forever.
func (d *buildDispatch) Start(ctx context.Context, job build.Job) error {
	if d.registry == "" {
		return errors.New("the embedded registry is off: start the server with --registry-listen")
	}
	ok, err := d.hasBuilder(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no builder node: label a node shoal.dev/builder=true")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.jobs[job.Name]; ok {
		return nil
	}
	d.prune()
	d.jobs[job.Name] = &dispatchJob{name: job.Name, source: job.Source, state: jobQueued}
	d.queue = append(d.queue, job.Name)
	return nil
}

// Status reports a job. Queued and claimed jobs are Running.
func (d *buildDispatch) Status(_ context.Context, name string) (build.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, ok := d.jobs[name]
	if !ok {
		return build.Status{}, build.ErrUnknownJob
	}
	if j.state == jobFinished {
		return j.result, nil
	}
	return build.Status{State: build.Running}, nil
}

// Cancel stops a queued job at once, and asks a claimed job's builder to stop
// on its next report. A finished job is left alone.
func (d *buildDispatch) Cancel(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, ok := d.jobs[name]
	if !ok {
		return build.ErrUnknownJob
	}
	switch j.state {
	case jobQueued:
		d.dequeue(name)
		d.finish(j, build.Status{State: build.Failed, Message: "cancelled"})
	case jobClaimed:
		j.cancel = true
	}
	return nil
}

// Logs returns the lines of a Build. The file outlives the job's memory, so
// a finished Build's logs are still served until the file is removed.
func (d *buildDispatch) Logs(_ context.Context, name string) ([]build.Line, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, known := d.jobs[name]
	data, err := os.ReadFile(d.logPath(name))
	if errors.Is(err, os.ErrNotExist) {
		if !known {
			return nil, build.ErrUnknownJob
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read build logs: %w", err)
	}
	var out []build.Line
	for _, l := range strings.Split(string(data), "\n") {
		t, text, ok := strings.Cut(l, "\t")
		if !ok {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, t)
		if err != nil {
			continue
		}
		out = append(out, build.Line{Time: ts, Text: text})
	}
	return out, nil
}

// scope returns the repository a Build pushes to, and whether it is claimed
// (running). The registry grants a builder certificate only while it is.
func (d *buildDispatch) scope(_ context.Context, name string) (string, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, ok := d.jobs[name]
	if !ok {
		return "", false, nil
	}
	return j.source.GetOutput().GetRepository(), j.state == jobClaimed, nil
}

// scopeFor returns the repository of a Build claimed by node. It is what the
// builder's push certificate may use.
func (d *buildDispatch) scopeFor(name, node string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, ok := d.jobs[name]
	if !ok || j.state != jobClaimed || j.node != node {
		return "", status.Error(codes.NotFound, "build is not claimed by this node")
	}
	return j.source.GetOutput().GetRepository(), nil
}

// claim hands the oldest queued job to node, if node is a builder.
func (d *buildDispatch) claim(ctx context.Context, node string) (*v1alpha1.ClaimBuildResponse, error) {
	n, err := d.builderNode(ctx, node)
	if err != nil {
		return nil, apiError(err)
	}
	if !resource.IsBuilder(n.GetSpec().GetLabels()) {
		return &v1alpha1.ClaimBuildResponse{}, nil
	}
	d.mu.Lock()
	var j *dispatchJob
	for j == nil && len(d.queue) > 0 {
		name := d.queue[0]
		d.queue = d.queue[1:]
		if cand := d.jobs[name]; cand != nil && cand.state == jobQueued {
			j = cand
		}
	}
	if j == nil {
		d.mu.Unlock()
		return &v1alpha1.ClaimBuildResponse{}, nil
	}
	j.state, j.node = jobClaimed, node
	name, src := j.name, j.source
	d.mu.Unlock()

	creds, err := d.credentials(ctx, tenancy.KeyProject(name), src.GetCredentialsSecret())
	if err != nil {
		d.mu.Lock()
		if j.state == jobClaimed && j.node == node {
			d.finish(j, build.Status{State: build.Failed, Message: "could not read the credentials: " + err.Error()})
		}
		d.mu.Unlock()
		d.log.Warn("build failed before it started", "build", name, "err", err)
		return &v1alpha1.ClaimBuildResponse{}, nil
	}
	d.log.Info("build claimed", "build", name, "node", node)
	return &v1alpha1.ClaimBuildResponse{Build: name, Source: src, Credentials: creds, RegistryAddress: d.registry}, nil
}

// report stores a builder's log lines and, with a terminal phase, the result.
// It returns whether the Build has been cancelled.
func (d *buildDispatch) report(node string, req *v1alpha1.ReportBuildRequest) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, ok := d.jobs[req.GetBuild()]
	if !ok || j.state != jobClaimed || j.node != node {
		return false, status.Error(codes.NotFound, "build is not claimed by this node")
	}
	if err := d.appendLines(j.name, req.GetLines()); err != nil {
		return false, status.Error(codes.Internal, err.Error())
	}
	switch req.GetPhase() {
	case "":
	case "Complete":
		if req.GetCommit() == "" || req.GetImage() == "" || req.GetDigest() == "" {
			d.finish(j, build.Status{State: build.Failed, Message: "builder reported success without a commit, image and digest"})
			break
		}
		d.finish(j, build.Status{State: build.Succeeded, Commit: req.GetCommit(), Image: req.GetImage(), Digest: req.GetDigest()})
	case "Failed", "Error":
		d.finish(j, build.Status{State: build.Failed, Message: req.GetMessage()})
	default:
		return false, status.Errorf(codes.InvalidArgument, "unknown build phase %q", req.GetPhase())
	}
	return j.cancel, nil
}

// credentials reads the git credentials of a Build from its Secret. The
// values are returned to the builder only.
func (d *buildDispatch) credentials(ctx context.Context, project, secret string) (*v1alpha1.BuildCredentials, error) {
	if secret == "" {
		return &v1alpha1.BuildCredentials{}, nil
	}
	rec, err := d.st.Get(ctx, v1alpha1.KindSecret, tenancy.Key(project, secret))
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
func (d *buildDispatch) finish(j *dispatchJob, st build.Status) {
	j.state = jobFinished
	j.result = st
	j.endAt = d.now()
	if st.State == build.Failed {
		d.log.Info("build finished", "build", j.name, "message", st.Message)
	} else {
		d.log.Info("build finished", "build", j.name, "image", st.Image, "digest", st.Digest)
	}
}

// dequeue removes name from the queue. d.mu must be held.
func (d *buildDispatch) dequeue(name string) {
	for i, n := range d.queue {
		if n == name {
			d.queue = append(d.queue[:i], d.queue[i+1:]...)
			return
		}
	}
}

// prune drops finished jobs that are past finishedJobKeep. d.mu must be held.
func (d *buildDispatch) prune() {
	now := d.now()
	for name, j := range d.jobs {
		if j.state == jobFinished && now.Sub(j.endAt) > finishedJobKeep {
			delete(d.jobs, name)
		}
	}
}

// appendLines writes lines to the Build's log file, one per line as
// "<RFC 3339 time>\t<text>". d.mu must be held.
func (d *buildDispatch) appendLines(name string, lines []*v1alpha1.LogLine) error {
	if len(lines) == 0 {
		return nil
	}
	if err := os.MkdirAll(d.dir, 0o700); err != nil {
		return err
	}
	var b strings.Builder
	for _, l := range lines {
		ts := d.now()
		if t := l.GetTime(); t != nil {
			ts = t.AsTime()
		}
		text := strings.NewReplacer("\r", " ", "\n", " ").Replace(l.GetLine())
		b.WriteString(ts.UTC().Format(time.RFC3339Nano))
		b.WriteByte('\t')
		b.WriteString(text)
		b.WriteByte('\n')
	}
	f, err := os.OpenFile(d.logPath(name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (d *buildDispatch) logPath(name string) string {
	return filepath.Join(d.dir, filepath.Base(name)+".log")
}

// hasBuilder reports whether any node is a builder.
func (d *buildDispatch) hasBuilder(ctx context.Context) (bool, error) {
	recs, err := d.st.List(ctx, v1alpha1.KindNode)
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		n, err := objects.Node(r)
		if err != nil {
			return false, err
		}
		if resource.IsBuilder(n.GetSpec().GetLabels()) {
			return true, nil
		}
	}
	return false, nil
}

// builderNode reads a Node object.
func (d *buildDispatch) builderNode(ctx context.Context, node string) (*v1alpha1.Node, error) {
	rec, err := d.st.Get(ctx, v1alpha1.KindNode, node)
	if err != nil {
		return nil, err
	}
	return objects.Node(rec)
}
