package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/store/memory"
)

// cmClock is the test clock for the certificate manager.
type cmClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *cmClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *cmClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *cmClock) Add(d time.Duration) { c.Set(c.Now().Add(d)) }

// cmCA is a fake ACME server. It issues 90-day certificates from its own
// root, counts calls, and can fail the next AuthorizeOrder calls on demand.
type cmCA struct {
	now    func() time.Time
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey

	mu        sync.Mutex
	fail      []error // consumed by AuthorizeOrder, one per call
	registers int
	orders    int
	accepts   int
	issued    int
	accepted  []string
	chains    map[string][][]byte
}

func newCMCA(t *testing.T, now func() time.Time) *cmCA {
	t.Helper()
	key, err := newP256Key()
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fake ACME Root"},
		NotBefore: now().Add(-time.Hour), NotAfter: now().Add(10 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &cmCA{now: now, caCert: cert, caKey: key, chains: map[string][][]byte{}}
}

func (c *cmCA) failNext(errs ...error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = append(c.fail, errs...)
}

func (c *cmCA) counts() (orders, accepts, issued int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.orders, c.accepts, c.issued
}

func (c *cmCA) Register(ctx context.Context, email string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.registers++
	return "https://acme.test/acct/1", nil
}

func (c *cmCA) AuthorizeOrder(ctx context.Context, host string) (acmeOrder, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.fail) > 0 {
		err := c.fail[0]
		c.fail = c.fail[1:]
		return acmeOrder{}, err
	}
	c.orders++
	return acmeOrder{URL: fmt.Sprintf("order-%d", c.orders), Authorizations: []string{fmt.Sprintf("authz-%d", c.orders)}}, nil
}

func (c *cmCA) HTTP01(ctx context.Context, authzURL string) (acmeChallenge, error) {
	token := "tok-" + authzURL
	return acmeChallenge{URL: "chal-" + authzURL, Token: token, KeyAuth: token + ".keyauth"}, nil
}

func (c *cmCA) Accept(ctx context.Context, ch acmeChallenge) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accepts++
	c.accepted = append(c.accepted, ch.Token)
	return nil
}

func (c *cmCA) WaitOrder(ctx context.Context, orderURL string) (string, error) {
	return "finalize-" + orderURL, nil
}

func (c *cmCA) CreateCert(ctx context.Context, orderURL, finalizeURL string, csr []byte) (string, error) {
	req, err := x509.ParseCertificateRequest(csr)
	if err != nil {
		return "", err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: req.Subject, DNSNames: req.DNSNames,
		NotBefore: c.now().Add(-time.Minute), NotAfter: c.now().Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.caCert, req.PublicKey, c.caKey)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.issued++
	url := fmt.Sprintf("cert-%d", c.issued)
	c.chains[url] = [][]byte{der, c.caCert.Raw}
	return url, nil
}

func (c *cmCA) FetchCert(ctx context.Context, certURL string) ([][]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ders, ok := c.chains[certURL]
	if !ok {
		return nil, errors.New("unknown certificate " + certURL)
	}
	return ders, nil
}

// cmFixture is a certificate manager over an encrypted in-memory store, with
// one tls auto Route per host.
type cmFixture struct {
	clock *cmClock
	raw   store.Store // what is on disk: sealed specs
	st    store.Store // what the server sees: decrypted
	ca    *cmCA
	acks  *edgeAcks
	m     *certManager
}

