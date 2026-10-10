package edge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for Config, from ADR 0015.
const (
	DefaultDialTimeout           = 5 * time.Second
	DefaultResponseHeaderTimeout = 60 * time.Second
	DefaultIdleTimeout           = 120 * time.Second
	DefaultCooldown              = 10 * time.Second
	readHeaderTimeout            = 10 * time.Second
)

// Config sets timeouts and hooks. Zero fields take the defaults above.
type Config struct {
	DialTimeout           time.Duration
	ResponseHeaderTimeout time.Duration
	IdleTimeout           time.Duration // upstream idle connections and the inbound server
	Cooldown              time.Duration // how long a dial failure keeps an endpoint out

	// AccessLog, when set, is called once per request after the response ends.
	AccessLog func(AccessEntry)
	// Now is the clock for cooldowns and durations. Nil means time.Now.
	Now func() time.Time
}

func (c Config) withDefaults() Config {
	if c.DialTimeout <= 0 {
		c.DialTimeout = DefaultDialTimeout
	}
	if c.ResponseHeaderTimeout <= 0 {
		c.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.Cooldown <= 0 {
		c.Cooldown = DefaultCooldown
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// AccessEntry is one access-log record. The query string is never included,
// because it often carries tokens.
type AccessEntry struct {
	Time     time.Time
	Method   string
	Host     string
	Path     string
	Route    string // empty when no route matched
	Status   int
	Bytes    int64
	Duration time.Duration
	Upstream string // host:port that answered, empty when none was dialed
}

// SlogAccess returns an AccessLog hook that writes one structured line per request.
func SlogAccess(l *slog.Logger) func(AccessEntry) {
	return func(e AccessEntry) {
		l.Info("edge request",
			"method", e.Method,
			"host", e.Host,
			"path", e.Path,
			"route", e.Route,
			"status", e.Status,
			"bytes", e.Bytes,
			"duration", e.Duration,
			"upstream", e.Upstream,
		)
	}
}

// Proxy is the edge HTTP handler. Its routing table is swapped atomically with
// SetTable, so updates never block requests.
type Proxy struct {
	cfg   Config
	table atomic.Pointer[Table]
	rp    *httputil.ReverseProxy

	mu  sync.Mutex
	bad map[string]time.Time // endpoint -> time its cooldown ends
}

type stateKey struct{}

// reqState travels in the request context so the transport can report back
// which endpoint answered.
type reqState struct {
	entry    *Entry
	upstream string
}

var errNoEndpoints = errors.New("edge: no usable endpoint")

// NewProxy returns a Proxy with no routes. Call SetTable before serving.
func NewProxy(cfg Config) *Proxy {
	cfg = cfg.withDefaults()
	p := &Proxy{cfg: cfg, bad: make(map[string]time.Time)}
	p.rp = &httputil.ReverseProxy{
		Rewrite: p.rewrite,
		Transport: &upstreamTransport{
			p: p,
			tr: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: cfg.DialTimeout}).DialContext,
				ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
				IdleConnTimeout:       cfg.IdleTimeout,
				MaxIdleConnsPerHost:   32,
			},
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "502 Bad Gateway\n")
		},
	}
	return p
}

// SetTable installs a new routing table. A nil table answers every request with 404.
func (p *Proxy) SetTable(t *Table) {
	p.table.Store(t)
}

// NewServer returns an http.Server for the edge with the inbound timeouts from
// ADR 0015. WriteTimeout stays zero so streams and websockets keep working.
func (p *Proxy) NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       p.cfg.IdleTimeout,
	}
}

