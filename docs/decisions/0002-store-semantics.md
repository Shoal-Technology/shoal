# 0002: Store semantics

Status: Accepted (implemented in Phase 2)

## Context
Phase 2 uses SQLite. Phase 6 replaces it with embedded Raft. Swapping is only
cheap if the `Store` contract doesn't depend on SQLite features such as
multi-row transactions.

## Decision
- One generic table of objects keyed by (kind, name), with spec and status
  stored separately as opaque bytes (binary protobuf).
- A global, monotonically increasing revision. Every write sets the object's
  revision.
- Writes are single-object compare-and-swap. `Update(kind, name, spec,
  generation)` succeeds only if the stored **generation** still matches, and
  returns `ErrConflict` otherwise. Generation changes only when the spec does,
  so status writes never make spec writes conflict. (The first version of
  this ADR compared revisions, but every heartbeat-driven status write would
  then have conflicted with `apply`.) `UpdateStatus` is unconditional: each
  kind's status has a single writer.
- `Watch(kinds...)` exists only to trigger controllers early. Delivery is
  best effort, with no replay from a revision: a slow watcher loses events.
  Every controller also resyncs fully every 30s, so a dropped event delays
  work but doesn't change the outcome.
- Secret specs will be encrypted by an `EncryptingStore` decorator (AES-256-GCM
  with a key ID) in Phase 5.
- A shared conformance test suite (`internal/store/storetest`) runs against
  every implementation.

## Consequences
Each write maps 1:1 to a Raft log entry. Controllers can't rely on atomic
writes across several objects, so they must tolerate partial progress (which
level-triggered reconciliation already requires).
