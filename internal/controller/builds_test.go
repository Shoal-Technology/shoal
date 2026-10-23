package controller

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/build"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/store"
)

const buildSHA = "0123456789abcdef0123456789abcdef01234567"

// newBuildEnv returns an env whose controller runs builds through a fake
// executor.
func newBuildEnv(t *testing.T) (*env, *build.Fake) {
	t.Helper()
	e := newEnv(t)
	f := build.NewFake()
	e.c = New(Config{Store: e.st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return e.now }, Executor: f})
	return e, f
}

// buildConfig applies a BuildConfig the way the API does (defaulted, validated).
func (e *env) buildConfig(name string) {
	e.t.Helper()
	s := &v1alpha1.BuildConfigSpec{Git: &v1alpha1.GitSource{Url: "https://github.com/acme/" + name + ".git"}}
	resource.DefaultBuildConfig(name, s)
	if err := resource.ValidateBuildConfig(name, s); err != nil {
		e.t.Fatal(err)
	}
	spec, err := objects.Encode(s)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindBuildConfig, Name: name, Spec: spec}); err != nil {
		e.t.Fatal(err)
	}
}

// startBuild creates a Build of config the way StartBuild does, one second
// after the previous record, and returns its name.
func (e *env) startBuild(config, name string) string {
	e.t.Helper()
	e.now = e.now.Add(time.Second)
	cfg, err := objects.BuildConfig(e.record(v1alpha1.KindBuildConfig, config))
	if err != nil {
		e.t.Fatal(err)
	}
	spec, err := objects.Encode(&v1alpha1.BuildSpec{Config: config, Source: cfg.GetSpec(), Trigger: resource.BuildTriggerManual})
	if err != nil {
		e.t.Fatal(err)
	}
	status, err := objects.Encode(&v1alpha1.BuildStatus{Phase: resource.BuildNew})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindBuild, Name: name, Spec: spec, Status: status}); err != nil {
		e.t.Fatal(err)
	}
	return name
}

