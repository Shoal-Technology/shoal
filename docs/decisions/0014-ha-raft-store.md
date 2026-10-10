# 0014: HA Raft store

Status: Accepted (implemented in Phase 6, single-node and 3-node in tests;
server wiring and forwarding are pending)

## Context
ADR 0002 keeps the `Store` contract small (single-object compare-and-swap,
one global revision) so that it maps 1:1 onto a replicated log. The control
plane needs to survive the loss of one node without an external database,
so the state must be replicated and durable.

## Decision
- Replication uses `github.com/hashicorp/raft`. The Raft log and stable
  store use `raft-boltdb/v2` (bbolt, pure Go). Snapshots use
  `raft.NewFileSnapshotStore` (2 retained). The network uses
  `raft.NewTCPTransport`.
- The state machine is a `memory.Store`. Every write (Create, Update,
  UpdateStatus, Delete) is a JSON command applied with `raft.Apply`.
- The proposer picks the non-deterministic inputs before proposing: the UID
  (for Create) and the timestamp (for every write). They travel inside the
  command, so every replica computes identical records. The FSM never reads
  the clock or random numbers during apply.
- Error results (ErrNotFound, ErrExists, ErrConflict) are returned from the
  FSM in the apply response, so the same errors come back to the caller.
- Snapshot serializes all records and the global revision. Restore replaces
  the memory state wholesale.
- Bootstrap happens only when the data dir holds no Raft state
  (`raft.HasExistingState`). A restart never resets the cluster.

## Startup and readiness
`Open` does not wait for anything, so a cluster can form while nodes are
still catching up. The server must call `WaitReady(ctx)` before it serves
reads or runs controllers. Leadership alone is not enough after a restart:
the log tail replayed from disk is applied asynchronously.
- On the leader, `WaitReady` runs a Raft barrier, which applies everything
  committed so far.
- On a follower, `WaitReady` waits until the local applied index reaches the
  commit index learned from the leader.
`WaitReady` returns once the node is current. It does not wait for the node
to become a leader.

## Read consistency
Get, List and Watch read the local replica. Once `WaitReady` has returned, a
follower reflects everything committed up to that point, but it can fall
behind again between reads. The leader is authoritative: a read that must
observe the latest committed write has to go to the leader. Nothing in this
ADR relies on follower reads for correctness yet.

## Write contract
A write on a follower returns `ErrNotLeader` (checkable with `errors.Is`).
Nothing is forwarded yet. Forwarding to the leader is the next pass, and
callers must not assume a write reached the leader until it succeeds.

## Watch and snapshots
Watch delivers events from applied commands. Restore publishes no events
for the records it loads, so a follower that catches up by snapshot misses
those events. Watchers already resync periodically (ADR 0002), so they
converge without them.

## Persistence layout
Each node uses one data directory:
- `raft.db`: bbolt file holding the Raft log and stable store.
- `snapshots/`: FSM snapshots (2 kept).

Snapshots trigger after 1024 applied entries, checked every 30s. These
values are deliberately below Raft's defaults (8192 entries, 120s) so that a
restart replays a short log.

## Consequences
- A three-node cluster keeps writing with one node down, and a restarted
  node recovers from its data dir without a fresh bootstrap.
- Every write costs a Raft round trip and a log fsync. That is acceptable
  for control-plane traffic, but status heartbeats need to be watched for
  write amplification.
- Watch consumers must tolerate missed events after a snapshot restore.
- Not done yet: server wiring (which must call `WaitReady`), write
  forwarding from followers, a membership CLI, and a backup command.

## Wiring
Pass 2a wires the raft store into the server as an opt-in backend
(shoal server --store=raft, default still sqlite). Only one node is
supported.
- The store lives in <data-dir>/raft. The server calls WaitReady before
  it reads the store or starts any background task.
- Background writers (node status, controllers, join token cleanup) run only
  while 
aftstore.LeaderCh() reports this node as leader. The server
  starts them on gaining leadership and cancels them on losing it. With the
  sqlite store the node is always the leader.
- Writes on a follower return ErrNotLeader. Forwarding and multi-node join
  are still to do.