// ServeHTTP routes and proxies one request. Unknown hosts and unmatched paths
// get the stock 404 body, which says nothing about the routes.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := p.cfg.Now()
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	st := &reqState{}
	defer func() {
		if p.cfg.AccessLog == nil {
			return
		}
		e := AccessEntry{
			Time:     start,
			Method:   r.Method,
			Host:     r.Host,
			Path:     r.URL.Path,
			Status:   rec.status,
			Bytes:    rec.bytes,
			Duration: p.cfg.Now().Sub(start),
			Upstream: st.upstream,
		}
		if st.entry != nil {
			e.Route = st.entry.Name
		}
		p.cfg.AccessLog(e)
	}()

	e := p.table.Load().Lookup(r.Host, r.URL.Path)
	if e == nil {
		http.NotFound(rec, r)
		return
	}
	st.entry = e
	p.rp.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), stateKey{}, st)))
}

// rewrite sets the forwarding headers. Any X-Forwarded-* value from the client
// is dropped first, so the upstream only sees what the edge observed.
func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	h := pr.Out.Header
	h.Del("X-Forwarded-For")
	h.Del("X-Forwarded-Host")
	h.Del("X-Forwarded-Proto")

	ip, _, err := net.SplitHostPort(pr.In.RemoteAddr)
	if err != nil {
		ip = pr.In.RemoteAddr
	}
	h.Set("X-Forwarded-For", ip)
	h.Set("X-Forwarded-Host", pr.In.Host)
	proto := "http"
	if pr.In.TLS != nil {
		proto = "https"
	}
	h.Set("X-Forwarded-Proto", proto)
}

// pick returns the next endpoint in round robin order that is not in cooldown
// and not already tried. If every untried endpoint is cooling, it returns one
// of them anyway: a stale cooldown should not refuse traffic outright.
func (p *Proxy) pick(e *Entry, tried map[string]bool) (string, bool) {
	n := len(e.endpoints)
	if n == 0 {
		return "", false
	}
	start := e.next.Add(1) - 1
	now := p.cfg.Now()

	p.mu.Lock()
	defer p.mu.Unlock()
	var fallback string
	haveFallback := false
	for i := 0; i < n; i++ {
		ep := e.endpoints[int((start+uint64(i))%uint64(n))]
		if tried[ep] {
			continue
		}
		if until, ok := p.bad[ep]; ok && now.Before(until) {
			if !haveFallback {
				fallback, haveFallback = ep, true
			}
			continue
		}
		return ep, true
	}
	return fallback, haveFallback
}

// markBad starts the passive cooldown for an endpoint after a dial failure.
func (p *Proxy) markBad(ep string) {
	p.mu.Lock()
	p.bad[ep] = p.cfg.Now().Add(p.cfg.Cooldown)
	p.mu.Unlock()
}

// upstreamTransport picks an endpoint, dials it, and on a dial failure retries
// once on another endpoint, but only for idempotent methods with no body.
type upstreamTransport struct {
	p  *Proxy
	tr *http.Transport
}

func (u *upstreamTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	st, _ := r.Context().Value(stateKey{}).(*reqState)
	if st == nil || st.entry == nil {
		return nil, errNoEndpoints
	}
	attempts := 1
	if idempotent(r.Method) && (r.Body == nil || r.Body == http.NoBody) {
		attempts = 2
	}
	tried := make(map[string]bool, attempts)
	var lastErr error = errNoEndpoints
	for i := 0; i < attempts; i++ {
		ep, ok := u.p.pick(st.entry, tried)
		if !ok {
			break
		}
		tried[ep] = true
		st.upstream = ep

		out := r.Clone(r.Context())
		out.RequestURI = ""
		out.URL.Scheme = "http"
		out.URL.Host = ep
		resp, err := u.tr.RoundTrip(out)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if r.Context().Err() != nil || !isDialError(err) {
			break
		}
		u.p.markBad(ep)
	}
	return nil, lastErr
}

func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace,
		http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// isDialError reports whether err came from connecting, before any bytes were
// sent, so retrying is safe.
func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// recorder keeps the status and byte count for the access log. It forwards
// Flush and exposes the underlying writer through Unwrap, so websocket
// hijacking via http.ResponseController keeps working.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
