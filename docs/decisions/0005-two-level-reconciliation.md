# 0005: Two-level reconciliation

Status: Accepted (agent level in Phase 1, server level in Phase 3; see
ADR 0009)

## Context
Restarting crashed containers should be fast and should keep working while
the server is unreachable. Placement decisions need a cluster-wide view.

## Decision
- **Server:** Workloads become Replica objects, which are bound to nodes;
  replicas on nodes that are lost get replaced.
- **Agent:** assigned replicas become containers. Local restarts follow the
  restart policy with exponential backoff (1s doubling to 5m, reset after 10m
  of running).
- Both levels are level-triggered and idempotent, and keep going after
  partial failures.

## Consequences
The agent reconciler doesn't care where assignments come from: Phase 1 reads
a file, Phase 2 uses the server stream. Restart counts live in container
labels. Backoff is in memory, so an agent restart allows one immediate retry.
