package agent

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/edge"
	"github.com/Shoal-Technology/shoal/internal/security"
)

// RouteSink receives the route snapshots the server sends to edge nodes.
type RouteSink interface {
	Apply(*v1alpha1.RouteSnapshot) error
	// AppliedRevision is the revision of the last snapshot applied. The
	// heartbeat reports it so the leader knows the edge has the tokens.
	AppliedRevision() int64
}

// cacheFile is the name of the route cache inside the cache directory.
const cacheFile = "routes.pb"

// Edge runs the ingress listeners on a node the server marks as an edge (ADR
// 0015, pass B2). It stays idle until a snapshot says the node is an edge, and
// it closes its listeners when a later snapshot says it is not.
//
// Certificates and private keys live only in memory. Nothing is written to
// disk, as ADR 0015 decides. The optional route cache (UseCache, pass E)
// holds routes and endpoints only; it is written with 0600 and never with
// certificate material.
type Edge struct {
	httpAddr, httpsAddr string
	log                 *slog.Logger
	proxy               *edge.Proxy
	certs               *edge.CertStore
	challenges          *edge.ChallengeStore
	cacheDir            string // "" disables the route cache

	mu      sync.Mutex
	servers []*http.Server
	// lns are the bound listeners. They are closed directly as well as through
	// the servers: http.Server.Close does not close a listener whose Serve
	// goroutine has not started yet, so a dial right after a stop could succeed.
	lns     []net.Listener
	bound   map[string]string // "http" and "https" -> bound address while running
	applied int64             // revision of the last snapshot applied
	self    map[string]*tls.Certificate
	warned  map[string]bool
}

// UseCache keeps a copy of the routes and endpoints in dir, so a restarted
// edge can serve HTTP routes while the server is unreachable. An empty dir
// (the default) disables the cache. Call it before LoadCache and Apply.
func (e *Edge) UseCache(dir string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cacheDir = dir
}

// AppliedRevision returns the revision of the last snapshot applied.
func (e *Edge) AppliedRevision() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.applied
}

// LoadCache applies the cached snapshot, if there is one. Its certificates
// are not cached, so tls: secret routes answer without a certificate until
// the server sends one, and tls: auto hosts use the self-signed fallback.
// A missing cache is not an error.
func (e *Edge) LoadCache() error {
	if e.cacheDir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(e.cacheDir, cacheFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read route cache: %w", err)
	}
	snap := &v1alpha1.RouteSnapshot{}
	if err := proto.Unmarshal(b, snap); err != nil {
		return fmt.Errorf("route cache is not valid: %w", err)
	}
	if err := e.Apply(snap); err != nil {
		return fmt.Errorf("apply route cache: %w", err)
	}
	e.log.Info("edge routes restored from cache", "revision", snap.GetRevision(), "routes", len(snap.GetRoutes()))
	return nil
}

