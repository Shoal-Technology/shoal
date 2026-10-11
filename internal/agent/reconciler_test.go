package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/internal/runtime"
	"github.com/Shoal-Technology/shoal/internal/runtime/fake"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time              { return c.t }
func (c *clock) advance(d time.Duration)     { c.t = c.t.Add(d) }
func discardLogger() *slog.Logger            { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func newClock() *clock                       { return &clock{t: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)} }
func withImage(img string) func(*Assignment) { return func(a *Assignment) { a.Spec.Image = img } }
func withPolicy(p RestartPolicy) func(*Assignment) {
	return func(a *Assignment) { a.RestartPolicy = p }
}
func withDesired(d DesiredState) func(*Assignment) { return func(a *Assignment) { a.Desired = d } }

func assign(id string, opts ...func(*Assignment)) Assignment {
	a := Assignment{
		ReplicaID:     id,
		Workload:      "web",
		Spec:          runtime.ContainerSpec{Image: "busybox", Env: []string{"A=1"}},
		RestartPolicy: RestartAlways,
		Desired:       DesiredRunning,
	}
	for _, o := range opts {
		o(&a)
	}
	return a
}

// addExisting inserts a container as the agent would have created it for a.
func addExisting(rt *fake.Runtime, a Assignment, st runtime.ContainerStatus) {
	rt.AddContainer(a.containerSpec(), st)
}

func running(starts int) runtime.ContainerStatus {
	return runtime.ContainerStatus{State: runtime.StateRunning, Starts: starts}
}

func exited(code int) runtime.ContainerStatus {
	return runtime.ContainerStatus{State: runtime.StateExited, ExitCode: code, Starts: 1}
}

// stoppedByAgent is a container that exited because the agent stopped it.
func stoppedByAgent(code int) runtime.ContainerStatus {
	st := exited(code)
	st.StopRequested = true
	return st
}

func newReconciler(rt runtime.Runtime, c *clock) *Reconciler {
	return NewReconciler(rt, discardLogger(), Config{Now: c.now})
}

