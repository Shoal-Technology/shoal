package agent

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/edge"
)

// upstream answers every request with its own name and the Host it saw.
func upstream(t *testing.T, name string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s host=%s", name, r.Host)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func testEdge(t *testing.T) *Edge {
	t.Helper()
	e := NewEdge("127.0.0.1:0", "127.0.0.1:0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// getHTTP sends a GET to the edge's HTTP listener with the given Host header.
func getHTTP(t *testing.T, addr, host, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := (&http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// getHTTPS dials the HTTPS listener with the given SNI name and returns the
// certificate it presented and the response body.
func getHTTPS(t *testing.T, addr, sni string) (*tls.Certificate, string) {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: sni, InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("handshake for %s: %v", sni, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	peer := conn.ConnectionState().PeerCertificates[0]
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", sni); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return &tls.Certificate{Certificate: [][]byte{peer.Raw}, Leaf: peer}, string(b)
}

func TestEdgeAppliesSnapshotHTTPAndSNI(t *testing.T) {
	plain := upstream(t, "plain")
	secure := upstream(t, "secure")

	c, err := edge.SelfSigned("secure.example.com")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := edge.EncodePEM(c)
	if err != nil {
		t.Fatal(err)
	}

	e := testEdge(t)
	if err := e.Apply(&v1alpha1.RouteSnapshot{Revision: 1, Edge: true, Routes: []*v1alpha1.EdgeRoute{
		{Name: "plain", Host: "app.example.com", PathPrefix: "/", Tls: "none", Endpoints: []string{plain}},
		{Name: "secure", Host: "secure.example.com", PathPrefix: "/", Tls: "secret", Endpoints: []string{secure}, CertPem: certPEM, KeyPem: keyPEM},
		{Name: "auto", Host: "auto.example.com", PathPrefix: "/", Tls: "auto", Endpoints: []string{plain}},
	}}); err != nil {
		t.Fatal(err)
	}
	httpAddr, httpsAddr := e.HTTPAddr(), e.HTTPSAddr()
	if httpAddr == "" || httpsAddr == "" {
		t.Fatalf("listeners not running: http=%q https=%q", httpAddr, httpsAddr)
	}

	// HTTP reaches the upstream endpoint of the route for that host.
	if code, body := getHTTP(t, httpAddr, "app.example.com", "/"); code != http.StatusOK || !strings.HasPrefix(body, "plain host=") {
		t.Fatalf("http: %d %q, want the plain upstream", code, body)
	}
	// An unknown host gets the same 404 as any unmatched request.
	if code, _ := getHTTP(t, httpAddr, "nope.example.com", "/"); code != http.StatusNotFound {
		t.Fatalf("unknown host: %d, want 404", code)
	}

	// HTTPS with tls: secret serves the delivered certificate for its SNI.
	peer, body := getHTTPS(t, httpsAddr, "secure.example.com")
	if !bytes.Equal(peer.Certificate[0], c.Certificate[0]) {
		t.Fatal("secure.example.com was not served the Secret's certificate")
	}
	if !strings.HasPrefix(body, "secure host=") {
		t.Fatalf("https body = %q, want the secure upstream", body)
	}

	// tls: auto gets a self-signed certificate for its host until ACME exists.
	autoPeer, _ := getHTTPS(t, httpsAddr, "auto.example.com")
	if autoPeer.Leaf == nil || !slicesContains(autoPeer.Leaf.DNSNames, "auto.example.com") {
		t.Fatalf("auto host certificate names = %v", autoPeer.Leaf)
	}

	// An unknown SNI fails the handshake; there is no default certificate.
	conn, err := net.Dial("tcp", httpsAddr)
	if err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(conn, &tls.Config{ServerName: "other.example.com", InsecureSkipVerify: true})
	if err := tc.Handshake(); err == nil {
		t.Fatal("handshake for an unknown name succeeded")
	}
	_ = tc.Close()
}

func TestEdgeIgnoresStaleAndDisablesOnNonEdge(t *testing.T) {
	ep := upstream(t, "up")
	e := testEdge(t)
	if err := e.Apply(&v1alpha1.RouteSnapshot{Revision: 5, Edge: true, Routes: []*v1alpha1.EdgeRoute{
		{Name: "web", Host: "app.example.com", PathPrefix: "/", Tls: "none", Endpoints: []string{ep}},
	}}); err != nil {
		t.Fatal(err)
	}
	addr := e.HTTPAddr()

	// An older snapshot changes nothing, even one that would disable the edge.
	if err := e.Apply(&v1alpha1.RouteSnapshot{Revision: 4, Edge: false}); err != nil {
		t.Fatal(err)
	}
	if e.HTTPAddr() != addr {
		t.Fatal("a stale snapshot stopped the edge")
	}

	// A newer snapshot for a node that is no longer an edge stops the listeners.
	if err := e.Apply(&v1alpha1.RouteSnapshot{Revision: 6, Edge: false}); err != nil {
		t.Fatal(err)
	}
	if e.HTTPAddr() != "" || e.HTTPSAddr() != "" {
		t.Fatal("listeners still running after the node stopped being an edge")
	}
	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Fatal("HTTP listener still accepts connections")
	}
}

func TestEdgeRejectsBadSnapshotAndKeepsRoutes(t *testing.T) {
	ep := upstream(t, "up")
	e := testEdge(t)
	if err := e.Apply(&v1alpha1.RouteSnapshot{Revision: 1, Edge: true, Routes: []*v1alpha1.EdgeRoute{
		{Name: "web", Host: "app.example.com", PathPrefix: "/", Tls: "none", Endpoints: []string{ep}},
	}}); err != nil {
		t.Fatal(err)
	}
	// Two routes that claim the same host and prefix are refused as a whole.
	dup := &v1alpha1.RouteSnapshot{Revision: 2, Edge: true, Routes: []*v1alpha1.EdgeRoute{
		{Name: "a", Host: "x.example.com", PathPrefix: "/", Tls: "none", Endpoints: []string{ep}},
		{Name: "b", Host: "x.example.com", PathPrefix: "/", Tls: "none", Endpoints: []string{ep}},
	}}
	if err := e.Apply(dup); err == nil {
		t.Fatal("duplicate routes accepted")
	}
	if code, body := getHTTP(t, e.HTTPAddr(), "app.example.com", "/"); code != http.StatusOK || !strings.HasPrefix(body, "up") {
		t.Fatalf("previous routes lost after a rejected snapshot: %d %q", code, body)
	}
}

func slicesContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
