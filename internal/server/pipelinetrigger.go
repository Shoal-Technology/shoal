package server

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/build/buildkit"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/pipeline"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

// Push triggers for pipelines (ADR 0022, pass B, section 4). A push that the
// BuildConfig's webhook receives starts a PipelineRun of every Pipeline linked
// to that push: the Pipeline's repository is the BuildConfig's, and its ref is
// the pushed branch (HEAD means the config's branch). The webhook's signature
// check has already passed when this runs. Tags do not start pipelines in pass B.

// pipelineTriggerTimeout bounds the read of the file and the run's start.
const pipelineTriggerTimeout = 5 * time.Minute

// maxPipelineFile bounds shoal-ci.yml. The parser refuses a larger file anyway.
const maxPipelineFile = 1 << 20

// startWebhookPipelines starts the runs of the Pipelines a push starts. It runs
// in the background, after the webhook has answered, and logs what it does.
func (s *Server) startWebhookPipelines(ctx context.Context, cfg *v1alpha1.BuildConfigSpec, ev resource.WebhookEvent) {
	branch, isBranch := strings.CutPrefix(ev.Ref, "refs/heads/")
	if !isBranch {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, pipelineTriggerTimeout)
	defer cancel()
	recs, err := s.store.List(ctx, v1alpha1.KindPipeline)
	if err != nil {
		s.log.Error("pipeline trigger: list pipelines", "err", apiError(err))
		return
	}
	configBranch := resource.WebhookBranch(cfg)
	for _, rec := range recs {
		p, err := objects.Pipeline(rec)
		if err != nil {
			s.log.Warn("pipeline trigger: skipping an undecodable pipeline", "pipeline", rec.Name, "err", err)
			continue
		}
		if !sameRepository(p.GetSpec().GetRepo(), cfg.GetGit().GetUrl()) {
			continue
		}
		ref := p.GetSpec().GetRef()
		if ref == "" || ref == resource.DefaultPipelineRef {
			ref = configBranch
		}
		if ref != branch {
			continue
		}
		s.startPipelinePush(ctx, rec.Name, p, branch, ev)
	}
}

// startPipelinePush reads the Pipeline's file at the pushed commit and starts a
// run of it. A run that already covers the commit is kept, not repeated.
func (s *Server) startPipelinePush(ctx context.Context, name string, p *v1alpha1.Pipeline, branch string, ev resource.WebhookEvent) {
	// name is the Pipeline's store key; the run is in its project (ADR 0023, pass B1).
	project, bare := tenancy.KeyProject(name), tenancy.KeyName(name)
	creds, err := gitCredentials(ctx, s.store, project, p.GetSpec().GetCredentialsSecret())
	if err != nil {
		s.log.Warn("pipeline trigger: credentials", "pipeline", name, "err", err)
		return
	}
	data, err := readPipelineFile(ctx, p.GetSpec().GetRepo(), creds, ev.Commit, p.GetSpec().GetPath())
	if err != nil {
		s.log.Warn("pipeline trigger: could not read the file", "pipeline", name, "commit", ev.Commit, "err", err)
		return
	}
	src, err := pipeline.Parse(data)
	if err != nil {
		s.log.Warn("pipeline trigger: the file is not valid, no run started", "pipeline", name, "commit", ev.Commit, "err", err)
		return
	}
	if err := s.checkPipelineSecrets(ctx, project, src); err != nil {
		s.log.Warn("pipeline trigger: the file names a Secret that is not usable, no run started", "pipeline", name, "err", err)
		return
	}
	trust := resource.TrustOfPush(branch, resource.ProtectedBranchesOf(p.GetSpec()))
	if err := s.checkPipelineEnvironments(ctx, src, trust); err != nil {
		s.log.Warn("pipeline trigger: the file names an environment that is not usable, no run started", "pipeline", name, "err", err)
		return
	}

	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	if run, ok, err := s.pipelineRunForCommit(ctx, name, ev.Commit); err != nil {
		s.log.Error("pipeline trigger: list runs", "pipeline", name, "err", err)
		return
	} else if ok {
		s.log.Info("pipeline trigger: commit already has a run", "pipeline", name, "run", run, "commit", ev.Commit)
		return
	}
	rname, err := s.createPipelineRun(ctx, project, bare, &v1alpha1.PipelineRunSpec{
		Pipeline:     bare,
		Source:       src,
		Trigger:      resource.TriggerPush,
		Commit:       ev.Commit,
		Ref:          branch,
		Trust:        trust,
		ChangedPaths: ev.ChangedPaths,
	})
	if err != nil {
		s.log.Error("pipeline trigger: run not created", "pipeline", name, "err", err)
		return
	}
	s.log.Info("pipeline run started by push", "pipeline", name, "run", rname, "commit", ev.Commit)
}

// pipelineRunForCommit returns a push run of the Pipeline at commit, if one
// exists.
func (s *Server) pipelineRunForCommit(ctx context.Context, pipelineName, commit string) (string, bool, error) {
	recs, err := s.store.List(ctx, v1alpha1.KindPipelineRun)
	if err != nil {
		return "", false, apiError(err)
	}
	for _, rec := range recs {
		r, err := objects.PipelineRun(rec)
		if err != nil {
			continue
		}
		sp := r.GetSpec()
		if tenancy.Key(tenancy.KeyProject(rec.Name), sp.GetPipeline()) == pipelineName && sp.GetCommit() == commit && sp.GetTrigger() == resource.TriggerPush {
			return rec.Name, true, nil
		}
	}
	return "", false, nil
}

// sameRepository reports whether two git URLs name one repository, ignoring a
// trailing slash, a trailing .git and the case of the URL.
func sameRepository(a, b string) bool {
	norm := func(u string) string {
		u = strings.TrimSuffix(strings.TrimSpace(u), "/")
		return strings.ToLower(strings.TrimSuffix(u, ".git"))
	}
	return a != "" && norm(a) == norm(b)
}

// readPipelineFile returns the file at path in repo at commit. It clones the
// repository with the Build clone path (credentials in the environment, never
// in argv), reads the file and removes the clone. A path that leaves the
// repository, or a symlink, is refused.
func readPipelineFile(ctx context.Context, repo string, creds *v1alpha1.BuildCredentials, commit, path string) ([]byte, error) {
	if path == "" {
		path = pipeline.FileName
	}
	clean := filepath.Clean(path)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path %q leaves the repository", path)
	}
	dir, err := os.MkdirTemp("", "shoal-pipeline-file-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	job := buildkit.Job{
		Dir:         dir,
		Source:      &v1alpha1.BuildConfigSpec{Git: &v1alpha1.GitSource{Url: repo, Ref: commit}},
		Credentials: creds,
	}
	src := filepath.Join(dir, "repo")
	if _, err := buildkit.CloneRepo(ctx, job, src, io.Discard); err != nil {
		return nil, fmt.Errorf("clone at %s: %w", commit, err)
	}
	full := filepath.Join(src, clean)
	info, err := os.Lstat(full)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", clean, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", clean)
	}
	if info.Size() > maxPipelineFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", clean, maxPipelineFile)
	}
	return os.ReadFile(full)
}
