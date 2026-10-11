package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeACMEServer imitates the ACME endpoints a client uses to finalize an
// order and download the certificate. withLocation sets whether the finalize
// response carries a Location header: Let's Encrypt sends one, pebble does not.
type fakeACMEServer struct {
	srv          *httptest.Server
	withLocation bool
	caCert       *x509.Certificate
	caKey        *ecdsa.PrivateKey

	mu     sync.Mutex
	nonces int
	polls  int    // GETs of the order
	chain  []byte // PEM chain issued by the last finalize
}

func newFakeACMEServer(t *testing.T, withLocation bool) *fakeACMEServer {
	t.Helper()
	key := fakeACMEKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake acme ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeACMEServer{withLocation: withLocation, caCert: caCert, caKey: key}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeACMEServer) serve(w http.ResponseWriter, r *http.Request) {
	base := "http://" + r.Host
	f.mu.Lock()
	f.nonces++
	w.Header().Set("Replay-Nonce", fmt.Sprintf("nonce-%d", f.nonces))
	f.mu.Unlock()

	switch r.URL.Path {
	case "/directory":
		writeACMEJSON(w, http.StatusOK, map[string]string{
			"newNonce":   base + "/nonce",
			"newAccount": base + "/account",
			"newOrder":   base + "/new-order",
		})
	case "/nonce":
		w.WriteHeader(http.StatusOK)
	case "/account":
		w.Header().Set("Location", base+"/acct/1")
		writeACMEJSON(w, http.StatusCreated, map[string]string{"status": "valid"})
	case "/finalize/1":
		f.finalize(w, r, base)
	case "/order/1":
		f.mu.Lock()
		f.polls++
		status := "processing"
		if f.polls > 1 {
			status = "valid"
		}
		f.mu.Unlock()
		o := map[string]any{"status": status, "finalize": base + "/finalize/1", "authorizations": []string{}}
		if status == "valid" {
			o["certificate"] = base + "/cert/1"
		}
		writeACMEJSON(w, http.StatusOK, o)
	case "/cert/1":
		f.mu.Lock()
		chain := f.chain
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_, _ = w.Write(chain)
	default:
		http.NotFound(w, r)
	}
}

// finalize reads the CSR from the JWS payload, issues the certificate and
// answers "processing", which makes the client poll the order.
func (f *fakeACMEServer) finalize(w http.ResponseWriter, r *http.Request, base string) {
	var env struct {
		Payload string `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(env.Payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req struct {
		CSR string `json:"csr"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	csrDER, err := base64.RawURLEncoding.DecodeString(req.CSR)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	chain, err := f.issue(csr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f.mu.Lock()
	f.chain = chain
	f.mu.Unlock()
	if f.withLocation {
		w.Header().Set("Location", base+"/order/1")
	}
	writeACMEJSON(w, http.StatusOK, map[string]any{
		"status": "processing", "finalize": base + "/finalize/1", "authorizations": []string{},
	})
}

func (f *fakeACMEServer) issue(csr *x509.CertificateRequest) ([]byte, error) {
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()), Subject: csr.Subject, DNSNames: csr.DNSNames,
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, f.caCert, csr.PublicKey, f.caKey)
	if err != nil {
		return nil, err
	}
	leaf := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return append(leaf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.caCert.Raw})...), nil
}

func writeACMEJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fakeACMEKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := newP256Key()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// TestCreateCertWithoutFinalizeLocation runs CreateCert through the real
// acmeHTTP wrapper against a CA that omits Location on finalize (pebble) and
// against one that sends it (Let's Encrypt). Both must return a certificate
// URL whose download is the issued chain.
func TestCreateCertWithoutFinalizeLocation(t *testing.T) {
	const host = "app.example.test"
	for _, withLocation := range []bool{true, false} {
		t.Run(fmt.Sprintf("location=%t", withLocation), func(t *testing.T) {
			ca := newFakeACMEServer(t, withLocation)
			client := newACMEClient(ca.srv.URL+"/directory", fakeACMEKey(t), nil)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := client.Register(ctx, ""); err != nil {
				t.Fatalf("Register: %v", err)
			}
			csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
				Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
			}, fakeACMEKey(t))
			if err != nil {
				t.Fatal(err)
			}
			certURL, err := client.CreateCert(ctx, ca.srv.URL+"/order/1", ca.srv.URL+"/finalize/1", csr)
			if err != nil {
				t.Fatalf("CreateCert: %v", err)
			}
			der, err := client.FetchCert(ctx, certURL)
			if err != nil {
				t.Fatalf("FetchCert(%s): %v", certURL, err)
			}
			if len(der) != 2 {
				t.Fatalf("chain has %d certificates, want 2", len(der))
			}
			leaf, err := x509.ParseCertificate(der[0])
			if err != nil {
				t.Fatal(err)
			}
			if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != host {
				t.Fatalf("leaf DNS names %v, want [%s]", leaf.DNSNames, host)
			}
			if err := leaf.CheckSignatureFrom(ca.caCert); err != nil {
				t.Fatalf("leaf not signed by the CA: %v", err)
			}
		})
	}
}
