package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/objects"
	"example.com/shoal/internal/store"
)

// Automatic certificates (ADR 0015, pass C). The manager runs only on the
// leader, next to the controllers. For every Route with tls: auto it keeps a
// Certificate for the host valid: it issues one when none exists, renews it
// within certRenewBefore of notAfter, and backs off after failures.
const (
	certRenewBefore = 30 * 24 * time.Hour
	certBackoffBase = time.Minute
	certBackoffMax  = 24 * time.Hour
	challengeTTL    = 10 * time.Minute
	defaultAckWait  = 2 * time.Minute
	defaultAckPoll  = 100 * time.Millisecond
	defaultCertTick = time.Minute
	maxCertErrorLen = 1024
)

// edgeAcks records, per connected agent stream, whether the node is an edge
// and which HTTP-01 tokens its last snapshot carried. The leader waits on it
// before it asks the CA to validate a challenge.
//
// The signal is "sent": the server has handed the snapshot to the node's gRPC
// stream. It does not prove the edge applied it. The window is one stream
// send, and the CA's validation takes at least a round trip after Accept. An
// acknowledgement on the heartbeat would close the window; it is left for a
// later pass.
type edgeAcks struct {
	mu    sync.Mutex
	conns map[string]*edgeConn // node -> stream state, present while connected
}

type edgeConn struct {
	edge   bool
	tokens map[string]bool
}

func newEdgeAcks() *edgeAcks { return &edgeAcks{conns: map[string]*edgeConn{}} }

// connect registers a node's Sync stream. It has no tokens until a snapshot is sent.
func (a *edgeAcks) connect(node string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conns[node] = &edgeConn{tokens: map[string]bool{}}
}

func (a *edgeAcks) disconnect(node string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.conns, node)
}

// sent records the route snapshot just handed to node's stream.
func (a *edgeAcks) sent(node string, edge bool, tokens []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.conns[node]
	if !ok {
		return
	}
	c.edge = edge
	c.tokens = make(map[string]bool, len(tokens))
	for _, t := range tokens {
		c.tokens[t] = true
	}
}

// pushed reports whether at least one edge is connected and every connected
// edge was sent all of tokens.
func (a *edgeAcks) pushed(tokens []string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	edges := 0
	for _, c := range a.conns {
		if !c.edge {
			continue
		}
		edges++
		for _, t := range tokens {
			if !c.tokens[t] {
				return false
			}
		}
	}
	return edges > 0
}

// challengeTokens returns the tokens a snapshot carries.
func challengeTokens(snap *v1alpha1.RouteSnapshot) []string {
	out := make([]string, 0, len(snap.GetChallenges()))
	for _, c := range snap.GetChallenges() {
		out = append(out, c.GetToken())
	}
	return out
}

// certManager issues and renews the certificates of tls: auto Routes.
type certManager struct {
	st        store.Store
	log       *slog.Logger
	now       func() time.Time
	directory string
	email     string
	// newClient returns an ACME client for the account key.
	newClient func(key *ecdsa.PrivateKey) acmeClient
	acks      *edgeAcks
	interval  time.Duration // pass period; a Route change also triggers one
	ackWait   time.Duration // how long to wait for edges to be sent a challenge
	ackPoll   time.Duration
}

