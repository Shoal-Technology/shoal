package edge

import (
	"testing"

	"example.com/shoal/api/v1alpha1"
)

func mkRoute(name, host, prefix, tlsMode string, eps ...string) Target {
	return Target{
		Route: &v1alpha1.Route{
			Metadata: &v1alpha1.ObjectMeta{Name: name},
			Spec: &v1alpha1.RouteSpec{
				Host:       host,
				PathPrefix: prefix,
				Service:    "svc",
				Port:       80,
				Tls:        tlsMode,
			},
		},
		Endpoints: eps,
	}
}

func mustTable(t *testing.T, ts ...Target) *Table {
	t.Helper()
	tab, err := NewTable(ts)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	return tab
}

func TestLookup(t *testing.T) {
	tab := mustTable(t,
		mkRoute("web", "app.example.test", "/", "auto"),
		mkRoute("api", "app.example.test", "/api", "auto"),
		mkRoute("v2", "app.example.test", "/api/v2", "auto"),
		mkRoute("docs", "docs.example.test", "/", "none"),
	)
	cases := []struct {
		name, host, path, want string
	}{
		{"root", "app.example.test", "/", "web"},
		{"case and port", "APP.Example.Test:8443", "/index", "web"},
		{"trailing dot", "app.example.test.", "/", "web"},
		{"exact prefix", "app.example.test", "/api", "api"},
		{"prefix with slash", "app.example.test", "/api/", "api"},
		{"whole segment", "app.example.test", "/api/users", "api"},
		{"not a segment", "app.example.test", "/apix", "web"},
		{"longest wins", "app.example.test", "/api/v2/items", "v2"},
		{"longest exact", "app.example.test", "/api/v2", "v2"},
		{"not a segment of longest", "app.example.test", "/api/v20", "api"},
		{"other host, root only", "docs.example.test", "/api", "docs"},
		{"unknown host", "other.example.test", "/", ""},
		{"empty host", "", "/", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ""
			if e := tab.Lookup(c.host, c.path); e != nil {
				got = e.Name
			}
			if got != c.want {
				t.Errorf("Lookup(%q, %q) = %q, want %q", c.host, c.path, got, c.want)
			}
		})
	}
}

func TestNilTableMatchesNothing(t *testing.T) {
	var tab *Table
	if e := tab.Lookup("app.example.test", "/"); e != nil {
		t.Fatalf("nil table matched %q", e.Name)
	}
}

func TestNewTableRejectsDuplicateHostAndPrefix(t *testing.T) {
	_, err := NewTable([]Target{
		mkRoute("a", "App.example.test", "/", "auto"),
		mkRoute("b", "app.example.test", "/", "auto"),
	})
	if err == nil {
		t.Fatal("duplicate host and prefix was accepted")
	}
}

func TestNewTableRejectsMissingSpec(t *testing.T) {
	_, err := NewTable([]Target{{Route: &v1alpha1.Route{Metadata: &v1alpha1.ObjectMeta{Name: "x"}}}})
	if err == nil {
		t.Fatal("route without spec was accepted")
	}
}
