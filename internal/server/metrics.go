package server

import (
	"context"
	"errors"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/metrics"
)

// observedKinds are the kinds counted by shoal_objects.
var observedKinds = []string{
	v1alpha1.KindNode, v1alpha1.KindWorkload, v1alpha1.KindReplica,
	v1alpha1.KindSecret, v1alpha1.KindConfig, v1alpha1.KindService,
	v1alpha1.KindRoute, v1alpha1.KindVolume, v1alpha1.KindDatabase,
	"Certificate", "DatabaseBackup",
}

var errNoLeader = errors.New("no raft leader known")

// serverMetrics is the server's /metrics state (see ADR 0018).
type serverMetrics struct {
	reg      *metrics.Registry
	requests *metrics.Counter
	latency  *metrics.Histogram
}

// initMetrics builds the registry. It runs in New, after the store and raft
// are set up, and before the gRPC server starts serving.
func (s *Server) initMetrics() {
	reg := metrics.NewRegistry()
	s.obs = &serverMetrics{
		reg: reg,
		requests: reg.NewCounter("shoal_api_requests_total",
			"API calls by gRPC method and status code.", "method", "code"),
		latency: reg.NewHistogram("shoal_api_request_duration_seconds",
			"API call latency by gRPC method.", nil, "method"),
	}
	reg.NewCollector("shoal_objects", "Stored objects per kind.", "gauge", []string{"kind"}, s.collectObjects)
	if s.raft != nil {
		reg.NewCollector("shoal_raft_is_leader", "1 if this node is the raft leader.", "gauge", nil,
			func(emit func(float64, ...string)) {
				v := 0.0
				if s.raft.IsLeader() {
					v = 1
				}
				emit(v)
			})
		for _, key := range []string{"commit_index", "applied_index", "last_log_index"} {
			key := key
			reg.NewCollector("shoal_raft_"+key, "Raft "+key+" as this node sees it.", "gauge", nil,
				func(emit func(float64, ...string)) { emit(statNumber(s.raft.Stats(), key)) })
		}
	}
}

// Metrics returns the server's Prometheus registry, served on --metrics-listen.
func (s *Server) Metrics() *metrics.Registry { return s.obs.reg }

// Ready reports nil when the store answers and, with raft, a leader is known.
// It backs /readyz.
func (s *Server) Ready() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.store.List(ctx, v1alpha1.KindNode); err != nil {
		return err
	}
	if s.raft != nil {
		if addr, _ := s.raft.Leader(); addr == "" {
			return errNoLeader
		}
	}
	return nil
}

func (s *Server) collectObjects(emit func(float64, ...string)) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, kind := range observedKinds {
		recs, err := s.store.List(ctx, kind)
		if err != nil {
			continue
		}
		emit(float64(len(recs)), kind)
	}
}

// unaryMetrics is the outermost interceptor, so rejected calls are counted too.
func (s *Server) unaryMetrics(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	s.obs.requests.Inc(info.FullMethod, status.Code(err).String())
	s.obs.latency.Observe(time.Since(start).Seconds(), info.FullMethod)
	return resp, err
}

func statNumber(m map[string]string, key string) float64 {
	v, _ := strconv.ParseFloat(m[key], 64)
	return v
}
