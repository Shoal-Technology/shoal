package agent

import (
	"context"
	"crypto/x509"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/build/buildkit"
	"github.com/Shoal-Technology/shoal/internal/security"
)

// Builds on this node (ADR 0021, pass B). Every agent polls the server for a
// Build: the server answers with none unless the node is labelled
// shoal.dev/builder=true. A builder runs one Build at a time.
const (
	builderPollInterval = 5 * time.Second
	builderLogInterval  = 2 * time.Second
	// builderLogBatch bounds the lines in one ReportBuild message.
	builderLogBatch = 500
	// builderReportTries is how often a final report is sent before giving up.
	builderReportTries = 5
)

// RunBuilder runs the Builds the server gives this node, until ctx ends. It
// does nothing when git is not installed, because a clone needs it.
func (c *Connected) RunBuilder(ctx context.Context) {
	if _, err := exec.LookPath("git"); err != nil {
		c.log.Warn("builder: git is not installed, so this node will not run builds", "err", err)
		return
	}
	t := time.NewTicker(builderPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		resp, err := c.claimBuild(ctx)
		if err != nil {
			c.log.Debug("builder: claim failed", "err", err)
			continue
		}
		if resp != nil {
			c.runBuild(ctx, resp)
		}
	}
}

// claimBuild asks the server for a Build. It returns nil when there is none.
func (c *Connected) claimBuild(ctx context.Context) (*v1alpha1.ClaimBuildResponse, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := v1alpha1.NewAgentClient(conn).ClaimBuild(rctx, &v1alpha1.ClaimBuildRequest{})
	if err != nil {
		return nil, err
	}
	if resp.GetBuild() == "" {
		return nil, nil
	}
	return resp, nil
}

// runBuild runs one claimed Build, ships its log while it runs, and reports
// the result. The job directory is removed at the end, with its credentials.
func (c *Connected) runBuild(ctx context.Context, claim *v1alpha1.ClaimBuildResponse) {
	name := claim.GetBuild()
	log := c.log.With("build", name)
	dir := filepath.Join(filepath.Dir(c.cfg.CredentialsPath), "builds", name)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		c.finishBuild(ctx, name, buildkit.Result{}, err)
		return
	}
	defer os.RemoveAll(dir)

	certPEM, keyPEM, err := c.buildCertificate(ctx, name)
	if err != nil {
		c.finishBuild(ctx, name, buildkit.Result{}, err)
		return
	}
	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()

	done := make(chan buildOutcome, 1)
	go func() {
		res, err := buildkit.Run(jobCtx, c.cfg.Runtime, buildkit.Job{
			Build:         name,
			Source:        claim.GetSource(),
			Credentials:   claim.GetCredentials(),
			Registry:      claim.GetRegistryAddress(),
			Dir:           dir,
			ClientCertPEM: certPEM,
			ClientKeyPEM:  keyPEM,
			CAPEM:         security.EncodeCertPEM(c.caCert()),
		})
		done <- buildOutcome{res, err}
	}()
	log.Info("build started", "repository", claim.GetSource().GetOutput().GetRepository())

	tail := &logTail{path: buildkit.LogFile(dir)}
	secrets := []string{claim.GetCredentials().GetPassword()}
	ticker := time.NewTicker(builderLogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if c.shipLogs(ctx, name, tail, secrets) {
				cancelJob()
			}
		case out := <-done:
			if c.shipLogs(ctx, name, tail, secrets) {
				cancelJob()
			}
			c.finishBuild(ctx, name, out.res, out.err)
			log.Info("build ended", "err", out.err)
			return
		}
	}
}

type buildOutcome struct {
	res buildkit.Result
	err error
}

// finishBuild reports the result of a Build to the server. It is sent even
// when ctx has ended, so the server does not wait for the builder to come back.
func (c *Connected) finishBuild(ctx context.Context, name string, res buildkit.Result, runErr error) {
	req := &v1alpha1.ReportBuildRequest{Build: name, Phase: "Complete", Commit: res.Commit, Image: res.Image, Digest: res.Digest}
	if runErr != nil {
		req = &v1alpha1.ReportBuildRequest{Build: name, Phase: "Failed", Message: runErr.Error()}
	}
	base := context.WithoutCancel(ctx)
	for i := 0; i < builderReportTries; i++ {
		_, err := c.reportBuild(base, req)
		if err == nil {
			return
		}
		c.log.Warn("builder: could not report the build result", "build", name, "try", i+1, "err", err)
		time.Sleep(2 * time.Second)
	}
}

// shipLogs sends the new log lines of a Build. It returns true when the
// server has cancelled the Build. Lines that could not be sent stay queued.
func (c *Connected) shipLogs(ctx context.Context, name string, tail *logTail, secrets []string) bool {
	tail.read(time.Now(), secrets)
	cancel := false
	for len(tail.pending) > 0 {
		n := min(builderLogBatch, len(tail.pending))
		resp, err := c.reportBuild(ctx, &v1alpha1.ReportBuildRequest{Build: name, Lines: tail.pending[:n]})
		if err != nil {
			c.log.Debug("builder: log upload failed, will retry", "build", name, "err", err)
			break
		}
		tail.pending = tail.pending[n:]
		cancel = cancel || resp.GetCancel()
	}
	return cancel
}

// reportBuild sends one ReportBuild to the server.
func (c *Connected) reportBuild(ctx context.Context, req *v1alpha1.ReportBuildRequest) (*v1alpha1.ReportBuildResponse, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	return v1alpha1.NewAgentClient(conn).ReportBuild(rctx, req)
}

// buildCertificate asks the server for a push certificate for a Build this node
// has claimed. The key stays on the node; only the request leaves it.
func (c *Connected) buildCertificate(ctx context.Context, name string) (certPEM, keyPEM []byte, err error) {
	key, err := security.NewKey()
	if err != nil {
		return nil, nil, err
	}
	csr, err := security.NewCSR(key, name)
	if err != nil {
		return nil, nil, err
	}
	conn, err := c.dial()
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := v1alpha1.NewAgentClient(conn).IssueBuildCertificate(rctx, &v1alpha1.IssueBuildCertificateRequest{Build: name, Csr: csr})
	if err != nil {
		return nil, nil, err
	}
	cert, err := checkIssued(resp.GetCertificate(), c.caCert(), key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = security.EncodeKeyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	return security.EncodeCertPEM(cert), keyPEM, nil
}

// caCert returns the cluster CA the node trusts.
func (c *Connected) caCert() *x509.Certificate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ca
}

// logTail reads a build log as it grows. Complete lines wait in pending until
// they are sent.
type logTail struct {
	path    string
	off     int64
	partial string
	pending []*v1alpha1.LogLine
}

// read appends the lines written since the last call to pending, with secrets
// redacted. A line still being written waits for its newline.
func (t *logTail) read(now time.Time, secrets []string) {
	f, err := os.Open(t.path)
	if err != nil {
		return // the log does not exist yet
	}
	defer f.Close()
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return
	}
	t.off += int64(len(b))
	parts := strings.Split(t.partial+string(b), "\n")
	t.partial = parts[len(parts)-1]
	for _, line := range parts[:len(parts)-1] {
		line = strings.TrimSuffix(line, "\r")
		for _, s := range secrets {
			if s != "" {
				line = strings.ReplaceAll(line, s, "[redacted]")
			}
		}
		t.pending = append(t.pending, &v1alpha1.LogLine{Time: timestamppb.New(now), Stream: "stdout", Line: line})
	}
}
