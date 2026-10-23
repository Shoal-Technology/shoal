package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/security"
)

// Builder RPCs (ADR 0021, pass B). A builder node calls them with its node
// certificate. Each one acts only on a Build that this node has claimed.

// ClaimBuild hands the next queued Build to this node when it is a builder.
func (a *agentService) ClaimBuild(ctx context.Context, _ *v1alpha1.ClaimBuildRequest) (*v1alpha1.ClaimBuildResponse, error) {
	c, err := a.node(ctx)
	if err != nil {
		return nil, err
	}
	return a.s.builds.claim(ctx, c.id.Name)
}

// ReportBuild stores the logs and, with a phase, the result of a claimed Build.
func (a *agentService) ReportBuild(ctx context.Context, req *v1alpha1.ReportBuildRequest) (*v1alpha1.ReportBuildResponse, error) {
	c, err := a.node(ctx)
	if err != nil {
		return nil, err
	}
	cancel, err := a.s.builds.report(c.id.Name, req)
	if err != nil {
		return nil, err
	}
	return &v1alpha1.ReportBuildResponse{Cancel: cancel}, nil
}

// IssueBuildCertificate signs a builder's push certificate for a Build it has
// claimed. The certificate carries the Build's repository as its scope, and the
// registry grants it only while the Build runs (registry.go, principal).
func (a *agentService) IssueBuildCertificate(ctx context.Context, req *v1alpha1.IssueBuildCertificateRequest) (*v1alpha1.IssueBuildCertificateResponse, error) {
	c, err := a.node(ctx)
	if err != nil {
		return nil, err
	}
	repo, err := a.s.builds.scopeFor(req.GetBuild(), c.id.Name)
	if err != nil {
		return nil, err
	}
	cert, err := a.s.ca.SignBuilderCSR(req.GetCsr(), security.Identity{
		Name: req.GetBuild(), Role: security.RoleBuilder, Scope: repo,
	}, a.s.now(), security.BuildCertificateValidity)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	a.s.log.Info("builder certificate issued", "build", req.GetBuild(), "node", c.id.Name, "repository", repo)
	return &v1alpha1.IssueBuildCertificateResponse{Certificate: cert.Raw}, nil
}
