package controller

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/objects"
)

func image(img string) func(*v1alpha1.WorkloadSpec) {
	return func(s *v1alpha1.WorkloadSpec) { s.Image = img }
}

func maxUnavailable(n int32) func(*v1alpha1.WorkloadSpec) {
	return func(s *v1alpha1.WorkloadSpec) { s.Update = &v1alpha1.UpdateStrategy{MaxUnavailable: proto.Int32(n)} }
}

// readyAll makes every replica of the workload report ready on its current
// template, as healthy agents would.
func (e *env) readyAll() {
	e.t.Helper()
	for _, r := range e.replicas() {
		e.report(r.Metadata.Name, v1alpha1.ReplicaRunning)
	}
}

func (e *env) workloadObj(name string) *v1alpha1.Workload {
	e.t.Helper()
	r, err := e.st.Get(ctx, v1alpha1.KindWorkload, name)
	if err != nil {
		e.t.Fatal(err)
	}
	w, _ := objects.Workload(r)
	return w
}

// images counts replicas per template image.
func (e *env) images() map[string]int {
	m := map[string]int{}
	for _, r := range e.replicas() {
		m[r.Spec.Template.GetImage()]++
	}
	return m
}

// unavailable counts replicas that aren't ready on their current template.
func (e *env) unavailable() int {
	n := 0
	for _, r := range e.replicas() {
		if !(r.Spec.Node != "" && r.Status.Ready && r.Status.TemplateGeneration == r.Spec.TemplateGeneration) {
			n++
		}
	}
	return n
}

// deployed creates a workload and completes its first rollout.
func (e *env) deployed(name string, replicas int32, mutate ...func(*v1alpha1.WorkloadSpec)) {
	e.t.Helper()
	e.node("a", v1alpha1.NodeReady, 16000, 16384, 0)
	e.workload(name, replicas, mutate...)
	e.reconcile()
	e.readyAll()
	e.reconcile()
	ro := e.workloadObj(name).Status.Rollout
	if ro.GetPhase() != RolloutComplete || ro.GetStableGeneration() != 1 {
		e.t.Fatalf("first rollout: %v", ro)
	}
}

func TestRollingUpdateRespectsMaxUnavailable(t *testing.T) {
	for _, maxU := range []int32{1, 2} {
		t.Run("maxUnavailable="+string(rune('0'+maxU)), func(t *testing.T) {
			e := newEnv(t)
			e.deployed("web", 4, maxUnavailable(maxU))
			e.workload("web", 4, image("nginx:1.28"), maxUnavailable(maxU))

			for step := 0; ; step++ {
				e.reconcile()
				if u := e.unavailable(); u > int(maxU) {
					t.Fatalf("step %d: %d replicas unavailable, max %d", step, u, maxU)
				}
				// Without new reports nothing more moves: the next batch is
				// gated on the updated replicas becoming ready.
				before := e.images()
				e.reconcile()
				if after := e.images(); after["nginx:1.28"] != before["nginx:1.28"] {
					t.Fatalf("step %d: rollout advanced without updated replicas becoming ready", step)
				}
				if e.images()["nginx:1.28"] == 4 {
					break
				}
				if step > 4 {
					t.Fatalf("rollout not finishing: %v", e.images())
				}
				e.readyAll()
			}
			e.readyAll()
			e.reconcile()
			w := e.workloadObj("web")
			if ro := w.Status.Rollout; ro.Phase != RolloutComplete || ro.StableGeneration != 2 || ro.StableTemplate.GetImage() != "nginx:1.28" {
				t.Fatalf("rollout status = %v", ro)
			}
			if w.Status.ReadyReplicas != 4 || w.Status.UpdatedReplicas != 4 {
				t.Fatalf("status = %v", w.Status)
			}
		})
	}
}

func TestUnavailableOutdatedReplicaIsUpdatedFirst(t *testing.T) {
	e := newEnv(t)
	e.deployed("web", 3)
	broken := e.replicas()[2].Metadata.Name
	e.report(broken, v1alpha1.ReplicaFailed) // already down: replacing it costs nothing
	e.workload("web", 3, image("nginx:1.28"))
	e.reconcile()
	for _, r := range e.replicas() {
		isNew := r.Spec.Template.GetImage() == "nginx:1.28"
		if isNew != (r.Metadata.Name == broken) {
			t.Fatalf("updated %s (new=%v); want only the unavailable %s", r.Metadata.Name, isNew, broken)
		}
	}
}

