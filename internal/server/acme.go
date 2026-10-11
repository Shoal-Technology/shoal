package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"golang.org/x/crypto/acme"
)

// The directories for Config.AcmeDirectory. The production one is the
// default in the shoal server command; use staging for any test that reaches
// a real CA, since production enforces rate limits.
const (
	LetsEncryptProduction = "https://acme-v02.api.letsencrypt.org/directory"
	LetsEncryptStaging    = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// acmeClient is the part of the ACME protocol the certificate manager uses
// (ADR 0015, pass C). The real implementation wraps golang.org/x/crypto/acme.
// Tests use a fake, so no test contacts an ACME server.
type acmeClient interface {
	// Register creates the account for the client's key, or finds it, and
	// returns its URI.
	Register(ctx context.Context, email string) (string, error)
	// AuthorizeOrder opens an order for host.
	AuthorizeOrder(ctx context.Context, host string) (acmeOrder, error)
	// HTTP01 returns the HTTP-01 challenge of an authorization. The challenge
	// has an empty URL when the authorization is already valid.
	HTTP01(ctx context.Context, authzURL string) (acmeChallenge, error)
	// Accept tells the server the challenge response is in place.
	Accept(ctx context.Context, ch acmeChallenge) error
	// WaitOrder waits until the order is ready and returns its finalize URL.
	WaitOrder(ctx context.Context, orderURL string) (string, error)
	// CreateCert finalizes the order with a DER CSR and returns the URL of
	// the issued certificate. orderURL is the order's own URL: the CA may omit
	// it from the finalize response, and then the order is polled there.
	CreateCert(ctx context.Context, orderURL, finalizeURL string, csr []byte) (string, error)
	// FetchCert downloads the certificate chain as DER, leaf first.
	FetchCert(ctx context.Context, certURL string) ([][]byte, error)
}

type acmeOrder struct {
	URL            string
	Authorizations []string
}

type acmeChallenge struct {
	URL     string // challenge URL, used by Accept
	Token   string
	KeyAuth string // key authorization, served for the token
}

// rateLimitError is an ACME rate-limit answer. After is the wait the server
// asked for (Retry-After), or zero when it did not say.
type rateLimitError struct {
	After time.Duration
	Err   error
}

func (e *rateLimitError) Error() string { return e.Err.Error() }
func (e *rateLimitError) Unwrap() error { return e.Err }

// newACMEClient returns the real client for directory. roots, when set, adds
// trust for a test CA such as pebble.
func newACMEClient(directory string, key *ecdsa.PrivateKey, roots *x509.CertPool) acmeClient {
	hc := &http.Client{Timeout: 60 * time.Second}
	if roots != nil {
		hc.Transport = &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		}
	}
	return &acmeHTTP{cl: &acme.Client{Key: key, DirectoryURL: directory, HTTPClient: hc}}
}

// loadCARoots reads a PEM file of extra roots for the ACME server.
func loadCARoots(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read acme CA roots: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("acme CA roots %s: no PEM certificates", path)
	}
	return pool, nil
}

// acmeHTTP adapts x/crypto/acme to acmeClient.
type acmeHTTP struct {
	cl *acme.Client
}

func (a *acmeHTTP) Register(ctx context.Context, email string) (string, error) {
	acct := &acme.Account{}
	if email != "" {
		acct.Contact = []string{"mailto:" + email}
	}
	got, err := a.cl.Register(ctx, acct, acme.AcceptTOS)
	if errors.Is(err, acme.ErrAccountAlreadyExists) && got != nil {
		return got.URI, nil
	}
	if err != nil {
		return "", rateLimit(err)
	}
	return got.URI, nil
}

func (a *acmeHTTP) AuthorizeOrder(ctx context.Context, host string) (acmeOrder, error) {
	o, err := a.cl.AuthorizeOrder(ctx, []acme.AuthzID{{Type: "dns", Value: host}})
	if err != nil {
		return acmeOrder{}, rateLimit(err)
	}
	return acmeOrder{URL: o.URI, Authorizations: o.AuthzURLs}, nil
}

func (a *acmeHTTP) HTTP01(ctx context.Context, authzURL string) (acmeChallenge, error) {
	authz, err := a.cl.GetAuthorization(ctx, authzURL)
	if err != nil {
		return acmeChallenge{}, rateLimit(err)
	}
	if authz.Status == acme.StatusValid {
		return acmeChallenge{}, nil
	}
	for _, ch := range authz.Challenges {
		if ch.Type != "http-01" {
			continue
		}
		ka, err := a.cl.HTTP01ChallengeResponse(ch.Token)
		if err != nil {
			return acmeChallenge{}, err
		}
		return acmeChallenge{URL: ch.URI, Token: ch.Token, KeyAuth: ka}, nil
	}
	return acmeChallenge{}, fmt.Errorf("authorization %s offers no http-01 challenge", authzURL)
}

func (a *acmeHTTP) Accept(ctx context.Context, ch acmeChallenge) error {
	if _, err := a.cl.Accept(ctx, &acme.Challenge{URI: ch.URL}); err != nil {
		return rateLimit(err)
	}
	return nil
}

func (a *acmeHTTP) WaitOrder(ctx context.Context, orderURL string) (string, error) {
	o, err := a.cl.WaitOrder(ctx, orderURL)
	if err != nil {
		return "", rateLimit(err)
	}
	return o.FinalizeURL, nil
}

// CreateCert finalizes the order and returns the URL of the certificate.
//
// x/crypto/acme polls the order at the Location header of the finalize
// response. RFC 8555 allows a CA to omit that header (pebble does), and then
// the poll goes to "" and fails although the CA issued the certificate. In
// that case the order is polled at orderURL, which the caller already has.
func (a *acmeHTTP) CreateCert(ctx context.Context, orderURL, finalizeURL string, csr []byte) (string, error) {
	_, certURL, err := a.cl.CreateOrderCert(ctx, finalizeURL, csr, true)
	if err == nil && certURL != "" {
		return certURL, nil
	}
	if _, ok := acme.RateLimit(err); ok {
		return "", rateLimit(err)
	}
	o, werr := a.cl.WaitOrder(ctx, orderURL)
	if werr != nil {
		if err == nil {
			err = errors.New("order has no certificate URL")
		}
		return "", fmt.Errorf("finalize: %w; wait for order: %w", err, rateLimit(werr))
	}
	if o.CertURL == "" {
		return "", fmt.Errorf("order %s is %s with no certificate", orderURL, o.Status)
	}
	return o.CertURL, nil
}

func (a *acmeHTTP) FetchCert(ctx context.Context, certURL string) ([][]byte, error) {
	der, err := a.cl.FetchCert(ctx, certURL, true)
	if err != nil {
		return nil, rateLimit(err)
	}
	return der, nil
}

// rateLimit wraps err when the server reported a rate limit, keeping the
// Retry-After wait for the backoff.
func rateLimit(err error) error {
	if d, ok := acme.RateLimit(err); ok {
		return &rateLimitError{After: d, Err: err}
	}
	return err
}

// encodeECKey returns the PEM form of an EC private key.
func encodeECKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// decodeECKey parses the PEM form written by encodeECKey.
func decodeECKey(b []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("key is not PEM")
	}
	return x509.ParseECPrivateKey(blk.Bytes)
}

// newP256Key returns a fresh P-256 key.
func newP256Key() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}
