// Package raftstore is a store.Store replicated through Raft (ADR 0002).
//
// Writes are JSON commands applied by the FSM to a memory.Store. Anything
// random or time-based (UID, timestamps) is computed before the command is
// proposed and carried in it, so every replica applies identical state.
// Reads and Watch are served from the local memory.Store.
//
// Open gives a durable node; the server runs one node so far. Multi-node
// join and write forwarding are not wired yet.
package raftstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/hashicorp/raft"

	"github.com/kaiserern/shoal/internal/store"
	"github.com/kaiserern/shoal/internal/store/memory"
)

// ErrNotLeader means this node cannot accept writes. Writes are not
// forwarded yet.
var ErrNotLeader = errors.New("not the raft leader")

const (
	applyTimeout  = 10 * time.Second
	leaderTimeout = 10 * time.Second
	nodeID        = "node-1"
)

const (
	opCreate = "create"
	opUpdate = "update"
	opStatus = "status"
	opLabels = "labels"
	opDelete = "delete"
	opImport = "import"
)

// command is one replicated write. Now and UID are chosen by the proposer.
type command struct {
	Op         string            `json:"op"`
	Now        time.Time         `json:"now"`
	UID        string            `json:"uid,omitempty"`
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	Spec       []byte            `json:"spec,omitempty"`
	Status     []byte            `json:"status,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Generation int64             `json:"generation,omitempty"`
	// Dump is set on an import: the whole state to load into an empty FSM.
	Dump *store.Dump `json:"dump,omitempty"`
}

// result is the FSM's response to a command.
type result struct {
	rec store.Record
	err error
}

// Store is a Raft-backed store.Store.
type Store struct {
	closers []func() error
	raft    *raft.Raft
	fsm     *fsm
	mem     *memory.Store
	addr    raft.ServerAddress
}

// RaftAddr returns the address this node listens on for Raft. It is the
// bound address, so a configured port 0 shows the port actually picked.
func (s *Store) RaftAddr() raft.ServerAddress { return s.addr }

var _ store.Store = (*Store)(nil)

// Config describes one Raft node. Transport and the stores are injected so
// tests can connect nodes in-process and later swap in durable storage.
type Config struct {
	// NodeID identifies this node in the cluster.
	NodeID raft.ServerID
	// Address is this node's address on Transport.
	Address raft.ServerAddress
	// Transport carries Raft RPCs between nodes.
	Transport raft.Transport
	// Logs, Stable and Snapshots hold Raft state.
	Logs      raft.LogStore
	Stable    raft.StableStore
	Snapshots raft.SnapshotStore
	// Raft overrides the Raft timing and tuning. Nil uses fast defaults
	// suited to in-process clusters. NodeID is always applied to it.
	Raft *raft.Config
	// Bootstrap is the initial voter set. Give the same set to every node
	// that starts the cluster. Empty means the node joins an existing
	// cluster and does not bootstrap.
	Bootstrap []raft.Server
}

// New starts a single-node cluster with in-memory Raft storage, bootstraps
// it, and waits until this node is leader.
func New(ctx context.Context) (*Store, error) {
	addr, trans := raft.NewInmemTransport(raft.ServerAddress(nodeID))
	s, err := NewNode(Config{
		NodeID:    nodeID,
		Address:   addr,
		Transport: trans,
		Logs:      raft.NewInmemStore(),
		Stable:    raft.NewInmemStore(),
		Snapshots: raft.NewInmemSnapshotStore(),
		Bootstrap: []raft.Server{{Suffrage: raft.Voter, ID: nodeID, Address: addr}},
	})
	if err != nil {
		return nil, err
	}
	if err := s.waitLeader(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// NewNode starts a node from cfg without waiting for leadership. Use
// Leader or wait on the Raft state to observe elections.
func NewNode(cfg Config) (*Store, error) {
	f := newFSM()

	conf := raft.DefaultConfig()
	if cfg.Raft != nil {
		conf = cfg.Raft
	} else {
		// In-process clusters need no network round trips, so use short
		// timers to keep startup and elections fast.
		conf.HeartbeatTimeout = 100 * time.Millisecond
		conf.ElectionTimeout = 100 * time.Millisecond
		conf.LeaderLeaseTimeout = 50 * time.Millisecond
		conf.CommitTimeout = 5 * time.Millisecond
	}
	conf.LocalID = cfg.NodeID
	conf.LogOutput = io.Discard

	r, err := raft.NewRaft(conf, f, cfg.Logs, cfg.Stable, cfg.Snapshots, cfg.Transport)
	if err != nil {
		return nil, fmt.Errorf("start raft: %w", err)
	}
	if len(cfg.Bootstrap) > 0 {
		err := r.BootstrapCluster(raft.Configuration{Servers: cfg.Bootstrap}).Error()
		if err != nil && !errors.Is(err, raft.ErrCantBootstrap) {
			_ = r.Shutdown().Error()
			return nil, fmt.Errorf("bootstrap raft: %w", err)
		}
	}
	return &Store{raft: r, fsm: f, mem: f.mem, addr: cfg.Address}, nil
}

// Leader returns the address and ID of the current leader as this node
// knows it. Both are empty if there is none.
func (s *Store) Leader() (raft.ServerAddress, raft.ServerID) {
	return s.raft.LeaderWithID()
}

// IsLeader reports whether this node is the leader right now.
func (s *Store) IsLeader() bool {
	return s.raft.State() == raft.Leader
}

// LeaderCh delivers each change in this node's leadership: true when it
// becomes leader, false when it stops. Only one goroutine should receive
// from it. Raft keeps the latest value if the receiver is slow, so a
// receiver that starts late still sees the current state.
func (s *Store) LeaderCh() <-chan bool {
	return s.raft.LeaderCh()
}

// AddVoter adds a voting member through the leader. It returns ErrNotLeader
// on a follower.
func (s *Store) AddVoter(ctx context.Context, id raft.ServerID, addr raft.ServerAddress) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := s.raft.AddVoter(id, addr, 0, applyTimeout).Error()
	if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
		return ErrNotLeader
	}
	if err != nil {
		return fmt.Errorf("add voter %s: %w", id, err)
	}
	return nil
}

// RemoveServer removes a member through the leader. It returns ErrNotLeader
// on a follower. Removing the leader itself is allowed: it steps down after
// the change commits.
func (s *Store) RemoveServer(ctx context.Context, id raft.ServerID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := s.raft.RemoveServer(id, 0, applyTimeout).Error()
	if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
		return ErrNotLeader
	}
	if err != nil {
		return fmt.Errorf("remove server %s: %w", id, err)
	}
	return nil
}

// Configuration returns the membership this node currently knows. A
// follower may lag the leader by a few entries.
func (s *Store) Configuration() (raft.Configuration, error) {
	f := s.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return raft.Configuration{}, fmt.Errorf("read raft configuration: %w", err)
	}
	return f.Configuration(), nil
}

// AppliedIndex returns the highest log index this node has applied to its
// state.
func (s *Store) AppliedIndex() uint64 { return s.raft.AppliedIndex() }

func (s *Store) waitLeader(ctx context.Context) error {
	timeout := time.NewTimer(leaderTimeout)
	defer timeout.Stop()
	for {
		if s.raft.State() == raft.Leader {
			return nil
		}
		select {
		case <-s.raft.LeaderCh():
		case <-timeout.C:
			return fmt.Errorf("no leader after %s: %w", leaderTimeout, ErrNotLeader)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// apply proposes c and returns the FSM's result.
func (s *Store) apply(ctx context.Context, c command) (store.Record, error) {
	if err := ctx.Err(); err != nil {
		return store.Record{}, err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return store.Record{}, fmt.Errorf("encode command: %w", err)
	}
	f := s.raft.Apply(data, applyTimeout)
	if err := f.Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
			return store.Record{}, ErrNotLeader
		}
		return store.Record{}, fmt.Errorf("raft apply: %w", err)
	}
	res, ok := f.Response().(*result)
	if !ok {
		return store.Record{}, fmt.Errorf("raft apply: unexpected response %T", f.Response())
	}
	return res.rec, res.err
}

func (s *Store) Get(ctx context.Context, kind, name string) (store.Record, error) {
	return s.mem.Get(ctx, kind, name)
}

func (s *Store) List(ctx context.Context, kind string) ([]store.Record, error) {
	return s.mem.List(ctx, kind)
}

func (s *Store) Create(ctx context.Context, rec store.Record) (store.Record, error) {
	return s.apply(ctx, command{
		Op: opCreate, Now: time.Now().UTC(), UID: store.NewUID(),
		Kind: rec.Kind, Name: rec.Name, Spec: rec.Spec, Status: rec.Status, Labels: rec.Labels,
	})
}

func (s *Store) SetLabels(ctx context.Context, kind, name string, labels map[string]string, generation int64) (store.Record, error) {
	return s.apply(ctx, command{
		Op: opLabels, Now: time.Now().UTC(), Kind: kind, Name: name,
		Labels: labels, Generation: generation,
	})
}

func (s *Store) Update(ctx context.Context, kind, name string, spec []byte, generation int64) (store.Record, error) {
	return s.apply(ctx, command{
		Op: opUpdate, Now: time.Now().UTC(), Kind: kind, Name: name,
		Spec: spec, Generation: generation,
	})
}

func (s *Store) UpdateStatus(ctx context.Context, kind, name string, status []byte) (store.Record, error) {
	return s.apply(ctx, command{
		Op: opStatus, Now: time.Now().UTC(), Kind: kind, Name: name, Status: status,
	})
}

func (s *Store) Delete(ctx context.Context, kind, name string, generation int64) error {
	_, err := s.apply(ctx, command{
		Op: opDelete, Now: time.Now().UTC(), Kind: kind, Name: name, Generation: generation,
	})
	return err
}

func (s *Store) Watch(ctx context.Context, kinds ...string) <-chan store.Event {
	return s.mem.Watch(ctx, kinds...)
}

// WaitReady must be called before the node serves reads or runs
// controllers after a start or restart. Leadership alone does not mean the
// FSM has applied the log that was committed before the restart. WaitReady
// waits until the node has a known leader, then:
//   - on the leader, runs a Raft barrier, which applies everything committed
//     so far;
//   - on a follower, waits until the local applied index reaches the commit
//     index it learned from the leader.
//
// Open does not call WaitReady, so a cluster can form while its nodes are
// still catching up. Writes are not affected: they are proposed through the
// leader, which is already current when it accepts them.
func (s *Store) WaitReady(ctx context.Context) error {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		if s.raft.State() == raft.Leader {
			err := s.raft.Barrier(applyTimeout).Error()
			if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
				return ErrNotLeader
			}
			return err
		}
		if addr, _ := s.raft.LeaderWithID(); addr != "" {
			commit, err := strconv.ParseUint(s.raft.Stats()["commit_index"], 10, 64)
			if err != nil {
				return fmt.Errorf("read commit index: %w", err)
			}
			// A commit index of 0 means the leader's first AppendEntries has not
			// set it yet, so the node is not caught up even if it knows a leader.
			if commit > 0 && s.raft.AppliedIndex() >= commit {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// CommitIndex returns the highest log index this node knows to be committed.
func (s *Store) CommitIndex() (uint64, error) {
	return strconv.ParseUint(s.raft.Stats()["commit_index"], 10, 64)
}

// WaitApplied waits until this node has applied log index idx, or ctx ends.
func (s *Store) WaitApplied(ctx context.Context, idx uint64) error {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		if s.raft.AppliedIndex() >= idx {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Close shuts down Raft and the local state.
func (s *Store) Close() error {
	err := s.raft.Shutdown().Error()
	for _, c := range s.closers {
		_ = c()
	}
	_ = s.mem.Close()
	return err
}

// fsm applies committed commands to the memory.Store. Raft calls it from a
// single goroutine, so now needs no locking.
type fsm struct {
	now time.Time
	mem *memory.Store
}

func newFSM() *fsm {
	f := &fsm{}
	f.mem = memory.New(func() time.Time { return f.now })
	return f
}

var _ raft.FSM = (*fsm)(nil)

func (f *fsm) Apply(l *raft.Log) any {
	var c command
	if err := json.Unmarshal(l.Data, &c); err != nil {
		return &result{err: fmt.Errorf("decode command: %w", err)}
	}
	f.now = c.Now
	ctx := context.Background()
	switch c.Op {
	case opCreate:
		rec, err := f.mem.Create(ctx, store.Record{Kind: c.Kind, Name: c.Name, UID: c.UID, Spec: c.Spec, Status: c.Status, Labels: c.Labels})
		return &result{rec: rec, err: err}
	case opLabels:
		rec, err := f.mem.SetLabels(ctx, c.Kind, c.Name, c.Labels, c.Generation)
		return &result{rec: rec, err: err}
	case opUpdate:
		rec, err := f.mem.Update(ctx, c.Kind, c.Name, c.Spec, c.Generation)
		return &result{rec: rec, err: err}
	case opStatus:
		rec, err := f.mem.UpdateStatus(ctx, c.Kind, c.Name, c.Status)
		return &result{rec: rec, err: err}
	case opDelete:
		return &result{err: f.mem.Delete(ctx, c.Kind, c.Name, c.Generation)}
	case opImport:
		if c.Dump == nil {
			return &result{err: errors.New("import without data")}
		}
		return &result{err: f.mem.Import(ctx, *c.Dump)}
	default:
		return &result{err: fmt.Errorf("unknown command %q", c.Op)}
	}
}

// snapshotFile is the serialized FSM state.
type snapshotFile struct {
	Revision int64          `json:"revision"`
	Records  []store.Record `json:"records"`
}

type fsmSnapshot struct {
	data snapshotFile
}

func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	rev, recs := f.mem.Dump()
	return &fsmSnapshot{data: snapshotFile{Revision: rev, Records: recs}}, nil
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if err := json.NewEncoder(sink).Encode(s.data); err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("encode snapshot: %w", err)
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}

// Restore replaces the state from a snapshot. It publishes no Watch events for
// the restored records: a follower that restores from a snapshot misses the
// events for those writes, and watchers rely on their periodic resync.
func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	var data snapshotFile
	if err := json.NewDecoder(rc).Decode(&data); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	f.mem.Load(data.Revision, data.Records)
	return nil
}
