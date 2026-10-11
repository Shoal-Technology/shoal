package resource

import (
	"strings"
	"testing"

	"github.com/kaiserern/shoal/api/v1alpha1"
)

// routeSpec is a valid Route spec; each case changes one field.
func routeSpec() *v1alpha1.RouteSpec {
	return &v1alpha1.RouteSpec{Host: "app.example.com", Service: "web", Port: 80}
}

func TestValidateRoute(t *testing.T) {
	tests := []struct {
		name    string
		route   string // metadata.name; empty means "web"
		mutate  func(*v1alpha1.RouteSpec)
		wantErr string // empty = valid
	}{
		{name: "valid defaults", mutate: func(*v1alpha1.RouteSpec) {}},
		{name: "valid tls none", mutate: func(s *v1alpha1.RouteSpec) { s.Tls = "none" }},
		{name: "valid tls secret", mutate: func(s *v1alpha1.RouteSpec) {
			s.Tls, s.TlsSecret = "secret", "web-cert"
		}},
		{name: "valid subdomain and path", mutate: func(s *v1alpha1.RouteSpec) {
			s.Host, s.PathPrefix = "a-b.c.example.org", "/api/v1/"
		}},
		{name: "valid long host", mutate: func(s *v1alpha1.RouteSpec) {
			s.Host = strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + ".example"
		}},
		{name: "bad name", route: "Web", wantErr: "metadata.name", mutate: func(*v1alpha1.RouteSpec) {}},
		{name: "empty host", wantErr: "spec.host: required", mutate: func(s *v1alpha1.RouteSpec) { s.Host = "" }},
		{name: "uppercase host", wantErr: "spec.host: must be lowercase", mutate: func(s *v1alpha1.RouteSpec) { s.Host = "App.example.com" }},
		{name: "wildcard host", wantErr: "wildcard hosts are not supported in v1", mutate: func(s *v1alpha1.RouteSpec) { s.Host = "*.example.com" }},
		{name: "ipv4 host", wantErr: "not an IP address", mutate: func(s *v1alpha1.RouteSpec) { s.Host = "192.0.2.10" }},
		{name: "ipv6 host", wantErr: "not an IP address", mutate: func(s *v1alpha1.RouteSpec) { s.Host = "2001:db8::1" }},
		{name: "single label", wantErr: "at least two labels", mutate: func(s *v1alpha1.RouteSpec) { s.Host = "localhost" }},
		{name: "bad label", wantErr: "invalid label \"-app\"", mutate: func(s *v1alpha1.RouteSpec) { s.Host = "-app.example.com" }},
		{name: "underscore in label", wantErr: "invalid label", mutate: func(s *v1alpha1.RouteSpec) { s.Host = "my_app.example.com" }},
		{name: "empty label", wantErr: "invalid label", mutate: func(s *v1alpha1.RouteSpec) { s.Host = "app..example.com" }},
		{name: "trailing dot", wantErr: "must not end with a dot", mutate: func(s *v1alpha1.RouteSpec) { s.Host = "app.example.com." }},
		{name: "host too long", wantErr: "at most 253", mutate: func(s *v1alpha1.RouteSpec) {
			s.Host = strings.Repeat("a"+strings.Repeat("b", 60)+".", 5) + "com"
		}},
		{name: "all-digit last label", wantErr: "all digits", mutate: func(s *v1alpha1.RouteSpec) { s.Host = "app.123" }},
		{name: "path without slash", wantErr: "spec.pathPrefix: must start with /", mutate: func(s *v1alpha1.RouteSpec) { s.PathPrefix = "api" }},
		{name: "path with query", wantErr: "must not contain ? or #", mutate: func(s *v1alpha1.RouteSpec) { s.PathPrefix = "/api?x=1" }},
		{name: "path with fragment", wantErr: "must not contain ? or #", mutate: func(s *v1alpha1.RouteSpec) { s.PathPrefix = "/api#top" }},
		{name: "path too long", wantErr: "spec.pathPrefix: at most 256", mutate: func(s *v1alpha1.RouteSpec) { s.PathPrefix = "/" + strings.Repeat("a", 256) }},
		{name: "missing service", wantErr: "spec.service", mutate: func(s *v1alpha1.RouteSpec) { s.Service = "" }},
		{name: "bad service name", wantErr: "spec.service", mutate: func(s *v1alpha1.RouteSpec) { s.Service = "Web" }},
		{name: "port zero", wantErr: "spec.port: must be between 1 and 65535", mutate: func(s *v1alpha1.RouteSpec) { s.Port = 0 }},
		{name: "port too high", wantErr: "spec.port", mutate: func(s *v1alpha1.RouteSpec) { s.Port = 70000 }},
		{name: "unknown tls mode", wantErr: "spec.tls: must be auto, none or secret", mutate: func(s *v1alpha1.RouteSpec) { s.Tls = "bogus" }},
		{name: "secret without tlsSecret", wantErr: "spec.tlsSecret: required", mutate: func(s *v1alpha1.RouteSpec) { s.Tls = "secret" }},
		{name: "secret with bad tlsSecret", wantErr: "spec.tlsSecret: required", mutate: func(s *v1alpha1.RouteSpec) {
			s.Tls, s.TlsSecret = "secret", "Web_Cert"
		}},
		{name: "tlsSecret with auto", wantErr: "only allowed with tls: secret", mutate: func(s *v1alpha1.RouteSpec) { s.TlsSecret = "web-cert" }},
		{name: "tlsSecret with none", wantErr: "only allowed with tls: secret", mutate: func(s *v1alpha1.RouteSpec) {
			s.Tls, s.TlsSecret = "none", "web-cert"
		}},
		{name: "redirect with tls none", wantErr: "spec.redirectHttp: not allowed with tls: none", mutate: func(s *v1alpha1.RouteSpec) {
			s.Tls, s.RedirectHttp = "none", boolPtr(true)
		}},
		{name: "no redirect with tls none", mutate: func(s *v1alpha1.RouteSpec) {
			s.Tls, s.RedirectHttp = "none", boolPtr(false)
		}},
		{name: "redirect with tls secret", mutate: func(s *v1alpha1.RouteSpec) {
			s.Tls, s.TlsSecret, s.RedirectHttp = "secret", "web-cert", boolPtr(true)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := tt.route
			if name == "" {
				name = "web"
			}
			s := routeSpec()
			tt.mutate(s)
			DefaultRoute(s)
			err := ValidateRoute(name, s)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestDefaultRoute(t *testing.T) {
	s := routeSpec()
	DefaultRoute(s)
	if s.PathPrefix != "/" || s.GetRedirectHttp() != true || s.Tls != "auto" {
		t.Fatalf("auto defaults = %v", s)
	}
	s = routeSpec()
	s.Tls = "none"
	DefaultRoute(s)
	if s.GetRedirectHttp() {
		t.Fatalf("tls none should default redirectHttp to false: %v", s)
	}
}

func TestRouteKey(t *testing.T) {
	a := routeSpec()
	DefaultRoute(a)
	b := routeSpec()
	b.PathPrefix = "/api"
	DefaultRoute(b)
	if RouteKey(a) == RouteKey(b) {
		t.Fatalf("different prefixes share a key: %q", RouteKey(a))
	}
	c := routeSpec()
	c.PathPrefix = "/"
	DefaultRoute(c)
	if RouteKey(a) != RouteKey(c) {
		t.Fatalf("defaulted and explicit / differ: %q vs %q", RouteKey(a), RouteKey(c))
	}
}

func TestDecodeRoute(t *testing.T) {
	objs, err := Decode([]byte(`
apiVersion: v1alpha1
kind: Route
metadata:
  name: web
spec:
  host: app.example.com
  service: web
  port: 80
  tls: secret
  tlsSecret: web-cert
`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(objs) != 1 || objs[0].GetRoute() == nil {
		t.Fatalf("decoded %v", objs)
	}
	r := objs[0].GetRoute().GetSpec()
	if r.GetTls() != "secret" || r.GetTlsSecret() != "web-cert" || r.GetHost() != "app.example.com" {
		t.Fatalf("spec = %v", r)
	}
	if _, _, _, err := Prepare(objs[0]); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
}

func TestPrepareRouteRejectsBadTls(t *testing.T) {
	objs, err := Decode([]byte(`
kind: Route
metadata:
  name: web
spec:
  host: app.example.com
  service: web
  port: 80
  tls: https
`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if _, _, _, err := Prepare(objs[0]); err == nil || !strings.Contains(err.Error(), "spec.tls") {
		t.Fatalf("Prepare err = %v, want spec.tls error for tls: https", err)
	}
}
