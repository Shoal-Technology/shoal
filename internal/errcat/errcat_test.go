package errcat

import (
	"fmt"
	"strings"
	"testing"
)

func TestCatalogueHasE01ToE15(t *testing.T) {
	all := All()
	if len(all) != 15 {
		t.Fatalf("%d entries, want 15", len(all))
	}
	for i, e := range all {
		if want := fmt.Sprintf("E%02d", i+1); e.Code != want {
			t.Errorf("entry %d code %q, want %q", i, e.Code, want)
		}
	}
}

func TestEveryEntryHasTitleTemplateAndHint(t *testing.T) {
	for _, e := range All() {
		if e.Title == "" {
			t.Errorf("%s has no title", e.Code)
		}
		if e.Template == "" {
			t.Errorf("%s has no message template", e.Code)
		}
		if strings.TrimSpace(e.Hint) == "" {
			t.Errorf("%s has no hint", e.Code)
		}
		if !strings.HasPrefix(e.Line(), e.Code+": "+e.Title+" — ") {
			t.Errorf("%s Line() = %q", e.Code, e.Line())
		}
	}
}

func TestGetAcceptsShortCodes(t *testing.T) {
	for _, c := range []string{"E07", "e07", "E7", " e7 "} {
		e, ok := Get(c)
		if !ok || e.Code != "E07" {
			t.Errorf("Get(%q) = %v, %v; want E07", c, e, ok)
		}
	}
	if _, ok := Get("E16"); ok {
		t.Error("Get(E16) found an entry")
	}
}

// TestMatchRealReasons uses the reason strings the server, the controller
// and the agent emit today.
func TestMatchRealReasons(t *testing.T) {
	cases := []struct {
		name, kind, state, message, want string
	}{
		{"acme terms", "Route", "CertError", "ACME terms not accepted; start the server with --acme-accept-tos", "E06"},
		{"http-01", "Route", "CertError", "certificate for blog.example.com: HTTP-01 challenge failed: connection refused", "E07"},
		{"rate limit", "Route", "CertError", "rate limited until 2026-10-12", "E07"},
		{"no endpoints", "Route", "NoEndpoints", `Service "blog" has no ready endpoints`, "E15"},
		{"conflict", "Route", "Conflict", "route blog.example.com is already used by app blog-old", "E13"},
		{"secret invalid", "Route", "SecretInvalid", `Secret "blog-tls": the Secret must hold tls.crt and tls.key`, "E12"},
		{"secret missing", "Route", "SecretNotFound", `Secret "blog-tls" not found`, "E12"},
		{"database failed", "Database", "Failed", "container exited 1: initdb: could not change permissions of directory", "E11"},
		{"volume node lost", "Volume", "NodeLost", "node edge-1 is not ready; its data stays on that node", "E10"},
		{"unschedulable", "Replica", "Pending", "unschedulable: no ready node has memory free", "E09"},
		{"pull unauthorized", "Replica", "", "pull ghcr.io/example/blog:1.4.2: unauthorized", "E01"},
		{"pull manifest unknown", "Replica", "", "pull ghcr.io/example/blog:latest: manifest unknown", "E02"},
		{"insufficient disk", "Volume", "", "need 5Gi for volume blog-uploads; 2.1Gi free under /var/lib/shoal", "E08"},
		{"crash loop", "Replica", "", "replica blog-1 crashed 4 times; last exit 1: panic: missing DATABASE_URL", "E14"},
		{"health check", "Replica", "", "replica blog-1 not ready: GET /healthz on :3000 returned 404", "E15"},
		{"healthy route", "Route", "Ready", "", ""},
		{"service not found is transient", "Route", "ServiceNotFound", `Service "blog" not found`, ""},
		{"port not found is transient", "Route", "PortNotFound", `Service "blog" has no port 80`, ""},
		{"pending database", "Database", "Provisioning", "waiting for the database to start", ""},
		{"degraded database", "Database", "Degraded", "no replica is ready", ""},
		{"volume bound", "Volume", "Bound", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, ok := Match(c.kind, c.state, c.message)
			switch {
			case c.want == "" && ok:
				t.Fatalf("Match matched %s, want none", e.Code)
			case c.want != "" && !ok:
				t.Fatalf("Match found nothing, want %s", c.want)
			case ok && e.Code != c.want:
				t.Fatalf("Match = %s, want %s", e.Code, c.want)
			}
		})
	}
}

func TestTerminalSet(t *testing.T) {
	terminal := map[string]bool{"E06": true, "E11": true, "E12": true, "E13": true}
	for _, e := range All() {
		if got := Terminal(e); got != terminal[e.Code] {
			t.Errorf("Terminal(%s) = %v, want %v", e.Code, got, terminal[e.Code])
		}
	}
}
