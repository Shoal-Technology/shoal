// Package edge is the HTTP and TLS core of the ingress edge (ADR 0015, pass B1).
//
// It matches requests to Routes, proxies them to ready endpoints, and serves
// certificates chosen by SNI. It has no agent wiring and no ACME: the caller
// supplies the Routes, their endpoints and the certificates.
package edge

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync/atomic"

	"example.com/shoal/api/v1alpha1"
)

// Target is one Route with the Ready endpoints (host:port) that serve it.
type Target struct {
	Route     *v1alpha1.Route
	Endpoints []string
}

// Entry is a Route as the edge matches it. Its fields and endpoint list do not
// change after the Table is built; only the round robin counter moves.
type Entry struct {
	Name         string
	Host         string
	PathPrefix   string
	TLS          string
	RedirectHTTP bool

	endpoints []string
	next      atomic.Uint64
}

// Endpoints returns a copy of the endpoints the Entry was built with.
func (e *Entry) Endpoints() []string {
	return append([]string(nil), e.endpoints...)
}

// Table is an immutable set of routes. To change routes, build a new Table and
// hand it to Proxy.SetTable.
type Table struct {
	byHost map[string][]*Entry // per host, longest path prefix first
}

// NewTable builds a Table from Targets. Two Routes that claim the same host and
// path prefix are an error, as ADR 0015 requires. Host matching is
// case-insensitive, so the comparison is made on lowercased hosts.
func NewTable(targets []Target) (*Table, error) {
	t := &Table{byHost: make(map[string][]*Entry)}
	owner := make(map[string]string)
	var errs []error
	for _, tg := range targets {
		name := tg.Route.GetMetadata().GetName()
		s := tg.Route.GetSpec()
		if s == nil {
			errs = append(errs, fmt.Errorf("route %q has no spec", name))
			continue
		}
		host := strings.ToLower(s.GetHost())
		prefix := s.GetPathPrefix()
		if prefix == "" {
			prefix = "/"
		}
		if host == "" || !strings.HasPrefix(prefix, "/") {
			errs = append(errs, fmt.Errorf("route %q: needs a host and a path prefix starting with /", name))
			continue
		}
		key := host + " " + prefix
		if prev, dup := owner[key]; dup {
			errs = append(errs, fmt.Errorf("routes %q and %q both claim %s %s", prev, name, host, prefix))
			continue
		}
		owner[key] = name

		tlsMode := s.GetTls()
		if tlsMode == "" {
			tlsMode = "auto"
		}
		redirect := tlsMode != "none"
		if s.RedirectHttp != nil {
			redirect = *s.RedirectHttp
		}
		t.byHost[host] = append(t.byHost[host], &Entry{
			Name:         name,
			Host:         host,
			PathPrefix:   prefix,
			TLS:          tlsMode,
			RedirectHTTP: redirect,
			endpoints:    append([]string(nil), tg.Endpoints...),
		})
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	for _, es := range t.byHost {
		sort.SliceStable(es, func(i, j int) bool {
			return len(es[i].PathPrefix) > len(es[j].PathPrefix)
		})
	}
	return t, nil
}

// Lookup returns the Route for a request host and path, or nil. The host is
// case-folded and its port and trailing dot are dropped. The path matches a
// prefix only on whole segments, and the longest matching prefix wins.
// A nil Table matches nothing.
func (t *Table) Lookup(host, path string) *Entry {
	if t == nil {
		return nil
	}
	for _, e := range t.byHost[normalizeHost(host)] {
		if prefixMatch(e.PathPrefix, path) {
			return e
		}
	}
	return nil
}

// prefixMatch reports whether path falls under prefix on segment boundaries:
// "/api" matches "/api" and "/api/x" but not "/apix".
func prefixMatch(prefix, path string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return len(path) == len(prefix) || strings.HasSuffix(prefix, "/") || path[len(prefix)] == '/'
}

// normalizeHost lowercases a host, strips any port and any trailing dot.
func normalizeHost(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}
