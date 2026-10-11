package agent

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// backendAddr starts an upstream that answers with body, and returns its host:port.
func backendAddr(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// secretPEM returns a self-signed certificate and key in PEM for host.
func secretPEM(t *testing.T, host string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	return certPEM, keyPEM
}

// getHost sends a GET to the edge's HTTP listener with the given Host header.
func getHost(t *testing.T, addr, host string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", host, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestEdgeAppliedRevisionFollowsSnapshots(t *testing.T) {
	e := NewEdge("127.0.0.1:0", "", quietLog())
	defer e.Close()
	if got := e.AppliedRevision(); got != 0 {
		t.Fatalf("AppliedRevision before any snapshot = %d", got)
	}
	snap := &v1alpha1.RouteSnapshot{Edge: true, Revision: 7}
	if err := e.Apply(snap); err != nil {
		t.Fatal(err)
	}
	if got := e.AppliedRevision(); got != 7 {
		t.Fatalf("AppliedRevision = %d, want 7", got)
	}
}

// The cache keeps routes and endpoints, never keys or certificates, and is
// written 0600 with no leftover temp files.
func TestEdgeCacheHoldsNoKeysAndIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not enforced on Windows")
	}
	dir := t.TempDir()
	certPEM, keyPEM := secretPEM(t, "secure.test")
	e := NewEdge("127.0.0.1:0", "", quietLog())
	defer e.Close()
	e.UseCache(dir)
	snap := &v1alpha1.RouteSnapshot{Edge: true, Revision: 3, Routes: []*v1alpha1.EdgeRoute{{
		Name: "secure", Host: "secure.test", PathPrefix: "/", Tls: "secret",
		Endpoints: []string{"10.0.0.5:8080"}, CertPem: certPEM, KeyPem: keyPEM,
	}}, Certificates: []*v1alpha1.EdgeCertificate{{Host: "secure.test", ChainPem: certPEM, KeyPem: keyPEM}}}
	if err := e.Apply(snap); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, cacheFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("cache file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("cache mode = %o, want 600", mode)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("PRIVATE KEY")) || bytes.Contains(b, keyPEM) || bytes.Contains(b, certPEM) {
		t.Fatal("the route cache holds key or certificate material")
	}
	if !bytes.Contains(b, []byte("10.0.0.5:8080")) {
		t.Fatal("the route cache lost the endpoints")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("cache directory has %d entries, want only the cache file", len(entries))
	}
}

// A restarted edge with the server unreachable restores its revision from the
// cache, and an older snapshot cannot replace it. Its tls: secret route has no
// certificate until one arrives (no key is ever cached).
func TestEdgeRestartRestoresRevisionWithoutServer(t *testing.T) {
	dir := t.TempDir()
	upstream := backendAddr(t, "from-cache")
	first := NewEdge("127.0.0.1:0", "", quietLog())
	first.UseCache(dir)
	if err := first.Apply(&v1alpha1.RouteSnapshot{Edge: true, Revision: 10, Routes: []*v1alpha1.EdgeRoute{{
		Name: "web", Host: "web.test", PathPrefix: "/", Tls: "none", Endpoints: []string{upstream},
	}, {
		Name: "sec", Host: "sec.test", PathPrefix: "/", Tls: "secret", Endpoints: []string{upstream},
	}}}); err != nil {
		t.Fatal(err)
	}
	first.Close()

	restarted := NewEdge("127.0.0.1:0", "", quietLog())
	defer restarted.Close()
	restarted.UseCache(dir)
	if err := restarted.LoadCache(); err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if got := restarted.AppliedRevision(); got != 10 {
		t.Fatalf("restored revision = %d, want 10", got)
	}
	if err := restarted.Apply(&v1alpha1.RouteSnapshot{Edge: true, Revision: 9}); err != nil {
		t.Fatal(err)
	}
	if got := restarted.AppliedRevision(); got != 10 {
		t.Fatalf("an older snapshot replaced the restored revision: %d", got)
	}
}
func TestEdgeRestoredRoutesServeHTTP(t *testing.T) {
	dir := t.TempDir()
	upstream := backendAddr(t, "cached-body")
	first := NewEdge("127.0.0.1:0", "", quietLog())
	first.UseCache(dir)
	if err := first.Apply(&v1alpha1.RouteSnapshot{Edge: true, Revision: 1, Routes: []*v1alpha1.EdgeRoute{{
		Name: "web", Host: "web.test", PathPrefix: "/", Tls: "none", Endpoints: []string{upstream},
	}}}); err != nil {
		t.Fatal(err)
	}
	first.Close()

	restarted := NewEdge("127.0.0.1:0", "", quietLog())
	defer restarted.Close()
	restarted.UseCache(dir)
	if err := restarted.LoadCache(); err != nil {
		t.Fatal(err)
	}
	if got := restarted.HTTPAddr(); got == "" {
		t.Fatal("the restored edge did not start its HTTP listener")
	}
	code, body := getHost(t, restarted.HTTPAddr(), "web.test")
	if code != http.StatusOK || body != "cached-body" {
		t.Fatalf("GET web.test = %d %q, want 200 cached-body", code, body)
	}
}

// Without a cache directory nothing is written and LoadCache does nothing.
func TestEdgeCacheOffByDefault(t *testing.T) {
	e := NewEdge("127.0.0.1:0", "", quietLog())
	defer e.Close()
	if err := e.LoadCache(); err != nil {
		t.Fatal(err)
	}
	if err := e.Apply(&v1alpha1.RouteSnapshot{Edge: true, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("cache written without UseCache")
	}
}

// A node that stops being an edge removes its cache.
func TestEdgeCacheRemovedWhenNodeStopsBeingEdge(t *testing.T) {
	dir := t.TempDir()
	e := NewEdge("127.0.0.1:0", "", quietLog())
	defer e.Close()
	e.UseCache(dir)
	if err := e.Apply(&v1alpha1.RouteSnapshot{Edge: true, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, cacheFile)); err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	if err := e.Apply(&v1alpha1.RouteSnapshot{Edge: false, Revision: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, cacheFile)); !os.IsNotExist(err) {
		t.Fatalf("cache still present after the label was removed: %v", err)
	}
}
