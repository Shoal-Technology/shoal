package containerd

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/internal/runtime"
	"github.com/Shoal-Technology/shoal/internal/security"
)

// fakeRegistry is a TLS server that requires a client certificate from the
// cluster CA, the way the embedded registry does (ADR 0020). It returns 200 to
// any request that gets past the handshake.
func fakeRegistry(t *testing.T, ca *security.CA) string {
	t.Helper()
	key, err := security.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.IssueServerFor(key.Public(), time.Now(), []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "no client certificate", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// nodeCert issues a node client certificate from ca.
func nodeCert(t *testing.T, ca *security.CA, name string) *tls.Certificate {
	t.Helper()
	key, err := security.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.IssueClient(key.Public(), security.Identity{Role: security.RoleNode, Name: name}, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
}

func TestImageNameMapsRegistryAlias(t *testing.T) {
	ca, _, err := security.LoadOrCreateCA(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert := nodeCert(t, ca, "n1")
	reg := &runtime.Registry{Address: "10.0.0.5:5000", CA: ca.Cert, ClientCertificate: func() (*tls.Certificate, error) { return cert, nil }}
	withReg := &containerdRuntime{opts: Options{Registry: reg}}
	without := &containerdRuntime{}
	for _, tc := range []struct {
		r    *containerdRuntime
		in   string
		want string
	}{
		{withReg, "shoal.local/team/app:v1", "10.0.0.5:5000/team/app:v1"},
		{withReg, "10.0.0.5:5000/team/app:v1", "10.0.0.5:5000/team/app:v1"},
		{withReg, "nginx:1.27", "docker.io/library/nginx:1.27"},
		{without, "shoal.local/team/app:v1", "shoal.local/team/app:v1"},
	} {
		got, err := tc.r.imageName(tc.in)
		if err != nil {
			t.Fatalf("imageName(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("imageName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRegistryHostsUseClusterTLS(t *testing.T) {
	ca, _, err := security.LoadOrCreateCA(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	addr := fakeRegistry(t, ca)
	cert := nodeCert(t, ca, "n1")
	reg := &runtime.Registry{Address: addr, CA: ca.Cert, ClientCertificate: func() (*tls.Certificate, error) { return cert, nil }}

	hosts, err := registryHosts(nil, reg)(addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Scheme != "https" {
		t.Fatalf("registry hosts = %+v, want one https host", hosts)
	}
	resp, err := hosts[0].Client.Get("https://" + addr + "/v2/")
	if err != nil {
		t.Fatalf("registry with node certificate: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("registry with node certificate: status %d", resp.StatusCode)
	}

	// The node certificate is read per handshake: no certificate means the
	// handshake fails, and the agent never falls back to an unverified one.
	none := &runtime.Registry{Address: addr, CA: ca.Cert, ClientCertificate: func() (*tls.Certificate, error) { return nil, nil }}
	hosts, err = registryHosts(nil, none)(addr)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := hosts[0].Client.Get("https://" + addr + "/v2/"); err == nil {
		resp.Body.Close()
		t.Fatalf("registry without a client certificate answered with %d", resp.StatusCode)
	}

	// A registry certificate from another CA is refused.
	other, _, err := security.LoadOrCreateCA(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	wrong := &runtime.Registry{Address: addr, CA: other.Cert, ClientCertificate: func() (*tls.Certificate, error) { return cert, nil }}
	hosts, err = registryHosts(nil, wrong)(addr)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := hosts[0].Client.Get("https://" + addr + "/v2/"); err == nil {
		resp.Body.Close()
		t.Fatal("registry certificate from another CA was accepted")
	}
}

func TestOtherHostsDoNotTrustRegistryCA(t *testing.T) {
	ca, _, err := security.LoadOrCreateCA(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	addr := fakeRegistry(t, ca)
	cert := nodeCert(t, ca, "n1")
	reg := &runtime.Registry{Address: addr, CA: ca.Cert, ClientCertificate: func() (*tls.Certificate, error) { return cert, nil }}

	// A host that is not the registry is reached the default way: the
	// registry's CA is not a root there, so the same server is refused.
	hosts, err := registryHosts(nil, reg)("other.example.test:5000")
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := hosts[0].Client.Get("https://" + addr + "/v2/"); err == nil {
		resp.Body.Close()
		t.Fatal("a non-registry host trusted the registry's CA")
	}
}
