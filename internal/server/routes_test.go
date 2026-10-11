package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/agent"
	"github.com/Shoal-Technology/shoal/internal/edge"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/store/memory"
)

// routeFixture is a store with two nodes (one labelled as an edge), a
// Service with two ready endpoints, a TLS Secret and three Routes.
type routeFixture struct {
	st      store.Store
	certPEM []byte
	keyPEM  []byte
}

func newRouteFixture(t *testing.T) *routeFixture {
	t.Helper()
	ctx := context.Background()
	st := memory.New(func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) })
	f := &routeFixture{st: st}

	c, err := edge.SelfSigned("secure.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if f.certPEM, f.keyPEM, err = edge.EncodePEM(c); err != nil {
		t.Fatal(err)
	}

	put := func(kind, name string, spec proto.Message) {
		t.Helper()
		b, err := objects.Encode(spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Create(ctx, store.Record{Kind: kind, Name: name, Spec: b}); err != nil {
			t.Fatal(err)
		}
	}
	put(v1alpha1.KindNode, "edge", &v1alpha1.NodeSpec{Labels: map[string]string{"shoal.dev/edge": "true"}})
	put(v1alpha1.KindNode, "plain", &v1alpha1.NodeSpec{})
	put(v1alpha1.KindNode, "edge-off", &v1alpha1.NodeSpec{Labels: map[string]string{"shoal.dev/edge": "false"}})

	svc, err := objects.Encode(&v1alpha1.ServiceSpec{Selector: "web", Ports: []*v1alpha1.ServicePort{{Port: 80, TargetPort: 8080}}})
	if err != nil {
		t.Fatal(err)
	}
	stat, err := objects.Encode(&v1alpha1.ServiceStatus{ClusterIp: "10.96.0.10", Endpoints: []string{"10.42.0.2", "10.42.1.2"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, store.Record{Kind: v1alpha1.KindService, Name: "web", Spec: svc, Status: stat}); err != nil {
		t.Fatal(err)
	}
	put(v1alpha1.KindSecret, "web-cert", &v1alpha1.SecretSpec{Data: map[string]string{
		"tls.crt": string(f.certPEM), "tls.key": string(f.keyPEM),
	}})

	put(v1alpha1.KindRoute, "plain", &v1alpha1.RouteSpec{Host: "app.example.com", PathPrefix: "/", Service: "web", Port: 80, Tls: "none"})
	put(v1alpha1.KindRoute, "secure", &v1alpha1.RouteSpec{Host: "secure.example.com", PathPrefix: "/", Service: "web", Port: 80, Tls: "secret", TlsSecret: "web-cert"})
	put(v1alpha1.KindRoute, "api", &v1alpha1.RouteSpec{Host: "app.example.com", PathPrefix: "/api", Service: "nope", Port: 80, Tls: "none"})
	return f
}

func (f *routeFixture) server() *Server {
	return &Server{store: f.st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// addDuplicate stores a Route on the same host and prefix as "plain", newer
// than it: a legacy duplicate.
func (f *routeFixture) addDuplicate(t *testing.T) {
	t.Helper()
	spec, err := objects.Encode(&v1alpha1.RouteSpec{Host: "app.example.com", PathPrefix: "/", Service: "web", Port: 80, Tls: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Create(context.Background(), store.Record{Kind: v1alpha1.KindRoute, Name: "dup", Spec: spec}); err != nil {
		t.Fatal(err)
	}
}

func routeNames(snap *v1alpha1.RouteSnapshot) []string {
	var names []string
	for _, r := range snap.GetRoutes() {
		names = append(names, r.GetName())
	}
	return names
}

// The Route that holds a key's claim is the only one sent: the Conflict Route
// is left out, so the edge never sees two Routes on one host and prefix.
func TestRouteSnapshotLeavesOutConflictRoute(t *testing.T) {
	f := newRouteFixture(t)
	ctx := context.Background()
	f.addDuplicate(t)
	if _, err := f.st.Create(ctx, store.Record{Kind: v1alpha1.KindRouteClaim, Name: "app.example.com /", Spec: []byte("plain")}); err != nil {
		t.Fatal(err)
	}
	snap, err := f.server().routeSnapshot(ctx, "edge")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"api", "plain", "secure"}; !slices.Equal(routeNames(snap), want) {
		t.Fatalf("snapshot routes = %v, want %v", routeNames(snap), want)
	}
}

// Before the backfill has run a key has no claim. The oldest Route on it is
// sent and the newer legacy duplicate is not.
func TestRouteSnapshotLegacyDuplicateWithoutClaims(t *testing.T) {
	f := newRouteFixture(t)
	f.addDuplicate(t)
	snap, err := f.server().routeSnapshot(context.Background(), "edge")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"api", "plain", "secure"}; !slices.Equal(routeNames(snap), want) {
		t.Fatalf("snapshot routes = %v, want %v", routeNames(snap), want)
	}
}

// The snapshot built from a store with a legacy duplicate is applied by the
// edge. A snapshot that still held both Routes would be rejected whole, and
// the edge would keep stale routes for every host.
func TestEdgeAppliesSnapshotWithLegacyDuplicate(t *testing.T) {
	f := newRouteFixture(t)
	f.addDuplicate(t)
	snap, err := f.server().routeSnapshot(context.Background(), "edge")
	if err != nil {
		t.Fatal(err)
	}
	snap.Revision = 7

	e := agent.NewEdge("127.0.0.1:0", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Apply(snap); err != nil {
		t.Fatalf("edge rejected the snapshot: %v", err)
	}
	if got := e.AppliedRevision(); got != 7 {
		t.Fatalf("applied revision = %d, want 7", got)
	}

	// Control: the same routes with the duplicate put back are rejected.
	dup := proto.Clone(snap.GetRoutes()[0]).(*v1alpha1.EdgeRoute)
	dup.Name = "dup"
	bad := proto.Clone(snap).(*v1alpha1.RouteSnapshot)
	bad.Routes = append(bad.Routes, dup)
	bad.Revision = 8
	if err := e.Apply(bad); err == nil {
		t.Fatal("edge accepted two routes on one host and prefix")
	}
}

func TestRouteSnapshotEdgeNode(t *testing.T) {
	f := newRouteFixture(t)
	snap, err := f.server().routeSnapshot(context.Background(), "edge")
	if err != nil {
		t.Fatal(err)
	}
	if !snap.GetEdge() || len(snap.GetRoutes()) != 3 {
		t.Fatalf("edge=%v routes=%d, want an edge with 3 routes", snap.GetEdge(), len(snap.GetRoutes()))
	}
	// Longest prefix first within a host, and hosts sorted.
	var order []string
	for _, r := range snap.GetRoutes() {
		order = append(order, r.GetName())
	}
	if want := []string{"api", "plain", "secure"}; !slices.Equal(order, want) {
		t.Fatalf("route order = %v, want %v", order, want)
	}

	byName := map[string]*v1alpha1.EdgeRoute{}
	for _, r := range snap.GetRoutes() {
		byName[r.GetName()] = r
	}
	// Endpoints are the Service's ready IPs at the target port (8080).
	if got := byName["plain"].GetEndpoints(); !slices.Equal(got, []string{"10.42.0.2:8080", "10.42.1.2:8080"}) {
		t.Fatalf("plain endpoints = %v", got)
	}
	// A Service that does not exist gives no endpoints, not an error.
	if got := byName["api"].GetEndpoints(); len(got) != 0 {
		t.Fatalf("api endpoints = %v, want none", got)
	}
	// The secret route carries the decrypted PEM; the others carry none.
	if !bytes.Equal(byName["secure"].GetCertPem(), f.certPEM) || !bytes.Equal(byName["secure"].GetKeyPem(), f.keyPEM) {
		t.Fatal("secure route does not carry the Secret's certificate and key")
	}
	if len(byName["plain"].GetCertPem()) != 0 || len(byName["plain"].GetKeyPem()) != 0 {
		t.Fatal("tls none route carries key material")
	}
}

func TestRouteSnapshotNonEdgeNodeGetsNoRoutesOrSecrets(t *testing.T) {
	f := newRouteFixture(t)
	for _, node := range []string{"plain", "edge-off"} {
		snap, err := f.server().routeSnapshot(context.Background(), node)
		if err != nil {
			t.Fatal(err)
		}
		if snap.GetEdge() || len(snap.GetRoutes()) != 0 {
			t.Fatalf("%s: edge=%v routes=%d, want nothing", node, snap.GetEdge(), len(snap.GetRoutes()))
		}
		b, err := proto.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, f.keyPEM) || bytes.Contains(b, f.certPEM) {
			t.Fatalf("%s: snapshot contains the certificate or key", node)
		}
	}
}

func TestRouteSnapshotSecretThatDoesNotParseIsOmitted(t *testing.T) {
	f := newRouteFixture(t)
	b, err := objects.Encode(&v1alpha1.SecretSpec{Data: map[string]string{"tls.crt": "junk", "tls.key": "junk"}})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := f.st.Get(context.Background(), v1alpha1.KindSecret, "web-cert")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Update(context.Background(), v1alpha1.KindSecret, "web-cert", b, rec.Generation); err != nil {
		t.Fatal(err)
	}
	snap, err := f.server().routeSnapshot(context.Background(), "edge")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range snap.GetRoutes() {
		if r.GetName() == "secure" && (len(r.GetCertPem()) != 0 || len(r.GetKeyPem()) != 0) {
			t.Fatal("an invalid Secret was sent to the edge")
		}
	}
}

// fakeRouteStream records what the server sends on a Sync stream.
type fakeRouteStream struct {
	grpc.ServerStream
	sent []*v1alpha1.RouteSnapshot
}

func (s *fakeRouteStream) Send(m *v1alpha1.ServerMessage) error {
	if r := m.GetRoutes(); r != nil {
		s.sent = append(s.sent, r)
	}
	return nil
}

func (s *fakeRouteStream) Recv() (*v1alpha1.AgentMessage, error) { return nil, io.EOF }

func TestRoutePusherSendsOnChangeAndDisables(t *testing.T) {
	f := newRouteFixture(t)
	srv := f.server()
	ctx := context.Background()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	stream := &fakeRouteStream{}
	var p routePusher

	edgeSnap := func() *v1alpha1.RouteSnapshot {
		s, err := srv.routeSnapshot(ctx, "edge")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	plainSnap := func() *v1alpha1.RouteSnapshot {
		s, err := srv.routeSnapshot(ctx, "plain")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	// A node that was never an edge gets nothing.
	if err := p.push(stream, plainSnap(), now, true); err != nil {
		t.Fatal(err)
	}
	if len(stream.sent) != 0 {
		t.Fatalf("non-edge node was sent %d snapshots", len(stream.sent))
	}

	if err := p.push(stream, edgeSnap(), now, false); err != nil {
		t.Fatal(err)
	}
	if err := p.push(stream, edgeSnap(), now, false); err != nil { // unchanged
		t.Fatal(err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("sent %d snapshots for unchanged content, want 1", len(stream.sent))
	}
	first := stream.sent[0]
	if first.GetRevision() == 0 || !first.GetEdge() || first.GetVersion() == "" {
		t.Fatalf("first snapshot revision=%d edge=%v version=%q", first.GetRevision(), first.GetEdge(), first.GetVersion())
	}

	// A resync resends the same content with a larger revision.
	if err := p.push(stream, edgeSnap(), now, true); err != nil {
		t.Fatal(err)
	}
	if len(stream.sent) != 2 || stream.sent[1].GetRevision() <= first.GetRevision() {
		t.Fatalf("resync did not send a newer revision: %d snapshots", len(stream.sent))
	}

	// Losing the label sends one disabled snapshot, then nothing.
	if err := p.push(stream, plainSnap(), now, false); err != nil {
		t.Fatal(err)
	}
	if err := p.push(stream, plainSnap(), now, false); err != nil {
		t.Fatal(err)
	}
	if len(stream.sent) != 3 || stream.sent[2].GetEdge() || len(stream.sent[2].GetRoutes()) != 0 {
		t.Fatalf("after losing the label: %d snapshots, want one disabled snapshot", len(stream.sent))
	}
}

func TestSetNodeLabels(t *testing.T) {
	f := newRouteFixture(t)
	c := &controlService{s: f.server()}
	ctx := context.Background()
	resp, err := c.SetNodeLabels(ctx, &v1alpha1.SetNodeLabelsRequest{Node: "plain", Set: map[string]string{"shoal.dev/edge": "true", "zone": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetLabels()["shoal.dev/edge"] != "true" || resp.GetLabels()["zone"] != "a" {
		t.Fatalf("labels after set = %v", resp.GetLabels())
	}
	// The snapshot builder now sees the node as an edge.
	snap, err := f.server().routeSnapshot(ctx, "plain")
	if err != nil || !snap.GetEdge() {
		t.Fatalf("plain node after labelling: edge=%v err=%v", snap.GetEdge(), err)
	}
	resp, err = c.SetNodeLabels(ctx, &v1alpha1.SetNodeLabelsRequest{Node: "plain", Remove: []string{"shoal.dev/edge"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.GetLabels()["shoal.dev/edge"]; ok || resp.GetLabels()["zone"] != "a" {
		t.Fatalf("labels after remove = %v", resp.GetLabels())
	}
	if _, err := c.SetNodeLabels(ctx, &v1alpha1.SetNodeLabelsRequest{Node: "plain", Set: map[string]string{"bad key!": "x"}}); err == nil {
		t.Fatal("invalid label key accepted")
	}
}
