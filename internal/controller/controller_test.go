package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/objects"
	"example.com/shoal/internal/resource"
	"example.com/shoal/internal/store"
	"example.com/shoal/internal/store/memory"
)

var ctx = context.Background()

type env struct {
	t   *testing.T
	now time.Time
	st  *memory.Store
	c   *Controller
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	e.st = memory.New(func() time.Time { return e.now })
	e.c = New(Config{Store: e.st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return e.now }})
	return e
}

func (e *env) withStore(s store.Store) *env {
	e.c = New(Config{Store: s, Log: e.c.log, Now: func() time.Time { return e.now }})
	return e
}

// node adds a node with the given phase and capacity (cpu in millicores,
// memory in MiB), whose last heartbeat was ago.
func (e *env) node(name, phase string, cpu, memMi int64, ago time.Duration) {
	e.t.Helper()
	st, _ := objects.Encode(&v1alpha1.NodeStatus{
		Phase:         phase,
		LastHeartbeat: timestamppb.New(e.now.Add(-ago)),
		Capacity:      &v1alpha1.NodeCapacity{CpuMillis: cpu, MemoryBytes: memMi << 20},
	})
	if r, err := e.st.Get(ctx, v1alpha1.KindNode, name); err == nil {
		if _, err := e.st.UpdateStatus(ctx, v1alpha1.KindNode, r.Name, st); err != nil {
			e.t.Fatal(err)
		}
		return
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindNode, Name: name, Status: st}); err != nil {
		e.t.Fatal(err)
	}
}

// workload applies a workload the way the API does (defaulted, validated).
func (e *env) workload(name string, replicas int32, mutate ...func(*v1alpha1.WorkloadSpec)) {
	e.t.Helper()
	w := &v1alpha1.Workload{
		Metadata: &v1alpha1.ObjectMeta{Name: name},
		Spec:     &v1alpha1.WorkloadSpec{Image: "nginx:1.27", Replicas: proto.Int32(replicas)},
	}
	for _, m := range mutate {
		m(w.Spec)
	}
	resource.DefaultWorkload(w)
	if err := resource.ValidateWorkload(w); err != nil {
		e.t.Fatal(err)
	}
	spec, _ := objects.Encode(w.Spec)
	r, err := e.st.Get(ctx, v1alpha1.KindWorkload, name)
	if errors.Is(err, store.ErrNotFound) {
		_, err = e.st.Create(ctx, store.Record{Kind: v1alpha1.KindWorkload, Name: name, Spec: spec})
	} else if err == nil {
		_, err = e.st.Update(ctx, v1alpha1.KindWorkload, name, spec, r.Generation)
	}
	if err != nil {
		e.t.Fatal(err)
	}
}

func resources(cpu, mem string) func(*v1alpha1.WorkloadSpec) {
	return func(s *v1alpha1.WorkloadSpec) { s.Resources = &v1alpha1.Resources{Cpu: cpu, Memory: mem} }
}

func hostPort(p int32) func(*v1alpha1.WorkloadSpec) {
	return func(s *v1alpha1.WorkloadSpec) {
		s.Ports = []*v1alpha1.Port{{ContainerPort: 80, HostPort: p, Protocol: "tcp"}}
	}
}

