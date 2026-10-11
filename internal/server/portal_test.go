package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kaiserern/shoal/api/v1alpha1"
)

type portalClient struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newPortal(t *testing.T) (*testServer, *portalClient) {
	t.Helper()
	ts := startServer(t, t.TempDir(), &clock{})
	srv := httptest.NewTLSServer(ts.PortalHandler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	c := srv.Client()
	c.Jar = jar
	c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // test server: its certificate is self-signed
	return ts, &portalClient{t: t, base: srv.URL, c: c}
}

func (p *portalClient) do(method, path, body string, csrf bool) (*http.Response, string) {
	p.t.Helper()
	req, err := http.NewRequest(method, p.base+path, strings.NewReader(body))
	if err != nil {
		p.t.Fatal(err)
	}
	if csrf {
		req.Header.Set(csrfHeader, "1")
	}
	resp, err := p.c.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (p *portalClient) want(code int, method, path, body string) string {
	p.t.Helper()
	resp, out := p.do(method, path, body, true)
	if resp.StatusCode != code {
		p.t.Fatalf("%s %s = %d %s, want %d", method, path, resp.StatusCode, out, code)
	}
	return out
}

func initialPassword(t *testing.T, ts *testServer) string {
	t.Helper()
	b, err := os.ReadFile(UIInitialPasswordPath(ts.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func login(p *portalClient, pw string) *http.Response {
	p.t.Helper()
	resp, _ := p.do("POST", "/api/login", `{"password":"`+pw+`"}`, false)
	return resp
}

func TestPortalAuth(t *testing.T) {
	ts, p := newPortal(t)
	pw := initialPassword(t, ts)
	if fi, _ := os.Stat(UIInitialPasswordPath(ts.dataDir)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("password file mode %v", fi.Mode().Perm())
	}

	p.want(http.StatusUnauthorized, "GET", "/api/objects/workloads", "")
	if out := p.want(http.StatusOK, "GET", "/api/me", ""); !strings.Contains(out, `"authenticated":false`) {
		t.Fatalf("me = %s", out)
	}
	if resp := login(p, "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}

	resp := login(p, pw)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %+v", cookie)
	}
	p.want(http.StatusOK, "GET", "/api/objects/workloads", "")

	// Changes need the CSRF header.
	if resp, _ := p.do("POST", "/api/apply", webYAML, false); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("apply without CSRF header: %d", resp.StatusCode)
	}

	// Security headers on everything, the UI itself is served.
	resp, body := p.do("GET", "/", "", false)
	if !strings.Contains(body, "app.js") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") ||
		resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("index: headers %v", resp.Header)
	}

	// Resetting the password logs everyone out and retires the file.
	newPW, err := ts.admin(t).ResetUIPassword(context.Background(), &v1alpha1.ResetUIPasswordRequest{})
	if err != nil {
		t.Fatal(err)
	}
	p.want(http.StatusUnauthorized, "GET", "/api/objects/workloads", "")
	if _, err := os.Stat(UIInitialPasswordPath(ts.dataDir)); !os.IsNotExist(err) {
		t.Fatal("initial password file not removed after reset")
	}
	if resp := login(p, pw); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("old password still accepted")
	}
	if resp := login(p, newPW.GetPassword()); resp.StatusCode != http.StatusNoContent {
		t.Fatal("new password rejected")
	}
	p.want(http.StatusNoContent, "POST", "/api/logout", "")
	p.want(http.StatusUnauthorized, "GET", "/api/objects/workloads", "")
}

func TestPortalLoginLockout(t *testing.T) {
	ts, p := newPortal(t)
	for range loginFailures {
		login(p, "nope")
	}
	if resp := login(p, initialPassword(t, ts)); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after %d failures even the right password must wait: %d", loginFailures, resp.StatusCode)
	}
}

func TestPortalAPI(t *testing.T) {
	ts, p := newPortal(t)
	login(p, initialPassword(t, ts))

	out := p.want(http.StatusOK, "POST", "/api/apply", webYAML)
	if !strings.Contains(out, `"action":"created"`) {
		t.Fatalf("apply = %s", out)
	}
	var lst struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(p.want(http.StatusOK, "GET", "/api/objects/workloads", "")), &lst); err != nil || len(lst.Items) != 1 {
		t.Fatalf("list = %v, %v", lst, err)
	}
	if y := p.want(http.StatusOK, "GET", "/api/objects/workloads/web/yaml", ""); !strings.Contains(y, "image: nginx:1.27") || strings.Contains(y, "status") {
		t.Fatalf("yaml = %s", y)
	}
	p.want(http.StatusBadRequest, "POST", "/api/apply", "kind: Workload\nmetadata: {name: Bad}\n")

	// Scale and restart.
	p.want(http.StatusNoContent, "POST", "/api/workloads/web/scale", `{"replicas": 5}`)
	p.want(http.StatusBadRequest, "POST", "/api/workloads/web/scale", `{"replicas": -1}`)
	p.want(http.StatusNoContent, "POST", "/api/workloads/web/restart", "")
	w := workloadStatus(t, ts.admin(t), "web")
	if w.GetSpec().GetReplicas() != 5 || w.GetSpec().GetRestartedAt() == nil {
		t.Fatalf("after scale and restart: %v", w.GetSpec())
	}

	// Secrets: set via the API, values never come back.
	p.want(http.StatusNoContent, "PATCH", "/api/data/secrets/db", `{"set": {"password": "hunter2", "user": "app"}}`)
	p.want(http.StatusNoContent, "PATCH", "/api/data/secrets/db", `{"remove": ["user"]}`)
	if s := p.want(http.StatusOK, "GET", "/api/objects/secrets/db", ""); strings.Contains(s, "hunter2") || !strings.Contains(s, `"password":""`) || strings.Contains(s, "user") {
		t.Fatalf("secret = %s", s)
	}
	p.want(http.StatusBadRequest, "GET", "/api/objects/secrets/db/yaml", "")
	p.want(http.StatusNoContent, "PATCH", "/api/data/configs/app", `{"set": {"mode": "prod"}}`)
	if c := p.want(http.StatusOK, "GET", "/api/objects/configs/app", ""); !strings.Contains(c, `"mode":"prod"`) {
		t.Fatalf("config = %s", c)
	}

	// Join tokens.
	if tok := p.want(http.StatusOK, "POST", "/api/tokens", `{"ttlHours": 1, "maxUses": 1}`); !strings.Contains(tok, "shoal agent --server") || !strings.Contains(tok, "--ca-hash sha256:") {
		t.Fatalf("token = %s", tok)
	}

	// Errors keep their meaning.
	p.want(http.StatusNotFound, "GET", "/api/objects/workloads/nope", "")
	p.want(http.StatusNotFound, "GET", "/api/objects/joker", "")
	p.want(http.StatusNoContent, "DELETE", "/api/objects/workloads/web", "")
	p.want(http.StatusNotFound, "DELETE", "/api/objects/workloads/web", "")

	// The CA is downloadable without logging in (to trust the portal).
	p.want(http.StatusNoContent, "POST", "/api/logout", "")
	if ca := p.want(http.StatusOK, "GET", "/ca.crt", ""); !strings.Contains(ca, "BEGIN CERTIFICATE") {
		t.Fatal("ca.crt")
	}
}
