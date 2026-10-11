package metrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var sampleRE = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{.*\})? (\S+)$`)

// parseExposition is a strict checker for the text format: every family has
// HELP and TYPE before its samples, names and labels are valid, and values
// parse as floats. It returns the set of family names and sample names seen.
func parseExposition(t *testing.T, text string) (families map[string]string, samples map[string]int) {
	t.Helper()
	families = map[string]string{}
	samples = map[string]int{}
	helped := map[string]bool{}
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if line == "" {
			t.Fatalf("line %d: blank line", i+1)
		}
		if strings.HasPrefix(line, "# HELP ") {
			name := strings.Fields(line)[2]
			helped[name] = true
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			f := strings.Fields(line)
			if len(f) != 4 {
				t.Fatalf("line %d: bad TYPE: %q", i+1, line)
			}
			if !helped[f[2]] {
				t.Fatalf("line %d: TYPE before HELP for %s", i+1, f[2])
			}
			switch f[3] {
			case "counter", "gauge", "histogram", "summary", "untyped":
			default:
				t.Fatalf("line %d: unknown type %q", i+1, f[3])
			}
			families[f[2]] = f[3]
			continue
		}
		if strings.HasPrefix(line, "#") {
			t.Fatalf("line %d: unexpected comment %q", i+1, line)
		}
		m := sampleRE.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("line %d: not a sample: %q", i+1, line)
		}
		name := m[1]
		if m[2] != "" {
			checkLabels(t, i+1, m[2])
		}
		if _, err := strconv.ParseFloat(m[3], 64); err != nil {
			t.Fatalf("line %d: bad value %q: %v", i+1, m[3], err)
		}
		base := name
		for _, suf := range []string{"_bucket", "_sum", "_count"} {
			if strings.HasSuffix(name, suf) {
				if typ, ok := families[strings.TrimSuffix(name, suf)]; ok && typ == "histogram" {
					base = strings.TrimSuffix(name, suf)
				}
			}
		}
		if _, ok := families[base]; !ok {
			t.Fatalf("line %d: sample %s has no TYPE", i+1, name)
		}
		samples[name]++
	}
	return families, samples
}

// checkLabels validates {k="v",...} with escapes inside values.
func checkLabels(t *testing.T, line int, s string) {
	t.Helper()
	body := s[1 : len(s)-1]
	for body != "" {
		eq := strings.Index(body, `="`)
		if eq <= 0 || !labelNameRE.MatchString(body[:eq]) {
			t.Fatalf("line %d: bad label in %s", line, s)
		}
		j := eq + 2
		for {
			if j >= len(body) {
				t.Fatalf("line %d: unterminated label value in %s", line, s)
			}
			if body[j] == '\\' {
				j += 2
				continue
			}
			if body[j] == '"' {
				break
			}
			j++
		}
		body = body[j+1:]
		if body == "" {
			return
		}
		if body[0] != ',' {
			t.Fatalf("line %d: expected comma in %s", line, s)
		}
		body = body[1:]
	}
}

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var b bytes.Buffer
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestExpositionParsesAndHasExpectedNames(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("shoal_api_requests_total", "API requests.", "method", "code")
	c.Inc("/shoal.v1alpha1.Control/Get", "OK")
	c.Add(2, "/shoal.v1alpha1.Control/List", "NotFound")
	g := r.NewGauge("shoal_raft_is_leader", "1 if leader.")
	g.Set(1)
	h := r.NewHistogram("shoal_api_request_duration_seconds", "Latency.", nil, "method")
	h.Observe(0.02, "Get")
	h.Observe(3, "Get")
	r.NewCollector("shoal_objects", "Objects per kind.", "gauge", []string{"kind"},
		func(emit func(float64, ...string)) {
			emit(3, "Node")
			emit(1, "Deployment")
		})

	text := render(t, r)
	families, samples := parseExposition(t, text)
	want := map[string]string{
		"shoal_api_requests_total":           "counter",
		"shoal_raft_is_leader":               "gauge",
		"shoal_api_request_duration_seconds": "histogram",
		"shoal_objects":                      "gauge",
		"shoal_metrics_dropped_series_total": "counter",
	}
	for name, typ := range want {
		if families[name] != typ {
			t.Errorf("family %s: type %q, want %q", name, families[name], typ)
		}
	}
	for _, name := range []string{
		"shoal_api_request_duration_seconds_bucket",
		"shoal_api_request_duration_seconds_sum",
		"shoal_api_request_duration_seconds_count",
	} {
		if samples[name] == 0 {
			t.Errorf("missing sample %s", name)
		}
	}
	if !strings.Contains(text, `shoal_api_request_duration_seconds_bucket{method="Get",le="+Inf"} 2`) {
		t.Errorf("+Inf bucket should equal count:\n%s", text)
	}
	if !strings.Contains(text, `shoal_objects{kind="Deployment"} 1`) {
		t.Errorf("collector sample missing:\n%s", text)
	}
}

