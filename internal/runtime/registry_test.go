package runtime

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/internal/security"
)

func TestRegistryMapImage(t *testing.T) {
	reg := &Registry{Address: "10.0.0.5:5000"}
	for _, tc := range []struct{ in, want string }{
		{"shoal.local/team/app:v1", "10.0.0.5:5000/team/app:v1"},
		{"shoal.local/app@sha256:abc", "10.0.0.5:5000/app@sha256:abc"},
		{"shoal.localhost/app:v1", "shoal.localhost/app:v1"},
		{"shoal.local.evil.example/app:v1", "shoal.local.evil.example/app:v1"},
		{"10.0.0.5:5000/team/app:v1", "10.0.0.5:5000/team/app:v1"},
		{"nginx:1.27", "nginx:1.27"},
	} {
		if got := reg.MapImage(tc.in); got != tc.want {
			t.Errorf("MapImage(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Without a registry the alias is left alone: the pull then fails on the
	// unknown host instead of pulling from somewhere else.
	var none *Registry
	if got := none.MapImage("shoal.local/app:v1"); got != "shoal.local/app:v1" {
		t.Errorf("nil registry MapImage = %q", got)
	}
}

func TestRegistryTLSConfig(t *testing.T) {
	ca, _, err := security.LoadOrCreateCA(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key, err := security.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.IssueClient(key.Public(), security.Identity{Role: security.RoleNode, Name: "n1"}, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert := &tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
	reg := &Registry{Address: "10.0.0.5:5000", CA: ca.Cert, ClientCertificate: func() (*tls.Certificate, error) { return cert, nil }}

	tc := reg.TLSConfig()
	if tc.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want TLS 1.3", tc.MinVersion)
	}
	if tc.InsecureSkipVerify {
		t.Fatal("registry TLS skips verification")
	}
	if tc.RootCAs == nil {
		t.Fatal("registry TLS has no cluster CA")
	}
	// Only the cluster CA is a root: a certificate from another CA must not verify.
	other, _, err := security.LoadOrCreateCA(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Cert.Verify(x509.VerifyOptions{Roots: tc.RootCAs}); err == nil {
		t.Fatal("a foreign CA verifies against the registry roots")
	}
	if _, err := ca.Cert.Verify(x509.VerifyOptions{Roots: tc.RootCAs}); err != nil {
		t.Fatalf("cluster CA does not verify against the registry roots: %v", err)
	}
	got, err := tc.GetClientCertificate(nil)
	if err != nil || got != cert {
		t.Fatalf("GetClientCertificate = %v, %v; want the node certificate", got, err)
	}
	// Before the node has a certificate the handshake fails; it must not panic.
	none := (&Registry{CA: ca.Cert, ClientCertificate: func() (*tls.Certificate, error) { return nil, nil }}).TLSConfig()
	if c, err := none.GetClientCertificate(nil); err == nil {
		t.Fatalf("GetClientCertificate without a node certificate = %v, want an error", c)
	}

	tr, ok := reg.HTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatal("HTTP client has no http.Transport")
	}
	if tr.Proxy != nil {
		t.Error("registry client goes through a proxy")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.InsecureSkipVerify || tr.TLSClientConfig.RootCAs == nil {
		t.Error("registry client TLS is not verified against the cluster CA")
	}
}
