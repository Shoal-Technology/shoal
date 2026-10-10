package server

import (
	"context"
	"crypto/tls"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/security"
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

// adminTLS returns the admin client TLS config from a server's data dir.
func adminTLS(t *testing.T, dir string) *tls.Config {
	t.Helper()
	cfg, err := security.LoadClientConfig(AdminConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	p, err := cfg.Parse()
	if err != nil {
		t.Fatal(err)
	}
	return p.TLS()
}

// forwardingClient dials addr the way the CLI does: writes that reach a
// follower are retried once on the leader.
func forwardingClient(t *testing.T, addr string, tc *tls.Config) v1alpha1.ControlClient {
	t.Helper()
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tc)),
		grpc.WithUnaryInterceptor(LeaderRetry(tc)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return v1alpha1.NewControlClient(conn)
}

// workloadYAML is webYAML under another name, so each write creates a record.
func workloadYAML(name string) string {
	return strings.ReplaceAll(webYAML, "name: web }", "name: "+name+" }")
}

// TestRaftWriteForwarding runs the three-node cluster through a write on a
// follower, a leader failover and more writes. It checks that the leader
// tasks run on one node at a time (ADR 0014, pass 2d).
func TestRaftWriteForwarding(t *testing.T) {
	ctx := context.Background()
	gate := newLeaderGate()
	gated := func(nodeID, join string) func(*Config) {
		return func(c *Config) {
			raftOpts(nodeID, join)(c)
			c.leaderGate = gate
		}
	}

	dir1 := t.TempDir()
	n1 := startServer(t, dir1, &clock{}, gated("node-1", ""))
	dir2 := t.TempDir()
	copyCluster(t, dir1, dir2)
	n2 := startServer(t, dir2, &clock{}, gated("node-2", n1.addr))
	dir3 := t.TempDir()
	copyCluster(t, dir1, dir3)
	n3 := startServer(t, dir3, &clock{}, gated("node-3", n1.addr))
	tc := adminTLS(t, dir1)

	eventually(t, "leader record on every node", func() bool {
		for _, n := range []*testServer{n1, n2, n3} {
			info, err := n.ClusterLeader(ctx)
			if err != nil || info.APIAddress != n1.addr {
				return false
			}
		}
		return true
	})
	eventually(t, "leader tasks on node-1", func() bool {
		holders, _ := gate.snapshot()
		return len(holders) == 1 && holders[0] == "node-1"
	})

	// (a) A plain client gets the leader's address from a follower. The
	// forwarding client gets the write done.
	_, err := n2.admin(t).Apply(ctx, &v1alpha1.ApplyRequest{Objects: decode(t, workloadYAML("before"))})
	wantCode(t, err, codes.FailedPrecondition, n1.addr)
	if _, err := forwardingClient(t, n2.addr, tc).Apply(ctx, &v1alpha1.ApplyRequest{Objects: decode(t, workloadYAML("before"))}); err != nil {
		t.Fatalf("write via follower: %v", err)
	}
	admin1 := n1.admin(t)
	eventually(t, "forwarded write visible on the leader", func() bool {
		_, err := admin1.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: "before"})
		return err == nil
	})

	// (b) The leader stops. A survivor takes over and rewrites the leader
	// record. A write through the other survivor still succeeds.
	n1.stop()
	var leader, follower *testServer
	eventually(t, "new leader record on the survivors", func() bool {
		i2, err2 := n2.ClusterLeader(ctx)
		i3, err3 := n3.ClusterLeader(ctx)
		if err2 != nil || err3 != nil || i2 != i3 || i2.RaftNodeID == "node-1" {
			return false
		}
		switch i2.APIAddress {
		case n2.addr:
			leader, follower = n2, n3
		case n3.addr:
			leader, follower = n3, n2
		default:
			return false
		}
		return true
	})
	if _, err := forwardingClient(t, follower.addr, tc).Apply(ctx, &v1alpha1.ApplyRequest{Objects: decode(t, workloadYAML("after"))}); err != nil {
		t.Fatalf("write via surviving follower after failover: %v", err)
	}
	for _, n := range []*testServer{leader, follower} {
		client := n.admin(t)
		eventually(t, "records from before and after failover readable on "+n.addr, func() bool {
			for _, name := range []string{"before", "after"} {
				if _, err := client.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: name}); err != nil {
					return false
				}
			}
			return true
		})
	}

	// (c) The leader tasks run on the new leader only, and never on two
	// nodes at once.
	wantID := leader.cfg.RaftNodeID
	eventually(t, "leader tasks on the new leader only", func() bool {
		holders, _ := gate.snapshot()
		return len(holders) == 1 && holders[0] == wantID
	})
	if _, violations := gate.snapshot(); violations != 0 {
		t.Fatalf("leader tasks ran on two nodes at once %d times", violations)
	}
}
