package server

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"

	"example.com/shoal/api/v1alpha1"
)

func raftOpts(nodeID, joinAddr string) func(*Config) {
	return func(c *Config) {
		c.StoreBackend = StoreRaft
		c.RaftBindAddr = "127.0.0.1:0"
		c.RaftNodeID = nodeID
		c.RaftJoinAddr = joinAddr
	}
}

// copyCluster copies the cluster CA and the secret encryption key from one
// server's data dir to another, as an operator does before a node joins.
func copyCluster(t *testing.T, from, to string) {
	t.Helper()
	copyFile := func(src, dst string) {
		t.Helper()
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err := filepath.WalkDir(filepath.Join(from, "pki"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		copyFile(p, dst)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	copyFile(filepath.Join(from, "secret.key"), filepath.Join(to, "secret.key"))
}

// TestRaftJoinCluster starts node 1, which bootstraps, and nodes 2 and 3,
// which join it. Every node sees the leader record. A follower redirects a
// join to the leader, and a restarted node re-joins without error.
func TestRaftJoinCluster(t *testing.T) {
	ctx := context.Background()
	dir1 := t.TempDir()
	n1 := startServer(t, dir1, &clock{}, raftOpts("node-1", ""))

	dir2 := t.TempDir()
	copyCluster(t, dir1, dir2)
	n2 := startServer(t, dir2, &clock{}, raftOpts("node-2", n1.addr))

	dir3 := t.TempDir()
	copyCluster(t, dir1, dir3)
	n3 := startServer(t, dir3, &clock{}, raftOpts("node-3", n1.addr))

	want := LeaderInfo{APIAddress: n1.addr, RaftNodeID: "node-1", RaftAddress: string(n1.raft.RaftAddr())}
	for _, n := range []struct {
		name string
		ts   *testServer
	}{{"node-1", n1}, {"node-2", n2}, {"node-3", n3}} {
		eventually(t, "leader record on "+n.name, func() bool {
			info, err := n.ts.ClusterLeader(ctx)
			return err == nil && info == want
		})
	}

	// A follower does not add voters. It names the leader's API address so
	// the caller can retry there.
	_, err := n2.admin(t).JoinCluster(ctx, &v1alpha1.JoinClusterRequest{NodeId: "node-x", RaftAddr: "127.0.0.1:1"})
	wantCode(t, err, codes.FailedPrecondition, n1.addr)

	// Joining again with the same ID and address is a no-op on the leader.
	if _, err := n1.admin(t).JoinCluster(ctx, &v1alpha1.JoinClusterRequest{NodeId: "node-2", RaftAddr: string(n2.raft.RaftAddr())}); err != nil {
		t.Fatalf("repeat join: %v", err)
	}

	// Restart node 2 on the same data dir. It gets a new raft port and
	// re-joins without error.
	n2.stop()
	n2b := startServer(t, dir2, &clock{}, raftOpts("node-2", n1.addr))
	eventually(t, "leader record after restart", func() bool {
		info, err := n2b.ClusterLeader(ctx)
		return err == nil && info == want
	})
}
