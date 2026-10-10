package edge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// acmeChallengePrefix is the path ACME HTTP-01 validation uses.
const acmeChallengePrefix = "/.well-known/acme-challenge/"

var errNoCertificate = errors.New("edge: no certificate for this server name")

// CertStore holds certificates keyed by host name. Lookups read an atomic
// pointer, so Replace never blocks a handshake.
type CertStore struct {
	certs atomic.Pointer[map[string]*tls.Certificate]
}

// Replace swaps in a new certificate set. Existing connections keep the
// certificate they already negotiated.
func (s *CertStore) Replace(certs map[string]*tls.Certificate) {
	m := make(map[string]*tls.Certificate, len(certs))
	for host, c := range certs {
		m[normalizeHost(host)] = c
	}
	s.certs.Store(&m)
}

// GetCertificate picks the certificate for the SNI name. An unknown or empty
// name fails the handshake; there is no default certificate.
func (s *CertStore) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if m := s.certs.Load(); m != nil {
		if c, ok := (*m)[normalizeHost(hello.ServerName)]; ok && c != nil {
			return c, nil
		}
	}
	return nil, errNoCertificate
}

// TLSConfig returns the edge's server TLS config: SNI selection from this
// store, HTTP/2 preferred with HTTP/1.1 as fallback.
func (s *CertStore) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: s.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}
}

// SelfSigned makes a self-signed ECDSA P-256 certificate valid for hosts. It
// is for tests and local development only.
func SelfSigned(hosts ...string) (*tls.Certificate, error) {
	if len(hosts) == 0 {
		return nil, errors.New("edge: SelfSigned needs at least one host")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hosts[0]},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// DefaultChallengeHandler answers every ACME challenge with 404. It stands in
// until pass C serves real HTTP-01 tokens.
var DefaultChallengeHandler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
})

// HTTPHandler is the port 80 handler. ACME challenge paths go to challenge
// (DefaultChallengeHandler when nil). Routes with redirectHttp get a 308 to
// https. Everything else goes through the proxy.
func HTTPHandler(p *Proxy, challenge http.Handler) http.Handler {
	if challenge == nil {
		challenge = DefaultChallengeHandler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, acmeChallengePrefix) {
			challenge.ServeHTTP(w, r)
			return
		}
		if e := p.table.Load().Lookup(r.Host, r.URL.Path); e != nil && e.RedirectHTTP {
			w.Header().Set("Location", "https://"+e.Host+r.URL.RequestURI())
			w.WriteHeader(http.StatusPermanentRedirect)
			return
		}
		p.ServeHTTP(w, r)
	})
}
