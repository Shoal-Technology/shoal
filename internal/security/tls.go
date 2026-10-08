package security

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync"
)

// ServerTLS returns the server's TLS config. Client certificates are
// verified when presented; which services require one is decided per RPC
// (Join is the only one that doesn't).
func ServerTLS(ca, leaf *x509.Certificate, key crypto.Signer) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The CA is part of the chain so that joining agents can check it
		// against their pinned hash.
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw, ca.Raw}, PrivateKey: key, Leaf: leaf}},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
	}
}

// ClientTLS returns a client TLS config that trusts only ca and presents the
// certificate returned by getCert (which may change, e.g. after renewal).
func ClientTLS(ca *x509.Certificate, getCert func() (*tls.Certificate, error)) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
		ServerName: ServerName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return getCert()
		},
	}
}

// PinnedCA records the outcome of PinnedTLS verification.
type PinnedCA struct {
	mu   sync.Mutex
	cert *x509.Certificate
	err  error
}

// Cert returns the verified CA, or nil before a successful handshake.
func (p *PinnedCA) Cert() *x509.Certificate {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cert
}

// Err returns why the last handshake's server was not trusted, if it
// wasn't. gRPC reports such failures as a generic Unavailable error, which
// looks retryable; this tells callers that retrying won't help.
func (p *PinnedCA) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *PinnedCA) set(cert *x509.Certificate, err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cert, p.err = cert, err
	return err
}

// PinnedTLS returns a client TLS config for joining, before the agent has
// the CA certificate. The server is trusted only if its chain contains a CA
// whose public key matches caHash and that CA signed a valid server
// certificate for ServerName.
func PinnedTLS(caHash string, got *PinnedCA) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Standard verification needs the CA up front; VerifyPeerCertificate
		// below does the equivalent check against the pinned key instead.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			ca, err := verifyPinned(raw, caHash)
			return got.set(ca, err)
		},
	}
}

func verifyPinned(raw [][]byte, caHash string) (*x509.Certificate, error) {
	if len(raw) == 0 {
		return nil, errors.New("server presented no certificate")
	}
	certs := make([]*x509.Certificate, 0, len(raw))
	for _, der := range raw {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("parse server certificate: %w", err)
		}
		certs = append(certs, c)
	}
	var ca *x509.Certificate
	for _, c := range certs[1:] {
		if c.IsCA && CAHash(c) == caHash {
			ca = c
		}
	}
	if ca == nil {
		return nil, fmt.Errorf("server CA does not match the pinned hash %s", caHash)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := certs[0].Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   ServerName,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return nil, fmt.Errorf("verify server certificate: %w", err)
	}
	return ca, nil
}
