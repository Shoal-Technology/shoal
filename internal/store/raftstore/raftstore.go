// Package raftstore is a store.Store replicated through Raft (ADR 0002).
//
// Writes are JSON commands applied by the FSM to a memory.Store. Anything
// random or time-based (UID, timestamps) is computed before the command is
// proposed and carried in it, so every replica applies identical state.
// Reads and Watch are served from the local memory.Store.
//
// Only a single-node, in-memory cluster is supported so far.
package raftstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hashicorp/raft"

	"example.com/shoal/internal/store"
	"example.com/shoal/internal/store/memory"
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
	opDelete = "delete"
)

// command is one replicated write. Now and UID are chosen by the proposer.
type command struct {
	Op         string    `json:"op"`
	Now        time.Time `json:"now"`
	UID        string    `json:"uid,omitempty"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Spec       []byte    `json:"spec,omitempty"`
	Status     []byte    `json:"status,omitempty"`
	Generation int64     `json:"generation,omitempty"`
}

// result is the FSM's response to a command.
type result struct {
	rec store.Record
	err error
}

// Store is a single-node Raft-backed store.Store.
type Store struct {
	raft *raft.Raft
	fsm  *fsm
	mem  *memory.Store
}

var _ store.Store = (*Store)(nil)

// New starts a single-node cluster with in-memory Raft storage, bootstraps
// it, and waits until this node is leader.
func New(ctx context.Context) (*Store, error) {
	f := newFSM()

	conf := raft.DefaultConfig()
	conf.LocalID = nodeID
	// A one-node cluster needs no network round trips, so use short timers
	// to keep startup fast.
	conf.HeartbeatTimeout = 100 * time.Millisecond
	conf.ElectionTimeout = 100 * time.Millisecond
	conf.LeaderLeaseTimeout = 50 * time.Millisecond
	conf.LogOutput = io.Discard

	addr, trans := raft.NewInmemTransport(raft.ServerAddress(nodeID))
	r, err := raft.NewRaft(conf, f, raft.NewInmemStore(), raft.NewInmemStore(), raft.NewInmemSnapshotStore(), trans)
	if err != nil {
		return nil, fmt.Errorf("start raft: %w", err)
	}
	boot := r.BootstrapCluster(raft.Configuration{Servers: []raft.Server{
		{Suffrage: raft.Voter, ID: conf.LocalID, Address: addr},
	}})
	if err := boot.Error(); err != nil {
		_ = r.Shutdown().Error()
		return nil, fmt.Errorf("bootstrap raft: %w", err)
	}
	s := &Store{raft: r, fsm: f, mem: f.mem}
	if err := s.waitLeader(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

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
		Kind: rec.Kind, Name: rec.Name, Spec: rec.Spec, Status: rec.Status,
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

// Close shuts down Raft and the local state.
func (s *Store) Close() error {
	err := s.raft.Shutdown().Error()
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
		rec, err := f.mem.Create(ctx, store.Record{Kind: c.Kind, Name: c.Name, UID: c.UID, Spec: c.Spec, Status: c.Status})
		return &result{rec: rec, err: err}
	case opUpdate:
		rec, err := f.mem.Update(ctx, c.Kind, c.Name, c.Spec, c.Generation)
		return &result{rec: rec, err: err}
	case opStatus:
		rec, err := f.mem.UpdateStatus(ctx, c.Kind, c.Name, c.Status)
		return &result{rec: rec, err: err}
	case opDelete:
		return &result{err: f.mem.Delete(ctx, c.Kind, c.Name, c.Generation)}
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

func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	var data snapshotFile
	if err := json.NewDecoder(rc).Decode(&data); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	f.mem.Load(data.Revision, data.Records)
	return nil
}
