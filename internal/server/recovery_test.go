package server

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/agent"
	"example.com/shoal/internal/network"
	"example.com/shoal/internal/runtime"
	"example.com/shoal/internal/runtime/fake"
)

// testAgent is an agent with an in-memory runtime, connected to a test
// server.
type testAgent struct {
	name string
	dir  string // credentials and cached assignments
	rt   *fake.Runtime
	net  *fakeNetwork
	stop func()
}

// fakeNetwork records what the agent asks of the node's networking.
type fakeNetwork struct {
	mu       sync.Mutex
	services []network.Service
	peers    []network.Peer
}

func (f *fakeNetwork) Run(ctx context.Context) error { <-ctx.Done(); return nil }

func (f *fakeNetwork) Sync(_ context.Context, services []network.Service, peers []network.Peer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services, f.peers = services, peers
	return nil
}

func (f *fakeNetwork) WireGuard() (string, int) { return "", 0 }

func (f *fakeNetwork) state() ([]network.Service, []network.Peer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.services, f.peers
}

// startAgent joins a new node and runs its agent.
func (ts *testServer) startAgent(t *testing.T, admin v1alpha1.ControlClient, name string) *testAgent {
	t.Helper()
	a := &testAgent{name: name, dir: t.TempDir(), rt: fake.New(nil), net: &fakeNetwork{}}
	ts.run(t, a, ts.token(t, admin, 1).GetToken())
	return a
}

// resume restarts a stopped agent, as after a reboot or the end of a
// network partition: same credentials, cache and containers, no token.
func (ts *testServer) resume(t *testing.T, a *testAgent) {
	t.Helper()
	ts.run(t, a, "")
}