func TestReconcile(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(rt *fake.Runtime)
		desired    []Assignment
		wantCalls  []string
		wantPhases map[string]Phase
		wantErr    bool
	}{
		{
			name:       "creates and starts missing replica",
			desired:    []Assignment{assign("a1")},
			wantCalls:  []string{"pull busybox", "create a1", "start a1"},
			wantPhases: map[string]Phase{"a1": PhaseRunning},
		},
		{
			name:       "running replica is left alone",
			setup:      func(rt *fake.Runtime) { addExisting(rt, assign("a1"), running(1)) },
			desired:    []Assignment{assign("a1")},
			wantPhases: map[string]Phase{"a1": PhaseRunning},
		},
		{
			name: "created but never started replica is started",
			setup: func(rt *fake.Runtime) {
				addExisting(rt, assign("a1"), runtime.ContainerStatus{State: runtime.StateCreated})
			},
			desired:    []Assignment{assign("a1")},
			wantCalls:  []string{"start a1"},
			wantPhases: map[string]Phase{"a1": PhaseRunning},
		},
		{
			name:       "Always restarts exited replica",
			setup:      func(rt *fake.Runtime) { addExisting(rt, assign("a1"), exited(0)) },
			desired:    []Assignment{assign("a1")},
			wantCalls:  []string{"start a1"},
			wantPhases: map[string]Phase{"a1": PhaseRunning},
		},
		{
			name:       "OnFailure keeps successful exit",
			setup:      func(rt *fake.Runtime) { addExisting(rt, assign("a1", withPolicy(RestartOnFailure)), exited(0)) },
			desired:    []Assignment{assign("a1", withPolicy(RestartOnFailure))},
			wantPhases: map[string]Phase{"a1": PhaseSucceeded},
		},
		{
			name:       "OnFailure restarts failed exit",
			setup:      func(rt *fake.Runtime) { addExisting(rt, assign("a1", withPolicy(RestartOnFailure)), exited(2)) },
			desired:    []Assignment{assign("a1", withPolicy(RestartOnFailure))},
			wantCalls:  []string{"start a1"},
			wantPhases: map[string]Phase{"a1": PhaseRunning},
		},
		{
			name:       "OnFailure restarts container whose task was lost",
			setup:      func(rt *fake.Runtime) { addExisting(rt, assign("a1", withPolicy(RestartOnFailure)), exited(-1)) },
			desired:    []Assignment{assign("a1", withPolicy(RestartOnFailure))},
			wantCalls:  []string{"start a1"},
			wantPhases: map[string]Phase{"a1": PhaseRunning},
		},
		{
			name:       "Never leaves failed replica",
			setup:      func(rt *fake.Runtime) { addExisting(rt, assign("a1", withPolicy(RestartNever)), exited(1)) },
			desired:    []Assignment{assign("a1", withPolicy(RestartNever))},
			wantPhases: map[string]Phase{"a1": PhaseFailed},
		},
		{
			name:      "unassigned container is stopped and removed",
			setup:     func(rt *fake.Runtime) { addExisting(rt, assign("orphan"), running(1)) },
			desired:   nil,
			wantCalls: []string{"stop orphan", "remove orphan"},
		},
		{
			name:       "changed spec recreates container",
			setup:      func(rt *fake.Runtime) { addExisting(rt, assign("a1", withImage("busybox:old")), running(1)) },
			desired:    []Assignment{assign("a1")},
			wantCalls:  []string{"pull busybox", "stop a1", "remove a1", "create a1", "start a1"},
			wantPhases: map[string]Phase{"a1": PhaseRunning},
		},
		{
			name:       "restart policy change does not recreate",
			setup:      func(rt *fake.Runtime) { addExisting(rt, assign("a1"), running(1)) },
			desired:    []Assignment{assign("a1", withPolicy(RestartNever))},
			wantPhases: map[string]Phase{"a1": PhaseRunning},
		},
		{
			name:       "desired Stopped stops running replica",
			setup:      func(rt *fake.Runtime) { addExisting(rt, assign("a1"), running(1)) },
			desired:    []Assignment{assign("a1", withDesired(DesiredStopped))},
			wantCalls:  []string{"stop a1"},
			wantPhases: map[string]Phase{"a1": PhaseStopped},
		},
		{
			name:       "desired Stopped on exited replica is a no-op",
			setup:      func(rt *fake.Runtime) { addExisting(rt, assign("a1"), exited(1)) },
			desired:    []Assignment{assign("a1", withDesired(DesiredStopped))},
			wantPhases: map[string]Phase{"a1": PhaseStopped},
		},
		{
			name:       "Never resumes replica stopped by the agent",
			setup:      func(rt *fake.Runtime) { addExisting(rt, assign("a1", withPolicy(RestartNever)), stoppedByAgent(143)) },
			desired:    []Assignment{assign("a1", withPolicy(RestartNever))},
			wantCalls:  []string{"start a1"},
			wantPhases: map[string]Phase{"a1": PhaseRunning},
		},
		{
			name:       "OnFailure resumes replica that exited cleanly when stopped by the agent",
			setup:      func(rt *fake.Runtime) { addExisting(rt, assign("a1", withPolicy(RestartOnFailure)), stoppedByAgent(0)) },
			desired:    []Assignment{assign("a1", withPolicy(RestartOnFailure))},
			wantCalls:  []string{"start a1"},
			wantPhases: map[string]Phase{"a1": PhaseRunning},
		},
		{
			name:       "desired Stopped for absent replica creates nothing",
			desired:    []Assignment{assign("a1", withDesired(DesiredStopped))},
			wantPhases: map[string]Phase{"a1": PhaseStopped},
		},
		{
			name:       "pull failure does not block other replicas",
			setup:      func(rt *fake.Runtime) { rt.FailOn("pull", "bad", errors.New("registry down")) },
			desired:    []Assignment{assign("a2", withImage("bad")), assign("a1")},
			wantCalls:  []string{"pull busybox", "create a1", "start a1", "pull bad"},
			wantPhases: map[string]Phase{"a1": PhaseRunning, "a2": PhasePending},
			wantErr:    true,
		},
		{
			name:       "start failure is reported",
			setup:      func(rt *fake.Runtime) { rt.FailOn("start", "a1", errors.New("boom")) },
			desired:    []Assignment{assign("a1")},
			wantCalls:  []string{"pull busybox", "create a1", "start a1"},
			wantPhases: map[string]Phase{"a1": PhasePending},
			wantErr:    true,
		},
		{
			name: "remove failure of orphan is reported but desired still reconciled",
			setup: func(rt *fake.Runtime) {
				addExisting(rt, assign("orphan"), running(1))
				rt.FailOn("remove", "orphan", errors.New("busy"))
			},
			desired:    []Assignment{assign("a1")},
			wantCalls:  []string{"stop orphan", "remove orphan", "pull busybox", "create a1", "start a1"},
			wantPhases: map[string]Phase{"a1": PhaseRunning},
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClock()
			rt := fake.New(c.now)
			if tt.setup != nil {
				tt.setup(rt)
			}
			r := newReconciler(rt, c)
			statuses, err := r.Reconcile(context.Background(), tt.desired)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Reconcile() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got := rt.Calls(); !slices.Equal(got, tt.wantCalls) {
				t.Errorf("calls = %q, want %q", got, tt.wantCalls)
			}
			gotPhases := map[string]Phase{}
			for _, st := range statuses {
				gotPhases[st.ReplicaID] = st.Phase
			}
			if len(tt.wantPhases) == 0 {
				tt.wantPhases = map[string]Phase{}
			}
			if !mapsEqual(gotPhases, tt.wantPhases) {
				t.Errorf("phases = %v, want %v", gotPhases, tt.wantPhases)
			}
		})
	}
}

