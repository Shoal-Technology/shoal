package server

import (
	"context"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/store/memory"
)

// TestNodeTrackerGracePeriod checks that a new leader does not mark nodes
// NotReady based on stale stored heartbeats. Nodes get notReadyAfter from the
// start of the term to heartbeat the new leader.
func TestNodeTrackerGracePeriod(t *testing.T) {
	c := &clock{}
	ctx := context.Background()
	st := memory.New(c.now)
	const notReadyAfter = 20 * time.Second

	// Both nodes were last seen an hour ago, under the previous leader.
	stale, err := encode(&v1alpha1.NodeStatus{Phase: NodeReady, LastHeartbeat: ts(c.now().Add(-time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if _, err := st.Create(ctx, store.Record{Kind: v1alpha1.KindNode, Name: name, Status: stale}); err != nil {
			t.Fatal(err)
		}
	}
	phase := func(name string) string {
		t.Helper()
		rec, err := st.Get(ctx, v1alpha1.KindNode, name)
		if err != nil {
			t.Fatal(err)
		}
		n, err := objects.Node(rec)
		if err != nil {
			t.Fatal(err)
		}
		return n.GetStatus().GetPhase()
	}

	tr := newNodeTracker(st, discard(), c.now, notReadyAfter, time.Hour)
	tr.beginTerm(c.now())

	// Inside the grace period nothing is evicted, even though the stored
	// heartbeats are an hour old.
	c.advance(notReadyAfter - time.Second)
	if err := tr.check(ctx); err != nil {
		t.Fatal(err)
	}
	if p := phase("a"); p != NodeReady {
		t.Fatalf("during grace: a = %q, want Ready", p)
	}
	if p := phase("b"); p != NodeReady {
		t.Fatalf("during grace: b = %q, want Ready", p)
	}

	// Only a heartbeats during the grace period.
	if err := tr.heartbeat(ctx, "a", "127.0.0.1", &v1alpha1.Heartbeat{}); err != nil {
		t.Fatal(err)
	}

	// After the grace period b, silent since the term began, is evicted and
	// a, which heartbeat in time, stays Ready.
	c.advance(2 * time.Second)
	if err := tr.check(ctx); err != nil {
		t.Fatal(err)
	}
	if p := phase("b"); p != NodeNotReady {
		t.Fatalf("after grace: b = %q, want NotReady", p)
	}
	if p := phase("a"); p != NodeReady {
		t.Fatalf("after grace: a = %q, want Ready", p)
	}
}