// Run makes passes until ctx is done. It runs only while this node is leader.
func (m *certManager) Run(ctx context.Context) {
	if m.interval <= 0 {
		m.interval = defaultCertTick
	}
	events := m.st.Watch(ctx, v1alpha1.KindRoute, v1alpha1.KindCertificate)
	tick := time.NewTicker(m.interval)
	defer tick.Stop()
	for {
		if err := m.syncOnce(ctx); err != nil && ctx.Err() == nil {
			m.log.Warn("certificate pass incomplete", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case _, ok := <-events:
			if !ok {
				return
			}
		}
	}
}

// syncOnce makes one pass over the auto Routes. It also deletes the
// Certificate of a host that no longer has an auto Route.
func (m *certManager) syncOnce(ctx context.Context) error {
	rrecs, err := m.st.List(ctx, v1alpha1.KindRoute)
	if err != nil {
		return err
	}
	hosts := map[string]bool{}
	for _, r := range rrecs {
		rt, err := objects.Route(r)
		if err != nil {
			return err // without the full list, certificates must not be dropped
		}
		if rt.Spec.GetTls() == "auto" {
			hosts[rt.Spec.GetHost()] = true
		}
	}
	var errs []error
	crecs, err := m.st.List(ctx, v1alpha1.KindCertificate)
	if err != nil {
		return err
	}
	for _, c := range crecs {
		if hosts[c.Name] {
			continue
		}
		if err := m.st.Delete(ctx, v1alpha1.KindCertificate, c.Name, 0); err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	for _, h := range slices.Sorted(maps.Keys(hosts)) {
		if ctx.Err() != nil {
			break
		}
		if err := m.ensure(ctx, h); err != nil {
			errs = append(errs, fmt.Errorf("certificate for %s: %w", h, err))
		}
	}
	return errors.Join(errs...)
}

// ensure makes the host's certificate valid, if it is due and allowed.
func (m *certManager) ensure(ctx context.Context, host string) error {
	now := m.now()
	cert, err := m.get(ctx, host)
	if err != nil {
		return err
	}
	if cert != nil {
		if cert.Spec.GetChainPem() != "" && now.Before(cert.Status.GetNotAfter().AsTime().Add(-certRenewBefore)) {
			return nil // valid, not yet due for renewal
		}
		if next := cert.Status.GetNextAttempt(); next != nil && now.Before(next.AsTime()) {
			return nil // backing off
		}
	}
	if err := m.issue(ctx, host); err != nil {
		return m.fail(ctx, host, err)
	}
	return nil
}

// get returns the host's Certificate, or nil when there is none.
func (m *certManager) get(ctx context.Context, host string) (*v1alpha1.Certificate, error) {
	rec, err := m.st.Get(ctx, v1alpha1.KindCertificate, host)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return objects.Certificate(rec)
}

// issue runs one HTTP-01 order for host and stores the certificate. The
// challenge records are deleted when it returns, whatever the outcome.
func (m *certManager) issue(ctx context.Context, host string) error {
	client, err := m.account(ctx)
	if err != nil {
		return err
	}
	order, err := client.AuthorizeOrder(ctx, host)
	if err != nil {
		return err
	}
	var pending []acmeChallenge
	for _, authz := range order.Authorizations {
		ch, err := client.HTTP01(ctx, authz)
		if err != nil {
			return err
		}
		if ch.URL != "" {
			pending = append(pending, ch)
		}
	}
	defer m.dropChallenges(context.WithoutCancel(ctx), pending)

	tokens := make([]string, 0, len(pending))
	for _, ch := range pending {
		if err := m.putChallenge(ctx, host, ch); err != nil {
			return err
		}
		tokens = append(tokens, ch.Token)
	}
	if len(pending) > 0 {
		if err := m.waitPushed(ctx, tokens); err != nil {
			return err
		}
		for _, ch := range pending {
			if err := client.Accept(ctx, ch); err != nil {
				return err
			}
		}
	}
	finalize, err := client.WaitOrder(ctx, order.URL)
	if err != nil {
		return err
	}
	key, err := newP256Key()
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
	}, key)
	if err != nil {
		return err
	}
	certURL, err := client.CreateCert(ctx, order.URL, finalize, csr)
	if err != nil {
		return err
	}
	der, err := client.FetchCert(ctx, certURL)
	if err != nil {
		return err
	}
	if len(der) == 0 {
		return errors.New("CA returned an empty chain")
	}
	leaf, err := x509.ParseCertificate(der[0])
	if err != nil {
		return fmt.Errorf("parse issued certificate: %w", err)
	}
	var chain []byte
	for _, d := range der {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})...)
	}
	keyPEM, err := encodeECKey(key)
	if err != nil {
		return err
	}
	if err := m.storeCert(ctx, host, chain, keyPEM, leaf); err != nil {
		return err
	}
	m.log.Info("certificate issued", "host", host, "notAfter", leaf.NotAfter)
	return nil
}

