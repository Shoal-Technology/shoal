package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"example.com/shoal/api/v1alpha1"
)

// TestRaftStoreApplyGet runs the server on the raft store (single node) and
// checks that a create and a get work through the API.
func TestRaftStoreApplyGet(t *testing.T) {
	dir := t.TempDir()
	ts := startServer(t, dir, &clock{}, func(c *Config) {
		c.StoreBackend = StoreRaft
		c.RaftBindAddr = "127.0.0.1:0"
		c.RaftNodeID = "test-node"
	})
	if _, err := os.Stat(filepath.Join(dir, "raft", "raft.db")); err != nil {
		t.Fatalf("raft state not on disk: %v", err)
	}
	admin := ts.admin(t)
	ctx := context.Background()

	resp, err := admin.Apply(ctx, &v1alpha1.ApplyRequest{Objects: decode(t, webYAML)})
	if err != nil {
		t.Fatal(err)
	}
	if a := resp.Results[0].Action; a != "created" {
		t.Fatalf("apply on raft store: %s", a)
	}
	obj, err := admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	w := obj.GetWorkload()
	if w.GetSpec().GetReplicas() != 3 || w.GetMetadata().GetUid() == "" {
		t.Fatalf("stored workload = %v", w)
	}
}