// writeCache stores the routes and endpoints of an edge snapshot. Keys and
// certificates are removed from the copy first, and challenges are left out
// because they expire.
func (e *Edge) writeCache(snap *v1alpha1.RouteSnapshot) {
	if e.cacheDir == "" {
		return
	}
	c := proto.Clone(snap).(*v1alpha1.RouteSnapshot)
	c.Certificates, c.Challenges = nil, nil
	for _, r := range c.GetRoutes() {
		r.CertPem, r.KeyPem = nil, nil
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	if err == nil {
		err = security.WriteFileAtomic(filepath.Join(e.cacheDir, cacheFile), b, 0o600)
	}
	if err != nil {
		e.log.Warn("cannot write the route cache", "err", err)
	}
}

// removeCache deletes the cache when the node stops being an edge.
func (e *Edge) removeCache() {
	if e.cacheDir == "" {
		return
	}
	if err := os.Remove(filepath.Join(e.cacheDir, cacheFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.log.Warn("cannot remove the route cache", "err", err)
	}
}

// NewEdge returns an Edge that listens on httpAddr and httpsAddr once a
// snapshot makes the node an edge. An empty address disables that listener.
func NewEdge(httpAddr, httpsAddr string, log *slog.Logger) *Edge {
	return &Edge{
		httpAddr: httpAddr, httpsAddr: httpsAddr, log: log,
		proxy:      edge.NewProxy(edge.Config{AccessLog: edge.SlogAccess(log)}),
		certs:      &edge.CertStore{},
		challenges: &edge.ChallengeStore{},
		bound:      map[string]string{}, self: map[string]*tls.Certificate{}, warned: map[string]bool{},
	}
}

// Apply installs a route snapshot. A snapshot older than the one already
// applied is ignored. A bad snapshot is rejected and the previous routes stay.
func (e *Edge) Apply(snap *v1alpha1.RouteSnapshot) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if snap.GetRevision() < e.applied {
		return nil
	}
	if !snap.GetEdge() {
		e.stopLocked()
		e.removeCache()
		e.applied = snap.GetRevision()
		return nil
	}

	targets := make([]edge.Target, 0, len(snap.GetRoutes()))
	certs := map[string]*tls.Certificate{}
	for _, r := range snap.GetRoutes() {
		redirect := r.GetRedirectHttp()
		targets = append(targets, edge.Target{
			Route: &v1alpha1.Route{
				Metadata: &v1alpha1.ObjectMeta{Name: r.GetName()},
				Spec: &v1alpha1.RouteSpec{
					Host: r.GetHost(), PathPrefix: r.GetPathPrefix(), Tls: r.GetTls(), RedirectHttp: &redirect,
				},
			},
			Endpoints: r.GetEndpoints(),
		})
		if r.GetTls() == "secret" {
			host := strings.ToLower(r.GetHost())
			if _, dup := certs[host]; dup {
				continue
			}
			if len(r.GetCertPem()) == 0 {
				continue // no certificate yet (for example after a cache restore)
			}
			c, err := tls.X509KeyPair(r.GetCertPem(), r.GetKeyPem())
			if err != nil {
				e.log.Warn("route certificate is not usable; the host gets no certificate", "route", r.GetName(), "host", host, "err", err)
				continue
			}
			certs[host] = &c
		}
	}
	// Issued certificates (ADR 0015, pass C) cover tls auto hosts. A Secret
	// certificate for the same host was taken above and wins.
	for _, ic := range snap.GetCertificates() {
		host := strings.ToLower(ic.GetHost())
		if _, ok := certs[host]; ok {
			continue
		}
		c, err := tls.X509KeyPair(ic.GetChainPem(), ic.GetKeyPem())
		if err != nil {
			e.log.Warn("issued certificate is not usable; the host keeps its fallback", "host", host, "err", err)
			continue
		}
		certs[host] = &c
	}
	// tls: auto without an issued certificate gets a self-signed one until
	// issuance succeeds.
	// It never replaces a certificate from a Secret on the same host.
	for _, r := range snap.GetRoutes() {
		host := strings.ToLower(r.GetHost())
		if r.GetTls() != "auto" {
			continue
		}
		if _, ok := certs[host]; ok {
			continue
		}
		c, err := e.selfSignedLocked(host)
		if err != nil {
			e.log.Warn("cannot make a self-signed certificate", "host", host, "err", err)
			continue
		}
		certs[host] = c
	}

	table, err := edge.NewTable(targets)
	if err != nil {
		return err
	}
	if err := e.startLocked(); err != nil {
		return err
	}
	tokens := make(map[string]string, len(snap.GetChallenges()))
	for _, ch := range snap.GetChallenges() {
		tokens[ch.GetToken()] = ch.GetKeyAuthorization()
	}
	e.challenges.Replace(tokens)
	e.certs.Replace(certs)
	e.proxy.SetTable(table)
	e.applied = snap.GetRevision()
	e.writeCache(snap)
	return nil
}

// selfSignedLocked returns the in-memory self-signed certificate for host,
// creating it on first use, and warns once per host.
func (e *Edge) selfSignedLocked(host string) (*tls.Certificate, error) {
	if c, ok := e.self[host]; ok {
		return c, nil
	}
	c, err := edge.SelfSigned(host)
	if err != nil {
		return nil, err
	}
	e.self[host] = c
	if !e.warned[host] {
		e.warned[host] = true
		e.log.Warn("tls auto: no issued certificate yet; serving a self-signed certificate until one is issued", "host", host)
	}
	return c, nil
}

// startLocked binds the configured listeners unless they are already running.
func (e *Edge) startLocked() error {
	if len(e.servers) > 0 {
		return nil
	}
	var started []*http.Server
	fail := func(err error) error {
		for _, s := range started {
			_ = s.Close()
		}
		for _, ln := range e.lns {
			_ = ln.Close()
		}
		e.lns, e.servers = nil, nil
		clear(e.bound)
		return err
	}
	if e.httpAddr != "" {
		ln, err := net.Listen("tcp", e.httpAddr)
		if err != nil {
			return fail(err)
		}
		srv := e.proxy.NewServer(e.httpAddr, edge.HTTPHandler(e.proxy, e.challenges))
		started = append(started, srv)
		e.lns = append(e.lns, ln)
		e.bound["http"] = ln.Addr().String()
		go e.serve(srv, ln, "http")
	}
	if e.httpsAddr != "" {
		ln, err := net.Listen("tcp", e.httpsAddr)
		if err != nil {
			return fail(err)
		}
		srv := e.proxy.NewServer(e.httpsAddr, edge.HTTPSHandler(e.proxy))
		started = append(started, srv)
		e.lns = append(e.lns, ln)
		e.bound["https"] = ln.Addr().String()
		go e.serve(srv, tls.NewListener(ln, e.certs.TLSConfig()), "https")
	}
	e.servers = started
	if len(started) > 0 {
		e.log.Info("edge listening", "http", e.bound["http"], "https", e.bound["https"])
	}
	return nil
}

func (e *Edge) serve(srv *http.Server, ln net.Listener, kind string) {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		e.log.Error("edge listener stopped", "kind", kind, "err", err)
	}
}

// stopLocked closes the listeners and drops the routes and certificates
// from memory.
func (e *Edge) stopLocked() {
	for _, s := range e.servers {
		_ = s.Close()
	}
	for _, ln := range e.lns {
		_ = ln.Close()
	}
	e.lns = nil
	if len(e.servers) > 0 {
		e.log.Info("edge stopped: this node is no longer an edge")
	}
	e.servers = nil
	clear(e.bound)
	e.certs.Replace(nil)
	e.challenges.Replace(nil)
	if t, err := edge.NewTable(nil); err == nil {
		e.proxy.SetTable(t)
	}
}

// Close stops the listeners.
func (e *Edge) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopLocked()
	return nil
}

// HTTPAddr returns the bound address of the HTTP listener, or "" when it is not running.
func (e *Edge) HTTPAddr() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bound["http"]
}

// HTTPSAddr returns the bound address of the HTTPS listener, or "" when it is not running.
func (e *Edge) HTTPSAddr() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bound["https"]
}
