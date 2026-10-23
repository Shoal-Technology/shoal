package security

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// x509 stores times with second precision.
var now = time.Now().Truncate(time.Second)

func newCA(t *testing.T) *CA {
	t.Helper()
	ca, created, err := LoadOrCreateCA(t.TempDir(), now)
	if err != nil || !created {
		t.Fatalf("LoadOrCreateCA: created=%v err=%v", created, err)
	}
	return ca
}

func TestLoadOrCreateCA(t *testing.T) {
	dir := t.TempDir()
	ca, created, err := LoadOrCreateCA(dir, now)
	if err != nil || !created {
		t.Fatalf("first call: created=%v err=%v", created, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "ca.key")); err != nil || (goruntime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("ca.key mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	again, created, err := LoadOrCreateCA(dir, now)
	if err != nil || created {
		t.Fatalf("second call: created=%v err=%v", created, err)
	}
	if CAHash(again.Cert) != CAHash(ca.Cert) {
		t.Fatal("reloaded CA differs")
	}
	if err := os.Remove(filepath.Join(dir, "ca.key")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateCA(dir, now); err == nil {
		t.Fatal("expected an error for a CA certificate without its key, not a silent new CA")
	}
}

func TestSignCSRIgnoresRequestedSubject(t *testing.T) {
	ca := newCA(t)
	key, _ := NewKey()
	csr, err := NewCSR(key, "admin") // asks for a different name
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.SignCSR(csr, Identity{Name: "node-1", Role: RoleNode}, now, NodeValidity)
	if err != nil {
		t.Fatal(err)
	}
	id, err := IdentityFromCert(cert)
	if err != nil || id != (Identity{Name: "node-1", Role: RoleNode}) {
		t.Fatalf("identity = %v, %v", id, err)
	}
	if !cert.NotBefore.Before(now) || cert.NotAfter.Sub(now) != NodeValidity {
		t.Fatalf("validity %s - %s", cert.NotBefore, cert.NotAfter)
	}
	if _, err := ca.SignCSR([]byte("garbage"), Identity{Name: "n", Role: RoleNode}, now, time.Hour); err == nil {
		t.Fatal("expected error for invalid CSR")
	}
}

// handshake runs a TLS handshake over an in-memory pipe and returns the
// client certificates the server saw and the errors of both sides. (In TLS
// 1.3 the client finishes before the server has checked its certificate, so
// a rejected client certificate shows up only as a server error.)
func handshake(t *testing.T, server, client *tls.Config) ([]*x509.Certificate, error) {
	t.Helper()
	// Loopback TCP rather than net.Pipe: an unbuffered pipe deadlocks when
	// one side writes an alert the other side isn't reading.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	cc, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	sc, err := lis.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	type result struct {
		peers []*x509.Certificate
		err   error
	}
	done := make(chan result, 1)
	go func() {
		s := tls.Server(sc, server)
		err := s.Handshake()
		done <- result{s.ConnectionState().PeerCertificates, err}
	}()
	c := tls.Client(cc, client)
	cerr := c.Handshake()
	if cerr != nil {
		cc.Close() // unblock the server
	}
	r := <-done
	return r.peers, errors.Join(cerr, r.err)
}

func serverConfig(t *testing.T, ca *CA) *tls.Config {
	t.Helper()
	key, _ := NewKey()
	leaf, err := ca.IssueServer(key.Public(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ServerTLS(ca.Cert, leaf, key)
}

func clientCert(t *testing.T, ca *CA, id Identity) *tls.Certificate {
	t.Helper()
	key, _ := NewKey()
	cert, err := ca.IssueClient(key.Public(), id, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key, Leaf: cert}
}

func TestMutualTLS(t *testing.T) {
	ca := newCA(t)
	cert := clientCert(t, ca, Identity{Name: "admin", Role: RoleAdmin})
	peers, err := handshake(t, serverConfig(t, ca), ClientTLS(ca.Cert, func() (*tls.Certificate, error) { return cert, nil }))
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) == 0 {
		t.Fatal("server saw no client certificate")
	}
	if id, err := IdentityFromCert(peers[0]); err != nil || id.Role != RoleAdmin {
		t.Fatalf("identity = %v, %v", id, err)
	}

	// A server signed by another CA is rejected.
	other := newCA(t)
	if _, err := handshake(t, serverConfig(t, other), ClientTLS(ca.Cert, func() (*tls.Certificate, error) { return cert, nil })); err == nil {
		t.Fatal("client accepted a server from another CA")
	}
	// A client certificate from another CA is rejected by the server.
	foreign := clientCert(t, other, Identity{Name: "admin", Role: RoleAdmin})
	if _, err := handshake(t, serverConfig(t, ca), ClientTLS(ca.Cert, func() (*tls.Certificate, error) { return foreign, nil })); err == nil {
		t.Fatal("server accepted a client certificate from another CA")
	}
}

func TestClientCertCannotActAsServer(t *testing.T) {
	ca := newCA(t)
	// A node tries to impersonate the server with its own (client) certificate.
	node := clientCert(t, ca, Identity{Name: ServerName, Role: RoleNode})
	fake := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{*node}}
	admin := clientCert(t, ca, Identity{Name: "admin", Role: RoleAdmin})
	if _, err := handshake(t, fake, ClientTLS(ca.Cert, func() (*tls.Certificate, error) { return admin, nil })); err == nil {
		t.Fatal("client accepted a client certificate as server certificate")
	}
}

func TestPinnedTLS(t *testing.T) {
	ca := newCA(t)
	var got PinnedCA
	if _, err := handshake(t, serverConfig(t, ca), PinnedTLS(CAHash(ca.Cert), &got)); err != nil {
		t.Fatal(err)
	}
	if got.Cert() == nil || CAHash(got.Cert()) != CAHash(ca.Cert) {
		t.Fatal("pinned CA not recorded")
	}

	var wrong PinnedCA
	_, err := handshake(t, serverConfig(t, newCA(t)), PinnedTLS(CAHash(ca.Cert), &wrong))
	if err == nil || !strings.Contains(err.Error(), "pinned hash") {
		t.Fatalf("err = %v, want pinned hash mismatch", err)
	}
	if wrong.Cert() != nil {
		t.Fatal("CA recorded despite failed verification")
	}
}

func TestTokens(t *testing.T) {
	id, secret, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	gotID, gotSecret, err := ParseToken(id + "." + secret)
	if err != nil || gotID != id || gotSecret != secret {
		t.Fatalf("ParseToken = %q, %q, %v", gotID, gotSecret, err)
	}
	hash := HashSecret(secret)
	// Swap the last character for the next one in the alphabet, so the
	// wrong secret always differs from the real one.
	last := strings.IndexByte(tokenAlphabet, secret[len(secret)-1])
	wrong := secret[:len(secret)-1] + string(tokenAlphabet[(last+1)%len(tokenAlphabet)])
	if !SecretMatches(hash, secret) || SecretMatches(hash, wrong) {
		t.Fatal("SecretMatches gives wrong answers")
	}
	for _, bad := range []string{"", "abc", id, id + ".", "ABCDEF.0123456789abcdef", id + "." + secret + "x"} {
		if _, _, err := ParseToken(bad); err == nil {
			t.Errorf("ParseToken(%q) succeeded", bad)
		}
	}
}

func TestClientConfigRoundTrip(t *testing.T) {
	ca := newCA(t)
	key, _ := NewKey()
	cert, err := ca.IssueClient(key.Public(), Identity{Name: "admin", Role: RoleAdmin}, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := NewClientConfig("10.0.0.1:7443", ca.Cert, cert, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sub", "admin.conf")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || (goruntime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	loaded, err := LoadClientConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := loaded.Parse()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Server != "10.0.0.1:7443" || !p.Cert.Equal(cert) || !p.CA.Equal(ca.Cert) {
		t.Fatal("round trip changed the config")
	}
}