func newCMFixture(t *testing.T, hosts ...string) *cmFixture {
	t.Helper()
	ctx := context.Background()
	clock := &cmClock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	raw := memory.New(clock.Now)
	st, err := store.NewEncryptingStore(raw, bytes.Repeat([]byte{7}, 32), encryptedKinds...)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hosts {
		b, err := objects.Encode(&v1alpha1.RouteSpec{Host: h, PathPrefix: "/", Service: "web", Port: 80, Tls: "auto"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Create(ctx, store.Record{Kind: v1alpha1.KindRoute, Name: "r-" + h, Spec: b}); err != nil {
			t.Fatal(err)
		}
	}
	ca := newCMCA(t, clock.Now)
	acks := newEdgeAcks()
	m := &certManager{
		st: st, log: discard(), now: clock.Now,
		directory: "https://acme.test/directory", email: "ops@example.com",
		newClient: func(*ecdsa.PrivateKey) acmeClient { return ca },
		acks:      acks, interval: time.Hour, ackWait: 2 * time.Second, ackPoll: time.Millisecond,
		acceptTOS: true,
	}
	return &cmFixture{clock: clock, raw: raw, st: st, ca: ca, acks: acks, m: m}
}

// ackEdge connects an edge and, until ctx is done, records every current
// challenge as sent to it. That stands in for the server pushing snapshots.
func (f *cmFixture) ackEdge(ctx context.Context, node string) {
	f.acks.connect(node)
	go cmAck(ctx, f.st, f.acks, node)
}

func cmAck(ctx context.Context, st store.Store, acks *edgeAcks, node string) {
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	var rev int64
	for {
		recs, _ := st.List(ctx, v1alpha1.KindAcmeChallenge)
		tokens := make([]string, 0, len(recs))
		for _, r := range recs {
			tokens = append(tokens, r.Name)
		}
		// The stand-in edge applies each snapshot as soon as it is sent.
		rev++
		acks.sent(node, true, rev, tokens)
		acks.applied(node, rev)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// certOf returns the host's Certificate, decoded.
func (f *cmFixture) certOf(t *testing.T, host string) *v1alpha1.Certificate {
	t.Helper()
	rec, err := f.st.Get(context.Background(), v1alpha1.KindCertificate, host)
	if err != nil {
		t.Fatalf("certificate for %s: %v", host, err)
	}
	c, err := objects.Certificate(rec)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *cmFixture) pass(t *testing.T) {
	t.Helper()
	if err := f.m.syncOnce(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
}

func TestCertIssuedForNewRoute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newCMFixture(t, "app.example.com")
	f.ackEdge(ctx, "edge-1")

	f.pass(t)
	c := f.certOf(t, "app.example.com")
	if c.Spec.GetChainPem() == "" || c.Spec.GetKeyPem() == "" {
		t.Fatal("certificate has no chain or key")
	}
	if want := f.clock.Now().Add(90 * 24 * time.Hour); !c.Status.GetNotAfter().AsTime().Equal(want) {
		t.Fatalf("notAfter = %v, want %v", c.Status.GetNotAfter().AsTime(), want)
	}
	if c.Status.GetLastError() != "" || c.Status.GetFailures() != 0 {
		t.Fatalf("status after success: error=%q failures=%d", c.Status.GetLastError(), c.Status.GetFailures())
	}
	if _, accepts, issued := f.ca.counts(); accepts != 1 || issued != 1 {
		t.Fatalf("accepts=%d issued=%d, want 1 and 1", accepts, issued)
	}
	// The challenge is gone once the order completed.
	if recs, _ := f.st.List(context.Background(), v1alpha1.KindAcmeChallenge); len(recs) != 0 {
		t.Fatalf("%d challenge records left after completion", len(recs))
	}
}

func TestNoReissueWhileValid(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newCMFixture(t, "app.example.com")
	f.ackEdge(ctx, "edge-1")
	f.pass(t)
	f.clock.Add(24 * time.Hour)
	f.pass(t)
	f.clock.Add(30 * 24 * time.Hour) // 31 days before notAfter: not yet due
	f.pass(t)
	if orders, _, issued := f.ca.counts(); orders != 1 || issued != 1 {
		t.Fatalf("orders=%d issued=%d while the certificate is valid, want 1 and 1", orders, issued)
	}
}

func TestRenewWithin30Days(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newCMFixture(t, "app.example.com")
	f.ackEdge(ctx, "edge-1")
	f.pass(t)
	first := f.certOf(t, "app.example.com")
	notAfter := first.Status.GetNotAfter().AsTime()

	f.clock.Set(notAfter.Add(-31 * 24 * time.Hour))
	f.pass(t)
	if _, _, issued := f.ca.counts(); issued != 1 {
		t.Fatalf("renewed 31 days early: issued=%d", issued)
	}
	f.clock.Set(notAfter.Add(-29 * 24 * time.Hour))
	f.pass(t)
	if _, _, issued := f.ca.counts(); issued != 2 {
		t.Fatalf("not renewed within 30 days: issued=%d", issued)
	}
	second := f.certOf(t, "app.example.com")
	if second.Status.GetNotAfter().AsTime().Equal(notAfter) || second.Spec.GetChainPem() == first.Spec.GetChainPem() {
		t.Fatal("renewal did not replace the certificate")
	}
}

func TestBackoffAfterFailureAndNextAttemptHonoured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newCMFixture(t, "app.example.com")
	f.ackEdge(ctx, "edge-1")
	f.ca.failNext(errors.New("ca unavailable"), errors.New("ca unavailable again"))

	// First failure: 1 minute.
	if err := f.m.syncOnce(ctx); err == nil {
		t.Fatal("first pass should report the failure")
	}
	c := f.certOf(t, "app.example.com")
	if c.Status.GetFailures() != 1 || c.Status.GetLastError() != "ca unavailable" {
		t.Fatalf("after 1st failure: failures=%d error=%q", c.Status.GetFailures(), c.Status.GetLastError())
	}
	if got, want := c.Status.GetNextAttempt().AsTime(), f.clock.Now().Add(time.Minute); !got.Equal(want) {
		t.Fatalf("nextAttempt after 1st failure = %v, want %v", got, want)
	}
	// Before nextAttempt the manager does not call the CA at all.
	before, _, _ := f.ca.counts()
	f.clock.Add(30 * time.Second)
	_ = f.m.syncOnce(ctx)
	if orders, _, _ := f.ca.counts(); orders != before {
		t.Fatalf("CA called before nextAttempt: orders %d -> %d", before, orders)
	}

	// Second failure: 2 minutes.
	f.clock.Add(31 * time.Second)
	if err := f.m.syncOnce(ctx); err == nil {
		t.Fatal("second attempt should fail")
	}
	c = f.certOf(t, "app.example.com")
	if c.Status.GetFailures() != 2 {
		t.Fatalf("failures = %d, want 2", c.Status.GetFailures())
	}
	if got, want := c.Status.GetNextAttempt().AsTime(), f.clock.Now().Add(2*time.Minute); !got.Equal(want) {
		t.Fatalf("nextAttempt after 2nd failure = %v, want %v", got, want)
	}

	// After the wait the attempt succeeds and the failure state clears.
	f.clock.Add(2 * time.Minute)
	f.pass(t)
	c = f.certOf(t, "app.example.com")
	if c.Status.GetFailures() != 0 || c.Status.GetLastError() != "" || c.Status.GetNextAttempt() != nil {
		t.Fatalf("success did not clear failure state: %+v", c.Status)
	}
	if c.Spec.GetChainPem() == "" {
		t.Fatal("no certificate after the successful attempt")
	}
}

func TestRetryAfterFromRateLimitIsHonoured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newCMFixture(t, "app.example.com")
	f.ackEdge(ctx, "edge-1")
	f.ca.failNext(&rateLimitError{After: 3 * time.Hour, Err: errors.New("too many orders")})
	_ = f.m.syncOnce(ctx)
	c := f.certOf(t, "app.example.com")
	if got, want := c.Status.GetNextAttempt().AsTime(), f.clock.Now().Add(3*time.Hour); !got.Equal(want) {
		t.Fatalf("nextAttempt = %v, want the Retry-After %v", got, want)
	}
	if c.Status.GetLastError() == "" {
		t.Fatal("rate limit not recorded in lastError")
	}
}

func TestBackoffCapsAt24Hours(t *testing.T) {
	if got := backoffFor(1); got != time.Minute {
		t.Fatalf("backoff(1) = %v", got)
	}
	if got := backoffFor(2); got != 2*time.Minute {
		t.Fatalf("backoff(2) = %v", got)
	}
	if got := backoffFor(50); got != 24*time.Hour {
		t.Fatalf("backoff(50) = %v, want 24h", got)
	}
}

func TestFailedChallengeWithoutEdgeIsCleanedUp(t *testing.T) {
	f := newCMFixture(t, "app.example.com")
	// No edge connected: the challenge cannot be sent anywhere.
	f.m.ackWait = 50 * time.Millisecond
	if err := f.m.syncOnce(context.Background()); err == nil {
		t.Fatal("issuance without an edge should fail")
	}
	c := f.certOf(t, "app.example.com")
	if c.Status.GetLastError() == "" || c.Spec.GetChainPem() != "" {
		t.Fatalf("status error=%q chain=%d bytes", c.Status.GetLastError(), len(c.Spec.GetChainPem()))
	}
	if recs, _ := f.st.List(context.Background(), v1alpha1.KindAcmeChallenge); len(recs) != 0 {
		t.Fatalf("%d challenge records left after a failed order", len(recs))
	}
	if _, accepts, _ := f.ca.counts(); accepts != 0 {
		t.Fatal("challenge accepted before the edge was sent it")
	}
}

func TestCertificateAndAccountStoredEncrypted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newCMFixture(t, "app.example.com")
	f.ackEdge(ctx, "edge-1")
	f.pass(t)

	cert := f.certOf(t, "app.example.com")
	keyPEM := []byte(cert.Spec.GetKeyPem())
	if len(keyPEM) == 0 {
		t.Fatal("no key to check against")
	}
	raw, err := f.raw.Get(ctx, v1alpha1.KindCertificate, "app.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw.Spec, keyPEM) || bytes.Contains(raw.Spec, []byte("PRIVATE KEY")) {
		t.Fatal("raw Certificate record holds a plaintext key")
	}

	acct, err := f.raw.Get(ctx, v1alpha1.KindAcmeAccount, f.m.directory)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(acct.Spec, []byte("PRIVATE KEY")) {
		t.Fatal("raw AcmeAccount record holds a plaintext key")
	}
	// The server still reads the key back through the encrypting store.
	dec, err := f.st.Get(ctx, v1alpha1.KindAcmeAccount, f.m.directory)
	if err != nil {
		t.Fatal(err)
	}
	a, err := objects.AcmeAccount(dec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeECKey([]byte(a.Spec.GetAccountKeyPem())); err != nil {
		t.Fatalf("account key does not round-trip: %v", err)
	}
}

func TestCertificateForRemovedRouteIsDropped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newCMFixture(t, "app.example.com")
	f.ackEdge(ctx, "edge-1")
	f.pass(t)
	if err := f.st.Delete(ctx, v1alpha1.KindRoute, "r-app.example.com", 0); err != nil {
		t.Fatal(err)
	}
	f.pass(t)
	if _, err := f.st.Get(ctx, v1alpha1.KindCertificate, "app.example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("certificate of a deleted Route still exists: %v", err)
	}
}

// TestCertManagerRunsOnlyWhileLeader drives the real leader tasks through
// runWhileLeader. A node that is not leader must not issue anything.
func TestCertManagerRunsOnlyWhileLeader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &cmClock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	ca := newCMCA(t, clock.Now)
	gate := newLeaderGate()
	s, err := New(ctx, Config{
		DataDir: t.TempDir(), AdvertiseAddress: "127.0.0.1:7443", Log: discard(), Now: clock.Now,
		AcmeDirectory: "https://acme.test/directory", AcmeEmail: "ops@example.com", AcmeAcceptTOS: true,
		acmeClientFor: func(string, *ecdsa.PrivateKey) acmeClient { return ca },
		leaderGate:    gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.certs == nil {
		t.Fatal("certificate manager not built")
	}
	b, err := objects.Encode(&v1alpha1.RouteSpec{Host: "app.example.com", PathPrefix: "/", Service: "web", Port: 80, Tls: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Create(ctx, store.Record{Kind: v1alpha1.KindRoute, Name: "r", Spec: b}); err != nil {
		t.Fatal(err)
	}
	s.acks.connect("edge-1")
	go cmAck(ctx, s.store, s.acks, "edge-1")

	leadership := make(chan bool)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runWhileLeader(ctx, leadership, s.runLeaderTasks)
	}()

	time.Sleep(300 * time.Millisecond)
	if holders, _ := gate.snapshot(); len(holders) != 0 {
		t.Fatalf("leader tasks ran on a follower: %v", holders)
	}
	if orders, _, _ := ca.counts(); orders != 0 {
		t.Fatalf("follower ordered %d certificates", orders)
	}

	leadership <- true
	eventually(t, "certificate issued by the leader", func() bool {
		_, _, issued := ca.counts()
		return issued == 1
	})
	if holders, _ := gate.snapshot(); len(holders) != 1 {
		t.Fatalf("leader tasks holders = %v, want one", holders)
	}
	cancel()
	<-done
}

// TestRouteSnapshotCarriesChallengesAndCertificate checks what an edge gets
// during and after issuance: the token while it is pending, the chain and
// key once issued, and nothing for a node that is not an edge.
func TestRouteSnapshotCarriesChallengesAndCertificate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newCMFixture(t, "app.example.com")
	put := func(kind, name string, spec proto.Message, labels map[string]string) {
		t.Helper()
		b, err := objects.Encode(spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.Create(ctx, store.Record{Kind: kind, Name: name, Spec: b}); err != nil {
			t.Fatal(err)
		}
	}
	put(v1alpha1.KindNode, "edge-1", &v1alpha1.NodeSpec{Labels: map[string]string{"shoal.dev/edge": "true"}}, nil)
	put(v1alpha1.KindNode, "plain", &v1alpha1.NodeSpec{}, nil)
	srv := &Server{store: f.st, log: discard()}
	srv.cfg.Now = f.clock.Now

	// Before issuance: a pending challenge with a fresh expiry is sent.
	authz, err := f.ca.HTTP01(ctx, "authz-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.putChallenge(ctx, "app.example.com", authz); err != nil {
		t.Fatal(err)
	}
	snap, err := srv.routeSnapshot(ctx, "edge-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.GetChallenges()) != 1 || snap.GetChallenges()[0].GetToken() != authz.Token ||
		snap.GetChallenges()[0].GetKeyAuthorization() != authz.KeyAuth {
		t.Fatalf("challenges = %v", snap.GetChallenges())
	}
	if len(snap.GetCertificates()) != 0 {
		t.Fatal("certificate sent before issuance")
	}

	// An expired token is not sent.
	f.clock.Add(11 * time.Minute)
	snap, err = srv.routeSnapshot(ctx, "edge-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.GetChallenges()) != 0 {
		t.Fatal("expired challenge was sent")
	}

	// After issuance the chain and key go to the edge, and only the edge.
	f.clock.Add(-11 * time.Minute)
	f.acks.connect("edge-1")
	ctxAck, cancelAck := context.WithCancel(ctx)
	defer cancelAck()
	go cmAck(ctxAck, f.st, f.acks, "edge-1")
	f.pass(t)
	snap, err = srv.routeSnapshot(ctx, "edge-1")
	if err != nil {
		t.Fatal(err)
	}
	cert := f.certOf(t, "app.example.com")
	if len(snap.GetCertificates()) != 1 || snap.GetCertificates()[0].GetHost() != "app.example.com" ||
		string(snap.GetCertificates()[0].GetChainPem()) != cert.Spec.GetChainPem() ||
		string(snap.GetCertificates()[0].GetKeyPem()) != cert.Spec.GetKeyPem() {
		t.Fatalf("edge certificates = %v", snap.GetCertificates())
	}
	plain, err := srv.routeSnapshot(ctx, "plain")
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.GetCertificates()) != 0 || len(plain.GetChallenges()) != 0 {
		t.Fatal("a non-edge node received certificate or challenge material")
	}
}

func TestEdgeAcksPushedSemantics(t *testing.T) {
	a := newEdgeAcks()
	if a.pushed([]string{"t1"}) {
		t.Fatal("no edge connected, yet pushed")
	}
	a.connect("edge-1")
	a.connect("plain")
	a.sent("plain", false, 1, []string{"t1"})
	if a.pushed([]string{"t1"}) {
		t.Fatal("a connected node that is not an edge counted")
	}
	a.sent("edge-1", true, 1, []string{"t1"})
	a.applied("edge-1", 1)
	a.connect("edge-2")
	a.sent("edge-2", true, 1, nil)
	a.applied("edge-2", 1)
	if a.pushed([]string{"t1"}) {
		t.Fatal("edge-2 has not been sent t1, yet pushed")
	}
	a.sent("edge-2", true, 2, []string{"t1"})
	if a.pushed([]string{"t1"}) {
		t.Fatal("edge-2 was sent t1 at revision 2 but has applied 1, yet pushed")
	}
	a.applied("edge-2", 2)
	if !a.pushed([]string{"t1"}) {
		t.Fatal("every edge applied t1, not pushed")
	}
	a.disconnect("edge-2")
	a.disconnect("edge-1")
	if a.pushed([]string{"t1"}) {
		t.Fatal("no edge left, yet pushed")
	}
}

// An edge that has been sent the challenge but has not reported it applied
// keeps the leader waiting, and a stale applied report never moves it back.
func TestEdgeAcksWaitForApplied(t *testing.T) {
	a := newEdgeAcks()
	a.connect("edge-1")
	a.sent("edge-1", true, 5, []string{"t1"})
	a.applied("edge-1", 4)
	if a.pushed([]string{"t1"}) {
		t.Fatal("applied revision 4 is older than the sent revision 5, yet pushed")
	}
	a.applied("edge-1", 5)
	if !a.pushed([]string{"t1"}) {
		t.Fatal("applied revision 5 reached, not pushed")
	}
	a.applied("edge-1", 2)
	if !a.pushed([]string{"t1"}) {
		t.Fatal("a stale applied report moved the acknowledgement back")
	}
}

// The leader waits for an edge that is slow to apply the snapshot carrying
// the challenge, and succeeds once the edge reports it applied.
func TestWaitPushedWaitsForEdgeToApply(t *testing.T) {
	a := newEdgeAcks()
	a.connect("edge-1")
	a.sent("edge-1", true, 1, []string{"t1"})
	m := &certManager{acks: a, ackWait: 2 * time.Second, ackPoll: time.Millisecond}
	go func() {
		time.Sleep(30 * time.Millisecond)
		a.applied("edge-1", 1)
	}()
	start := time.Now()
	if err := m.waitPushed(context.Background(), []string{"t1"}); err != nil {
		t.Fatalf("waitPushed: %v", err)
	}
	if time.Since(start) < 20*time.Millisecond {
		t.Fatal("waitPushed returned before the edge applied the snapshot")
	}
}

// If the edge never reports the revision applied, waitPushed gives up at the cap.
func TestWaitPushedCapsWhenEdgeNeverApplies(t *testing.T) {
	a := newEdgeAcks()
	a.connect("edge-1")
	a.sent("edge-1", true, 1, []string{"t1"})
	m := &certManager{acks: a, ackWait: 50 * time.Millisecond, ackPoll: time.Millisecond}
	err := m.waitPushed(context.Background(), []string{"t1"})
	if err == nil {
		t.Fatal("waitPushed succeeded although the edge never applied the snapshot")
	}
}

// Without the operator's --acme-accept-tos no account is registered, no
// order is placed, and the Route reports CertError with the fixed message.
func TestTOSNotAcceptedSetsCertError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newCMFixture(t, "app.example.com")
	f.ackEdge(ctx, "edge-1")
	f.m.acceptTOS = false

	f.pass(t)
	f.pass(t) // a second pass must not write the same error again
	orders, accepts, _ := f.ca.counts()
	if orders != 0 || accepts != 0 || f.ca.registers != 0 {
		t.Fatalf("CA was contacted without accepting terms: registers=%d orders=%d accepts=%d", f.ca.registers, orders, accepts)
	}
	c := f.certOf(t, "app.example.com")
	if got := c.Status.GetLastError(); got != errTOSNotAccepted {
		t.Fatalf("lastError = %q, want %q", got, errTOSNotAccepted)
	}
	if c.Spec.GetChainPem() != "" {
		t.Fatal("a certificate exists without accepted terms")
	}
	if errTOSNotAccepted != "ACME terms not accepted; start the server with --acme-accept-tos" {
		t.Fatalf("message changed: %q", errTOSNotAccepted)
	}

	// Accepting the terms later issues the certificate on the next pass.
	f.m.acceptTOS = true
	f.pass(t)
	if orders, _, _ := f.ca.counts(); orders == 0 {
		t.Fatal("no order after the terms were accepted")
	}
}
