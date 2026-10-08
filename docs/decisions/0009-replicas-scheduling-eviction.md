# 0009: Replicas, scheduling and eviction

Status: Accepted (Phase 3)

## Context
ADR 0005 splits reconciliation between server (placement) and agent
(running containers). Phase 3 has to decide how placements are stored, how
nodes learn about them and report back, and what happens when a node goes
silent.

## Decision
- **Replica objects.** The controller creates one `Replica` per desired
  instance, named `<workload>-<5 random chars>`. It holds the workload name,
  the bound node (empty until scheduled) and a **copy of the container
  template**: the workload spec without `replicas` and `update`, so scaling
  never touches running replicas. Replicas never move: a replica that has to
  go elsewhere is deleted and replaced.
- **One controller pass** (level-triggered, on store events debounced by
  50ms, and every 5s). It loads all workloads, nodes and replicas, then:
  1. deletes replicas of deleted workloads, and replicas on nodes that are
     deleted or have been NotReady with no heartbeat for 60s;
  2. updates templates and creates or deletes replicas to match `replicas`.
     Scale-down removes unscheduled replicas first, then those on not-ready
     nodes, then those not running, then the newest;
  3. schedules unbound replicas, oldest first;
  4. writes each workload's status (replicas, ready, observed generation).
- **Scheduler.** Only Ready nodes; requested CPU and memory must fit in
  capacity minus the requests already bound (an unknown capacity counts as
  unlimited); no host port may clash. The node with the largest average free
  fraction wins, ties going to fewer replicas of the same workload, then
  fewer replicas in total, then the name. Requests equal limits (Phase 0).
  The scheduler writes a replica's status only while the replica is unbound
  (e.g. "unschedulable: …"), and sets "scheduled to X" *before* binding.
  From then on the node's agent is the status's only writer.
- **Template changes in Phase 3** are applied to all of a workload's replicas
  at once; agents then recreate the containers. Phase 4 replaces this with
  rolling updates.
- **Server → agent.** On `Sync` the server pushes the node's complete
  `AssignmentSnapshot` whenever it changes (watch on Replica) and every 30s.
- **Agent → server.** Each heartbeat (5s, or immediately after a change)
  carries the status of every assigned replica. The server stores only
  changes, and only for replicas bound to the reporting node.
- **Agent cache.** The agent saves each snapshot to
  `<state-dir>/agent/assignments.pb` and uses it on start until the server
  sends a new one. Without a cache the agent touches nothing: an empty
  desired set would remove every container.
- **Partitions.** A cut-off node keeps running its containers. If the
  partition lasts longer than the eviction window, the replicas are replaced
  elsewhere. When the node returns, its next snapshot no longer contains them,
  so it removes the old containers.

## Consequences
For up to the eviction window after a partition, both copies of an evicted
replica can run. That is acceptable for stateless workloads, the target here;
a fencing mechanism would be needed for anything else. Recovery from a lost
node takes about 60s from the last heartbeat (measured: 64s).
