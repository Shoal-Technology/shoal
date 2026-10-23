package controller

import (
	"testing"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/build"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/store"
)

const rolloutDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// buildConfigOnSuccess creates a BuildConfig that updates workload when a
// Build of it completes (OB-45).
func (e *env) buildConfigOnSuccess(name, workload string) {
	e.t.Helper()
	s := &v1alpha1.BuildConfigSpec{
		Git:       &v1alpha1.GitSource{Url: "https://github.com/acme/" + name + ".git"},
		OnSuccess: &v1alpha1.BuildOnSuccess{UpdateWorkload: workload},
	}
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

// finishBuild runs one Build to Succeeded through the fake executor and two
// passes: one that starts it, one that records the outcome.
func finishBuild(e *env, f *build.Fake, config, name string) {
	e.t.Helper()
	e.startBuild(config, name)
	e.reconcile()
	f.Finish(name, build.Status{State: build.Succeeded, Commit: buildSHA, Image: "shoal.local/app:0123456", Digest: rolloutDigest})
	e.reconcile()
}

func TestBuildCompleteSetsWorkloadDigest(t *testing.T) {
	e, f := newBuildEnv(t)
	e.workload("web", 1)
	e.buildConfigOnSuccess("app", "web")
	before := e.workloadObj("web").GetMetadata().GetGeneration()

	finishBuild(e, f, "app", "app-bcdfg")

	w := e.workloadObj("web")
	if got, want := w.GetSpec().GetImage(), "shoal.local/app@"+rolloutDigest; got != want {
		t.Fatalf("workload image = %q, want %q", got, want)
	}
	if w.GetMetadata().GetGeneration() <= before {
		t.Fatalf("generation = %d, was %d: the spec change should start a rollout", w.GetMetadata().GetGeneration(), before)
	}
	if got := e.build("app-bcdfg").GetStatus().GetWorkloadUpdated(); got != "web" {
		t.Fatalf("build workloadUpdated = %q, want web", got)
	}

	// A later pass changes nothing: the Build recorded the update.
	after := w.GetMetadata().GetGeneration()
	e.reconcile()
	if got := e.workloadObj("web").GetMetadata().GetGeneration(); got != after {
		t.Fatalf("generation moved from %d to %d on a converged pass", after, got)
	}
}

func TestBuildCompleteMissingWorkloadWaitsForIt(t *testing.T) {
	e, f := newBuildEnv(t)
	e.buildConfigOnSuccess("app", "gone")

	finishBuild(e, f, "app", "app-bcdfg")

	st := e.build("app-bcdfg").GetStatus()
	if st.GetPhase() != resource.BuildComplete {
		t.Fatalf("phase = %s, want Complete", st.GetPhase())
	}
	if st.GetWorkloadUpdated() != "" || st.GetMessage() == "" {
		t.Fatalf("status = %+v, want no workloadUpdated and a message", st)
	}

	// The workload appears later: the next pass sets its image.
	e.workload("gone", 1)
	e.reconcile()
	if got, want := e.workloadObj("gone").GetSpec().GetImage(), "shoal.local/app@"+rolloutDigest; got != want {
		t.Fatalf("workload image = %q, want %q", got, want)
	}
}