func TestRollbackWhenNotReadyInTime(t *testing.T) {
	e := newEnv(t)
	e.deployed("web", 3)
	e.workload("web", 3, image("nginx:broken"))
	e.reconcile()
	if got := e.images()["nginx:broken"]; got != 1 {
		t.Fatalf("%d replicas updated, want 1", got)
	}
	// The updated replica never gets ready.
	for _, r := range e.replicas() {
		if r.Spec.Template.GetImage() == "nginx:broken" {
			e.reportStatus(r.Metadata.Name, func(rp *v1alpha1.Replica) *v1alpha1.ReplicaStatus {
				return &v1alpha1.ReplicaStatus{Phase: v1alpha1.ReplicaRunning, TemplateGeneration: rp.Spec.TemplateGeneration,
					Message: "health check failed (2/3): HTTP 500"}
			})
		}
	}
	e.now = e.now.Add(119 * time.Second)
	e.reconcile()
	if ro := e.workloadObj("web").Status.Rollout; ro.Phase != RolloutProgressing {
		t.Fatalf("before the deadline: %v", ro)
	}

	e.now = e.now.Add(2 * time.Second)
	e.reconcile()
	w := e.workloadObj("web")
	ro := w.Status.Rollout
	if ro.Phase != RolloutRolledBack || ro.FailedGeneration != 2 || !strings.Contains(ro.Message, "not ready after 2m0s: health check failed") {
		t.Fatalf("after the deadline: %v", ro)
	}
	if imgs := e.images(); imgs["nginx:1.27"] != 3 {
		t.Fatalf("templates after rollback = %v, want all back on nginx:1.27", imgs)
	}
	if w.Spec.GetImage() != "nginx:broken" {
		t.Fatal("rollback must not rewrite the user's spec")
	}

	// It stays rolled back (no retry of the same generation)...
	e.readyAll()
	e.reconcile()
	e.reconcile()
	if ro := e.workloadObj("web").Status.Rollout; ro.Phase != RolloutRolledBack || ro.StableGeneration != 1 {
		t.Fatalf("after recovery: %v", ro)
	}
	if imgs := e.images(); imgs["nginx:1.27"] != 3 {
		t.Fatalf("rolled-back generation retried: %v", imgs)
	}

	// ...until a new spec is applied.
	e.workload("web", 3, image("nginx:1.28"))
	e.reconcile()
	if ro := e.workloadObj("web").Status.Rollout; ro.Phase != RolloutProgressing || e.images()["nginx:1.28"] != 1 {
		t.Fatalf("new rollout after rollback: %v %v", ro, e.images())
	}
}

func TestRollbackOnCrashLoop(t *testing.T) {
	e := newEnv(t)
	e.deployed("web", 2)
	e.workload("web", 2, image("nginx:crashy"))
	e.reconcile()
	for _, r := range e.replicas() {
		if r.Spec.Template.GetImage() == "nginx:crashy" {
			e.reportStatus(r.Metadata.Name, func(rp *v1alpha1.Replica) *v1alpha1.ReplicaStatus {
				return &v1alpha1.ReplicaStatus{Phase: v1alpha1.ReplicaFailed, Restarts: 3, TemplateGeneration: rp.Spec.TemplateGeneration}
			})
		}
	}
	e.reconcile() // no waiting for the deadline
	if ro := e.workloadObj("web").Status.Rollout; ro.Phase != RolloutRolledBack || !strings.Contains(ro.Message, "restarted 3 times") {
		t.Fatalf("rollout = %v", ro)
	}
	if imgs := e.images(); imgs["nginx:1.27"] != 2 {
		t.Fatalf("templates = %v", imgs)
	}
}

func TestOldRestartsDoNotFailANewTemplate(t *testing.T) {
	e := newEnv(t)
	e.deployed("web", 1)
	e.workload("web", 1, image("nginx:1.28"))
	e.reconcile()
	// The agent still reports the old container (old template generation)
	// with restarts from before: that says nothing about the new template.
	r := e.replicas()[0]
	e.reportStatus(r.Metadata.Name, func(*v1alpha1.Replica) *v1alpha1.ReplicaStatus {
		return &v1alpha1.ReplicaStatus{Phase: v1alpha1.ReplicaRunning, Restarts: 5, TemplateGeneration: 1}
	})
	e.reconcile()
	if ro := e.workloadObj("web").Status.Rollout; ro.Phase != RolloutProgressing {
		t.Fatalf("rollout = %v", ro)
	}
}

func TestFirstRolloutCanOnlyFail(t *testing.T) {
	e := newEnv(t)
	e.node("a", v1alpha1.NodeReady, 16000, 16384, 0)
	e.workload("web", 2)
	e.reconcile()
	e.now = e.now.Add(3 * time.Minute)
	e.reconcile()
	ro := e.workloadObj("web").Status.Rollout
	if ro.Phase != RolloutFailed || !strings.Contains(ro.Message, "no earlier version") {
		t.Fatalf("rollout = %v", ro)
	}
	if imgs := e.images(); imgs["nginx:1.27"] != 2 {
		t.Fatalf("templates = %v", imgs)
	}
	// It recovers once the replicas get ready.
	e.readyAll()
	e.reconcile()
	if ro := e.workloadObj("web").Status.Rollout; ro.Phase != RolloutComplete || ro.StableGeneration != 1 {
		t.Fatalf("after recovery: %v", ro)
	}
}

func TestScalingDuringRolloutUsesTarget(t *testing.T) {
	e := newEnv(t)
	e.deployed("web", 2)
	e.workload("web", 4, image("nginx:1.28"))
	e.reconcile()
	if imgs := e.images(); imgs["nginx:1.28"] < 2 {
		t.Fatalf("new replicas not on the new template: %v", imgs)
	}
}

func TestScalingAloneIsNotARollout(t *testing.T) {
	e := newEnv(t)
	e.deployed("web", 2)
	before := e.replicas()
	e.workload("web", 3)
	e.reconcile()
	for _, r := range before {
		got, _ := e.st.Get(ctx, v1alpha1.KindReplica, r.Metadata.Name)
		if got.Generation != r.Metadata.Generation {
			t.Fatalf("scaling touched existing replica %s", r.Metadata.Name)
		}
	}
	e.readyAll()
	e.reconcile()
	if ro := e.workloadObj("web").Status.Rollout; ro.Phase != RolloutComplete || ro.StableGeneration != 2 {
		t.Fatalf("rollout = %v", ro)
	}
}

func TestReconcileIsIdempotentDuringRollout(t *testing.T) {
	e := newEnv(t)
	e.deployed("web", 3)
	e.workload("web", 3, image("nginx:1.28"))
	e.reconcile()
	rev := e.st.Revision()
	e.reconcile()
	e.reconcile()
	if got := e.st.Revision(); got != rev {
		t.Fatalf("passes during a gated rollout wrote to the store (revision %d -> %d)", rev, got)
	}
}
