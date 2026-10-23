package server

import (
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/build"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/store/memory"
)

// newDispatchEnv returns a dispatcher over an in-memory store with two nodes:
// b1 is a builder and n1 is not.
func newDispatchEnv(t *testing.T, registry string) (*buildDispatch, store.Store) {
	t.Helper()
	ctx := t.Context()
	now := time.Now
	st := memory.New(now)
	for name, labels := range map[string]map[string]string{
		"b1": {"shoal.dev/builder": "true"},
		"n1": {},
	} {
		spec, err := objects.Encode(&v1alpha1.NodeSpec{Labels: labels})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Create(ctx, store.Record{Kind: v1alpha1.KindNode, Name: name, Spec: spec}); err != nil {
			t.Fatal(err)
		}
	}
	d := newBuildDispatch(st, slog.Default(), t.TempDir(), now, registry)
	return d, st
}

func testJob(name string) build.Job {
	return build.Job{Name: name, Config: "web", Source: &v1alpha1.BuildConfigSpec{
		Git:    &v1alpha1.GitSource{Url: "https://example.com/acme/web.git", Ref: "main"},
		Output: &v1alpha1.BuildOutput{Repository: "web", Tag: "{short-sha}"},
	}}
}

// TestBuildDispatchContract walks one Build through the executor seam the
// controller uses: start, claim, certificate scope, logs, cancel and result.
func TestBuildDispatchContract(t *testing.T) {
	ctx := t.Context()
	d, _ := newDispatchEnv(t, "10.0.0.5:5000")

	if err := d.Start(ctx, testJob("web-abc12")); err != nil {
		t.Fatalf("start with a builder: %v", err)
	}
	if st, err := d.Status(ctx, "web-abc12"); err != nil || st.State != build.Running {
		t.Fatalf("queued build status = %+v, %v; want Running", st, err)
	}

	if resp, err := d.claim(ctx, "n1"); err != nil || resp.GetBuild() != "" {
		t.Fatalf("a non-builder claimed %q (%v)", resp.GetBuild(), err)
	}
	resp, err := d.claim(ctx, "b1")
	if err != nil || resp.GetBuild() != "web-abc12" {
		t.Fatalf("builder claim = %q, %v", resp.GetBuild(), err)
	}
	if resp.GetRegistryAddress() != "10.0.0.5:5000" {
		t.Fatalf("registry address %q", resp.GetRegistryAddress())
	}

	if repo, active, err := d.scope(ctx, "web-abc12"); err != nil || repo != "web" || !active {
		t.Fatalf("scope = %q, %v, %v; want web, active", repo, active, err)
	}
	if _, err := d.scopeFor("web-abc12", "n1"); status.Code(err) != codes.NotFound {
		t.Fatalf("another node got a certificate scope: %v", err)
	}
	if repo, err := d.scopeFor("web-abc12", "b1"); err != nil || repo != "web" {
		t.Fatalf("claimant scope = %q, %v", repo, err)
	}

	if cancel, err := d.report("b1", &v1alpha1.ReportBuildRequest{
		Build: "web-abc12",
		Lines: []*v1alpha1.LogLine{{Time: timestamppb.New(time.Unix(1700000000, 0)), Line: "step 1/2"}},
	}); err != nil || cancel {
		t.Fatalf("log report: cancel=%v err=%v", cancel, err)
	}
	if _, err := d.report("n1", &v1alpha1.ReportBuildRequest{Build: "web-abc12"}); status.Code(err) != codes.NotFound {
		t.Fatalf("a report from another node was accepted: %v", err)
	}

	if err := d.Cancel(ctx, "web-abc12"); err != nil {
		t.Fatal(err)
	}
	if cancel, err := d.report("b1", &v1alpha1.ReportBuildRequest{Build: "web-abc12"}); err != nil || !cancel {
		t.Fatalf("report after cancel: cancel=%v err=%v; want true", cancel, err)
	}

	if _, err := d.report("b1", &v1alpha1.ReportBuildRequest{
		Build: "web-abc12", Phase: "Complete", Commit: "abc1234def", Image: "web:abc1234", Digest: "sha256:ff",
	}); err != nil {
		t.Fatal(err)
	}
	st, err := d.Status(ctx, "web-abc12")
	if err != nil || st.State != build.Succeeded || st.Digest != "sha256:ff" || st.Commit != "abc1234def" {
		t.Fatalf("final status = %+v, %v", st, err)
	}
	if _, active, _ := d.scope(ctx, "web-abc12"); active {
		t.Fatal("a finished build still holds a registry scope")
	}
	if _, err := d.scopeFor("web-abc12", "b1"); err == nil {
		t.Fatal("a finished build still issues certificates")
	}

	lines, err := d.Logs(ctx, "web-abc12")
	if err != nil || len(lines) != 1 || lines[0].Text != "step 1/2" {
		t.Fatalf("logs = %+v, %v", lines, err)
	}
	if _, err := d.Logs(ctx, "never-started"); err != build.ErrUnknownJob {
		t.Fatalf("logs of an unknown build: %v", err)
	}
}

// TestBuildDispatchRefusals covers a start that cannot succeed, and a queued
// build cancelled before any builder takes it.
func TestBuildDispatchRefusals(t *testing.T) {
	ctx := t.Context()

	off, _ := newDispatchEnv(t, "")
	if err := off.Start(ctx, testJob("web-abc12")); err == nil {
		t.Fatal("a build started with the registry off")
	}

	// A cluster with no builder: only a plain node.
	st := memory.New(time.Now)
	plain, err := objects.Encode(&v1alpha1.NodeSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, store.Record{Kind: v1alpha1.KindNode, Name: "n1", Spec: plain}); err != nil {
		t.Fatal(err)
	}
	d := newBuildDispatch(st, slog.Default(), t.TempDir(), time.Now, "10.0.0.5:5000")
	if err := d.Start(ctx, testJob("web-abc12")); err == nil {
		t.Fatal("a build started with no builder node")
	}

	// A builder joins; the next build starts.
	spec, err := objects.Encode(&v1alpha1.NodeSpec{Labels: map[string]string{"shoal.dev/builder": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, store.Record{Kind: v1alpha1.KindNode, Name: "b2", Spec: spec}); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(ctx, testJob("web-def34")); err != nil {
		t.Fatal(err)
	}
	if err := d.Cancel(ctx, "web-def34"); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Status(ctx, "web-def34"); err != nil || got.State != build.Failed || got.Message != "cancelled" {
		t.Fatalf("cancelled queued build = %+v, %v", got, err)
	}
	if resp, err := d.claim(ctx, "b2"); err != nil || resp.GetBuild() != "" {
		t.Fatalf("a cancelled build was claimed: %q (%v)", resp.GetBuild(), err)
	}
}
