package agent

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/security"
)

// haCluster tells the fake servers which one is the leader.
type haCluster struct {
	mu     sync.Mutex
	leader string
}

func (h *haCluster) setLeader(addr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leader = addr
}

func (h *haCluster) current() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.leader
}

// fakeServer answers Sync the way a server does: the leader accepts the
// session, and a follower refuses it, naming the leader.
type fakeServer struct {
	v1alpha1.UnimplementedAgentServer
	addr     string
	cluster  *haCluster
	srv      *grpc.Server
	accepted atomic.Int32
	refused  atomic.Int32
}

func (f *fakeServer) Sync(stream v1alpha1.Agent_SyncServer) error {
	leader := f.cluster.current()
	if leader != f.addr {
		f.refused.Add(1)
		if leader == "" {
			return status.Error(codes.FailedPrecondition, "not the leader; no leader is known yet")
		}
		return status.Error(codes.FailedPrecondition, leaderPrefix+leader)
	}
	if err := stream.Send(&v1alpha1.ServerMessage{Msg: &v1alpha1.ServerMessage_Config{
		Config: &v1alpha1.SyncConfig{HeartbeatInterval: durationpb.New(time.Hour)},
	}}); err != nil {
		return err
	}
	f.accepted.Add(1)
	for {
		if _, err := stream.Recv(); err != nil {
			return err
		}
	}
}

