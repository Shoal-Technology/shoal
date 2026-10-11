package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/runtime"
	"github.com/Shoal-Technology/shoal/internal/runtime/fake"
)

// newOffline returns a Connected that has never reached a server, for
// testing the reconcile side alone.
func newOffline(rt runtime.Runtime, cache string) *Connected {
	return &Connected{
		cfg: ConnectedConfig{
			AssignmentsPath: cache, Runtime: rt, SyncInterval: 20 * time.Millisecond,
			Reconciler: Config{PullWait: 20 * time.Millisecond}, Log: discardLogger(),
		},
		log:           discardLogger(),
		desired:       make(chan *v1alpha1.AssignmentSnapshot, 1),
		statusChanged: make(chan struct{}, 1),
	}
}

func runLoop(t *testing.T, c *Connected) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.reconcileLoop(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", desc)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func snapshotOf(ids ...string) *v1alpha1.AssignmentSnapshot {
	s := &v1alpha1.AssignmentSnapshot{Version: "test"}
	for _, id := range ids {
		s.Assignments = append(s.Assignments, &v1alpha1.Assignment{
			Replica: id, Workload: "web",
			Template: &v1alpha1.WorkloadSpec{
				Image: "nginx", Env: []*v1alpha1.EnvVar{{Name: "A", Value: "1"}},
				Resources: &v1alpha1.Resources{Cpu: "200m", Memory: "64Mi"}, RestartPolicy: "Always",
			},
		})
	}
	return s
}

func TestNoCacheMeansHandsOff(t *testing.T) {
	rt := fake.New(nil)
	addExisting(rt, assign("survivor"), running(1))
	c := newOffline(rt, filepath.Join(t.TempDir(), "assignments.pb"))
	runLoop(t, c)
	time.Sleep(100 * time.Millisecond) // several sync intervals
	if calls := rt.Calls(); len(calls) != 0 {
		t.Fatalf("agent without any desired state touched containers: %q", calls)
	}
}

func TestCachedAssignmentsSurviveRestart(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "assignments.pb")
	first := newOffline(fake.New(nil), cache)
	first.setAssignments(snapshotOf("web-a", "web-b"))

	// A new agent process, server unreachable: it runs the cached replicas.
	rt := fake.New(nil)
	c := newOffline(rt, cache)
	runLoop(t, c)
	waitFor(t, "cached replicas running", func() bool {
		cs, _ := rt.List(context.Background())
		return len(cs) == 2 && cs[0].State == runtime.StateRunning && cs[1].State == runtime.StateRunning
	})
	st, _ := rt.Status(context.Background(), "web-a")
	if st.Labels[LabelWorkload] != "web" {
		t.Fatalf("labels = %v", st.Labels)
	}
	waitFor(t, "statuses for the heartbeat", func() bool { return len(c.heartbeat().GetReplicas()) == 2 })

	// A new snapshot from the server replaces the cached one.
	c.setAssignments(snapshotOf("web-a"))
	waitFor(t, "web-b removed", func() bool {
		cs, _ := rt.List(context.Background())
		return len(cs) == 1 && cs[0].ID == "web-a"
	})
}

func TestAssignmentFromAPI(t *testing.T) {
	a, err := AssignmentFromAPI(snapshotOf("web-a").Assignments[0])
	if err != nil {
		t.Fatal(err)
	}
	if a.ReplicaID != "web-a" || a.Workload != "web" || a.Spec.Image != "nginx" || a.Spec.CPUMillis != 200 ||
		a.Spec.MemoryBytes != 64<<20 || len(a.Spec.Env) != 1 || a.Spec.Env[0] != "A=1" ||
		a.RestartPolicy != RestartAlways || a.Desired != DesiredRunning {
		t.Fatalf("unexpected assignment %+v", a)
	}
	bad := snapshotOf("x").Assignments[0]
	bad.Template.Resources.Cpu = "lots"
	if _, err := AssignmentFromAPI(bad); err == nil {
		t.Fatal("expected an error for an invalid quantity")
	}
}
