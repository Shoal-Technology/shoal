package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/kaiserern/shoal/api/v1alpha1"
	"github.com/kaiserern/shoal/internal/security"
)

// membersOf counts the voters and leaders in a cluster status.
func membersOf(resp *v1alpha1.ClusterStatusResponse) (members, voters, leaders int) {
	for _, m := range resp.GetMembers() {
		members++
		if m.GetVoter() {
			voters++
		}
		if m.GetLeader() {
			leaders++
		}
	}
	return members, voters, leaders
}

// writeBundle fetches a join bundle from leader and writes it the way the CLI
// does: mode 0600, in a temp dir. It returns the file path.
func writeBundle(t *testing.T, leader *testServer) string {
	t.Helper()
	resp, err := leader.admin(t).JoinBundle(context.Background(), &v1alpha1.JoinBundleRequest{})
	if err != nil {
		t.Fatalf("JoinBundle: %v", err)
	}
	if resp.GetCaHash() != leader.CAHash() {
		t.Fatalf("bundle CA hash %q, want %q", resp.GetCaHash(), leader.CAHash())
	}
	path := filepath.Join(t.TempDir(), "join.bundle")
	if err := os.WriteFile(path, resp.GetBundle(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestClusterStatus starts three voters and checks that every node reports
// three voters and exactly one leader. It then removes node-3 through a
// follower (the write is forwarded to the leader) and checks that the
// membership drops to two.
func TestClusterStatus(t *testing.T) {
	ctx := context.Background()
	dir1 := t.TempDir()
	n1 := startServer(t, dir1, &clock{}, raftOpts("node-1", ""))
	dir2 := t.TempDir()
	copyCluster(t, dir1, dir2)
	n2 := startServer(t, dir2, &clock{}, raftOpts("node-2", n1.addr))
	dir3 := t.TempDir()
	copyCluster(t, dir1, dir3)
	n3 := startServer(t, dir3, &clock{}, raftOpts("node-3", n1.addr))

	for _, n := range []*testServer{n1, n2, n3} {
		admin := n.admin(t)
		eventually(t, "3 voters and one leader as seen by "+n.addr, func() bool {
			resp, err := admin.ClusterStatus(ctx, &v1alpha1.ClusterStatusRequest{})
			if err != nil {
				return false
			}
			_, voters, leaders := membersOf(resp)
			return voters == 3 && leaders == 1 && resp.GetLeaderApiAddress() == n1.addr
		})
	}

	// The answer names the node that gave it, with its indexes.
	resp, err := n2.admin(t).ClusterStatus(ctx, &v1alpha1.ClusterStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetNodeId() != "node-2" || resp.GetCommitIndex() == 0 || resp.GetAppliedIndex() == 0 {
		t.Fatalf("status from node-2: node %q commit %d applied %d", resp.GetNodeId(), resp.GetCommitIndex(), resp.GetAppliedIndex())
	}

	// Remove node-3 through follower node-2. The retry on the leader does
	// the work.
	tc := adminTLS(t, dir1)
	if _, err := forwardingClient(t, n2.addr, tc).RemoveServer(ctx, &v1alpha1.RemoveServerRequest{NodeId: "node-3"}); err != nil {
		t.Fatalf("remove node-3 via follower: %v", err)
	}
	n3.stop()
	for _, n := range []*testServer{n1, n2} {
		admin := n.admin(t)
		eventually(t, "2 members after removing node-3, as seen by "+n.addr, func() bool {
			resp, err := admin.ClusterStatus(ctx, &v1alpha1.ClusterStatusRequest{})
			if err != nil {
				return false
			}
			members, voters, leaders := membersOf(resp)
			return members == 2 && voters == 2 && leaders == 1
		})
	}
}

// TestClusterRefusesLastVoter removes the only voter, which must fail, and
// an unknown ID, which must report NotFound.
func TestClusterRefusesLastVoter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	n := startServer(t, dir, &clock{}, raftOpts("node-1", ""))
	admin := n.admin(t)

	_, err := admin.RemoveServer(ctx, &v1alpha1.RemoveServerRequest{NodeId: "node-1"})
	wantCode(t, err, codes.FailedPrecondition, "last voter")

	resp, err := admin.ClusterStatus(ctx, &v1alpha1.ClusterStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if members, voters, leaders := membersOf(resp); members != 1 || voters != 1 || leaders != 1 {
		t.Fatalf("after refused removal: members %d voters %d leaders %d, want 1 1 1", members, voters, leaders)
	}

	_, err = admin.RemoveServer(ctx, &v1alpha1.RemoveServerRequest{NodeId: "no-such"})
	wantCode(t, err, codes.NotFound, "no-such")
}

// TestClusterRemoveLeader removes the leader itself from a three-voter
// cluster. The two survivors must elect a new leader that is not node-1, and
// the cluster must report two voters.
func TestClusterRemoveLeader(t *testing.T) {
	ctx := context.Background()
	dir1 := t.TempDir()
	n1 := startServer(t, dir1, &clock{}, raftOpts("node-1", ""))
	dir2 := t.TempDir()
	copyCluster(t, dir1, dir2)
	n2 := startServer(t, dir2, &clock{}, raftOpts("node-2", n1.addr))
	dir3 := t.TempDir()
	copyCluster(t, dir1, dir3)
	n3 := startServer(t, dir3, &clock{}, raftOpts("node-3", n1.addr))

	if _, err := n1.admin(t).RemoveServer(ctx, &v1alpha1.RemoveServerRequest{NodeId: "node-1"}); err != nil {
		t.Fatalf("remove leader: %v", err)
	}
	n1.stop()

	for _, n := range []*testServer{n2, n3} {
		admin := n.admin(t)
		eventually(t, "new leader among two voters, as seen by "+n.addr, func() bool {
			resp, err := admin.ClusterStatus(ctx, &v1alpha1.ClusterStatusRequest{})
			if err != nil {
				return false
			}
			members, voters, leaders := membersOf(resp)
			if members != 2 || voters != 2 || leaders != 1 {
				return false
			}
			return resp.GetLeaderApiAddress() == n2.addr || resp.GetLeaderApiAddress() == n3.addr
		})
	}
}

// TestClusterJoinBundle joins node-2 with a join bundle instead of copying
// pki/ and secret.key by hand. It then checks that the installed files match
// the leader's, that the join succeeded, and that installing the same bundle
// again changes nothing.
func TestClusterJoinBundle(t *testing.T) {
	ctx := context.Background()
	dir1 := t.TempDir()
	n1 := startServer(t, dir1, &clock{}, raftOpts("node-1", ""))
	bundlePath := writeBundle(t, n1)

	dir2 := t.TempDir()
	n2 := startServer(t, dir2, &clock{}, raftOpts("node-2", n1.addr), func(c *Config) {
		c.RaftJoinBundle = bundlePath
	})

	for _, rel := range []string{filepath.Join("pki", "ca.crt"), filepath.Join("pki", "ca.key"), "secret.key"} {
		want, err := os.ReadFile(filepath.Join(dir1, rel))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dir2, rel))
		if err != nil {
			t.Fatalf("bundle did not install %s: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s differs from the leader's copy", rel)
		}
	}

	want := LeaderInfo{APIAddress: n1.addr, RaftNodeID: "node-1", RaftAddress: string(n1.raft.RaftAddr())}
	eventually(t, "node-2 sees the leader record", func() bool {
		info, err := n2.ClusterLeader(ctx)
		return err == nil && info == want
	})
	admin := n1.admin(t)
	eventually(t, "node-1 lists node-2 as a voter", func() bool {
		resp, err := admin.ClusterStatus(ctx, &v1alpha1.ClusterStatusRequest{})
		if err != nil {
			return false
		}
		members, voters, leaders := membersOf(resp)
		return members == 2 && voters == 2 && leaders == 1
	})

	// Installing the same bundle over the same PKI is a no-op.
	installed, err := installJoinBundle(bundlePath, dir2, filepath.Join(dir2, "secret.key"))
	if err != nil || installed {
		t.Fatalf("reinstall: installed %v, err %v; want false, nil", installed, err)
	}
}

// TestClusterJoinBundleConflictingPKI checks that a bundle is refused for a
// data dir that already holds a different CA, and that nothing in that dir
// changes.
func TestClusterJoinBundleConflictingPKI(t *testing.T) {
	dir1 := t.TempDir()
	n1 := startServer(t, dir1, &clock{}, raftOpts("node-1", ""))
	bundlePath := writeBundle(t, n1)

	dirOther := t.TempDir()
	if _, _, err := security.LoadOrCreateCA(filepath.Join(dirOther, "pki"), time.Now()); err != nil {
		t.Fatal(err)
	}
	caBefore, err := os.ReadFile(filepath.Join(dirOther, "pki", "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = New(context.Background(), Config{
		DataDir: dirOther, AdvertiseAddress: "127.0.0.1:1", Log: discard(),
		StoreBackend: StoreRaft, RaftBindAddr: "127.0.0.1:0", RaftNodeID: "node-x",
		RaftJoinBundle: bundlePath,
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to install join bundle") {
		t.Fatalf("New with a conflicting bundle: err = %v, want a refusal", err)
	}
	caAfter, err := os.ReadFile(filepath.Join(dirOther, "pki", "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(caBefore, caAfter) {
		t.Fatal("the existing CA was overwritten")
	}
	if _, err := os.Stat(filepath.Join(dirOther, "secret.key")); !os.IsNotExist(err) {
		t.Fatalf("secret.key was written despite the refusal (stat err %v)", err)
	}
}
