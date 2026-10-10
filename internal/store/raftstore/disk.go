package raftstore

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

// DiskConfig configures a durable node. Its state lives under DataDir:
// raft.db holds the Raft log and stable store (bbolt), and snapshots/ holds
// FSM snapshots.
type DiskConfig struct {
	DataDir  string
	NodeID   raft.ServerID
	BindAddr string // e.g. "127.0.0.1:7000"; port 0 picks a free port
	// Peers is the initial voter set, used only when the data dir has no
	// existing state. Empty means the node bootstraps alone with its own
	// address.
	Peers []raft.Server
	// Join means the node joins an existing cluster: with no Raft state it
	// does not bootstrap, and waits for an existing leader to add it (see
	// the server's JoinCluster call). Ignored when state already exists.
	Join bool
	// Raft overrides the Raft tuning. Nil uses the snapshot defaults below.
	Raft *raft.Config
}

// Open starts a durable node. Bootstrap happens only when DataDir holds no
// Raft state, so restarts recover from disk and never reset the cluster.
func Open(cfg DiskConfig) (*Store, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	bolt, err := raftboltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft.db"))
	if err != nil {
		return nil, fmt.Errorf("open raft log: %w", err)
	}
	snaps, err := raft.NewFileSnapshotStore(filepath.Join(cfg.DataDir, "snapshots"), 2, io.Discard)
	if err != nil {
		_ = bolt.Close()
		return nil, fmt.Errorf("open snapshots: %w", err)
	}
	trans, err := raft.NewTCPTransport(cfg.BindAddr, nil, 3, 10*time.Second, io.Discard)
	if err != nil {
		_ = bolt.Close()
		return nil, fmt.Errorf("listen %s: %w", cfg.BindAddr, err)
	}
	fail := func(err error) (*Store, error) {
		_ = trans.Close()
		_ = bolt.Close()
		return nil, err
	}

	existing, err := raft.HasExistingState(bolt, bolt, snaps)
	if err != nil {
		return fail(fmt.Errorf("check raft state: %w", err))
	}
	var boot []raft.Server
	if !existing && !cfg.Join {
		boot = cfg.Peers
		if len(boot) == 0 {
			boot = []raft.Server{{Suffrage: raft.Voter, ID: cfg.NodeID, Address: trans.LocalAddr()}}
		}
	}

	// Snapshots bound log replay on restart and disk use. A snapshot is
	// taken after 1024 applied entries, checked every 30s. Raft's defaults
	// (8192 entries, 120s) would replay more on a restart than a small
	// control plane needs. The threshold stays well above a burst of
	// heartbeat-driven status writes, so snapshots don't run constantly.
	rc := cfg.Raft
	if rc == nil {
		rc = raft.DefaultConfig()
		rc.SnapshotThreshold = 1024
		rc.SnapshotInterval = 30 * time.Second
	}

	s, err := NewNode(Config{
		NodeID:    cfg.NodeID,
		Address:   trans.LocalAddr(),
		Transport: trans,
		Logs:      bolt,
		Stable:    bolt,
		Snapshots: snaps,
		Raft:      rc,
		Bootstrap: boot,
	})
	if err != nil {
		return fail(err)
	}
	s.closers = []func() error{trans.Close, bolt.Close}
	return s, nil
}
