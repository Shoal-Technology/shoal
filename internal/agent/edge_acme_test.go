package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/edge"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// autoRoute is one tls auto Route with no endpoints; the tests only care
// about the listeners, challenges and certificates.
func autoRoute(host string) *v1alpha1.EdgeRoute {
	return &v1alpha1.EdgeRoute{Name: "app", Host: host, PathPrefix: "/", Tls: "auto"}
}

// fetch GETs url with Host set to host, through client.
func fetch(t *testing.T, client *http.Client, url, host string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// leafOf returns the DER leaf the edge presents for serverName.
func leafOf(t *testing.T, addr, serverName string) []byte {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: serverName, InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls dial %s as %s: %v", addr, serverName, err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].Raw
}

// TestEdgeAnswersHTTP01ChallengeFromSnapshot checks that the token in a
// snapshot is answered over HTTP, unknown tokens get 404, challenges are not
// answered on HTTPS, and a snapshot without the token withdraws it.
func TestEdgeAnswersHTTP01ChallengeFromSnapshot(t *testing.T) {
	e := NewEdge("127.0.0.1:0", "127.0.0.1:0", quietLog())
	defer e.Close()
	const host = "app.example.com"
	if err := e.Apply(&v1alpha1.RouteSnapshot{
		Edge: true, Revision: 1, Routes: []*v1alpha1.EdgeRoute{autoRoute(host)},
		Challenges: []*v1alpha1.EdgeChallenge{{Token: "tok1", KeyAuthorization: "tok1.keyauth"}},
	}); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{}
	base := "http://" + e.HTTPAddr() + "/.well-known/acme-challenge/"
	if code, body := fetch(t, client, base+"tok1", host); code != http.StatusOK || body != "tok1.keyauth" {
		t.Fatalf("known token: status %d body %q", code, body)
	}
	if code, _ := fetch(t, client, base+"unknown", host); code != http.StatusNotFound {
		t.Fatalf("unknown token: status %d, want 404", code)
	}
	if code, _ := fetch(t, client, base+"tok1/extra", host); code != http.StatusNotFound {
		t.Fatalf("token with a slash: status %d, want 404", code)
	}

	// HTTPS never answers the challenge path.
	tlsClient := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", e.HTTPSAddr())
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	if code, body := fetch(t, tlsClient, "https://"+host+"/.well-known/acme-challenge/tok1", host); code != http.StatusNotFound || body == "tok1.keyauth" {
		t.Fatalf("https challenge: status %d, want 404 and no key authorization", code)
	}

	// A later snapshot without the token withdraws it.
	if err := e.Apply(&v1alpha1.RouteSnapshot{Edge: true, Revision: 2, Routes: []*v1alpha1.EdgeRoute{autoRoute(host)}}); err != nil {
		t.Fatal(err)
	}
	if code, _ := fetch(t, client, base+"tok1", host); code != http.StatusNotFound {
		t.Fatalf("withdrawn token: status %d, want 404", code)
	}
}

// TestIssuedCertificateReplacesSelfSignedBySNI checks that a certificate in
// a later snapshot replaces the fallback for its host, without a restart.
func TestIssuedCertificateReplacesSelfSignedBySNI(t *testing.T) {
	e := NewEdge("127.0.0.1:0", "127.0.0.1:0", quietLog())
	defer e.Close()
	const host = "app.example.com"
	if err := e.Apply(&v1alpha1.RouteSnapshot{Edge: true, Revision: 1, Routes: []*v1alpha1.EdgeRoute{autoRoute(host)}}); err != nil {
		t.Fatal(err)
	}
	fallback := leafOf(t, e.HTTPSAddr(), host)

	issued, err := edge.SelfSigned(host)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := edge.EncodePEM(issued)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Apply(&v1alpha1.RouteSnapshot{
		Edge: true, Revision: 2, Routes: []*v1alpha1.EdgeRoute{autoRoute(host)},
		Certificates: []*v1alpha1.EdgeCertificate{{Host: host, ChainPem: certPEM, KeyPem: keyPEM}},
	}); err != nil {
		t.Fatal(err)
	}
	got := leafOf(t, e.HTTPSAddr(), host)
	if bytes.Equal(got, fallback) {
		t.Fatal("the self-signed certificate is still served after issuance")
	}
	if !bytes.Equal(got, issued.Certificate[0]) {
		t.Fatal("the issued certificate is not the one served by SNI")
	}
}

// TestIssuedCertificateDoesNotReplaceSecret checks that a Secret certificate
// for the same host keeps precedence over an issued one.
func TestIssuedCertificateDoesNotReplaceSecret(t *testing.T) {
	e := NewEdge("127.0.0.1:0", "127.0.0.1:0", quietLog())
	defer e.Close()
	const host = "secure.example.com"
	secret, err := edge.SelfSigned(host)
	if err != nil {
		t.Fatal(err)
	}
	secretCert, secretKey, err := edge.EncodePEM(secret)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := edge.SelfSigned(host)
	if err != nil {
		t.Fatal(err)
	}
	issuedCert, issuedKey, err := edge.EncodePEM(issued)
	if err != nil {
		t.Fatal(err)
	}
	secretRoute := &v1alpha1.EdgeRoute{Name: "secure", Host: host, PathPrefix: "/", Tls: "secret", CertPem: secretCert, KeyPem: secretKey}
	if err := e.Apply(&v1alpha1.RouteSnapshot{
		Edge: true, Revision: 1, Routes: []*v1alpha1.EdgeRoute{secretRoute},
		Certificates: []*v1alpha1.EdgeCertificate{{Host: host, ChainPem: issuedCert, KeyPem: issuedKey}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := leafOf(t, e.HTTPSAddr(), host); !bytes.Equal(got, secret.Certificate[0]) {
		t.Fatal("an issued certificate replaced the Secret certificate")
	}
}
