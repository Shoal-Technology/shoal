package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
)

// logAnswerTimeout bounds how long a node may take to start answering a
// log request.
const logAnswerTimeout = 10 * time.Second

// noteStream marks lines from shoal itself (e.g. "node not connected")
// rather than from the container.
const noteStream = "shoal"

// logBroker connects log requests from the CLI to the node agents: the
// request goes out over the node's Sync stream, the answer comes back on an
// UploadLogs stream carrying the request ID (ADR 0003).
type logBroker struct {
	mu       sync.Mutex
	sessions map[string]chan *v1alpha1.LogRequest // by node: requests for its Sync stream
	pending  map[string]*pendingLogs              // by request ID
}

type pendingLogs struct {
	node     string
	ctx      context.Context // the requester's; done when it gives up
	chunks   chan *v1alpha1.LogChunk
	once     sync.Once
	answered bool // an UploadLogs stream took it (guarded by logBroker.mu)
}

func (p *pendingLogs) finish() { p.once.Do(func() { close(p.chunks) }) }

func newLogBroker() *logBroker {
	return &logBroker{sessions: map[string]chan *v1alpha1.LogRequest{}, pending: map[string]*pendingLogs{}}
}

// attach registers a node's Sync stream; the returned channel carries log
// requests for it. detach undoes it unless a newer stream replaced it.
func (b *logBroker) attach(node string) chan *v1alpha1.LogRequest {
	ch := make(chan *v1alpha1.LogRequest, 16)
	b.mu.Lock()
	b.sessions[node] = ch
	b.mu.Unlock()
	return ch
}

func (b *logBroker) detach(node string, ch chan *v1alpha1.LogRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessions[node] != ch {
		return // a newer stream of the node took over
	}
	delete(b.sessions, node)
	// Requests the node can no longer receive or answer fail at once
	// instead of waiting for the answer timeout.
	for _, p := range b.pending {
		if p.node == node && !p.answered {
			p.finish()
		}
	}
}

// request asks node for logs. The caller reads the chunks until the channel
// closes and must call cancel when done.
func (b *logBroker) request(ctx context.Context, node string, req *v1alpha1.LogRequest) (<-chan *v1alpha1.LogChunk, func(), error) {
	var id [12]byte
	_, _ = rand.Read(id[:]) // never fails (crypto/rand panics instead)
	req.RequestId = hex.EncodeToString(id[:])
	p := &pendingLogs{node: node, ctx: ctx, chunks: make(chan *v1alpha1.LogChunk, 16)}
	b.mu.Lock()
	session, ok := b.sessions[node]
	if ok {
		b.pending[req.RequestId] = p
	}
	b.mu.Unlock()
	if !ok {
		return nil, nil, fmt.Errorf("node %s is not connected", node)
	}
	cancel := func() {
		b.mu.Lock()
		delete(b.pending, req.RequestId)
		b.mu.Unlock()
	}
	select {
	case session <- req:
	default:
		cancel()
		return nil, nil, fmt.Errorf("node %s is busy", node)
	}
	return p.chunks, cancel, nil
}

// take returns the pending request id, marking it answered; nil if it is
// unknown or was already given up.
func (b *logBroker) take(id string) *pendingLogs {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.pending[id]
	if p == nil || p.answered {
		return nil
	}
	select {
	case <-p.chunks: // closed: failed by detach
		return nil
	default:
	}
	p.answered = true
	return p
}

// UploadLogs receives the answer to a log request from the node it was
// sent to.
func (a *agentService) UploadLogs(stream v1alpha1.Agent_UploadLogsServer) error {
	c, ok := callerFrom(stream.Context())
	if !ok {
		return status.Error(codes.Unauthenticated, "no caller identity")
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	p := a.s.logs.take(first.GetRequestId())
	if p == nil {
		return status.Error(codes.NotFound, "no such log request (it may have been cancelled)")
	}
	if p.node != c.id.Name {
		return status.Error(codes.PermissionDenied, "log request was sent to another node")
	}
	defer p.finish()
	for chunk := first; ; {
		select {
		case p.chunks <- chunk:
		case <-p.ctx.Done():
			return status.Error(codes.Canceled, "log request cancelled")
		}
		chunk, err = stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&v1alpha1.UploadLogsResponse{})
		}
		if err != nil {
			return err
		}
	}
}

