package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/store"
)

// Node phases.
const (
	NodeReady    = v1alpha1.NodeReady
	NodeNotReady = v1alpha1.NodeNotReady
)

// nodeTracker is the only writer of Node status. Heartbeats are kept in
// memory; the stored status is written only when something other than the
// heartbeat time changes, or every persistEvery, so heartbeats don't turn
// into a stream of store writes (and later Raft entries).
type nodeTracker struct {
	store         store.Store
	log           *slog.Logger
	now           func() time.Time
	notReadyAfter time.Duration
	persistEvery  time.Duration

	mu   sync.Mutex
	live map[string]*liveNode
	// graceStart and graceUntil bound the period after this node gained
	// leadership. The in-memory heartbeats were lost with the previous
	// leadership, so no node is marked NotReady before graceUntil, and a
	// node with no live heartbeat is counted from graceStart. Zero when the
	// tracker has never begun a term.
	graceStart time.Time
	graceUntil time.Time
}

type liveNode struct {
	lastSeen    time.Time
	status      *v1alpha1.NodeStatus // as last written to the store
	persistedAt time.Time
}

func newNodeTracker(st store.Store, log *slog.Logger, now func() time.Time, notReadyAfter, persistEvery time.Duration) *nodeTracker {
	return &nodeTracker{
		store: st, log: log, now: now, notReadyAfter: notReadyAfter, persistEvery: persistEvery,
		live: map[string]*liveNode{},
	}
}

// beginTerm is called when this node gains leadership. It drops the in-memory
// heartbeats, which describe the previous leader's view, and opens a grace
// period of notReadyAfter. Agents need that long to reach the new leader, so
// nodes are not evicted based on empty or stale state.
func (t *nodeTracker) beginTerm(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.live = map[string]*liveNode{}
	t.graceStart = now
	t.graceUntil = now.Add(t.notReadyAfter)
}

// heartbeat records a heartbeat from node name, seen from address addr.
func (t *nodeTracker) heartbeat(ctx context.Context, name, addr string, hb *v1alpha1.Heartbeat) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	l := t.live[name]
	if l == nil {
		l = &liveNode{}
		t.live[name] = l
	}
	l.lastSeen = now
	st := &v1alpha1.NodeStatus{
		Phase:         NodeReady,
		LastHeartbeat: ts(now),
		Address:       addr,
		Capacity:      hb.GetCapacity(),
		AgentVersion:  hb.GetAgentVersion(),
		Os:            hb.GetOs(),
		Arch:          hb.GetArch(),

		WireguardPublicKey: hb.GetWireguardPublicKey(),
		WireguardEndpoint:  wireguardEndpoint(addr, hb),
	}
	if l.status != nil && sameExceptHeartbeat(l.status, st) && now.Sub(l.persistedAt) < t.persistEvery {
		return nil
	}
	if l.status == nil || l.status.Phase != NodeReady {
		t.log.Info("node ready", "node", name, "address", addr)
	}
	return t.persist(ctx, name, l, st, now)
}

func (t *nodeTracker) persist(ctx context.Context, name string, l *liveNode, st *v1alpha1.NodeStatus, now time.Time) error {
	b, err := encode(st)
	if err != nil {
		return err
	}
	if _, err := t.store.UpdateStatus(ctx, v1alpha1.KindNode, name, b); err != nil {
		return fmt.Errorf("update status of node %s: %w", name, err)
	}
	l.status, l.persistedAt = st, now
	return nil
}

func sameExceptHeartbeat(a, b *v1alpha1.NodeStatus) bool {
	a, b = proto.CloneOf(a), proto.CloneOf(b)
	a.LastHeartbeat, b.LastHeartbeat = nil, nil
	return proto.Equal(a, b)
}

// check marks nodes NotReady when their last heartbeat (in memory, or the
// stored one after a server restart) is older than notReadyAfter.
func (t *nodeTracker) check(ctx context.Context) error {
	recs, err := t.store.List(ctx, v1alpha1.KindNode)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	inGrace := now.Before(t.graceUntil)
	exists := map[string]bool{}
	var errs []error
	for _, r := range recs {
		exists[r.Name] = true
		n, err := objects.Node(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		l := t.live[r.Name]
		if l == nil {
			// Not seen this term: trust the stored status, but never count a
			// heartbeat from before the grace period started.
			l = &liveNode{status: n.Status, persistedAt: now, lastSeen: t.graceStart}
			if hb := n.Status.GetLastHeartbeat(); hb != nil && hb.AsTime().After(l.lastSeen) {
				l.lastSeen = hb.AsTime()
			}
			t.live[r.Name] = l
		}
		if inGrace || n.Status.GetPhase() == NodeNotReady || now.Sub(l.lastSeen) < t.notReadyAfter {
			continue
		}
		st := proto.CloneOf(n.Status)
		st.Phase = NodeNotReady
		if !l.lastSeen.IsZero() {
			st.LastHeartbeat = ts(l.lastSeen)
		}
		t.log.Warn("node not ready: no heartbeat", "node", r.Name, "lastSeen", l.lastSeen)
		if err := t.persist(ctx, r.Name, l, st, now); err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	for name := range t.live {
		if !exists[name] {
			delete(t.live, name)
		}
	}
	return errors.Join(errs...)
}

// run calls check every interval until ctx is done.
func (t *nodeTracker) run(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := t.check(ctx); err != nil && ctx.Err() == nil {
			t.log.Error("node status check failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// overlay replaces a stored Node's heartbeat time and phase with the live
// values, so `get nodes` is current even though the store lags.
func (t *nodeTracker) overlay(n *v1alpha1.Node) {
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.live[n.GetMetadata().GetName()]
	if l == nil || l.lastSeen.IsZero() || n.Status == nil {
		return
	}
	n.Status.LastHeartbeat = ts(l.lastSeen)
	n.Status.Phase = NodeReady
	if t.now().Sub(l.lastSeen) >= t.notReadyAfter {
		n.Status.Phase = NodeNotReady
	}
}

// forget drops in-memory state of a deleted node.
func (t *nodeTracker) forget(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.live, name)
}
