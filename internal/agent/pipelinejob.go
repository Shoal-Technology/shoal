package agent

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/jobrun"
	"github.com/Shoal-Technology/shoal/internal/pipeline"
)

// Pipeline jobs on this node (ADR 0022, pass B). Every agent polls the server
// for a job: the server answers with none unless the node is labelled
// shoal.dev/runner=true and the job's tags match the node's labels. A runner
// runs one job at a time (ADR 0022 section 5 allows N; N=1 is pass B).
const (
	jobPollInterval = 5 * time.Second
	jobLogInterval  = 2 * time.Second
	// jobLogBatch bounds the lines in one ReportJob message.
	jobLogBatch = 500
	// jobReportTries is how often a final report is sent before giving up.
	jobReportTries = 5
)

// RunJobs runs the pipeline jobs the server gives this node, until ctx ends.
// It does nothing when git is not installed, because a clone needs it, or
// when the node has no container runtime.
func (c *Connected) RunJobs(ctx context.Context) {
	if _, err := exec.LookPath("git"); err != nil {
		c.log.Warn("jobs: git is not installed, so this node will not run pipeline jobs", "err", err)
		return
	}
	if c.cfg.Runtime == nil {
		c.log.Warn("jobs: this node has no container runtime, so it will not run pipeline jobs")
		return
	}
	t := time.NewTicker(jobPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		resp, err := c.claimJob(ctx)
		if err != nil {
			c.log.Debug("jobs: claim failed", "err", err)
			continue
		}
		if resp.GetJob() != "" {
			c.runJob(ctx, resp)
		}
	}
}

// claimJob asks the server for a job. It returns an empty response when there
// is none.
func (c *Connected) claimJob(ctx context.Context) (*v1alpha1.ClaimJobResponse, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	return v1alpha1.NewAgentClient(conn).ClaimJob(rctx, &v1alpha1.ClaimJobRequest{})
}

// runJob runs one claimed job, ships its log while it runs, and reports how it
// ended. Its directory is removed at the end, with any credentials in it.
func (c *Connected) runJob(ctx context.Context, claim *v1alpha1.ClaimJobResponse) {
	name := claim.GetJob()
	log := c.log.With("job", name, "run", claim.GetRun())
	dir := filepath.Join(filepath.Dir(c.cfg.CredentialsPath), "jobs", name)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		c.finishJob(ctx, name, jobrun.Outcome{Phase: jobrun.PhaseError, Reason: pipeline.ReasonRunnerError, Message: err.Error()})
		return
	}
	defer os.RemoveAll(dir)

	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	done := make(chan jobrun.Outcome, 1)
	go func() {
		done <- jobrun.Run(jobCtx, c.cfg.Runtime, jobrun.Job{
			Name:        name,
			Dir:         dir,
			Image:       claim.GetImage(),
			Script:      claim.GetScript(),
			Env:         claim.GetEnv(),
			Git:         claim.GetGit(),
			Credentials: claim.GetCredentials(),
			Registry:    claim.GetRegistryAddress(),
			Timeout:     time.Duration(claim.GetTimeoutSeconds()) * time.Second,
			CPUMillis:   claim.GetCpuMillis(),
			MemoryBytes: claim.GetMemoryBytes(),
			PidsLimit:   claim.GetPidsLimit(),
		})
	}()
	log.Info("job started", "image", claim.GetImage())

	tail := &logTail{path: jobrun.LogFile(dir)}
	secrets := redactions(claim.GetRedact(), claim.GetCredentials().GetPassword())
	ticker := time.NewTicker(jobLogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if c.shipJobLogs(ctx, name, tail, secrets) {
				cancelJob()
			}
		case out := <-done:
			if c.shipJobLogs(ctx, name, tail, secrets) {
				cancelJob()
			}
			c.finishJob(ctx, name, out)
			log.Info("job ended", "phase", out.Phase, "exit", out.ExitCode)
			return
		}
	}
}

// redactions returns the values that must not appear in a log line: each
// secret, and its base64 form, which a script may print when it encodes one.
func redactions(values []string, password string) []string {
	out := make([]string, 0, 2*(len(values)+1))
	for _, v := range append(append([]string(nil), values...), password) {
		if v == "" {
			continue
		}
		out = append(out, v, base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return out
}

// finishJob reports how a job ended. It is sent even when ctx has ended, so
// the server does not wait for the runner to come back.
func (c *Connected) finishJob(ctx context.Context, name string, out jobrun.Outcome) {
	req := &v1alpha1.ReportJobRequest{Job: name, Phase: out.Phase, Message: out.Message, ExitCode: int32(out.ExitCode), Reason: out.Reason}
	base := context.WithoutCancel(ctx)
	for i := 0; i < jobReportTries; i++ {
		_, err := c.reportJob(base, req)
		if err == nil {
			return
		}
		c.log.Warn("jobs: could not report the job result", "job", name, "try", i+1, "err", err)
		time.Sleep(2 * time.Second)
	}
}

// shipJobLogs sends the new log lines of a job. It returns true when the server
// has cancelled the job's run. Lines that could not be sent stay queued.
func (c *Connected) shipJobLogs(ctx context.Context, name string, tail *logTail, secrets []string) bool {
	tail.read(time.Now(), secrets)
	cancel := false
	for len(tail.pending) > 0 {
		n := min(jobLogBatch, len(tail.pending))
		resp, err := c.reportJob(ctx, &v1alpha1.ReportJobRequest{Job: name, Lines: tail.pending[:n]})
		if err != nil {
			c.log.Debug("jobs: log upload failed, will retry", "job", name, "err", err)
			break
		}
		tail.pending = tail.pending[n:]
		cancel = cancel || resp.GetCancel()
	}
	return cancel
}

// reportJob sends one ReportJob to the server.
func (c *Connected) reportJob(ctx context.Context, req *v1alpha1.ReportJobRequest) (*v1alpha1.ReportJobResponse, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	return v1alpha1.NewAgentClient(conn).ReportJob(rctx, req)
}
