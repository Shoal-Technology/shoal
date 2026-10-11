package raftstore

import (
	"context"
	"errors"
	"testing"

	"github.com/kaiserern/shoal/internal/store"
)

// TestExportLeaderOnly checks that only the leader exports a dump, and that
// the dump includes a write just acknowledged by the leader.
func TestExportLeaderOnly(t *testing.T) {
	nodes := newCluster(t, 3)
	leader := waitLeader(t, nodes)
	ctx := context.Background()
	if _, err := leader.Create(ctx, store.Record{Kind: "Widget", Name: "a", Spec: []byte("1")}); err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n == leader {
			continue
		}
		if _, err := n.Export(ctx); !errors.Is(err, ErrNotLeader) {
			t.Errorf("follower export error = %v, want ErrNotLeader", err)
		}
	}
	d, err := leader.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Records) != 1 || d.Records[0].Name != "a" || d.Revision != 1 {
		t.Fatalf("leader export = %+v", d)
	}
}