// Logs streams the logs of a workload's replicas. Without follow, the lines
// of all replicas are merged in time order.
func (c *controlService) Logs(req *v1alpha1.LogsRequest, stream v1alpha1.Control_LogsServer) error {
	ctx := stream.Context()
	if req.GetWorkload() == "" {
		return status.Error(codes.InvalidArgument, "workload is required")
	}
	recs, err := c.s.store.List(ctx, v1alpha1.KindReplica)
	if err != nil {
		return apiError(err)
	}
	var replicas []*v1alpha1.Replica
	for _, r := range recs {
		rp, err := objects.Replica(r)
		if err != nil {
			return apiError(err)
		}
		if rp.Spec.GetWorkload() == req.GetWorkload() && (req.GetReplica() == "" || r.Name == req.GetReplica()) {
			replicas = append(replicas, rp)
		}
	}
	if len(replicas) == 0 {
		if req.GetReplica() != "" {
			return status.Errorf(codes.NotFound, "workload %q has no replica %q", req.GetWorkload(), req.GetReplica())
		}
		return status.Errorf(codes.NotFound, "workload %q has no replicas", req.GetWorkload())
	}

	lines := make(chan *v1alpha1.LogLine, 256)
	var wg sync.WaitGroup
	for _, rp := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.fetchLogs(ctx, rp, req, lines)
		}()
	}
	go func() { wg.Wait(); close(lines) }()

	if req.GetFollow() {
		for l := range lines {
			if err := stream.Send(l); err != nil {
				return err
			}
		}
		return nil
	}
	var all []*v1alpha1.LogLine
	for l := range lines {
		all = append(all, l)
	}
	slices.SortStableFunc(all, func(x, y *v1alpha1.LogLine) int {
		return cmp.Compare(x.GetTime().AsTime().UnixNano(), y.GetTime().AsTime().UnixNano())
	})
	for _, l := range all {
		if err := stream.Send(l); err != nil {
			return err
		}
	}
	return nil
}

// fetchLogs asks the replica's node for its logs and forwards the lines.
// Problems become note lines, so one unreachable node doesn't hide the
// logs of the others.
func (c *controlService) fetchLogs(ctx context.Context, rp *v1alpha1.Replica, req *v1alpha1.LogsRequest, out chan<- *v1alpha1.LogLine) {
	name := rp.Metadata.GetName()
	note := func(format string, args ...any) {
		select {
		case out <- &v1alpha1.LogLine{Time: ts(c.s.now()), Stream: noteStream, Replica: name, Line: fmt.Sprintf(format, args...)}:
		case <-ctx.Done():
		}
	}
	node := rp.Spec.GetNode()
	if node == "" {
		note("no logs: replica is not scheduled")
		return
	}
	chunks, cancel, err := c.s.logs.request(ctx, node, &v1alpha1.LogRequest{Replica: name, Follow: req.GetFollow(), Tail: req.GetTail()})
	if err != nil {
		note("no logs: %v", err)
		return
	}
	defer cancel()
	timeout := time.NewTimer(logAnswerTimeout)
	defer timeout.Stop()
	for answered := false; ; {
		var chunk *v1alpha1.LogChunk
		var ok bool
		if answered {
			select {
			case chunk, ok = <-chunks:
			case <-ctx.Done():
				return
			}
		} else {
			select {
			case chunk, ok = <-chunks:
				answered = ok // a closed channel means the node never answered
			case <-timeout.C:
				note("no logs: node %s did not answer", node)
				return
			case <-ctx.Done():
				return
			}
		}
		if !ok {
			if !answered {
				note("no logs: node %s is not connected", node)
			}
			return
		}
		for _, l := range chunk.GetLines() {
			l.Replica = name
			select {
			case out <- l:
			case <-ctx.Done():
				return
			}
		}
		if e := chunk.GetError(); e != "" {
			note("logs ended with an error: %s", e)
		}
	}
}
