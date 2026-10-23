package server

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

// Builds (ADR 0021, pass A). A Build is created here from a BuildConfig; the
// controller runs it and owns its status.

// buildSuffixAlphabet has no vowels, as the names of replicas do.
const buildSuffixAlphabet = "bcdfghjklmnpqrstvwxz2456789"

// StartBuild creates a Build of a BuildConfig. The Build copies the config's
// spec, so a later edit of the config does not change the run.
func (c *controlService) StartBuild(ctx context.Context, req *v1alpha1.StartBuildRequest) (*v1alpha1.StartBuildResponse, error) {
	name := req.GetConfig()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "config is required")
	}
	rec, err := c.s.store.Get(ctx, v1alpha1.KindBuildConfig, name)
	if err != nil {
		return nil, apiError(err)
	}
	cfg, err := objects.BuildConfig(rec)
	if err != nil {
		return nil, apiError(err)
	}
	if secret := cfg.GetSpec().GetCredentialsSecret(); secret != "" {
		if err := c.s.checkBuildCredentials(ctx, cfg.GetSpec().GetGit().GetUrl(), secret); err != nil {
			return nil, err
		}
	}
	bname, err := c.s.createBuild(ctx, name, cfg.GetSpec(), resource.BuildTriggerManual)
	if err != nil {
		return nil, err
	}
	return &v1alpha1.StartBuildResponse{Build: bname}, nil
}

// createBuild stores a new Build of config with the given spec and trigger,
// and returns its name. The controller starts it.
func (s *Server) createBuild(ctx context.Context, config string, source *v1alpha1.BuildConfigSpec, trigger string) (string, error) {
	specB, err := objects.Encode(&v1alpha1.BuildSpec{Config: config, Source: source, Trigger: trigger})
	if err != nil {
		return "", apiError(err)
	}
	statusB, err := objects.Encode(&v1alpha1.BuildStatus{Phase: resource.BuildNew, Message: "waiting to start"})
	if err != nil {
		return "", apiError(err)
	}
	// A Build is in the project of its BuildConfig (ADR 0023, section 2).
	var labels map[string]string
	if bc, err := s.store.Get(ctx, v1alpha1.KindBuildConfig, config); err == nil {
		labels = tenancy.WithProject(nil, tenancy.ProjectOf(bc.Labels))
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", apiError(err)
	}
	for range 5 {
		bname := resource.BuildName(config, randomBuildSuffix())
		_, err := s.store.Create(ctx, store.Record{Kind: v1alpha1.KindBuild, Name: bname, Spec: specB, Status: statusB, Labels: labels})
		if errors.Is(err, store.ErrExists) {
			continue
		}
		if err != nil {
			return "", apiError(err)
		}
		s.log.Info("build requested", "build", bname, "config", config, "trigger", trigger)
		return bname, nil
	}
	return "", status.Error(codes.Internal, "no free build name")
}

// CancelBuild asks a Build that has not finished to stop. The controller
// performs the stop and sets the phase to Cancelled.
func (c *controlService) CancelBuild(ctx context.Context, req *v1alpha1.CancelBuildRequest) (*v1alpha1.CancelBuildResponse, error) {
	name := req.GetBuild()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "build is required")
	}
	rec, err := c.s.store.Get(ctx, v1alpha1.KindBuild, name)
	if err != nil {
		return nil, apiError(err)
	}
	b, err := objects.Build(rec)
	if err != nil {
		return nil, apiError(err)
	}
	if phase := b.GetStatus().GetPhase(); resource.BuildTerminal(phase) {
		return nil, status.Errorf(codes.FailedPrecondition, "build %s is %s: it cannot be cancelled", name, phase)
	}
	if b.GetSpec().GetCancelRequested() {
		return &v1alpha1.CancelBuildResponse{}, nil
	}
	b.Spec.CancelRequested = true
	enc, err := objects.Encode(b.Spec)
	if err != nil {
		return nil, apiError(err)
	}
	if _, err := c.s.store.Update(ctx, v1alpha1.KindBuild, name, enc, rec.Generation); err != nil {
		return nil, apiError(err)
	}
	c.s.log.Info("build cancel requested", "build", name)
	return &v1alpha1.CancelBuildResponse{}, nil
}

// BuildLogs returns the output of a Build so far. Pass A returns what the
// executor has in memory; streaming (OB-20) comes with the BuildKit executor.
func (c *controlService) BuildLogs(ctx context.Context, req *v1alpha1.BuildLogsRequest) (*v1alpha1.BuildLogsResponse, error) {
	name := req.GetBuild()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "build is required")
	}
	if _, err := c.s.store.Get(ctx, v1alpha1.KindBuild, name); err != nil {
		return nil, apiError(err)
	}
	lines, err := c.s.ctrl.BuildLogs(ctx, name)
	if err != nil {
		return nil, apiError(err)
	}
	out := &v1alpha1.BuildLogsResponse{}
	for _, l := range lines {
		out.Lines = append(out.Lines, &v1alpha1.LogLine{Time: timestamppb.New(l.Time), Line: l.Text})
	}
	return out, nil
}

// checkBuildCredentials checks that the Secret of a build exists and has the
// type the URL needs. The data is not read here.
func (s *Server) checkBuildCredentials(ctx context.Context, gitURL, secret string) error {
	rec, err := s.store.Get(ctx, v1alpha1.KindSecret, secret)
	if errors.Is(err, store.ErrNotFound) {
		return status.Errorf(codes.FailedPrecondition, "credentialsSecret %q: no such Secret", secret)
	}
	if err != nil {
		return apiError(err)
	}
	sec, err := objects.Secret(rec)
	if err != nil {
		return apiError(err)
	}
	if want := resource.RequiredCredentialType(gitURL); sec.GetSpec().GetType() != want {
		return status.Errorf(codes.FailedPrecondition, "credentialsSecret %q must have type %s for this URL", secret, want)
	}
	return nil
}

// checkBuildDeletable refuses to delete a Build that has not finished: the
// executor would keep running it. Cancel it first.
func (s *Server) checkBuildDeletable(ctx context.Context, name string) error {
	rec, err := s.store.Get(ctx, v1alpha1.KindBuild, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil // the delete itself reports it
	}
	if err != nil {
		return apiError(err)
	}
	b, err := objects.Build(rec)
	if err != nil {
		return apiError(err)
	}
	if phase := b.GetStatus().GetPhase(); !resource.BuildTerminal(phase) {
		return status.Errorf(codes.FailedPrecondition, "build %s is %s: cancel it first", name, phase)
	}
	return nil
}

func randomBuildSuffix() string {
	b := make([]byte, resource.BuildSuffixLen)
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