func mapsEqual(a, b map[string]Phase) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestReconcileIsIdempotent(t *testing.T) {
	c := newClock()
	rt := fake.New(c.now)
	addExisting(rt, assign("orphan"), running(1))
	r := newReconciler(rt, c)
	desired := []Assignment{assign("a1"), assign("a2"), assign("a3", withDesired(DesiredStopped))}

	if _, err := r.Reconcile(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	rt.Calls()
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), desired); err != nil {
			t.Fatal(err)
		}
		if calls := rt.Calls(); len(calls) != 0 {
			t.Fatalf("pass %d made mutating calls %q; want none", i+2, calls)
		}
	}
}

func TestRestartBackoff(t *testing.T) {
	c := newClock()
	rt := fake.New(c.now)
	r := newReconciler(rt, c)
	desired := []Assignment{assign("a1")}
	reconcile := func() ReplicaStatus {
		t.Helper()
		st, err := r.Reconcile(context.Background(), desired)
		if err != nil {
			t.Fatal(err)
		}
		return st[0]
	}
	expectStart := func(want bool) {
		t.Helper()
		started := slices.Contains(rt.Calls(), "start a1")
		if started != want {
			t.Fatalf("at +%s: started = %v, want %v", c.t.Sub(newClock().t), started, want)
		}
	}

	reconcile() // initial start: next attempt allowed after 1s
	expectStart(true)

	steps := []struct {
		advance time.Duration
		start   bool
	}{
		{0, false},               // crashed immediately: wait 1s
		{time.Second, true},      // restart #1; next delay 2s
		{time.Second, false},     // 1s of 2s
		{time.Second, true},      // restart #2; next delay 4s
		{3 * time.Second, false}, // 3s of 4s
		{time.Second, true},      // restart #3
	}
	for i, s := range steps {
		rt.Exit("a1", 1)
		c.advance(s.advance)
		st := reconcile()
		expectStart(s.start)
		if !s.start && (st.Phase != PhaseFailed || !strings.Contains(st.Message, "back-off")) {
			t.Fatalf("step %d: status = %+v, want Failed with backoff message", i, st)
		}
	}
	if st := reconcile(); st.Restarts != 3 {
		t.Fatalf("restarts = %d, want 3", st.Restarts)
	}

	// Running for longer than BackoffReset clears the backoff.
	c.advance(10 * time.Minute)
	reconcile()
	rt.Exit("a1", 1)
	reconcile()
	expectStart(true)
}

