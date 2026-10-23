package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/build"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/store"
)

// Builds (ADR 0021, pass A). A Build is one run of a BuildConfig. Per config,
// one Build runs at a time: the others wait in Pending, oldest first. Each
// pass the controller reads the executor's state for the running Builds,
// stops the ones asked to cancel, starts the oldest waiting one, keeps the
// newest resource.MaxBuildHistory finished Builds of each config, and removes
// the Builds of a deleted config. Only the controller writes Build status.

// queuedBuild is a Build with its record, as one pass sees it.
type queuedBuild struct {
	rec store.Record
	obj *v1alpha1.Build
}

func (b *queuedBuild) phase() string { return b.obj.GetStatus().GetPhase() }

// logRef is the name the executor knows the job by.
func (b *queuedBuild) logRef() string {
	if ref := b.obj.GetStatus().GetLogRef(); ref != "" {
		return ref
	}
	return b.rec.Name
}

// syncBuilds runs one pass over the Builds and returns the errors it skipped
// over.
func (c *Controller) syncBuilds(ctx context.Context) []error {
	// Builds are listed before configs: a Build is created after its config,
	// so a config listed after the Builds is never missing for one of them.
	brecs, err := c.st.List(ctx, v1alpha1.KindBuild)
	if err != nil {
		return []error{err}
	}
	crecs, err := c.st.List(ctx, v1alpha1.KindBuildConfig)
	if err != nil {
		return []error{err}
	}
	configs := map[string]store.Record{}
	for _, r := range crecs {
		configs[r.Name] = r
	}
	var errs []error
	byConfig := map[string][]*queuedBuild{}
	for _, r := range brecs {
		b, err := objects.Build(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		name := b.GetSpec().GetConfig()
		qb := &queuedBuild{rec: r, obj: b}
		if _, ok := configs[name]; !ok {
			if err := c.removeOrphanBuild(ctx, qb); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		byConfig[name] = append(byConfig[name], qb)
	}
	for _, name := range sortedKeys(byConfig) {
		errs = append(errs, c.syncConfigBuilds(ctx, configs[name], byConfig[name])...)
	}
	return errs
}

// removeOrphanBuild removes a Build whose config is gone. A run still going is
// cancelled first; if that fails, the Build stays for the next pass.
func (c *Controller) removeOrphanBuild(ctx context.Context, b *queuedBuild) error {
	if b.phase() == resource.BuildRunning && c.cfg.Executor != nil {
		if err := c.cfg.Executor.Cancel(ctx, b.logRef()); err != nil && !errors.Is(err, build.ErrUnknownJob) {
			return fmt.Errorf("cancel build %s of a deleted config: %w", b.rec.Name, err)
		}
	}
	if err := c.deleteIgnoringMissing(ctx, v1alpha1.KindBuild, b.rec.Name); err != nil {
		return err
	}
	c.log.Info("build of a deleted config removed", "build", b.rec.Name, "config", b.obj.GetSpec().GetConfig())
	return nil
}

// syncConfigBuilds advances the Builds of one config, oldest first. Running
// Builds are checked, cancelled ones stop, and the oldest waiting Build starts
// when nothing runs. It then writes the config's status and prunes history.
func (c *Controller) syncConfigBuilds(ctx context.Context, cfg store.Record, bs []*queuedBuild) []error {
	slices.SortStableFunc(bs, func(a, b *queuedBuild) int {
		if d := a.rec.CreatedAt.Compare(b.rec.CreatedAt); d != 0 {
			return d
		}
		return cmp.Compare(a.rec.Name, b.rec.Name)
	})
	now := c.cfg.Now()
	var errs []error
	running := 0
	for _, b := range bs {
		if b.phase() != resource.BuildRunning {
			continue
		}
		if err := c.checkRunning(ctx, b, now); err != nil {
			errs = append(errs, err)
		}
		if b.phase() == resource.BuildRunning {
			running++
		}
	}
	for _, b := range bs {
		switch {
		case resource.BuildTerminal(b.phase()) || b.phase() == resource.BuildRunning:
			continue
		case b.obj.GetSpec().GetCancelRequested():
			if err := c.setBuildPhase(ctx, b, resource.BuildCancelled, "cancelled before it started", now); err != nil {
				errs = append(errs, err)
			}
		case running > 0:
			if err := c.setBuildPhase(ctx, b, resource.BuildPending, "waiting for an earlier build of this config to finish", now); err != nil {
				errs = append(errs, err)
			}
		default:
			if err := c.startBuild(ctx, b, now); err != nil {
				errs = append(errs, err)
			}
			if b.phase() == resource.BuildRunning {
				running++
			}
		}
	}
	if len(bs) > 0 {
		if err := c.writeConfigStatus(ctx, cfg, bs[len(bs)-1]); err != nil {
			errs = append(errs, err)
		}
	}
	return append(errs, c.pruneBuilds(ctx, bs)...)
}

// checkRunning cancels a running Build that was asked to stop, or reads the
// executor's state for it and records the outcome.
func (c *Controller) checkRunning(ctx context.Context, b *queuedBuild, now time.Time) error {
	if b.obj.GetSpec().GetCancelRequested() {
		if c.cfg.Executor != nil {
			if err := c.cfg.Executor.Cancel(ctx, b.logRef()); err != nil && !errors.Is(err, build.ErrUnknownJob) {
				return fmt.Errorf("cancel build %s: %w", b.rec.Name, err)
			}
		}
		c.log.Info("build cancelled", "build", b.rec.Name, "config", b.obj.GetSpec().GetConfig())
		return c.setBuildPhase(ctx, b, resource.BuildCancelled, "cancelled", now)
	}
	if c.cfg.Executor == nil {
		return c.setBuildPhase(ctx, b, resource.BuildError, noExecutorMessage, now)
	}
	st, err := c.cfg.Executor.Status(ctx, b.logRef())
	switch {
	case errors.Is(err, build.ErrUnknownJob):
		return c.setBuildPhase(ctx, b, resource.BuildError, "the build job is gone", now)
	case err != nil:
		return fmt.Errorf("status of build %s: %w", b.rec.Name, err)
	}
	switch st.State {
	case build.Succeeded:
		done := proto.CloneOf(b.obj.GetStatus())
		done.Phase, done.Message = resource.BuildComplete, ""
		done.Commit, done.Image, done.Digest = st.Commit, st.Image, st.Digest
		done.FinishedAt = timestamppb.New(now)
		c.log.Info("build complete", "build", b.rec.Name, "image", st.Image, "digest", st.Digest)
		return c.writeBuildStatus(ctx, b, done)
	case build.Failed:
		msg := st.Message
		if msg == "" {
			msg = "the build failed"
		}
		c.log.Info("build failed", "build", b.rec.Name, "message", msg)
		return c.setBuildPhase(ctx, b, resource.BuildFailed, msg, now)
	}
	return nil
}

const noExecutorMessage = "no build executor is configured on this server"

// startBuild starts a waiting Build through the executor. A start that fails
// leaves the Build in phase Error.
func (c *Controller) startBuild(ctx context.Context, b *queuedBuild, now time.Time) error {
	if c.cfg.Executor == nil {
		return c.setBuildPhase(ctx, b, resource.BuildError, noExecutorMessage, now)
	}
	job := build.Job{Name: b.rec.Name, Config: b.obj.GetSpec().GetConfig(), Source: b.obj.GetSpec().GetSource()}
	if err := c.cfg.Executor.Start(ctx, job); err != nil {
		return c.setBuildPhase(ctx, b, resource.BuildError, "could not start the build: "+err.Error(), now)
	}
	c.log.Info("build started", "build", job.Name, "config", job.Config)
	st := proto.CloneOf(b.obj.GetStatus())
	st.Phase, st.Message = resource.BuildRunning, ""
	st.StartedAt = timestamppb.New(now)
	st.LogRef = b.rec.Name
	return c.writeBuildStatus(ctx, b, st)
}

// setBuildPhase records a phase and message. A finished phase also records
// when the Build finished. Other fields of the status are kept.
func (c *Controller) setBuildPhase(ctx context.Context, b *queuedBuild, phase, msg string, now time.Time) error {
	st := proto.CloneOf(b.obj.GetStatus())
	st.Phase, st.Message = phase, msg
	if resource.BuildTerminal(phase) && st.FinishedAt == nil {
		st.FinishedAt = timestamppb.New(now)
	}
	return c.writeBuildStatus(ctx, b, st)
}

// writeBuildStatus stores a Build's status if it changed.
func (c *Controller) writeBuildStatus(ctx context.Context, b *queuedBuild, st *v1alpha1.BuildStatus) error {
	if proto.Equal(b.obj.GetStatus(), st) {
		return nil
	}
	enc, err := objects.Encode(st)
	if err != nil {
		return err
	}
	rec, err := c.st.UpdateStatus(ctx, v1alpha1.KindBuild, b.rec.Name, enc)
	if errors.Is(err, store.ErrNotFound) {
		return nil // deleted during the pass
	}
	if err != nil {
		return fmt.Errorf("update status of build %s: %w", b.rec.Name, err)
	}
	b.rec, b.obj.Status = rec, st
	return nil
}

// writeConfigStatus records the newest Build of a config and its phase.
func (c *Controller) writeConfigStatus(ctx context.Context, cfg store.Record, newest *queuedBuild) error {
	cur, err := objects.BuildConfig(cfg)
	if err != nil {
		return err
	}
	st := &v1alpha1.BuildConfigStatus{LastBuild: newest.rec.Name, LastPhase: newest.phase()}
	if proto.Equal(cur.GetStatus(), st) {
		return nil
	}
	enc, err := objects.Encode(st)
	if err != nil {
		return err
	}
	if _, err := c.st.UpdateStatus(ctx, v1alpha1.KindBuildConfig, cfg.Name, enc); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("update status of build config %s: %w", cfg.Name, err)
	}
	return nil
}

// pruneBuilds removes all but the newest resource.MaxBuildHistory finished
// Builds. bs is sorted oldest first. Builds that have not finished are never
// removed.
func (c *Controller) pruneBuilds(ctx context.Context, bs []*queuedBuild) []error {
	var finished []*queuedBuild
	for i := len(bs) - 1; i >= 0; i-- {
		if resource.BuildTerminal(bs[i].phase()) {
			finished = append(finished, bs[i])
		}
	}
	var errs []error
	for _, b := range finished[min(len(finished), resource.MaxBuildHistory):] {
		if err := c.deleteIgnoringMissing(ctx, v1alpha1.KindBuild, b.rec.Name); err != nil {
			errs = append(errs, err)
			continue
		}
		c.log.Info("build pruned", "build", b.rec.Name, "config", b.obj.GetSpec().GetConfig())
	}
	return errs
}

// BuildLogs returns the output the executor has for a Build. Without an
// executor there is none, and a job the executor does not know has none either.
func (c *Controller) BuildLogs(ctx context.Context, name string) ([]build.Line, error) {
	if c.cfg.Executor == nil {
		return nil, nil
	}
	rec, err := c.st.Get(ctx, v1alpha1.KindBuild, name)
	if err != nil {
		return nil, err
	}
	b, err := objects.Build(rec)
	if err != nil {
		return nil, err
	}
	lines, err := c.cfg.Executor.Logs(ctx, (&queuedBuild{rec: rec, obj: b}).logRef())
	if errors.Is(err, build.ErrUnknownJob) {
		return nil, nil
	}
	return lines, err
}
