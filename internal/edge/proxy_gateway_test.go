package edge

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// statusUpstream starts an upstream that always answers code and counts requests.
func statusUpstream(t *testing.T, code int) (string, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	addr := upstreamWith(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, "bad")
	}))
	return addr, &hits
}

// A GET that hits an endpoint answering 502, 503 or 504 is retried once on
// another endpoint, and the bad endpoint goes into the passive cooldown
// (ADR 0015, pass E).
func TestGatewayErrorIsRetriedForGET(t *testing.T) {
	for _, code := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			bad, badHits := statusUpstream(t, code)
			good, goodHits := echoID(t, "good")
			p := newProxy(t, Config{Cooldown: time.Minute}, mkRoute("web", "app.example.test", "/", "none", bad, good))
			front := httptest.NewServer(p)
			t.Cleanup(front.Close)

			for i := 0; i < 6; i++ {
				resp, body := fetch(t, http.DefaultClient, front.URL, http.MethodGet, "app.example.test", "/", nil)
				if resp.StatusCode != http.StatusOK || body != "good" {
					t.Fatalf("request %d: status %d body %q, want 200 good", i, resp.StatusCode, body)
				}
			}
			if n := badHits.Load(); n != 1 {
				t.Fatalf("bad endpoint hit %d times, want 1 (then cooled down)", n)
			}
			if goodHits.Load() != 6 {
				t.Fatalf("good endpoint hits = %d, want 6", goodHits.Load())
			}
			p.mu.Lock()
			_, cooling := p.bad[bad]
			p.mu.Unlock()
			if !cooling {
				t.Fatal("the endpoint that answered 5xx is not in cooldown")
			}
		})
	}
}

// A POST is not retried, so a 503 reaches the client as it is.
func TestGatewayErrorIsNotRetriedForPOST(t *testing.T) {
	bad, badHits := statusUpstream(t, http.StatusServiceUnavailable)
	good, goodHits := echoID(t, "good")
	p := newProxy(t, Config{}, mkRoute("web", "app.example.test", "/", "none", bad, good))
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	req, err := http.NewRequest(http.MethodPost, front.URL+"/", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "app.example.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("POST status %d, want the upstream's 503", resp.StatusCode)
	}
	if badHits.Load()+goodHits.Load() > 1 {
		t.Fatalf("POST was sent %d times, want once", badHits.Load()+goodHits.Load())
	}
}

// When every endpoint answers 5xx, the client gets an upstream answer, not a
// proxy error, and each endpoint was tried once.
func TestAll5xxReturnsUpstreamAnswer(t *testing.T) {
	a, aHits := statusUpstream(t, http.StatusServiceUnavailable)
	b, bHits := statusUpstream(t, http.StatusServiceUnavailable)
	p := newProxy(t, Config{Cooldown: time.Minute}, mkRoute("web", "app.example.test", "/", "none", a, b))
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	resp, body := fetch(t, http.DefaultClient, front.URL, http.MethodGet, "app.example.test", "/", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || body != "bad" {
		t.Fatalf("status %d body %q, want the upstream's 503 bad", resp.StatusCode, body)
	}
	if aHits.Load() != 1 || bHits.Load() != 1 {
		t.Fatalf("hits a=%d b=%d, want one each", aHits.Load(), bHits.Load())
	}
}

// A 500 is not a gateway error: it is the application's answer and is not retried.
func TestPlain500IsNotRetried(t *testing.T) {
	bad, badHits := statusUpstream(t, http.StatusInternalServerError)
	good, goodHits := echoID(t, "good")
	p := newProxy(t, Config{}, mkRoute("web", "app.example.test", "/", "none", bad, good))
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	resp, _ := fetch(t, http.DefaultClient, front.URL, http.MethodGet, "app.example.test", "/", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d, want the application's 500", resp.StatusCode)
	}
	if badHits.Load()+goodHits.Load() != 1 {
		t.Fatalf("a 500 was retried: hits %d", badHits.Load()+goodHits.Load())
	}
}
