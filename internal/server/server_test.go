package server

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/agent"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/security"
)

// clock is real time plus an adjustable offset: TLS checks use real time,
// so tests only move the server's view of time forward where that's safe.
type clock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.off += d
}

// discard returns the logger for test servers and agents: silent, unless
// SHOAL_TEST_LOG is set (then debug output goes to stderr).
func discard() *slog.Logger {
	if os.Getenv("SHOAL_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type testServer struct {
	*Server
	addr    string
	dataDir string
	clock   *clock
	stop    func()
}

func startServer(t *testing.T, dataDir string, c *clock, opts ...func(*Config)) *testServer {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := Config{
		DataDir: dataDir, AdvertiseAddress: lis.Addr().String(), Log: discard(), Now: c.now,
		HeartbeatInterval: 100 * time.Millisecond, NodeCheckInterval: time.Hour, // checks are triggered by hand
	}
	for _, o := range opts {
		o(&cfg)
	}
	s, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, lis) }()
	ts := &testServer{Server: s, addr: lis.Addr().String(), dataDir: dataDir, clock: c}
	var once sync.Once
	ts.stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Serve: %v", err)
			}
			s.Close()
		})
	}
	t.Cleanup(ts.stop)
	return ts
}

func (ts *testServer) admin(t *testing.T) v1alpha1.ControlClient {
	t.Helper()
	cfg, err := security.LoadClientConfig(AdminConfigPath(ts.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	p, err := cfg.Parse()
	if err != nil {
		t.Fatal(err)
	}
	return v1alpha1.NewControlClient(ts.conn(t, p.TLS()))
}

func (ts *testServer) conn(t *testing.T, tc *tls.Config) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(ts.addr, grpc.WithTransportCredentials(credentials.NewTLS(tc)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func (ts *testServer) token(t *testing.T, admin v1alpha1.ControlClient, maxUses int32) *v1alpha1.CreateJoinTokenResponse {
	t.Helper()
	resp, err := admin.CreateJoinToken(context.Background(), &v1alpha1.CreateJoinTokenRequest{MaxUses: maxUses, Ttl: durationpb.New(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (ts *testServer) newAgent(t *testing.T, tok *v1alpha1.CreateJoinTokenResponse, name, credsPath string) (*agent.Connected, error) {
	t.Helper()
	return agent.NewConnected(context.Background(), agent.ConnectedConfig{
		Servers: []string{ts.addr}, Token: tok.GetToken(), CAHash: tok.GetCaHash(), NodeName: name,
		CredentialsPath: credsPath, Version: "test", Log: discard(),
	})
}

func wantCode(t *testing.T, err error, code codes.Code, msg string) {
	t.Helper()
	if status.Code(err) != code || !strings.Contains(status.Convert(err).Message(), msg) {
		t.Fatalf("err = %v, want %s containing %q", err, code, msg)
	}
}

// wantErr checks an agent-side error, which carries the server's message.
func wantErr(t *testing.T, err error, msg string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), msg) {
		t.Fatalf("err = %v, want containing %q", err, msg)
	}
}

func eventually(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", desc)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const webYAML = `
kind: Workload
metadata: { name: web }
spec:
  image: nginx:1.27
  replicas: 3
  ports: [{ containerPort: 80 }]
  resources: { cpu: 200m, memory: 128Mi }
`

func decode(t *testing.T, y string) []*v1alpha1.Object {
	t.Helper()
	objs, err := resource.Decode([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

func TestApplyGetListDelete(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	admin := ts.admin(t)
	ctx := context.Background()

	apply := func(y string) string {
		t.Helper()
		resp, err := admin.Apply(ctx, &v1alpha1.ApplyRequest{Objects: decode(t, y)})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Results[0].Action
	}
	if a := apply(webYAML); a != "created" {
		t.Fatalf("first apply: %s", a)
	}
	if a := apply(webYAML); a != "unchanged" {
		t.Fatalf("second apply: %s", a)
	}
	if a := apply(strings.Replace(webYAML, "replicas: 3", "replicas: 5", 1)); a != "configured" {
		t.Fatalf("changed apply: %s", a)
	}

	obj, err := admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	w := obj.GetWorkload()
	if w.GetSpec().GetReplicas() != 5 || w.GetMetadata().GetGeneration() != 2 || w.GetMetadata().GetUid() == "" ||
		w.GetSpec().GetRestartPolicy() != "Always" {
		t.Fatalf("stored workload = %v", w)
	}
	// What `get -o yaml` prints can be applied back unchanged.
	y, err := resource.EncodeYAML(w)
	if err != nil {
		t.Fatal(err)
	}
	if a := apply(string(y)); a != "unchanged" {
		t.Fatalf("re-applying get output: %s", a)
	}

	list, err := admin.List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindWorkload})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}

	_, err = admin.Apply(ctx, &v1alpha1.ApplyRequest{Objects: decode(t, strings.Replace(webYAML, "nginx:1.27", "", 1))})
	wantCode(t, err, codes.InvalidArgument, "spec.image: required")
	_, err = admin.Apply(ctx, &v1alpha1.ApplyRequest{Objects: []*v1alpha1.Object{{Kind: &v1alpha1.Object_Node{Node: &v1alpha1.Node{}}}}})
	wantCode(t, err, codes.InvalidArgument, "only Workload")
	_, err = admin.List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindJoinToken})
	wantCode(t, err, codes.InvalidArgument, "unknown kind")

	if _, err := admin.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindWorkload, Name: "web"}); err != nil {
		t.Fatal(err)
	}
	_, err = admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: "web"})
	wantCode(t, err, codes.NotFound, "web")
}

func getNode(t *testing.T, admin v1alpha1.ControlClient, name string) *v1alpha1.Node {
	t.Helper()
	obj, err := admin.Get(context.Background(), &v1alpha1.GetRequest{Kind: v1alpha1.KindNode, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return obj.GetNode()
}

func TestJoinHeartbeatAndNodeLifecycle(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	admin := ts.admin(t)
	ctx := context.Background()
	tok := ts.token(t, admin, 1)
	creds := filepath.Join(t.TempDir(), "agent", "credentials.conf")

	a, err := ts.newAgent(t, tok, "node-1", creds)
	if err != nil {
		t.Fatal(err)
	}
	if n := getNode(t, admin, "node-1"); n.GetStatus().GetPhase() != NodeNotReady {
		t.Fatalf("just joined: phase = %q, want NotReady until the first heartbeat", n.GetStatus().GetPhase())
	}
	// The token allowed one use.
	_, err = ts.newAgent(t, tok, "node-2", filepath.Join(t.TempDir(), "c.conf"))
	wantErr(t, err, "no uses left")

	actx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan error, 1)
	go func() { agentDone <- a.Run(actx) }()
	eventually(t, "node Ready", func() bool {
		n := getNode(t, admin, "node-1")
		return n.GetStatus().GetPhase() == NodeReady && n.GetStatus().GetCapacity().GetCpuMillis() > 0
	})
	n := getNode(t, admin, "node-1")
	if n.GetStatus().GetAddress() != "127.0.0.1" || n.GetStatus().GetAgentVersion() != "test" {
		t.Fatalf("node status = %v", n.GetStatus())
	}

	// Restarting the agent reuses the saved credentials: no token needed.
	stopAgent()
	<-agentDone
	again, err := agent.NewConnected(ctx, agent.ConnectedConfig{CredentialsPath: creds, Log: discard()})
	if err != nil || again.Name() != "node-1" {
		t.Fatalf("reload credentials: %v, %v", again, err)
	}
	_, err = agent.NewConnected(ctx, agent.ConnectedConfig{CredentialsPath: creds, NodeName: "other", Log: discard()})
	if err == nil || !strings.Contains(err.Error(), "already joined") {
		t.Fatalf("renaming a joined node: err = %v", err)
	}

	// No heartbeats for longer than NodeNotReadyAfter.
	ts.clock.advance(25 * time.Second)
	if err := ts.nodes.check(ctx); err != nil {
		t.Fatal(err)
	}
	if n := getNode(t, admin, "node-1"); n.GetStatus().GetPhase() != NodeNotReady {
		t.Fatalf("after missed heartbeats: phase = %q", n.GetStatus().GetPhase())
	}

	// Deleting the node locks its agent out.
	if _, err := admin.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindNode, Name: "node-1"}); err != nil {
		t.Fatal(err)
	}
	p := loadParsed(t, creds)
	sync, err := v1alpha1.NewAgentClient(ts.conn(t, p.TLS())).Sync(ctx)
	if err == nil {
		_, err = sync.Recv()
	}
	wantCode(t, err, codes.PermissionDenied, "not registered")
}

func loadParsed(t *testing.T, path string) *security.Parsed {
	t.Helper()
	c, err := security.LoadClientConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Parse()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRolesAreEnforced(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	admin := ts.admin(t)
	ctx := context.Background()
	creds := filepath.Join(t.TempDir(), "node.conf")
	if _, err := ts.newAgent(t, ts.token(t, admin, 0), "node-1", creds); err != nil {
		t.Fatal(err)
	}
	node := loadParsed(t, creds)
	adminCreds := loadParsed(t, AdminConfigPath(ts.dataDir))

	// A node may not use the Control API.
	_, err := v1alpha1.NewControlClient(ts.conn(t, node.TLS())).List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindNode})
	wantCode(t, err, codes.PermissionDenied, "may not call")
	// The admin may not act as a node.
	_, err = v1alpha1.NewAgentClient(ts.conn(t, adminCreds.TLS())).RenewCertificate(ctx, &v1alpha1.RenewCertificateRequest{})
	wantCode(t, err, codes.PermissionDenied, "may not call")
	// Without a client certificate only Join works.
	noCert := security.ClientTLS(adminCreds.CA, func() (*tls.Certificate, error) { return &tls.Certificate{}, nil })
	_, err = v1alpha1.NewControlClient(ts.conn(t, noCert)).List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindNode})
	wantCode(t, err, codes.Unauthenticated, "client certificate is required")
	_, err = v1alpha1.NewJoinClient(ts.conn(t, noCert)).Join(ctx, &v1alpha1.JoinRequest{Token: "abcdef.0123456789abcdef", NodeName: "x"})
	wantCode(t, err, codes.InvalidArgument, "certificate request") // reached the handler
}

func TestJoinRejectsBadTokens(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	admin := ts.admin(t)
	tok := ts.token(t, admin, 0)
	dir := t.TempDir()

	bad := proto.CloneOf(tok)
	bad.Token = tok.Token[:7] + strings.Repeat("0", 16)
	_, err := ts.newAgent(t, bad, "n1", filepath.Join(dir, "1"))
	wantErr(t, err, "invalid join token")

	// A server whose CA doesn't match the pinned hash is not trusted.
	other := startServer(t, t.TempDir(), &clock{})
	_, err = other.newAgent(t, tok, "n2", filepath.Join(dir, "2"))
	if err == nil || !strings.Contains(err.Error(), "pinned hash") {
		t.Fatalf("join to a server with another CA: err = %v", err)
	}

	ts.clock.advance(2 * time.Hour) // the token was valid for 1h
	_, err = ts.newAgent(t, tok, "n3", filepath.Join(dir, "3"))
	wantErr(t, err, "join token expired")
	if err := ts.gcTokens(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recs, _ := ts.store.List(context.Background(), v1alpha1.KindJoinToken); len(recs) != 0 {
		t.Fatalf("expired token not cleaned up: %d left", len(recs))
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	c := &clock{}
	ts := startServer(t, dir, c)
	if _, err := ts.admin(t).Apply(context.Background(), &v1alpha1.ApplyRequest{Objects: decode(t, webYAML)}); err != nil {
		t.Fatal(err)
	}
	hash := ts.CAHash()
	ts.stop()

	ts2 := startServer(t, dir, c)
	if ts2.CAHash() != hash {
		t.Fatal("CA changed across restart")
	}
	if _, err := ts2.admin(t).Get(context.Background(), &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: "web"}); err != nil {
		t.Fatalf("workload lost across restart: %v", err)
	}
}