// TestStopThenRun toggles desired state: a replica with restart policy Never
// must come back when it is set to Running again after the agent stopped it,
// but must stay down after exiting on its own.
func TestStopThenRun(t *testing.T) {
	c := newClock()
	rt := fake.New(c.now)
	r := newReconciler(rt, c)
	pass := func(d DesiredState) ReplicaStatus {
		t.Helper()
		st, err := r.Reconcile(context.Background(), []Assignment{assign("a1", withPolicy(RestartNever), withDesired(d))})
		if err != nil {
			t.Fatal(err)
		}
		return st[0]
	}

	if st := pass(DesiredRunning); st.Phase != PhaseRunning {
		t.Fatalf("initial: phase = %s, want Running", st.Phase)
	}
	if st := pass(DesiredStopped); st.Phase != PhaseStopped {
		t.Fatalf("after stop: phase = %s, want Stopped", st.Phase)
	}
	rt.Calls()
	if st := pass(DesiredRunning); st.Phase != PhaseRunning {
		t.Fatalf("after resume: phase = %s, want Running", st.Phase)
	}
	if calls := rt.Calls(); !slices.Equal(calls, []string{"start a1"}) {
		t.Fatalf("resume calls = %q, want [start a1]", calls)
	}

	rt.Exit("a1", 1)
	if st := pass(DesiredRunning); st.Phase != PhaseFailed {
		t.Fatalf("after own exit: phase = %s, want Failed", st.Phase)
	}
	if calls := rt.Calls(); len(calls) != 0 {
		t.Fatalf("calls after own exit = %q, want none", calls)
	}
}