func (e *env) reconcile() {
	e.t.Helper()
	if err := e.c.Reconcile(ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) replicas() []*v1alpha1.Replica {
	e.t.Helper()
	recs, err := e.st.List(ctx, v1alpha1.KindReplica)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []*v1alpha1.Replica
	for _, r := range recs {
		rp, err := objects.Replica(r)
		if err != nil {
			e.t.Fatal(err)
		}
		out = append(out, rp)
	}
	return out
}

// placement counts replicas per node ("" = unscheduled).
func (e *env) placement() map[string]int {
	m := map[string]int{}
	for _, r := range e.replicas() {
		m[r.Spec.GetNode()]++
	}
	return m
}

// report sets a replica's status as its agent would: for the replica's
// current template, ready if running.
func (e *env) report(name, phase string) {
	e.t.Helper()
	e.reportStatus(name, func(rp *v1alpha1.Replica) *v1alpha1.ReplicaStatus {
		return &v1alpha1.ReplicaStatus{Phase: phase, Ready: phase == v1alpha1.ReplicaRunning, TemplateGeneration: rp.Spec.TemplateGeneration}
	})
}

func (e *env) reportStatus(name string, status func(*v1alpha1.Replica) *v1alpha1.ReplicaStatus) {
	e.t.Helper()
	r, err := e.st.Get(ctx, v1alpha1.KindReplica, name)
	if err != nil {
		e.t.Fatal(err)
	}
	rp, _ := objects.Replica(r)
	b, _ := objects.Encode(status(rp))
	if _, err := e.st.UpdateStatus(ctx, v1alpha1.KindReplica, name, b); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) workloadStatus(name string) *v1alpha1.WorkloadStatus {
	e.t.Helper()
	r, err := e.st.Get(ctx, v1alpha1.KindWorkload, name)
	if err != nil {
		e.t.Fatal(err)
	}
	w, _ := objects.Workload(r)
	return w.Status
}

func TestCreatesAndSpreadsReplicas(t *testing.T) {
	e := newEnv(t)
	e.node("a", v1alpha1.NodeReady, 2000, 4096, 0)
	e.node("b", v1alpha1.NodeReady, 2000, 4096, 0)
	e.workload("web", 3, resources("200m", "128Mi"))
	e.reconcile()

	reps := e.replicas()
	if len(reps) != 3 {
		t.Fatalf("got %d replicas, want 3", len(reps))
	}
	for _, r := range reps {
		if !strings.HasPrefix(r.Metadata.Name, "web-") || r.Spec.Workload != "web" || r.Spec.Template.GetImage() != "nginx:1.27" ||
			r.Spec.Template.Replicas != nil || r.Status.Phase != v1alpha1.ReplicaPending || !strings.Contains(r.Status.Message, "scheduled to") {
			t.Errorf("unexpected replica %v", r)
		}
	}
	if p := e.placement(); p["a"]+p["b"] != 3 || p["a"] == 0 || p["b"] == 0 {
		t.Fatalf("placement = %v, want spread over a and b", p)
	}
	if st := e.workloadStatus("web"); st.Replicas != 3 || st.ReadyReplicas != 0 || st.ObservedGeneration != 1 {
		t.Fatalf("workload status = %v", st)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.node("a", v1alpha1.NodeReady, 2000, 4096, 0)
	e.node("lost", v1alpha1.NodeNotReady, 2000, 4096, time.Hour)
	e.workload("web", 2)
	e.workload("tiny", 1, resources("100", "")) // never fits: stays unschedulable
	e.reconcile()
	rev := e.st.Revision()
	for i := range 3 {
		e.reconcile()
		if got := e.st.Revision(); got != rev {
			t.Fatalf("pass %d wrote to the store (revision %d -> %d)", i+2, rev, got)
		}
	}
}

func TestScheduling(t *testing.T) {
	tests := []struct {
		name      string
		nodes     func(e *env)
		workloads func(e *env)
		want      map[string]int // replicas per node; "" = unscheduled
		message   string         // expected in unscheduled replicas' status
	}{
		{
			name: "most free resources wins",
			nodes: func(e *env) {
				e.node("small", v1alpha1.NodeReady, 1000, 1024, 0)
				e.node("big", v1alpha1.NodeReady, 8000, 16384, 0)
			},
			workloads: func(e *env) { e.workload("web", 1, resources("500m", "256Mi")) },
			want:      map[string]int{"big": 1},
		},
		{
			name: "not ready nodes are skipped",
			nodes: func(e *env) {
				e.node("down", v1alpha1.NodeNotReady, 8000, 16384, 0)
				e.node("up", v1alpha1.NodeReady, 1000, 1024, 0)
			},
			workloads: func(e *env) { e.workload("web", 2) },
			want:      map[string]int{"up": 2},
		},
		{
			name:      "cpu capacity is respected",
			nodes:     func(e *env) { e.node("a", v1alpha1.NodeReady, 1000, 4096, 0) },
			workloads: func(e *env) { e.workload("web", 3, resources("400m", "")) },
			want:      map[string]int{"a": 2, "": 1},
			message:   "no ready node has cpu 400m free",
		},
		{
			name:      "memory capacity is respected",
			nodes:     func(e *env) { e.node("a", v1alpha1.NodeReady, 8000, 1024, 0) },
			workloads: func(e *env) { e.workload("web", 2, resources("", "600Mi")) },
			want:      map[string]int{"a": 1, "": 1},
			message:   "memory 600Mi",
		},
		{
			name: "host ports must not clash",
			nodes: func(e *env) {
				e.node("a", v1alpha1.NodeReady, 8000, 8192, 0)
				e.node("b", v1alpha1.NodeReady, 8000, 8192, 0)
			},
			workloads: func(e *env) { e.workload("web", 3, hostPort(8080)) },
			want:      map[string]int{"a": 1, "b": 1, "": 1},
			message:   "host ports 8080/tcp",
		},
		{
			name:      "no ready nodes",
			nodes:     func(e *env) {},
			workloads: func(e *env) { e.workload("web", 1) },
			want:      map[string]int{"": 1},
			message:   "no ready nodes",
		},
		{
			name: "unknown capacity means unlimited",
			nodes: func(e *env) {
				e.node("a", v1alpha1.NodeReady, 0, 0, 0)
			},
			workloads: func(e *env) { e.workload("web", 2, resources("64", "64Gi")) },
			want:      map[string]int{"a": 2},
		},
		{
			name: "replicas without requests spread by count",
			nodes: func(e *env) {
				e.node("a", v1alpha1.NodeReady, 2000, 2048, 0)
				e.node("b", v1alpha1.NodeReady, 2000, 2048, 0)
				e.node("c", v1alpha1.NodeReady, 2000, 2048, 0)
			},
			workloads: func(e *env) { e.workload("web", 6) },
			want:      map[string]int{"a": 2, "b": 2, "c": 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			tt.nodes(e)
			tt.workloads(e)
			e.reconcile()
			if got := e.placement(); !maps.Equal(got, tt.want) {
				t.Fatalf("placement = %v, want %v", got, tt.want)
			}
			for _, r := range e.replicas() {
				if r.Spec.Node == "" && !strings.Contains(r.Status.Message, tt.message) {
					t.Errorf("unscheduled replica message = %q, want containing %q", r.Status.Message, tt.message)
				}
			}
		})
	}
}

func TestUnschedulableReplicaIsPlacedWhenANodeArrives(t *testing.T) {
	e := newEnv(t)
	e.workload("web", 1)
	e.reconcile()
	if p := e.placement(); p[""] != 1 {
		t.Fatalf("placement = %v, want unscheduled", p)
	}
	e.node("a", v1alpha1.NodeReady, 1000, 1024, 0)
	e.reconcile()
	if p := e.placement(); p["a"] != 1 {
		t.Fatalf("placement = %v, want on a", p)
	}
}

func TestScaling(t *testing.T) {
	e := newEnv(t)
	e.node("a", v1alpha1.NodeReady, 8000, 8192, 0)
	e.workload("web", 3)
	e.reconcile()
	before := e.replicas()
	e.report(before[0].Metadata.Name, v1alpha1.ReplicaRunning)

	e.workload("web", 1)
	e.reconcile()
	after := e.replicas()
	if len(after) != 1 || after[0].Metadata.Name != before[0].Metadata.Name {
		t.Fatalf("scale down kept %v, want the running replica %s", after, before[0].Metadata.Name)
	}
	// Scaling doesn't touch the remaining replica.
	if after[0].Metadata.Generation != before[0].Metadata.Generation {
		t.Fatal("scaling changed the remaining replica's spec")
	}

	e.workload("web", 0)
	e.reconcile()
	if n := len(e.replicas()); n != 0 {
		t.Fatalf("%d replicas after scaling to 0", n)
	}
}

func TestTemplateChangeUpdatesAllReplicas(t *testing.T) {
	e := newEnv(t)
	e.node("a", v1alpha1.NodeReady, 8000, 8192, 0)
	e.workload("web", 2)
	e.reconcile()
	e.workload("web", 2, func(s *v1alpha1.WorkloadSpec) { s.Image = "nginx:1.28" })
	e.reconcile()
	for _, r := range e.replicas() {
		if r.Spec.Template.GetImage() != "nginx:1.28" || r.Spec.TemplateGeneration != 2 || r.Spec.Node != "a" {
			t.Fatalf("replica not updated in place: %v", r.Spec)
		}
	}
}

func TestDeletedWorkloadRemovesReplicas(t *testing.T) {
	e := newEnv(t)
	e.node("a", v1alpha1.NodeReady, 8000, 8192, 0)
	e.workload("web", 2)
	e.workload("api", 1)
	e.reconcile()
	if err := e.st.Delete(ctx, v1alpha1.KindWorkload, "web", 0); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	reps := e.replicas()
	if len(reps) != 1 || reps[0].Spec.Workload != "api" {
		t.Fatalf("replicas = %v, want only api's", reps)
	}
}

func TestEvictionFromLostNode(t *testing.T) {
	e := newEnv(t)
	e.node("a", v1alpha1.NodeReady, 8000, 8192, 0)
	e.workload("web", 2)
	e.reconcile()
	if p := e.placement(); p["a"] != 2 {
		t.Fatalf("placement = %v", p)
	}
	e.node("b", v1alpha1.NodeReady, 8000, 8192, 0)

	// a stops sending heartbeats: NotReady, but not long enough to evict.
	e.node("a", v1alpha1.NodeNotReady, 8000, 8192, 30*time.Second)
	e.reconcile()
	if p := e.placement(); p["a"] != 2 {
		t.Fatalf("evicted too early: %v", p)
	}

	e.node("a", v1alpha1.NodeNotReady, 8000, 8192, 61*time.Second)
	e.reconcile()
	if p := e.placement(); p["b"] != 2 || len(p) != 1 {
		t.Fatalf("after eviction placement = %v, want both on b", p)
	}
}

func TestDeletedNodeEvictsAtOnce(t *testing.T) {
	e := newEnv(t)
	e.node("a", v1alpha1.NodeReady, 8000, 8192, 0)
	e.workload("web", 1)
	e.reconcile()
	if err := e.st.Delete(ctx, v1alpha1.KindNode, "a", 0); err != nil {
		t.Fatal(err)
	}
	e.node("b", v1alpha1.NodeReady, 8000, 8192, 0)
	e.reconcile()
	if p := e.placement(); p["b"] != 1 || len(p) != 1 {
		t.Fatalf("placement = %v, want the replica moved to b", p)
	}
}

func TestDeletedReplicaIsReplaced(t *testing.T) {
	e := newEnv(t)
	e.node("a", v1alpha1.NodeReady, 8000, 8192, 0)
	e.workload("web", 2)
	e.reconcile()
	old := e.replicas()[0].Metadata.Name
	if err := e.st.Delete(ctx, v1alpha1.KindReplica, old, 0); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	reps := e.replicas()
	if len(reps) != 2 || slices.ContainsFunc(reps, func(r *v1alpha1.Replica) bool { return r.Metadata.Name == old }) {
		t.Fatalf("replicas = %v, want 2 without %s", reps, old)
	}
}

func TestWorkloadStatusCountsRunningReplicas(t *testing.T) {
	e := newEnv(t)
	e.node("a", v1alpha1.NodeReady, 8000, 8192, 0)
	e.workload("web", 3)
	e.reconcile()
	reps := e.replicas()
	e.report(reps[0].Metadata.Name, v1alpha1.ReplicaRunning)
	e.report(reps[1].Metadata.Name, v1alpha1.ReplicaRunning)
	e.report(reps[2].Metadata.Name, v1alpha1.ReplicaFailed)
	e.reconcile()
	if st := e.workloadStatus("web"); st.Replicas != 3 || st.ReadyReplicas != 2 {
		t.Fatalf("status = %v, want 2/3 ready", st)
	}
}

// failingStore fails spec updates of chosen records.
type failingStore struct {
	store.Store
	failUpdate map[string]bool
}

func (f *failingStore) Update(ctx context.Context, kind, name string, spec []byte, gen int64) (store.Record, error) {
	if f.failUpdate[name] {
		return store.Record{}, errors.New("disk on fire")
	}
	return f.Store.Update(ctx, kind, name, spec, gen)
}

func TestPartialFailureDoesNotBlockOthers(t *testing.T) {
	e := newEnv(t)
	e.node("a", v1alpha1.NodeReady, 8000, 8192, 0)
	e.workload("web", 3)
	e.reconcile()
	reps := e.replicas()
	stuck, lost := reps[0].Metadata.Name, reps[1].Metadata.Name

	// stuck needs scheduling again but its bind will fail; lost is gone
	// and must be replaced in the same pass.
	r, _ := e.st.Get(ctx, v1alpha1.KindReplica, stuck)
	rp, _ := objects.Replica(r)
	rp.Spec.Node = ""
	b, _ := objects.Encode(rp.Spec)
	if _, err := e.st.Update(ctx, v1alpha1.KindReplica, stuck, b, r.Generation); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Delete(ctx, v1alpha1.KindReplica, lost, 0); err != nil {
		t.Fatal(err)
	}
	fs := &failingStore{Store: e.st, failUpdate: map[string]bool{stuck: true}}
	e.withStore(fs)

	err := e.c.Reconcile(ctx)
	if err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("err = %v, want the injected failure reported", err)
	}
	if p := e.placement(); p["a"] != 2 || p[""] != 1 {
		t.Fatalf("placement = %v, want the replacement scheduled despite the failure", p)
	}

	delete(fs.failUpdate, stuck)
	e.reconcile()
	if p := e.placement(); p["a"] != 3 {
		t.Fatalf("placement = %v, want all scheduled once the store recovers", p)
	}
}