// account returns the client for the directory's account, registering one
// (and storing its key, encrypted) the first time.
func (m *certManager) account(ctx context.Context) (acmeClient, error) {
	rec, err := m.st.Get(ctx, v1alpha1.KindAcmeAccount, m.directory)
	if err == nil {
		acct, err := objects.AcmeAccount(rec)
		if err != nil {
			return nil, err
		}
		key, err := decodeECKey([]byte(acct.Spec.GetAccountKeyPem()))
		if err != nil {
			return nil, fmt.Errorf("account key: %w", err)
		}
		return m.newClient(key), nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	key, err := newP256Key()
	if err != nil {
		return nil, err
	}
	client := m.newClient(key)
	uri, err := client.Register(ctx, m.email)
	if err != nil {
		return nil, fmt.Errorf("register acme account: %w", err)
	}
	keyPEM, err := encodeECKey(key)
	if err != nil {
		return nil, err
	}
	spec, err := objects.Encode(&v1alpha1.AcmeAccountSpec{AccountKeyPem: string(keyPEM), DirectoryUrl: m.directory, Email: m.email})
	if err != nil {
		return nil, err
	}
	status, err := objects.Encode(&v1alpha1.AcmeAccountStatus{RegistrationUri: uri})
	if err != nil {
		return nil, err
	}
	if _, err := m.st.Create(ctx, store.Record{Kind: v1alpha1.KindAcmeAccount, Name: m.directory, Spec: spec, Status: status}); err != nil {
		return nil, fmt.Errorf("store acme account: %w", err)
	}
	m.log.Info("acme account registered", "directory", m.directory, "uri", uri)
	return client, nil
}

// putChallenge stores the token so the edges can answer it.
func (m *certManager) putChallenge(ctx context.Context, host string, ch acmeChallenge) error {
	spec, err := objects.Encode(&v1alpha1.AcmeChallengeSpec{
		Host: host, KeyAuthorization: ch.KeyAuth, ExpiresAt: timestamppb.New(m.now().Add(challengeTTL)),
	})
	if err != nil {
		return err
	}
	_, err = m.st.Create(ctx, store.Record{Kind: v1alpha1.KindAcmeChallenge, Name: ch.Token, Spec: spec})
	if errors.Is(err, store.ErrExists) {
		cur, err := m.st.Get(ctx, v1alpha1.KindAcmeChallenge, ch.Token)
		if err != nil {
			return err
		}
		_, err = m.st.Update(ctx, v1alpha1.KindAcmeChallenge, ch.Token, spec, cur.Generation)
		return err
	}
	return err
}

func (m *certManager) dropChallenges(ctx context.Context, chs []acmeChallenge) {
	for _, ch := range chs {
		if err := m.st.Delete(ctx, v1alpha1.KindAcmeChallenge, ch.Token, 0); err != nil && !errors.Is(err, store.ErrNotFound) {
			m.log.Warn("delete acme challenge", "token", ch.Token, "err", err)
		}
	}
}

// waitPushed blocks until every connected edge was sent the tokens, or the
// wait runs out. With no edge connected it waits too: a challenge nobody can
// answer fails the order, and the backoff handles the retry.
func (m *certManager) waitPushed(ctx context.Context, tokens []string) error {
	wait := m.ackWait
	if wait <= 0 {
		wait = defaultAckWait
	}
	poll := m.ackPoll
	if poll <= 0 {
		poll = defaultAckPoll
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		if m.acks.pushed(tokens) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("no connected edge was sent the challenge within %s", wait)
		case <-tick.C:
		}
	}
}