// reconcileUntil runs passes (in real time, pulls are asynchronous) until
// cond holds for the statuses, or fails after a few seconds.
func reconcileUntil(t *testing.T, r *Reconciler, desired []Assignment, cond func(map[string]ReplicaStatus) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		statuses, err := r.Reconcile(context.Background(), desired)
		if err != nil {
			t.Fatal(err)
		}
		byID := map[string]ReplicaStatus{}
		for _, st := range statuses {
			byID[st.ReplicaID] = st
		}
		if cond(byID) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not reached; statuses = %+v", statuses)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSlowPullDoesNotBlockOtherReplicas(t *testing.T) {
	c := newClock()
	rt := fake.New(c.now)
	release := rt.HoldPull("slow")
	defer release()
	// Long enough for the instant fake pull of a2's image to finish within
	// the pass even on a loaded machine.
	r := NewReconciler(rt, discardLogger(), Config{Now: c.now, PullWait: 200 * time.Millisecond})
	defer r.Close()
	desired := []Assignment{assign("a1", withImage("slow")), assign("a2")}

	statuses, err := r.Reconcile(context.Background(), desired)
	if err != nil {
		t.Fatal(err)
	}
	if st := statuses[0]; st.Phase != PhasePending || !strings.Contains(st.Message, "pulling image") {
		t.Fatalf("a1 = %+v, want Pending while pulling", st)
	}
	if st := statuses[1]; st.Phase != PhaseRunning {
		t.Fatalf("a2 = %+v, want Running in the same pass", st)
	}
	// A second pass must not start another pull of the same image.
	if _, err := r.Reconcile(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	calls := rt.Calls()
	if n := len(calls) - len(slices.DeleteFunc(slices.Clone(calls), func(s string) bool { return s == "pull slow" })); n != 1 {
		t.Fatalf("image pulled %d times, want once: %q", n, calls)
	}

	release()
	reconcileUntil(t, r, desired, func(s map[string]ReplicaStatus) bool { return s["a1"].Phase == PhaseRunning })
}

func TestImageChangeKeepsOldContainerDuringPull(t *testing.T) {
	c := newClock()
	rt := fake.New(c.now)
	addExisting(rt, assign("a1", withImage("old")), running(1))
	release := rt.HoldPull("new")
	defer release()
	r := NewReconciler(rt, discardLogger(), Config{Now: c.now, PullWait: 10 * time.Millisecond})
	defer r.Close()
	desired := []Assignment{assign("a1", withImage("new"))}

	statuses, err := r.Reconcile(context.Background(), desired)
	if err != nil {
		t.Fatal(err)
	}
	if st := statuses[0]; st.Phase != PhaseRunning || !strings.Contains(st.Message, "pulling image") {
		t.Fatalf("status = %+v, want old container Running while pulling", st)
	}
	if calls := rt.Calls(); slices.Contains(calls, "stop a1") || slices.Contains(calls, "remove a1") {
		t.Fatalf("old container touched before the new image was ready: %q", calls)
	}

	release()
	reconcileUntil(t, r, desired, func(s map[string]ReplicaStatus) bool {
		return s["a1"].Phase == PhaseRunning && s["a1"].Message == ""
	})
	if st, _ := rt.Status(context.Background(), "a1"); st.Image != "new" {
		t.Fatalf("container image = %q, want new", st.Image)
	}
}

// TestOutdatedContainerReportsItsOwnGeneration is a regression test: while
// the old container keeps running during the new image's pull, its status
// must carry the old template generation. Reporting the new one made the
// server take the old container's readiness for the new template's.
func TestOutdatedContainerReportsItsOwnGeneration(t *testing.T) {
	c := newClock()
	rt := fake.New(c.now)
	old := assign("a1", withImage("old"))
	old.TemplateGeneration = 1
	addExisting(rt, old, running(1))
	release := rt.HoldPull("new")
	defer release()
	r := NewReconciler(rt, discardLogger(), Config{Now: c.now, PullWait: 10 * time.Millisecond})
	defer r.Close()
	next := assign("a1", withImage("new"))
	next.TemplateGeneration = 2

	statuses, err := r.Reconcile(context.Background(), []Assignment{next})
	if err != nil {
		t.Fatal(err)
	}
	if st := statuses[0]; st.TemplateGeneration != 1 || !st.Ready {
		t.Fatalf("during the pull: %+v, want the old container (generation 1, ready)", st)
	}
	release()
	reconcileUntil(t, r, []Assignment{next}, func(s map[string]ReplicaStatus) bool {
		return s["a1"].TemplateGeneration == 2 && s["a1"].Phase == PhaseRunning
	})
}

func TestCloseCancelsPulls(t *testing.T) {
	c := newClock()
	rt := fake.New(c.now)
	rt.HoldPull("slow") // never released: only Close can end the pull
	r := NewReconciler(rt, discardLogger(), Config{Now: c.now, PullWait: time.Millisecond})
	if _, err := r.Reconcile(context.Background(), []Assignment{assign("a1", withImage("slow"))}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { r.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel the pull in progress")
	}
}

func TestReconcileListFailure(t *testing.T) {
	c := newClock()
	rt := fake.New(c.now)
	rt.FailOn("list", "*", errors.New("containerd down"))
	r := newReconciler(rt, c)
	if _, err := r.Reconcile(context.Background(), []Assignment{assign("a1")}); err == nil {
		t.Fatal("expected error when listing fails")
	}
	if calls := rt.Calls(); len(calls) != 0 {
		t.Fatalf("calls = %q, want none", calls)
	}
}

func TestReconcileCancelledContext(t *testing.T) {
	c := newClock()
	rt := fake.New(c.now)
	r := newReconciler(rt, c)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Reconcile(ctx, []Assignment{assign("a1")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls := rt.Calls(); len(calls) != 0 {
		t.Fatalf("calls = %q, want none", calls)
	}
}

func TestContainerSpecLabels(t *testing.T) {
	a := assign("a1")
	a.Spec.Labels = map[string]string{"custom": "x"}
	s := a.containerSpec()
	if s.ID != "a1" || s.Labels[LabelReplicaID] != "a1" || s.Labels[LabelWorkload] != "web" ||
		s.Labels[LabelSpecHash] != a.specHash() || s.Labels["custom"] != "x" {
		t.Fatalf("unexpected spec: %+v", s)
	}
	if _, ok := a.Spec.Labels[LabelSpecHash]; ok {
		t.Fatal("containerSpec mutated the assignment's labels")
	}
	b := assign("a1", withPolicy(RestartNever), withDesired(DesiredStopped))
	if a.specHash() != b.specHash() {
		t.Error("restart policy / desired state must not change the spec hash")
	}
	if a.specHash() == assign("a1", withImage("nginx")).specHash() {
		t.Error("image change must change the spec hash")
	}
}
