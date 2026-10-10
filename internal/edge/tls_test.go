package edge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func poolFor(cert *tls.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	return pool
}

// startTLS serves h on 127.0.0.1:0 with the store's TLS config and returns the address.
func startTLS(t *testing.T, store *CertStore, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, TLSConfig: store.TLSConfig(), ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func TestCertStoreSelectsBySNI(t *testing.T) {
	cert, err := SelfSigned("app.example.test")
	if err != nil {
		t.Fatal(err)
	}
	store := &CertStore{}
	store.Replace(map[string]*tls.Certificate{"app.example.test": cert})
	addr := startTLS(t, store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))

	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: "app.example.test", RootCAs: poolFor(cert)})
	if err != nil {
		t.Fatalf("known SNI: handshake failed: %v", err)
	}
	conn.Close()

	// InsecureSkipVerify keeps the client from failing on the name check, so
	// only the server's refusal can make these handshakes fail.
	if conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: "evil.example.test", InsecureSkipVerify: true}); err == nil {
		conn.Close()
		t.Fatal("handshake with unknown SNI succeeded")
	}

	store.Replace(map[string]*tls.Certificate{})
	if conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: "app.example.test", InsecureSkipVerify: true}); err == nil {
		conn.Close()
		t.Fatal("handshake succeeded after the certificate was removed")
	}
}

func TestHTTP2Negotiated(t *testing.T) {
	cert, err := SelfSigned("app.example.test")
	if err != nil {
		t.Fatal(err)
	}
	store := &CertStore{}
	store.Replace(map[string]*tls.Certificate{"app.example.test": cert})
	addr := startTLS(t, store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))

	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: poolFor(cert)},
		ForceAttemptHTTP2: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	resp, err := (&http.Client{Transport: tr}).Get("https://app.example.test/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.TLS == nil || resp.ProtoMajor != 2 || resp.TLS.NegotiatedProtocol != "h2" {
		t.Fatalf("protocol %s; want HTTP/2 negotiated via ALPN h2", resp.Proto)
	}
}

func TestHTTPHandlerRedirectsAndPassesACME(t *testing.T) {
	up, _ := echoID(t, "plain")
	p := newProxy(t, Config{},
		mkRoute("app", "app.example.test", "/", "auto", up),
		mkRoute("plain", "plain.example.test", "/", "none", up),
	)
	challenge := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "token-ok:"+r.URL.Path)
	})
	front := httptest.NewServer(HTTPHandler(p, challenge))
	t.Cleanup(front.Close)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, _ := fetch(t, noFollow, front.URL, http.MethodGet, "app.example.test", "/a?b=1", nil)
	if resp.StatusCode != http.StatusPermanentRedirect {
		t.Fatalf("redirect status %d, want 308", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://app.example.test/a?b=1" {
		t.Fatalf("Location = %q", loc)
	}

	resp, _ = fetch(t, noFollow, front.URL, http.MethodGet, "APP.example.test:80", "/x", nil)
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "https://app.example.test/x" {
		t.Fatalf("host with port and case: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, body := fetch(t, noFollow, front.URL, http.MethodGet, "app.example.test", acmeChallengePrefix+"tok", nil)
	if resp.StatusCode != http.StatusOK || body != "token-ok:"+acmeChallengePrefix+"tok" {
		t.Fatalf("ACME path not passed to challenge handler: %d %q", resp.StatusCode, body)
	}

	resp, body = fetch(t, noFollow, front.URL, http.MethodGet, "plain.example.test", "/", nil)
	if resp.StatusCode != http.StatusOK || body != "plain" {
		t.Fatalf("route without redirect should be proxied: %d %q", resp.StatusCode, body)
	}

	resp, _ = fetch(t, noFollow, front.URL, http.MethodGet, "unknown.example.test", "/", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown host on port 80: status %d, want 404", resp.StatusCode)
	}
}

func TestDefaultChallengeHandlerReturns404(t *testing.T) {
	p := newProxy(t, Config{}, mkRoute("app", "app.example.test", "/", "auto", "127.0.0.1:1"))
	front := httptest.NewServer(HTTPHandler(p, nil))
	t.Cleanup(front.Close)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, _ := fetch(t, noFollow, front.URL, http.MethodGet, "app.example.test", acmeChallengePrefix+"tok", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("default challenge handler: status %d, want 404", resp.StatusCode)
	}
}

func TestSelfSignedNeedsHost(t *testing.T) {
	if _, err := SelfSigned(); err == nil {
		t.Fatal("SelfSigned with no hosts succeeded")
	}
}