func TestHistogramBucketsCumulative(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("lat_seconds", "h", []float64{0.1, 1}, "m")
	h.Observe(0.05, "x")
	h.Observe(0.5, "x")
	h.Observe(5, "x")
	text := render(t, r)
	for _, want := range []string{
		`lat_seconds_bucket{m="x",le="0.1"} 1`,
		`lat_seconds_bucket{m="x",le="1"} 2`,
		`lat_seconds_bucket{m="x",le="+Inf"} 3`,
		`lat_seconds_count{m="x"} 3`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func TestLabelEscaping(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("esc_total", "help with \\ and\nnewline", "v")
	c.Inc("a\"b\\c\nd")
	text := render(t, r)
	parseExposition(t, text)
	if !strings.Contains(text, `esc_total{v="a\"b\\c\nd"} 1`) {
		t.Errorf("bad escaping:\n%s", text)
	}
}

func TestCardinalityCap(t *testing.T) {
	r := NewRegistry()
	r.maxSeries = 5
	c := r.NewCounter("req_total", "requests", "host")
	for i := 0; i < 100; i++ {
		c.Inc("host-" + strconv.Itoa(i))
	}
	// Existing series keep counting after the cap is reached.
	c.Inc("host-0")
	text := render(t, r)
	_, samples := parseExposition(t, text)
	if samples["req_total"] != 5 {
		t.Fatalf("series = %d, want cap 5", samples["req_total"])
	}
	if r.Dropped() != 95 {
		t.Fatalf("dropped = %d, want 95", r.Dropped())
	}
	if !strings.Contains(text, "shoal_metrics_dropped_series_total 95\n") {
		t.Fatalf("dropped counter not exported:\n%s", text)
	}
	if !strings.Contains(text, `req_total{host="host-0"} 2`) {
		t.Fatalf("existing series lost:\n%s", text)
	}
}

func TestDuplicateAndInvalidNamesPanic(t *testing.T) {
	r := NewRegistry()
	r.NewGauge("dup", "x")
	for name, fn := range map[string]func(){
		"duplicate":   func() { r.NewGauge("dup", "x") },
		"bad name":    func() { r.NewGauge("bad-name", "x") },
		"bad label":   func() { r.NewGauge("ok_name", "x", "bad-label") },
		"label count": func() { r.NewGauge("arity", "x", "a").Set(1) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: expected panic", name)
				}
			}()
			fn()
		}()
	}
}

func TestHealthzReadyz(t *testing.T) {
	r := NewRegistry()
	var ready error = errors.New("raft not ready")
	h := Handler(r, func() error { return ready })

	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, rec.Body.String()
	}
	if code, _ := get("/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", code)
	}
	if code, body := get("/readyz"); code != http.StatusServiceUnavailable || strings.Contains(body, "raft") {
		t.Errorf("/readyz not ready = %d %q, want 503 generic", code, body)
	}
	ready = nil
	if code, _ := get("/readyz"); code != http.StatusOK {
		t.Errorf("/readyz ready = %d, want 200", code)
	}
	if code, body := get("/metrics"); code != http.StatusOK || !strings.Contains(body, "shoal_metrics_dropped_series_total") {
		t.Errorf("/metrics = %d", code)
	}
}

func TestServeShutsDownOnCancel(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, lis, Handler(NewRegistry(), nil)) }()
	resp, err := http.Get(fmt.Sprintf("http://%s/healthz", lis.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok\n" {
		t.Fatalf("body %q", body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve returned %v", err)
	}
}

// BenchmarkScrape measures the cost of one scrape of a representative registry
// (about 150 series). Run with -benchmem for allocations per scrape.
func BenchmarkScrape(b *testing.B) {
	r := NewRegistry()
	c := r.NewCounter("api_total", "x", "method", "code")
	h := r.NewHistogram("api_seconds", "x", nil, "method")
	for i := 0; i < 50; i++ {
		m := "/svc/Method" + strconv.Itoa(i%10)
		c.Inc(m, "OK")
		h.Observe(float64(i)/100, m)
	}
	r.NewCollector("objects", "x", "gauge", []string{"kind"}, func(emit func(float64, ...string)) {
		for i := 0; i < 20; i++ {
			emit(float64(i), "Kind"+strconv.Itoa(i))
		}
	})
	var buf bytes.Buffer
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		r.WriteTo(&buf)
	}
}
