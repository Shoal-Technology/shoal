package server

import (
	"context"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// Pipeline job RPCs (ADR 0022, pass B). A runner node calls them with its node
// certificate. Each one acts only on a job that this node has claimed.

// ClaimJob hands the next queued pipeline job to this node when it is a runner
// and the job's tags match the node's labels.
func (a *agentService) ClaimJob(ctx context.Context, _ *v1alpha1.ClaimJobRequest) (*v1alpha1.ClaimJobResponse, error) {
	c, err := a.node(ctx)
	if err != nil {
		return nil, err
	}
	return a.s.pipes.claim(ctx, c.id.Name)
}

// ReportJob stores the log lines and, with a phase, the result of a claimed job.
func (a *agentService) ReportJob(ctx context.Context, req *v1alpha1.ReportJobRequest) (*v1alpha1.ReportJobResponse, error) {
	c, err := a.node(ctx)
	if err != nil {
		return nil, err
	}
	cancel, err := a.s.pipes.report(c.id.Name, req)
	if err != nil {
		return nil, err
	}
	return &v1alpha1.ReportJobResponse{Cancel: cancel}, nil
}
