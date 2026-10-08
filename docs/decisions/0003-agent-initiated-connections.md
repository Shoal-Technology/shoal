# 0003: Agent-initiated connections and snapshot sync

Status: Accepted (Sync stream with heartbeats in Phase 2; assignment
snapshots and replica statuses in Phase 3, see ADR 0009; log streaming
later)

## Context
Agents may sit behind NAT or firewalls. If the server pushed deltas, a single
lost message could leave a node permanently wrong.

## Decision
- The agent dials the server and keeps one bidirectional `Sync` stream open.
- The server sends a full `AssignmentSnapshot` (with a version) for the node
  whenever it changes, and periodically. The agent acknowledges the version.
- The agent sends heartbeats and replica status changes upstream.
- For logs, the server sends `OpenLogs{requestID}` over the stream and the
  agent opens a separate `UploadLogs` stream. No multiplexing on `Sync`.
- Agents need no inbound ports until WireGuard (Phase 5).

## Consequences
Level-triggered end to end: the agent's reconciler always receives the
complete desired set. Snapshots grow with replicas per node, which is fine at
this scale. Log requests have one extra hop through the server.
