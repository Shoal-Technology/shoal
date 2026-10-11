// Package metrics is a small Prometheus text-format (version 0.0.4)
// exposition library: counters, gauges, histograms and scrape-time
// collectors with bounded label sets. It is written in-tree instead of
// importing a client library to keep the binaries' footprint small (ADR 0018).
package metrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ContentType is the Prometheus text exposition content type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// DefaultMaxSeries bounds the number of series in one metric family.
// Samples beyond the cap are dropped and counted in
// shoal_metrics_dropped_series_total, so a runaway label cannot exhaust memory.
const DefaultMaxSeries = 200

// DefaultBuckets suit request latencies in seconds.
var DefaultBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

var (
	metricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// Registry holds metric families and writes them in exposition format.
type Registry struct {
	maxSeries int
	dropped   atomic.Uint64

	mu   sync.Mutex
	vecs []*vec
	seen map[string]bool
}

// NewRegistry returns an empty registry with DefaultMaxSeries.
func NewRegistry() *Registry {
	return &Registry{maxSeries: DefaultMaxSeries, seen: map[string]bool{}}
}

// Dropped reports how many samples were dropped by the series cap.
func (r *Registry) Dropped() uint64 { return r.dropped.Load() }

type sample struct {
	values []string
	v      float64  // counter and gauge value
	counts []uint64 // histogram: per-bucket (non-cumulative) counts
	sum    float64  // histogram sum
	n      uint64   // histogram count
}

// vec is one metric family.
type vec struct {
	reg     *Registry
	name    string
	help    string
	typ     string
	labels  []string
	buckets []float64

	// collect, when set, produces samples at scrape time instead of
	// storing them (used for gauges read from live state).
	collect func(emit func(v float64, values ...string))

	mu     sync.Mutex
	series map[string]*sample
}

func (r *Registry) register(name, help, typ string, labels []string, buckets []float64) *vec {
	if !metricNameRE.MatchString(name) {
		panic("metrics: invalid metric name " + name)
	}
	for _, l := range labels {
		if !labelNameRE.MatchString(l) {
			panic("metrics: invalid label name " + l)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen[name] {
		panic("metrics: duplicate metric " + name)
	}
	r.seen[name] = true
	v := &vec{reg: r, name: name, help: help, typ: typ, labels: labels, buckets: buckets,
		series: map[string]*sample{}}
	r.vecs = append(r.vecs, v)
	return v
}

// get returns the series for values, creating it under the cap. v.mu must be held.
func (v *vec) get(values []string) *sample {
	if len(values) != len(v.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", v.name, len(v.labels), len(values)))
	}
	key := ""
	for i, val := range values {
		if i > 0 {
			key += "\xff"
		}
		key += val
	}
	if s, ok := v.series[key]; ok {
		return s
	}
	if len(v.series) >= v.reg.maxSeries {
		v.reg.dropped.Add(1)
		return nil
	}
	s := &sample{values: append([]string(nil), values...)}
	if v.typ == "histogram" {
		s.counts = make([]uint64, len(v.buckets))
	}
	v.series[key] = s
	return s
}

func (v *vec) update(values []string, fn func(s *sample)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if s := v.get(values); s != nil {
		fn(s)
	}
}

// Counter is a monotonically increasing value.
type Counter struct{ v *vec }

// NewCounter registers a counter family.
func (r *Registry) NewCounter(name, help string, labels ...string) *Counter {
	return &Counter{r.register(name, help, "counter", labels, nil)}
}

// Inc adds 1 to the series for the label values.
func (c *Counter) Inc(values ...string) { c.Add(1, values...) }

// Add adds delta (which must be >= 0) to the series for the label values.
func (c *Counter) Add(delta float64, values ...string) {
	if delta < 0 {
		return
	}
	c.v.update(values, func(s *sample) { s.v += delta })
}

// Gauge is a value that can go up and down.
type Gauge struct{ v *vec }

// NewGauge registers a gauge family.
func (r *Registry) NewGauge(name, help string, labels ...string) *Gauge {
	return &Gauge{r.register(name, help, "gauge", labels, nil)}
}

// Set sets the series for the label values.
func (g *Gauge) Set(value float64, values ...string) {
	g.v.update(values, func(s *sample) { s.v = value })
}

// Histogram counts observations into buckets.
type Histogram struct{ v *vec }

// NewHistogram registers a histogram family. buckets must be sorted ascending;
// nil selects DefaultBuckets.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labels ...string) *Histogram {
	if buckets == nil {
		buckets = DefaultBuckets
	}
	return &Histogram{r.register(name, help, "histogram", labels, buckets)}
}

// Observe records one observation for the label values.
func (h *Histogram) Observe(x float64, values ...string) {
	h.v.update(values, func(s *sample) {
		for i, b := range h.v.buckets {
			if x <= b {
				s.counts[i]++
				break
			}
		}
		s.sum += x
		s.n++
	})
}

// NewCollector registers a family whose samples are produced at scrape time
// by fn. typ is "gauge" or "counter". fn calls emit once per series.
func (r *Registry) NewCollector(name, help, typ string, labels []string,
	fn func(emit func(v float64, values ...string))) {
	if typ != "gauge" && typ != "counter" {
		panic("metrics: collector type must be gauge or counter")
	}
	v := r.register(name, help, typ, labels, nil)
	v.collect = fn
}

// WriteTo writes every family in exposition format, sorted by name.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	vecs := append([]*vec(nil), r.vecs...)
	r.mu.Unlock()
	sort.Slice(vecs, func(i, j int) bool { return vecs[i].name < vecs[j].name })

	var b bytes.Buffer
	for _, v := range vecs {
		v.write(&b)
	}
	fmt.Fprintf(&b, "# HELP shoal_metrics_dropped_series_total Samples dropped by the per-family series cap.\n")
	fmt.Fprintf(&b, "# TYPE shoal_metrics_dropped_series_total counter\n")
	fmt.Fprintf(&b, "shoal_metrics_dropped_series_total %d\n", r.dropped.Load())
	return b.WriteTo(w)
}

func (v *vec) write(b *bytes.Buffer) {
	b.WriteString("# HELP " + v.name + " ")
	writeEscaped(b, v.help, false)
	b.WriteString("\n# TYPE " + v.name + " " + v.typ + "\n")

	if v.collect != nil {
		var out []sample
		v.collect(func(val float64, values ...string) {
			if len(values) != len(v.labels) {
				panic("metrics: collector " + v.name + " label count mismatch")
			}
			if len(out) >= v.reg.maxSeries {
				v.reg.dropped.Add(1)
				return
			}
			out = append(out, sample{values: append([]string(nil), values...), v: val})
		})
		sort.Slice(out, func(i, j int) bool { return lessValues(out[i].values, out[j].values) })
		for i := range out {
			writeSample(b, v.name, v.labels, out[i].values, "", out[i].v)
		}
		return
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	list := make([]*sample, 0, len(v.series))
	for _, s := range v.series {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return lessValues(list[i].values, list[j].values) })
	for _, s := range list {
		if v.typ != "histogram" {
			writeSample(b, v.name, v.labels, s.values, "", s.v)
			continue
		}
		var cum uint64
		for i, bound := range v.buckets {
			cum += s.counts[i]
			writeSample(b, v.name+"_bucket", v.labels, s.values, formatFloat(bound), float64(cum))
		}
		writeSample(b, v.name+"_bucket", v.labels, s.values, "+Inf", float64(s.n))
		writeSample(b, v.name+"_sum", v.labels, s.values, "", s.sum)
		writeSample(b, v.name+"_count", v.labels, s.values, "", float64(s.n))
	}
}

func lessValues(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// writeSample writes one line. le, when not empty, is appended as the
// histogram bucket label.
func writeSample(b *bytes.Buffer, name string, labels, values []string, le string, val float64) {
	b.WriteString(name)
	if len(labels) > 0 || le != "" {
		b.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l)
			b.WriteString(`="`)
			writeEscaped(b, values[i], true)
			b.WriteByte('"')
		}
		if le != "" {
			if len(labels) > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`le="` + le + `"`)
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(formatFloat(val))
	b.WriteByte('\n')
}

// writeEscaped escapes backslash and newline, and double quote for label values.
func writeEscaped(b *bytes.Buffer, s string, label bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '"' && label:
			b.WriteString(`\"`)
		default:
			b.WriteByte(c)
		}
	}
}

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// Handler serves /metrics, /healthz and /readyz. ready reports whether the
// process can serve; nil means always ready. Liveness (/healthz) only means
// the process answers.
func Handler(reg *Registry, ready func() error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ContentType)
		reg.WriteTo(w)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if ready != nil {
			if err := ready(); err != nil {
				// The error text is logged by the caller; the response stays generic.
				http.Error(w, "not ready\n", http.StatusServiceUnavailable)
				return
			}
		}
		io.WriteString(w, "ready\n")
	})
	return mux
}

// Serve serves h on lis until ctx is done. It returns nil after a clean shutdown.
func Serve(ctx context.Context, lis net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
		case <-stop:
			return
		}
		sc, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sc)
	}()
	err := srv.Serve(lis)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