// storeCert saves an issued certificate and clears the failure state.
func (m *certManager) storeCert(ctx context.Context, host string, chain, keyPEM []byte, leaf *x509.Certificate) error {
	spec, err := objects.Encode(&v1alpha1.CertificateSpec{ChainPem: string(chain), KeyPem: string(keyPEM)})
	if err != nil {
		return err
	}
	status, err := objects.Encode(&v1alpha1.CertificateStatus{
		NotBefore: timestamppb.New(leaf.NotBefore), NotAfter: timestamppb.New(leaf.NotAfter), Issuer: leaf.Issuer.String(),
	})
	if err != nil {
		return err
	}
	rec, err := m.st.Get(ctx, v1alpha1.KindCertificate, host)
	switch {
	case errors.Is(err, store.ErrNotFound):
		_, err = m.st.Create(ctx, store.Record{Kind: v1alpha1.KindCertificate, Name: host, Spec: spec, Status: status})
		return err
	case err != nil:
		return err
	}
	if _, err := m.st.Update(ctx, v1alpha1.KindCertificate, host, spec, rec.Generation); err != nil {
		return err
	}
	_, err = m.st.UpdateStatus(ctx, v1alpha1.KindCertificate, host, status)
	return err
}

// fail records a failed issuance and sets nextAttempt. The backoff doubles
// from certBackoffBase up to certBackoffMax; a Retry-After from the CA is
// honoured when it is longer.
func (m *certManager) fail(ctx context.Context, host string, cause error) error {
	cur, err := m.get(ctx, host)
	if err != nil {
		return err
	}
	st := &v1alpha1.CertificateStatus{}
	if cur != nil {
		st = proto.Clone(cur.Status).(*v1alpha1.CertificateStatus)
	}
	st.Failures++
	delay := backoffFor(st.Failures)
	var rl *rateLimitError
	if errors.As(cause, &rl) && rl.After > delay {
		delay = rl.After
	}
	msg := cause.Error()
	if len(msg) > maxCertErrorLen {
		msg = msg[:maxCertErrorLen]
	}
	st.LastError = msg
	st.NextAttempt = timestamppb.New(m.now().Add(delay))
	if err := m.putStatus(ctx, host, st); err != nil {
		return err
	}
	m.log.Warn("certificate issuance failed", "host", host, "retryIn", delay, "err", cause)
	return cause
}

// putStatus writes a Certificate status, creating an empty record first
// when the host has none yet, so that its backoff survives restarts.
func (m *certManager) putStatus(ctx context.Context, host string, st *v1alpha1.CertificateStatus) error {
	status, err := objects.Encode(st)
	if err != nil {
		return err
	}
	_, err = m.st.Get(ctx, v1alpha1.KindCertificate, host)
	switch {
	case errors.Is(err, store.ErrNotFound):
		empty, err := objects.Encode(&v1alpha1.CertificateSpec{})
		if err != nil {
			return err
		}
		_, err = m.st.Create(ctx, store.Record{Kind: v1alpha1.KindCertificate, Name: host, Spec: empty, Status: status})
		return err
	case err != nil:
		return err
	}
	_, err = m.st.UpdateStatus(ctx, v1alpha1.KindCertificate, host, status)
	return err
}

// newCertManager builds the manager for a server. The ACME client comes from
// the test hook when set; otherwise it is the real one for cfg.AcmeDirectory.
func newCertManager(s *Server, cfg Config) (*certManager, error) {
	factory := cfg.acmeClientFor
	if factory == nil {
		var roots *x509.CertPool
		if cfg.AcmeCARoots != "" {
			r, err := loadCARoots(cfg.AcmeCARoots)
			if err != nil {
				return nil, err
			}
			roots = r
		}
		factory = func(directory string, key *ecdsa.PrivateKey) acmeClient {
			return newACMEClient(directory, key, roots)
		}
	}
	dir := cfg.AcmeDirectory
	return &certManager{
		st: s.store, log: cfg.Log, now: s.now, directory: dir, email: cfg.AcmeEmail,
		newClient: func(key *ecdsa.PrivateKey) acmeClient { return factory(dir, key) },
		acks:      s.acks, interval: defaultCertTick, ackWait: defaultAckWait, ackPoll: defaultAckPoll,
	}, nil
}

// backoffFor returns the wait after the given number of consecutive failures.
func backoffFor(failures int32) time.Duration {
	d := certBackoffBase
	for i := int32(1); i < failures; i++ {
		d *= 2
		if d >= certBackoffMax {
			return certBackoffMax
		}
	}
	return d
}
