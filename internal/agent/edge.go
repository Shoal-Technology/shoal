package agent

import (
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/edge"
)

// RouteSink receives the route snapshots the server sends to edge nodes.
type RouteSink interface {
	Apply(*v1alpha1.RouteSnapshot) error
}

// Edge runs the ingress listeners on a node the server marks as an edge (ADR
// 0015, pass B2). It stays idle until a snapshot says the node is an edge, and
// it closes its listeners when a later snapshot says it is not.
//
// Certificates and private keys live only in memory. Nothing is written to
// disk, as ADR 0015 decides.
type Edge struct {
	httpAddr, httpsAddr string
	log                 *slog.Logger
	proxy               *edge.Proxy
	certs               *edge.CertStore

	mu      sync.Mutex
	servers []*http.Server
	bound   map[string]string // "http" and "https" -> bound address while running
	applied int64             // revision of the last snapshot applied
	self    map[string]*tls.Certificate
	warned  map[string]bool
}

// NewEdge returns an Edge that listens on httpAddr and httpsAddr once a
// snapshot makes the node an edge. An empty address disables that listener.
func NewEdge(httpAddr, httpsAddr string, log *slog.Logger) *Edge {
	return &Edge{
		httpAddr: httpAddr, httpsAddr: httpsAddr, log: log,
		proxy: edge.NewProxy(edge.Config{AccessLog: edge.SlogAccess(log)}),
		certs: &edge.CertStore{},
		bound: map[string]string{}, self: map[string]*tls.Certificate{}, warned: map[string]bool{},
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
			c, err := tls.X509KeyPair(r.GetCertPem(), r.GetKeyPem())
			if err != nil {
				e.log.Warn("route certificate is not usable; the host gets no certificate", "route", r.GetName(), "host", host, "err", err)
				continue
			}
			certs[host] = &c
		}
	}
	// tls: auto gets a self-signed certificate until ACME exists (pass C).
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
	e.certs.Replace(certs)
	e.proxy.SetTable(table)
	e.applied = snap.GetRevision()
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
		e.log.Warn("tls auto: serving a self-signed certificate, not a trusted one; automatic certificates are not in this build yet (ADR 0015 pass C)", "host", host)
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
		e.servers = nil
		clear(e.bound)
		return err
	}
	if e.httpAddr != "" {
		ln, err := net.Listen("tcp", e.httpAddr)
		if err != nil {
			return fail(err)
		}
		srv := e.proxy.NewServer(e.httpAddr, edge.HTTPHandler(e.proxy, nil))
		started = append(started, srv)
		e.bound["http"] = ln.Addr().String()
		go e.serve(srv, ln, "http")
	}
	if e.httpsAddr != "" {
		ln, err := net.Listen("tcp", e.httpsAddr)
		if err != nil {
			return fail(err)
		}
		srv := e.proxy.NewServer(e.httpsAddr, e.proxy)
		started = append(started, srv)
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
	if len(e.servers) > 0 {
		e.log.Info("edge stopped: this node is no longer an edge")
	}
	e.servers = nil
	clear(e.bound)
	e.certs.Replace(nil)
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