func (ts *testServer) run(t *testing.T, a *testAgent, token string) {
	t.Helper()
	c, err := agent.NewConnected(context.Background(), agent.ConnectedConfig{
		Server: ts.addr, Token: token, CAHash: ts.CAHash(), NodeName: a.name,
		CredentialsPath: filepath.Join(a.dir, "credentials.conf"), AssignmentsPath: filepath.Join(a.dir, "assignments.pb"),
		Runtime: a.rt, Network: a.net, SyncInterval: 50 * time.Millisecond,
		Reconciler: agent.Config{PullWait: 50 * time.Millisecond, BackoffInitial: 10 * time.Millisecond, FilesDir: filepath.Join(a.dir, "files")},
		Version:    "test", Log: discard(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	var stopped bool
	a.stop = func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	t.Cleanup(a.stop)
}

// running returns the IDs of running containers.
func (a *testAgent) running(t *testing.T) []string {
	t.Helper()
	cs, err := a.rt.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range cs {
		if c.State == runtime.StateRunning {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

func fastControlPlane(c *Config) {
	c.NodeCheckInterval = 50 * time.Millisecond
	c.ControllerInterval = 50 * time.Millisecond
	c.AssignmentResync = time.Second
}

func ready(t *testing.T, admin v1alpha1.ControlClient, workload string) (int32, int32) {
	t.Helper()
	obj, err := admin.Get(context.Background(), &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: workload})
	if err != nil {
		t.Fatal(err)
	}
	return obj.GetWorkload().GetStatus().GetReadyReplicas(), obj.GetWorkload().GetSpec().GetReplicas()
}

func replicas(t *testing.T, admin v1alpha1.ControlClient) []*v1alpha1.Replica {
	t.Helper()
	resp, err := admin.List(context.Background(), &v1alpha1.ListRequest{Kind: v1alpha1.KindReplica})
	if err != nil {
		t.Fatal(err)
	}
	var out []*v1alpha1.Replica
	for _, o := range resp.GetItems() {
		out = append(out, o.GetReplica())
	}
	return out
}

func applyWeb(t *testing.T, admin v1alpha1.ControlClient, n int) {
	t.Helper()
	y := strings.Replace(webYAML, "replicas: 3", "replicas: "+strconv.Itoa(n), 1)
	if _, err := admin.Apply(context.Background(), &v1alpha1.ApplyRequest{Objects: decode(t, y)}); err != nil {
		t.Fatal(err)
	}
}

// TestRecovery is Phase 3's "done when": replicas are placed and run, a
// killed container is restarted, and the replicas of a lost agent move to
// the remaining node.
func TestRecovery(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{}, fastControlPlane)
	admin := ts.admin(t)
	ctx := context.Background()
	a1 := ts.startAgent(t, admin, "node-1")
	a2 := ts.startAgent(t, admin, "node-2")
	eventually(t, "both nodes ready", func() bool {
		return getNode(t, admin, "node-1").GetStatus().GetPhase() == NodeReady &&
			getNode(t, admin, "node-2").GetStatus().GetPhase() == NodeReady
	})

	applyWeb(t, admin, 4)
	eventually(t, "4 replicas running, spread over both nodes, workload 4/4 ready", func() bool {
		r, want := ready(t, admin, "web")
		return len(a1.running(t)) == 2 && len(a2.running(t)) == 2 && r == 4 && want == 4
	})
	for _, r := range replicas(t, admin) {
		if r.GetStatus().GetPhase() != v1alpha1.ReplicaRunning || r.GetStatus().GetIp() == "" {
			t.Fatalf("replica %s status = %v, want Running with an IP", r.GetMetadata().GetName(), r.GetStatus())
		}
	}

	// A container crashes: the agent restarts it and reports the restart.
	victim := a1.running(t)[0]
	a1.rt.Exit(victim, 1)
	eventually(t, "crashed container restarted and reported", func() bool {
		for _, r := range replicas(t, admin) {
			if r.GetMetadata().GetName() == victim {
				return r.GetStatus().GetRestarts() == 1 && r.GetStatus().GetPhase() == v1alpha1.ReplicaRunning
			}
		}
		return false
	})

	// A replica is deleted: the server replaces it and the agent removes
	// the old container.
	deleted := a2.running(t)[0]
	if _, err := admin.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindReplica, Name: deleted}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "deleted replica replaced", func() bool {
		ids := append(a1.running(t), a2.running(t)...)
		for _, id := range ids {
			if id == deleted {
				return false
			}
		}
		r, _ := ready(t, admin, "web")
		return len(ids) == 4 && r == 4
	})

	// A whole agent dies. After the eviction timeout its replicas run on
	// node-2. (Its containers keep running on node-1, which is cut off;
	// they would be removed when it reconnects.)
	a1.stop()
	ts.clock.advance(2 * time.Minute)
	eventually(t, "replicas moved to node-2", func() bool {
		r, _ := ready(t, admin, "web")
		return len(a2.running(t)) == 4 && r == 4
	})
	if n := getNode(t, admin, "node-1"); n.GetStatus().GetPhase() != NodeNotReady {
		t.Fatalf("node-1 phase = %q, want NotReady", n.GetStatus().GetPhase())
	}
	for _, r := range replicas(t, admin) {
		if r.GetSpec().GetNode() != "node-2" {
			t.Fatalf("replica %s still bound to %s", r.GetMetadata().GetName(), r.GetSpec().GetNode())
		}
	}

	// Scale down, then delete: the agent removes the containers.
	applyWeb(t, admin, 1)
	eventually(t, "scaled down to 1", func() bool { return len(a2.running(t)) == 1 })
	if _, err := admin.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindWorkload, Name: "web"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "workload's containers removed", func() bool {
		cs, _ := a2.rt.List(ctx)
		return len(cs) == 0 && len(replicas(t, admin)) == 0
	})
}

// TestReturningNodeDropsEvictedReplicas checks the other half of a
// partition: a node whose replicas were moved away keeps running its
// containers while cut off, and removes them when it comes back.
func TestReturningNodeDropsEvictedReplicas(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{}, fastControlPlane)
	admin := ts.admin(t)
	a1 := ts.startAgent(t, admin, "node-1")
	eventually(t, "node-1 ready", func() bool { return getNode(t, admin, "node-1").GetStatus().GetPhase() == NodeReady })
	applyWeb(t, admin, 2)
	eventually(t, "2 running on node-1", func() bool { return len(a1.running(t)) == 2 })

	// Cut node-1 off until its replicas are evicted. With no other node the
	// replacements stay unscheduled; the old containers keep running.
	a1.stop()
	ts.clock.advance(2 * time.Minute)
	eventually(t, "replicas evicted and replaced", func() bool {
		reps := replicas(t, admin)
		return len(reps) == 2 && reps[0].GetSpec().GetNode() == "" && reps[1].GetSpec().GetNode() == ""
	})
	stale := a1.running(t)
	if len(stale) != 2 {
		t.Fatalf("node-1 containers = %v, want the 2 old ones still running", stale)
	}

	// node-1 comes back: the replacements are scheduled to it and the old
	// containers are removed.
	ts.resume(t, a1)
	eventually(t, "replacements running, stale containers gone", func() bool {
		cs, _ := a1.rt.List(context.Background())
		if len(cs) != 2 || len(a1.running(t)) != 2 {
			return false
		}
		for _, c := range cs {
			if c.ID == stale[0] || c.ID == stale[1] {
				return false
			}
		}
		r, _ := ready(t, admin, "web")
		return r == 2
	})
}