// startFake serves Sync over mutual TLS with the test CA.
func startFake(t *testing.T, ca *security.CA, cluster *haCluster) *fakeServer {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	key, err := security.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.IssueServer(key.Public(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{addr: lis.Addr().String(), cluster: cluster}
	f.srv = grpc.NewServer(grpc.Creds(credentials.NewTLS(security.ServerTLS(ca.Cert, leaf, key))))
	v1alpha1.RegisterAgentServer(f.srv, f)
	go f.srv.Serve(lis)
	t.Cleanup(f.srv.Stop)
	return f
}

// deadAddr returns an address that refuses connections.
func deadAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close()
	return addr
}

// haAgent returns an agent with a valid node certificate that starts at
// servers[0] and retries fast.
func haAgent(t *testing.T, ca *security.CA, servers ...string) *Connected {
	t.Helper()
	key, err := security.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.IssueClient(key.Public(), security.Identity{Name: "node-1", Role: security.RoleNode}, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &Connected{
		cfg:           ConnectedConfig{Servers: servers, Now: time.Now, Log: discardLogger()},
		log:           discardLogger(),
		creds:         &security.ClientConfig{Server: servers[0]},
		ca:            ca.Cert,
		cert:          &tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key, Leaf: cert},
		name:          "node-1",
		reconnect:     make(chan struct{}, 1),
		desired:       make(chan *v1alpha1.AssignmentSnapshot, 1),
		statusChanged: make(chan struct{}, 1),
		servers:       normalizeServers(servers),
		target:        servers[0],
		retry:         retryPolicy{min: time.Millisecond, max: 4 * time.Millisecond, denied: time.Millisecond},
	}
}

// runAgent runs c until the test ends.
func runAgent(t *testing.T, c *Connected) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func newTestCA(t *testing.T) *security.CA {
	t.Helper()
	ca, _, err := security.LoadOrCreateCA(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// savedServer returns the server named in the agent's credentials.
func (c *Connected) savedServer() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creds.Server
}

func TestFollowerRedirectsToLeader(t *testing.T) {
	ca := newTestCA(t)
	cluster := &haCluster{}
	follower := startFake(t, ca, cluster)
	leader := startFake(t, ca, cluster)
	cluster.setLeader(leader.addr)

	c := haAgent(t, ca, follower.addr, leader.addr)
	runAgent(t, c)
	waitFor(t, "session on the leader", func() bool {
		return leader.accepted.Load() > 0 && c.pickServer() == leader.addr && c.savedServer() == leader.addr
	})
	if follower.refused.Load() == 0 {
		t.Fatal("the follower was never asked")
	}
}

func TestFollowerHintAdoptsLeaderOutsideKnownServers(t *testing.T) {
	ca := newTestCA(t)
	cluster := &haCluster{}
	follower := startFake(t, ca, cluster)
	leader := startFake(t, ca, cluster)
	cluster.setLeader(leader.addr)

	// Only the follower is configured. The leader is learned from the hint and
	// accepted because its certificate chains to the cluster CA.
	c := haAgent(t, ca, follower.addr)
	runAgent(t, c)
	waitFor(t, "session on the hinted leader", func() bool {
		return leader.accepted.Load() > 0 && c.pickServer() == leader.addr && c.savedServer() == leader.addr
	})
	c.mu.Lock()
	known := append([]string(nil), c.servers...)
	c.mu.Unlock()
	if len(known) != 2 || known[1] != leader.addr {
		t.Fatalf("known servers = %v, want the leader added", known)
	}
}

func TestLeaderDeathReconnectsToNewLeader(t *testing.T) {
	ca := newTestCA(t)
	cluster := &haCluster{}
	a := startFake(t, ca, cluster)
	b := startFake(t, ca, cluster)
	cluster.setLeader(a.addr)

	c := haAgent(t, ca, a.addr, b.addr)
	runAgent(t, c)
	waitFor(t, "session on the first leader", func() bool { return a.accepted.Load() > 0 })

	// The leader dies. The follower keeps naming it until the next leader is
	// elected, so the agent's attempts must keep going until b takes over.
	a.srv.Stop()
	cluster.setLeader(b.addr)
	waitFor(t, "session on the new leader", func() bool {
		return b.accepted.Load() > 0 && c.pickServer() == b.addr && c.savedServer() == b.addr
	})
}

func TestAllServersDownBackoffIsBounded(t *testing.T) {
	ca := newTestCA(t)
	c := haAgent(t, ca, deadAddr(t), deadAddr(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		mu    sync.Mutex
		waits []time.Duration
	)
	c.retry.wait = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		n := len(waits)
		mu.Unlock()
		if n == 8 {
			cancel()
		}
		return ctx.Err()
	}
	done := make(chan struct{})
	go func() { _ = c.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("agent did not stop")
	}

	pol := c.policy()
	mu.Lock()
	defer mu.Unlock()
	if len(waits) != 8 {
		t.Fatalf("got %d waits, want 8", len(waits))
	}
	// Equal jitter: attempt i waits in [d/2, d], d = min(min*2^i, max).
	d := pol.min
	for i, w := range waits {
		if w < d/2 || w > d {
			t.Fatalf("wait %d = %s, want within [%s, %s]", i, w, d/2, d)
		}
		if w > pol.max {
			t.Fatalf("wait %d = %s exceeds the cap %s", i, w, pol.max)
		}
		d = min(d*2, pol.max)
	}
	if waits[len(waits)-1] < pol.max/2 {
		t.Fatalf("backoff never reached the cap: last wait %s", waits[len(waits)-1])
	}
}

func TestSingleServerKeepsItsAddress(t *testing.T) {
	ca := newTestCA(t)
	cluster := &haCluster{} // no leader yet: the server refuses without a hint
	s := startFake(t, ca, cluster)

	c := haAgent(t, ca, s.addr)
	runAgent(t, c)
	waitFor(t, "a few refused attempts", func() bool { return s.refused.Load() >= 2 })
	if c.pickServer() != s.addr {
		t.Fatalf("target moved to %q with a single server", c.pickServer())
	}
	cluster.setLeader(s.addr)
	waitFor(t, "session once the server leads", func() bool { return s.accepted.Load() > 0 })
	if got := c.savedServer(); got != s.addr {
		t.Fatalf("saved server = %q, want %q", got, s.addr)
	}
	c.mu.Lock()
	known := len(c.servers)
	c.mu.Unlock()
	if known != 1 {
		t.Fatalf("known servers = %d, want 1", known)
	}
}

func TestMoveOnRotatesAndHintsWin(t *testing.T) {
	c := &Connected{servers: []string{"a", "b", "c"}, target: "a"}
	for _, want := range []string{"b", "c", "a"} {
		c.moveOn(c.pickServer())
		if got := c.pickServer(); got != want {
			t.Fatalf("after a failure target = %q, want %q", got, want)
		}
	}
	c.target = "a"
	if !c.followHint("x") {
		t.Fatal("first hint refused")
	}
	c.moveOn("a") // the hint already chose the next server
	if got := c.pickServer(); got != "x" {
		t.Fatalf("target = %q after a hint, want x", got)
	}
}

func TestHintChainIsBounded(t *testing.T) {
	c := &Connected{servers: []string{"a"}, target: "a"}
	for i, leader := range []string{"b", "c"} {
		if !c.followHint(leader) {
			t.Fatalf("hint %d refused", i+1)
		}
	}
	if c.followHint("d") {
		t.Fatal("a third hint without a success was followed")
	}
	c.served("c")
	if !c.followHint("d") {
		t.Fatal("hints refused after a success")
	}
}

func TestLeaderHint(t *testing.T) {
	cases := []struct {
		err  error
		want string
		ok   bool
	}{
		{status.Error(codes.FailedPrecondition, leaderPrefix+"10.0.0.5:7443"), "10.0.0.5:7443", true},
		{status.Error(codes.FailedPrecondition, "not the leader; no leader is known yet"), "", false},
		{status.Error(codes.FailedPrecondition, leaderPrefix), "", false},
		{status.Error(codes.Unavailable, leaderPrefix+"10.0.0.5:7443"), "", false},
		{nil, "", false},
	}
	for _, tc := range cases {
		got, ok := leaderHint(tc.err)
		if got != tc.want || ok != tc.ok {
			t.Errorf("leaderHint(%v) = %q, %v; want %q, %v", tc.err, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNormalizeServers(t *testing.T) {
	got := normalizeServers([]string{"a:1", " b:2 ", "", "a:1"})
	if len(got) != 2 || got[0] != "a:1" || got[1] != "b:2" {
		t.Fatalf("normalizeServers = %q", got)
	}
}

func TestJitteredStaysInRange(t *testing.T) {
	for i := 0; i < 200; i++ {
		if w := jittered(time.Second); w < 500*time.Millisecond || w > time.Second {
			t.Fatalf("jittered(1s) = %s", w)
		}
	}
}
