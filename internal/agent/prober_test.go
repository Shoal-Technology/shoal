package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"example.com/shoal/internal/runtime/fake"
)

func TestRunCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
		case "/moved":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()

	tests := []struct {
		name string
		hc   HealthCheck
		ok   bool
	}{
		{"http 200", HealthCheck{HTTPPath: "/ok", Port: port}, true},
		{"http redirect counts as success", HealthCheck{HTTPPath: "/moved", Port: port}, true},
		{"http 500", HealthCheck{HTTPPath: "/broken", Port: port}, false},
		{"tcp open", HealthCheck{Port: port}, true},
		{"tcp closed", HealthCheck{Port: closedPort}, false},
		{"http closed", HealthCheck{HTTPPath: "/", Port: closedPort}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := runCheck(ctx, nil, probeTarget{ip: host, check: tt.hc}); (err == nil) != tt.ok {
				t.Fatalf("runCheck = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

// healthEnv runs a reconciler whose probe result is controlled by the test.
type healthEnv struct {
	t       *testing.T
	rt      *fake.Runtime
	r       *Reconciler
	passing atomic.Bool
}

func newHealthEnv(t *testing.T) *healthEnv {
	e := &healthEnv{t: t, rt: fake.New(nil)}
	e.r = NewReconciler(e.rt, discardLogger(), Config{PullWait: 50 * time.Millisecond})
	e.r.prober.check = func(context.Context, probeTarget) error {
		if e.passing.Load() {
			return nil
		}
		return errors.New("connection refused")
	}
	t.Cleanup(e.r.Close)
	return e
}

func (e *healthEnv) pass(a Assignment) ReplicaStatus {
	e.t.Helper()
	st, err := e.r.Reconcile(context.Background(), []Assignment{a})
	if err != nil {
		e.t.Fatal(err)
	}
	return st[0]
}

func (e *healthEnv) until(a Assignment, desc string, cond func(ReplicaStatus) bool) ReplicaStatus {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := e.pass(a)
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s; last status %+v", desc, st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func withHealth(threshold int) func(*Assignment) {
	return func(a *Assignment) {
		a.Health = &HealthCheck{HTTPPath: "/", Port: 80, Interval: 50 * time.Millisecond, Timeout: time.Second, FailureThreshold: threshold}
	}
}

func TestReadyWithoutHealthCheck(t *testing.T) {
	e := newHealthEnv(t)
	a := assign("a1")
	a.TemplateGeneration = 7
	if st := e.pass(a); !st.Ready || st.Phase != PhaseRunning || st.TemplateGeneration != 7 {
		t.Fatalf("status = %+v, want running, ready, generation 7", st)
	}
}

func TestHealthCheckGatesReadinessAndRestarts(t *testing.T) {
	e := newHealthEnv(t)
	a := assign("a1", withHealth(3))

	st := e.pass(a)
	if st.Phase != PhaseRunning || st.Ready || !strings.Contains(st.Message, "waiting for the first health check") {
		t.Fatalf("just started: %+v, want running but not ready", st)
	}
	// Failing before the first success: not ready, but not restarted until
	// the threshold is reached.
	st = e.until(a, "first failure reported", func(s ReplicaStatus) bool { return strings.Contains(s.Message, "health check failed (") })
	if st.Ready || st.Restarts != 0 {
		t.Fatalf("after a failure below the threshold: %+v", st)
	}

	e.passing.Store(true)
	e.until(a, "ready", func(s ReplicaStatus) bool { return s.Ready && s.Message == "" })

	// The app stops answering: after 3 failures the container is restarted.
	e.passing.Store(false)
	before := e.pass(a).Restarts
	st = e.until(a, "restart after 3 failures", func(s ReplicaStatus) bool { return s.Restarts > before })
	if st.Ready || !strings.Contains(st.Message, "restarted: health check failed (3/3)") {
		t.Fatalf("after restart: %+v", st)
	}
	if calls := e.rt.Calls(); !containsAll(calls, "stop a1", "start a1") {
		t.Fatalf("calls = %q, want a stop and a start", calls)
	}

	// The new container gets a fresh probe and becomes ready again.
	e.passing.Store(true)
	e.until(a, "ready again", func(s ReplicaStatus) bool { return s.Ready && s.Restarts > before })
}

func TestProbesStopWithTheReplica(t *testing.T) {
	e := newHealthEnv(t)
	a := assign("a1", withHealth(3))
	e.pass(a)
	if n := len(e.r.prober.probes); n != 1 {
		t.Fatalf("%d probes running, want 1", n)
	}
	if _, err := e.r.Reconcile(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if n := len(e.r.prober.probes); n != 0 {
		t.Fatalf("%d probes still running after the replica was removed", n)
	}
}

func containsAll(have []string, want ...string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}