func (e *env) build(name string) *v1alpha1.Build {
	e.t.Helper()
	b, err := objects.Build(e.record(v1alpha1.KindBuild, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) phase(name string) string { return e.build(name).GetStatus().GetPhase() }

// requestCancel sets cancel_requested on a Build, as CancelBuild does.
func (e *env) requestCancel(name string) {
	e.t.Helper()
	rec := e.record(v1alpha1.KindBuild, name)
	b, err := objects.Build(rec)
	if err != nil {
		e.t.Fatal(err)
	}
	b.Spec.CancelRequested = true
	enc, err := objects.Encode(b.Spec)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Update(ctx, v1alpha1.KindBuild, name, enc, rec.Generation); err != nil {
		e.t.Fatal(err)
	}
}

func TestBuildSucceeds(t *testing.T) {
	e, f := newBuildEnv(t)
	e.buildConfig("web")
	b := e.startBuild("web", "web-aaaaa")

	e.reconcile()
	if got := e.phase(b); got != resource.BuildRunning {
		t.Fatalf("phase = %s, want Running", got)
	}
	if got := f.Started(); len(got) != 1 || got[0] != b {
		t.Fatalf("started = %v", got)
	}
	if e.build(b).GetStatus().GetLogRef() != b || e.build(b).GetStatus().GetStartedAt() == nil {
		t.Fatalf("status = %+v", e.build(b).GetStatus())
	}

	f.Finish(b, build.Status{State: build.Succeeded, Commit: buildSHA, Image: "web:0123456", Digest: "sha256:abc"})
	e.reconcile()
	st := e.build(b).GetStatus()
	if st.GetPhase() != resource.BuildComplete || st.GetCommit() != buildSHA || st.GetImage() != "web:0123456" || st.GetDigest() != "sha256:abc" {
		t.Fatalf("status = %+v", st)
	}
	if st.GetFinishedAt() == nil {
		t.Fatal("finished_at not set")
	}
	cfg, err := objects.BuildConfig(e.record(v1alpha1.KindBuildConfig, "web"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GetStatus().GetLastBuild() != b || cfg.GetStatus().GetLastPhase() != resource.BuildComplete {
		t.Fatalf("config status = %+v", cfg.GetStatus())
	}
	e.reconcile() // a converged pass starts nothing and changes nothing
	if len(f.Started()) != 1 {
		t.Fatalf("second start: %v", f.Started())
	}
}

func TestBuildFails(t *testing.T) {
	e, f := newBuildEnv(t)
	e.buildConfig("web")
	b := e.startBuild("web", "web-aaaaa")
	e.reconcile()
	f.Finish(b, build.Status{State: build.Failed, Message: "dockerfile: no such file"})
	e.reconcile()
	if st := e.build(b).GetStatus(); st.GetPhase() != resource.BuildFailed || st.GetMessage() != "dockerfile: no such file" {
		t.Fatalf("status = %+v", st)
	}
}

func TestCancelRunningBuild(t *testing.T) {
	e, f := newBuildEnv(t)
	e.buildConfig("web")
	b := e.startBuild("web", "web-aaaaa")
	e.reconcile()
	e.requestCancel(b)
	e.reconcile()
	if got := e.phase(b); got != resource.BuildCancelled {
		t.Fatalf("phase = %s, want Cancelled", got)
	}
	if got := f.Cancelled(); len(got) != 1 || got[0] != b {
		t.Fatalf("cancelled = %v", got)
	}
}

func TestCancelQueuedBuildNeverStarts(t *testing.T) {
	e, f := newBuildEnv(t)
	e.buildConfig("web")
	first := e.startBuild("web", "web-aaaaa")
	second := e.startBuild("web", "web-bbbbb")
	e.reconcile()
	e.requestCancel(second)
	e.reconcile()
	if got := e.phase(second); got != resource.BuildCancelled {
		t.Fatalf("second phase = %s, want Cancelled", got)
	}
	f.Finish(first, build.Status{State: build.Succeeded, Commit: buildSHA, Image: "web:0123456"})
	e.reconcile()
	if got := f.Started(); len(got) != 1 {
		t.Fatalf("started = %v: a cancelled build must not start", got)
	}
}

func TestBuildsOfOneConfigRunOneAtATime(t *testing.T) {
	e, f := newBuildEnv(t)
	e.buildConfig("web")
	b1 := e.startBuild("web", "web-aaaaa")
	b2 := e.startBuild("web", "web-bbbbb")
	b3 := e.startBuild("web", "web-ccccc")

	e.reconcile()
	if e.phase(b1) != resource.BuildRunning || e.phase(b2) != resource.BuildPending || e.phase(b3) != resource.BuildPending {
		t.Fatalf("phases = %s %s %s", e.phase(b1), e.phase(b2), e.phase(b3))
	}
	if got := f.Started(); len(got) != 1 || got[0] != b1 {
		t.Fatalf("started = %v", got)
	}

	f.Finish(b1, build.Status{State: build.Succeeded, Commit: buildSHA, Image: "web:0123456"})
	e.reconcile()
	if e.phase(b1) != resource.BuildComplete || e.phase(b2) != resource.BuildRunning || e.phase(b3) != resource.BuildPending {
		t.Fatalf("after first: %s %s %s", e.phase(b1), e.phase(b2), e.phase(b3))
	}
}

func TestBuildsOfDifferentConfigsRunTogether(t *testing.T) {
	e, f := newBuildEnv(t)
	e.buildConfig("web")
	e.buildConfig("api")
	w := e.startBuild("web", "web-aaaaa")
	a := e.startBuild("api", "api-aaaaa")
	e.reconcile()
	if e.phase(w) != resource.BuildRunning || e.phase(a) != resource.BuildRunning {
		t.Fatalf("phases = %s %s", e.phase(w), e.phase(a))
	}
	if len(f.Started()) != 2 {
		t.Fatalf("started = %v", f.Started())
	}
}

func TestStartFailureIsError(t *testing.T) {
	e, f := newBuildEnv(t)
	f.StartErr = errors.New("no builder node")
	e.buildConfig("web")
	b := e.startBuild("web", "web-aaaaa")
	e.reconcile()
	st := e.build(b).GetStatus()
	if st.GetPhase() != resource.BuildError || !strings.Contains(st.GetMessage(), "no builder node") {
		t.Fatalf("status = %+v", st)
	}
}

func TestNoExecutorIsError(t *testing.T) {
	e := newEnv(t) // no executor configured
	e.buildConfig("web")
	b := e.startBuild("web", "web-aaaaa")
	e.reconcile()
	if st := e.build(b).GetStatus(); st.GetPhase() != resource.BuildError || st.GetMessage() != noExecutorMessage {
		t.Fatalf("status = %+v", st)
	}
}

func TestLostJobIsError(t *testing.T) {
	e, _ := newBuildEnv(t)
	e.buildConfig("web")
	// A Build that claims to run, but the executor has no job for it.
	b := e.startBuild("web", "web-aaaaa")
	status, err := objects.Encode(&v1alpha1.BuildStatus{Phase: resource.BuildRunning, LogRef: b})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpdateStatus(ctx, v1alpha1.KindBuild, b, status); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if st := e.build(b).GetStatus(); st.GetPhase() != resource.BuildError || st.GetMessage() != "the build job is gone" {
		t.Fatalf("status = %+v", st)
	}
}

func TestBuildsOfDeletedConfigAreRemoved(t *testing.T) {
	e, f := newBuildEnv(t)
	e.buildConfig("web")
	running := e.startBuild("web", "web-aaaaa")
	e.reconcile()
	if err := e.st.Delete(ctx, v1alpha1.KindBuildConfig, "web", 0); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if _, err := e.st.Get(ctx, v1alpha1.KindBuild, running); err == nil {
		t.Fatal("build of a deleted config is still there")
	}
	if got := f.Cancelled(); len(got) != 1 || got[0] != running {
		t.Fatalf("cancelled = %v: a running build of a deleted config must be cancelled", got)
	}
}

func TestHistoryKeepsTheNewestTenFinishedBuilds(t *testing.T) {
	e, f := newBuildEnv(t)
	e.buildConfig("web")
	var names []string
	for i := range 12 {
		name := fmt.Sprintf("web-%05d", i)
		names = append(names, e.startBuild("web", name))
		e.reconcile() // starts it
		f.Finish(name, build.Status{State: build.Succeeded, Commit: buildSHA, Image: "web:0123456"})
		e.reconcile() // completes it, then prunes
	}
	recs, err := e.st.List(ctx, v1alpha1.KindBuild)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != resource.MaxBuildHistory {
		t.Fatalf("kept %d builds, want %d", len(recs), resource.MaxBuildHistory)
	}
	for i, name := range names {
		_, err := e.st.Get(ctx, v1alpha1.KindBuild, name)
		kept := i >= len(names)-resource.MaxBuildHistory
		if kept && err != nil {
			t.Errorf("build %s pruned, want kept", name)
		}
		if !kept && err == nil {
			t.Errorf("build %s kept, want pruned", name)
		}
	}
}

func TestHistoryNeverPrunesUnfinishedBuilds(t *testing.T) {
	e, _ := newBuildEnv(t)
	e.buildConfig("web")
	old := e.startBuild("web", "web-aaaaa")
	e.reconcile() // old is running
	for i := range resource.MaxBuildHistory + 2 {
		name := e.startBuild("web", fmt.Sprintf("web-q%04d", i))
		e.requestCancel(name)
		e.reconcile()
	}
	if got := e.phase(old); got != resource.BuildRunning {
		t.Fatalf("running build is %s", got)
	}
}

func TestBuildLogsComeFromTheExecutor(t *testing.T) {
	e, f := newBuildEnv(t)
	e.buildConfig("web")
	b := e.startBuild("web", "web-aaaaa")
	e.reconcile()
	f.Log(b, "step 1/3: FROM alpine")
	lines, err := e.c.BuildLogs(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Text != "step 1/3: FROM alpine" {
		t.Fatalf("lines = %+v", lines)
	}
}
