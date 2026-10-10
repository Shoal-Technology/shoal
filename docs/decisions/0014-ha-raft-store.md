# 0014: HA Raft store

Status: Accepted (implemented in Phase 6; multi-node join (pass 2c) and write
forwarding (pass 2d) are done; a membership CLI and backup are pending)

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
A write on a follower returns `ErrNotLeader` (checkable with `errors.Is`) at
the store. At the API it becomes a FailedPrecondition naming the leader (see
pass 2d). The CLI retries once on the leader. Callers that do not retry must
not assume a write reached the leader until it succeeds.

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
- Not done yet: multi-node join, write forwarding from followers, a
  membership CLI, and a backup command (see pass 2b above).

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

## Pass 2b: readiness bound and leadership grace (done)
- `New` waits at most 30s for `WaitReady` (`raftReadyTimeout`). With no
  leader, or with a log that never catches up, it fails with
  "raft store not ready after 30s" instead of hanging. The operator fixes
  the cluster and restarts. Chosen over starting in a not-ready state, which
  would have every API handler return an error until a leader appears.
- Each node that gains leadership calls `nodeTracker.beginTerm`. That drops
  the in-memory heartbeats, which describe the previous leader's view, and
  opens a grace period of `NodeNotReadyAfter` (default 20s). During grace no
  node is marked NotReady. A node with no heartbeat in this term is counted
  from the start of grace, not from a stale stored heartbeat. Agents get one
  full window to reach the new leader before eviction can start.

## Pass 2b: not done yet (join is done in pass 2c, below)
- Join: `--raft-join` and `--raft-advertise`, the admin-only join RPC, and
  the idempotent rejoin path are not implemented. Bootstrap and join are
  described here as the target design: node 1 starts with `--store=raft` and
  bootstraps (no `--raft-join`); each later node starts with
  `--raft-join=<existing server API address>` and does not bootstrap. It
  asks that server for AddVoter(nodeID, advertise). The receiving server
  forwards to the leader if it is not the leader. A node that is already a
  voter returns success, so a restart is safe.
- Write forwarding: the design choice is Option B, a gRPC FailedPrecondition
  that carries the leader's API address, which the CLI retries. Option A
  (server-side forwarding) needs a server client that authenticates to the
  leader with its own admin identity, and that is larger. The leader's API
  address is still to be stored, either in a store record written on gaining
  leadership or in raft metadata. Neither is built yet.
- The 3-node in-process test (join, follower write, leader failover) is not
  written.

## Pass 2c: join (done)
- Bootstrap: the first node starts with `--store=raft` and no `--raft-join`.
  It bootstraps a one-voter cluster. When it gains leadership it writes the
  `Cluster/leader` record: its API address, raft node ID and raft address.
  The record is written before the leader tasks start.
- Join: a node started with `--raft-join=<leader API address>` and no raft
  state does not bootstrap. It calls the admin-only
  `Control.JoinCluster(node_id, raft_addr)` on that address, retrying with
  backoff for up to 2 minutes. If the call reaches a follower, the follower
  returns FailedPrecondition with the leader's API address in the message,
  and the joiner follows it once. The join runs before the readiness wait,
  because a new node knows no leader until it is added.
- `--raft-advertise` is the raft address other nodes dial. It defaults to
  the bound raft address.
- Idempotence: the leader calls AddVoter. Re-adding a voter with the same ID
  and address is a no-op. A restart with `--raft-join` calls JoinCluster
  again, so it is safe. A restarted node that binds a new raft port updates
  its address the same way.
- Shared PKI: all servers in one cluster share the cluster CA (`pki/`) and
  the secret encryption key (`secret.key`). An operator copies both from an
  existing server's data dir before a joining node's first start. The
  joiner's admin credentials come from that CA, so without the copy the
  leader rejects the join. A command for this copy is not built.
- Readiness fix: a joining follower can know a leader while its commit
  index is still 0. `WaitReady` now requires a non-zero commit index before
  it reports a follower ready.
- Not done: write forwarding. Only JoinCluster redirects. Other writes on a
  follower still return ErrNotLeader to the caller, and the CLI does not
  retry against the leader yet. (Done in pass 2d, below.)

## Pass 2d: write forwarding (done)
Option B is implemented.
- Server: `apiError` maps `raftstore.ErrNotLeader` to
  `FailedPrecondition` with the message `not the leader`. A unary
  interceptor (`unaryLeader`, in `internal/server/forward.go`) catches any
  not-leader error from a handler and replaces it with
  `not the leader; leader API address is <addr>`. The address comes from the
  `Cluster/leader` record. If no record exists yet, the message is
  `not the leader; no leader is known yet` and the client does not retry.
- Portal: a write that reaches a follower returns HTTP 503 with the same
  message, so the leader address is in the JSON `error` field. The portal
  does not redirect the browser; the operator opens the leader's URL.
- Client: `server.LeaderRetry(tls)` is a unary client interceptor. It is
  installed by the CLI's `connect`. On a not-leader error it dials the
  leader's address with the same TLS config and retries the call once.
  The retry connection has no interceptor, so it cannot loop. A follower
  rejects a write before it changes anything, so the retry cannot apply a
  write twice.
- Scope: unary RPCs only. Streaming RPCs that write (agent streams) are not
  forwarded. Agents reconnect to a leader through the join flow.
- Test: `TestRaftWriteForwarding` (cluster_test.go) covers a write through a
  follower, a leader stop with a new leader record, a write through the
  surviving follower, reads of pre- and post-failover records, and leader
  tasks running on the new leader only. A test-only gate counts overlapping
  leader-task runs.
- Still not done: a membership CLI and a backup command.