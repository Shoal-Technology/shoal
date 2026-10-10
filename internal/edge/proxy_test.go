package edge

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// upstreamWith starts an HTTP server on 127.0.0.1:0 and returns its address.
func upstreamWith(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// echoID starts an upstream that answers with id and counts its requests.
func echoID(t *testing.T, id string) (string, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	addr := upstreamWith(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, id)
	}))
	return addr, &hits
}

// deadAddr returns a loopback address with nothing listening on it.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func newProxy(t *testing.T, cfg Config, ts ...Target) *Proxy {
	t.Helper()
	p := NewProxy(cfg)
	p.SetTable(mustTable(t, ts...))
	return p
}

// fetch sends one request with the given Host header and returns the response and its body.
func fetch(t *testing.T, c *http.Client, base, method, host, path string, hdr http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s%s: %v", method, host, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func TestRoundRobinAcrossTwoUpstreams(t *testing.T) {
	a, _ := echoID(t, "a")
	b, _ := echoID(t, "b")
	p := newProxy(t, Config{}, mkRoute("web", "app.example.test", "/", "none", a, b))
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		resp, body := fetch(t, http.DefaultClient, front.URL, http.MethodGet, "app.example.test", "/", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
		counts[body]++
	}
	if counts["a"] != 3 || counts["b"] != 3 {
		t.Fatalf("round robin split = %v, want 3 and 3", counts)
	}
}

func TestUnknownHostAndPathGet404(t *testing.T) {
	addr, hits := echoID(t, "x")
	p := newProxy(t, Config{}, mkRoute("web", "app.example.test", "/api", "none", addr))
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	for _, c := range []struct{ host, path string }{
		{"other.example.test", "/api"},
		{"app.example.test", "/other"},
	} {
		resp, body := fetch(t, http.DefaultClient, front.URL, http.MethodGet, c.host, c.path, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s%s: status %d, want 404", c.host, c.path, resp.StatusCode)
		}
		if body != "404 page not found\n" {
			t.Errorf("%s%s: body %q leaks more than the generic 404", c.host, c.path, body)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("upstream was hit %d times for unmatched requests", n)
	}

	empty := httptest.NewServer(NewProxy(Config{}))
	t.Cleanup(empty.Close)
	if resp, _ := fetch(t, http.DefaultClient, empty.URL, http.MethodGet, "app.example.test", "/", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no table: status %d, want 404", resp.StatusCode)
	}
}

func TestDeadUpstreamCooldownAndFailover(t *testing.T) {
	var clock atomic.Int64 // nanoseconds past base
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	cfg := Config{Now: func() time.Time { return base.Add(time.Duration(clock.Load())) }}
	dead := deadAddr(t)
	live, _ := echoID(t, "live")
	p := newProxy(t, cfg, mkRoute("web", "app.example.test", "/", "none", dead, live))
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	badUntil := func() (time.Time, bool) {
		p.mu.Lock()
		defer p.mu.Unlock()
		u, ok := p.bad[dead]
		return u, ok
	}
	get := func(want string) {
		t.Helper()
		resp, body := fetch(t, http.DefaultClient, front.URL, http.MethodGet, "app.example.test", "/", nil)
		if resp.StatusCode != http.StatusOK || body != want {
			t.Fatalf("got %d %q, want 200 %q", resp.StatusCode, body, want)
		}
	}

	// The counter starts at the dead endpoint, so this request must fail over.
	get("live")
	if u, ok := badUntil(); !ok || !u.Equal(base.Add(DefaultCooldown)) {
		t.Fatalf("dead endpoint cooldown = %v (set %v), want %v", u, ok, base.Add(DefaultCooldown))
	}

	// Inside the cooldown the dead endpoint is skipped, so its stamp must not move.
	for i := 0; i < 3; i++ {
		get("live")
	}
	if u, _ := badUntil(); !u.Equal(base.Add(DefaultCooldown)) {
		t.Fatalf("dead endpoint was dialled during cooldown (stamp %v)", u)
	}

	// After the cooldown the dead endpoint is tried again and marked again.
	clock.Store(int64(11 * time.Second))
	get("live")
	get("live")
	if u, _ := badUntil(); !u.Equal(base.Add(11*time.Second + DefaultCooldown)) {
		t.Fatalf("dead endpoint not retried after cooldown (stamp %v)", u)
	}
}

func TestPostIsNotRetried(t *testing.T) {
	dead := deadAddr(t)
	live, hits := echoID(t, "live")
	p := newProxy(t, Config{}, mkRoute("web", "app.example.test", "/", "none", dead, live))
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	resp, _ := fetch(t, http.DefaultClient, front.URL, http.MethodPost, "app.example.test", "/", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("POST after dial failure: status %d, want 502", resp.StatusCode)
	}
	if hits.Load() != 0 {
		t.Fatal("non-idempotent request was retried on another endpoint")
	}
}

func TestAllEndpointsFailGet502(t *testing.T) {
	p := newProxy(t, Config{}, mkRoute("web", "app.example.test", "/", "none", deadAddr(t), deadAddr(t)))
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	if resp, _ := fetch(t, http.DefaultClient, front.URL, http.MethodGet, "app.example.test", "/", nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("all endpoints dead: status %d, want 502", resp.StatusCode)
	}
}

func TestNoEndpoints502(t *testing.T) {
	p := newProxy(t, Config{}, mkRoute("web", "app.example.test", "/", "none"))
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	if resp, _ := fetch(t, http.DefaultClient, front.URL, http.MethodGet, "app.example.test", "/", nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("no endpoints: status %d, want 502", resp.StatusCode)
	}
}

func TestSpoofedForwardedHeadersReplaced(t *testing.T) {
	addr := upstreamWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "xff=%s xfp=%s xfh=%s",
			strings.Join(r.Header.Values("X-Forwarded-For"), ","),
			r.Header.Get("X-Forwarded-Proto"),
			r.Header.Get("X-Forwarded-Host"))
	}))
	p := newProxy(t, Config{}, mkRoute("web", "app.example.test", "/", "none", addr))
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	hdr := http.Header{
		"X-Forwarded-For":   {"6.6.6.6"},
		"X-Forwarded-Proto": {"ftp"},
		"X-Forwarded-Host":  {"evil.example"},
	}
	_, body := fetch(t, http.DefaultClient, front.URL, http.MethodGet, "app.example.test", "/", hdr)
	want := "xff=127.0.0.1 xfp=http xfh=app.example.test"
	if body != want {
		t.Fatalf("forwarded headers = %q, want %q", body, want)
	}
}

