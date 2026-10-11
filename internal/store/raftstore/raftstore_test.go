package raftstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/kaiserern/shoal/internal/store"
	"github.com/kaiserern/shoal/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s, err := New(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// newCluster starts n in-process nodes, fully connected, all bootstrapped
// with the same voter set.
func newCluster(t *testing.T, n int) []*Store {
	t.Helper()
	var (
		addrs []raft.ServerAddress
		trans []*raft.InmemTransport
		boot  []raft.Server
	)
	for i := range n {
		id := raft.ServerID(fmt.Sprintf("node-%d", i+1))
		addr, tr := raft.NewInmemTransport(raft.ServerAddress(id))
		addrs = append(addrs, addr)
		trans = append(trans, tr)
		boot = append(boot, raft.Server{Suffrage: raft.Voter, ID: id, Address: addr})
	}
	for i := range n {
		for j := range n {
			if i != j {
				trans[i].Connect(addrs[j], trans[j])
			}
		}
	}
	var nodes []*Store
	for i := range n {
		s, err := NewNode(Config{
			NodeID:    boot[i].ID,
			Address:   addrs[i],
			Transport: trans[i],
			Logs:      raft.NewInmemStore(),
			Stable:    raft.NewInmemStore(),
			Snapshots: raft.NewInmemSnapshotStore(),
			Bootstrap: boot,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		nodes = append(nodes, s)
	}
	return nodes
}

// leaderOf returns the single live node in nodes that is leader, or nil.
func leaderOf(nodes []*Store) *Store {
	var found *Store
	for _, s := range nodes {
		if s.raft.State() == raft.Leader {
			if found != nil {
				return nil
			}
			found = s
		}
	}
	return found
}

func waitLeader(t *testing.T, nodes []*Store) *Store {
	t.Helper()
	var l *Store
	eventually(t, "a leader", func() bool { l = leaderOf(nodes); return l != nil })
	return l
}

func TestCluster(t *testing.T) {
	ctx := context.Background()
	nodes := newCluster(t, 3)

	// a. Elect a leader.
	leader := waitLeader(t, nodes)
	var followers []*Store
	for _, s := range nodes {
		if s != leader {
			followers = append(followers, s)
		}
	}

	// b. A write on the leader reaches every follower, and a Watch on each
	// follower sees it.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watches := make([]<-chan store.Event, len(followers))
	for i, f := range followers {
		watches[i] = f.Watch(wctx, "W")
	}
	if _, err := leader.Create(ctx, store.Record{Kind: "W", Name: "a", Spec: []byte("v1")}); err != nil {
		t.Fatal(err)
	}
	for i, f := range followers {
		eventually(t, "follower to see W/a", func() bool {
			r, err := f.Get(ctx, "W", "a")
			return err == nil && string(r.Spec) == "v1"
		})
		select {
		case e := <-watches[i]:
			if e.Kind != "W" || e.Name != "a" || e.Deleted {
				t.Fatalf("follower %d event = %+v", i, e)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("follower %d: no watch event", i)
		}
	}

	// c. Writes on a follower are refused.
	if _, err := followers[0].Create(ctx, store.Record{Kind: "W", Name: "x"}); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("follower Create: err = %v, want ErrNotLeader", err)
	}
	if err := followers[0].AddVoter(ctx, "node-9", "node-9"); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("follower AddVoter: err = %v, want ErrNotLeader", err)
	}
	if addr, id := followers[0].Leader(); addr == "" || id == "" {
		t.Fatalf("follower Leader() = %q, %q, want the leader", addr, id)
	}

	// d. Stop the leader. The two survivors elect a new one, keep the old
	// records, and accept writes.
	if err := leader.Close(); err != nil {
		t.Fatal(err)
	}
	newLeader := waitLeader(t, followers)
	if r, err := newLeader.Get(ctx, "W", "a"); err != nil || string(r.Spec) != "v1" {
		t.Fatalf("after failover Get = %+v, %v", r, err)
	}
	if _, err := newLeader.Create(ctx, store.Record{Kind: "W", Name: "b", Spec: []byte("v2")}); err != nil {
		t.Fatalf("Create after failover: %v", err)
	}
	other := followers[0]
	if other == newLeader {
		other = followers[1]
	}
	eventually(t, "surviving follower to see W/b", func() bool {
		r, err := other.Get(ctx, "W", "b")
		return err == nil && string(r.Spec) == "v2"
	})
}

// bufSink is an in-memory raft.SnapshotSink.
type bufSink struct{ bytes.Buffer }

func (*bufSink) ID() string    { return "test" }
func (*bufSink) Cancel() error { return nil }
func (*bufSink) Close() error  { return nil }

func TestSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	a, err := s.Create(ctx, store.Record{Kind: "W", Name: "a", Spec: []byte("v1")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateStatus(ctx, "W", "a", []byte("up")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, store.Record{Kind: "N", Name: "b", Spec: []byte("x")}); err != nil {
		t.Fatal(err)
	}

	snap, err := s.fsm.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var sink bufSink
	if err := snap.Persist(&sink); err != nil {
		t.Fatal(err)
	}
	snap.Release()

	restored := newFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatal(err)
	}
	r := &Store{fsm: restored, mem: restored.mem}

	got, err := r.Get(ctx, "W", "a")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := s.Get(ctx, "W", "a")
	if got.UID != a.UID || got.UID != want.UID || got.Generation != want.Generation ||
		got.Revision != want.Revision || !got.CreatedAt.Equal(want.CreatedAt) ||
		string(got.Spec) != "v1" || string(got.Status) != "up" {
		t.Fatalf("restored W/a = %+v, want %+v", got, want)
	}
	if _, err := r.Get(ctx, "N", "b"); err != nil {
		t.Fatalf("restored N/b: %v", err)
	}
	if rev := restored.mem.Revision(); rev != s.mem.Revision() {
		t.Fatalf("restored revision = %d, want %d", rev, s.mem.Revision())
	}
}