func TestWebSocketEcho(t *testing.T) {
	ws := upstreamWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "want websocket", http.StatusBadRequest)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); err != nil {
			return
		}
		if rw.Flush() != nil {
			return
		}
		_, _ = io.Copy(conn, rw.Reader) // echo until the client closes
	}))
	p := newProxy(t, Config{}, mkRoute("web", "app.example.test", "/", "none", ws))
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	conn, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, err = io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: app.example.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status %d, want 101", resp.StatusCode)
	}
	if _, err := io.WriteString(conn, "hello"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hello" {
		t.Fatalf("echo = %q, want hello", buf)
	}
}

func TestAccessLogLine(t *testing.T) {
	addr, _ := echoID(t, "ok")
	var mu sync.Mutex
	var got []AccessEntry
	cfg := Config{AccessLog: func(e AccessEntry) {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
	}}
	p := newProxy(t, cfg, mkRoute("web", "app.example.test", "/", "none", addr))
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	fetch(t, http.DefaultClient, front.URL, http.MethodGet, "app.example.test", "/x?token=secret", nil)

	var e AccessEntry
	found := false
	for i := 0; i < 300 && !found; i++ {
		mu.Lock()
		if len(got) > 0 {
			e, found = got[0], true
		}
		mu.Unlock()
		if !found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("no access log entry")
	}
	if e.Method != "GET" || e.Host != "app.example.test" || e.Path != "/x" ||
		e.Route != "web" || e.Upstream != addr || e.Status != 200 || e.Bytes != 2 {
		t.Fatalf("access entry = %+v", e)
	}
}

func TestTableSwapUnderConcurrentRequests(t *testing.T) {
	a, _ := echoID(t, "a")
	b, _ := echoID(t, "b")
	tables := []*Table{
		mustTable(t, mkRoute("web", "app.example.test", "/", "none", a)),
		mustTable(t, mkRoute("web", "app.example.test", "/", "none", b)),
		mustTable(t, mkRoute("web", "app.example.test", "/", "none", a, b)),
	}
	p := NewProxy(Config{})
	p.SetTable(tables[0])
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			p.SetTable(tables[i%len(tables)])
		}
	}()
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				req, err := http.NewRequest(http.MethodGet, front.URL+"/", nil)
				if err != nil {
					t.Error(err)
					return
				}
				req.Host = "app.example.test"
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Errorf("request during swap: %v", err)
					return
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK || (string(body) != "a" && string(body) != "b") {
					t.Errorf("during swap: status %d body %q", resp.StatusCode, body)
					return
				}
			}
		}()
	}
	wg.Wait()
}
