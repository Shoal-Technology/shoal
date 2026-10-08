# 0010: Health checks, rolling updates, rollback and logs

Status: Accepted (Phase 4)

## Context
Updates must replace replicas gradually, respecting `maxUnavailable`, gated
by health checks, with automatic rollback on failure. Logs must be
reachable from the CLI without exposing agents to inbound connections.

## Decision
- **Health checks run on the agent**, against the replica's IP (HTTP GET:
  2xx/3xx passes, redirects are not followed; or a TCP connect), every
  `intervalSeconds` with `timeoutSeconds`, starting after
  `initialDelaySeconds` (new, default 0).
  - A replica is **ready** after its first passing probe.
  - After `failureThreshold` failures in a row it is **unhealthy**: not ready,
    and the agent restarts it whatever the restart policy. The restart counts
    as a restart, with back-off.
  - Without a health check, running counts as ready.
  - One check serves both readiness and liveness, which is enough at this
    scale and keeps the API small.
- **Statuses describe the container that runs.** Each container carries
  the template generation it was created for (label
  `shoal.template-generation`), and the agent reports that generation along
  with readiness. A replica is **available** only if it is bound, ready, and
  the reported generation equals its spec's. *Found in testing:* reporting
  the assignment's generation instead let an old container, kept running
  while the new image was pulled, pass as the new version.
- **Rolling update (controller).** When the template changes, replicas are
  updated in place (the agent recreates the container).
  - Outdated replicas that are unavailable anyway are updated at once.
  - Available ones are updated only while fewer than `maxUnavailable`
    replicas are unavailable.
  - The next batch therefore waits until updated replicas pass their health
    checks.
  - Scale-ups during a rollout create replicas on the target template.
  - A rollout whose replicas are all updated and available becomes **stable**
    (`status.rollout.stableTemplate/stableGeneration`).
- **Automatic rollback.** A rollout fails when an updated replica either:
  - is not available `update.progressDeadlineSeconds` (new, default 120)
    after it was updated, or
  - has restarted 3 times on its new template (crash loop).

  The generation is then recorded as failed, and while the spec stays at that
  generation the controller's target is the stable template. Rollback uses
  the same rolling procedure, so broken replicas go first. **The user's spec
  is never rewritten**: `get -o yaml` still shows what was applied, and the
  status explains the rollback. Applying a new spec starts a new rollout. To
  retry the same spec, change something in it. A first deployment has nothing
  to roll back to, so it can only be marked `Failed` (it keeps trying, and
  becomes `Complete` once ready).
- **Logs** (ADR 0003's design). `Control.Logs` sends a `LogRequest` over each
  replica's node's `Sync` stream. The agent answers on a separate
  `UploadLogs` stream tagged with the request ID, which the server checks
  belongs to that node. Lines are merged by time (or streamed as they come
  with `--follow`). An unreachable node becomes a note line instead of
  failing the whole request.

## Consequences
A rollout costs at least one health-check pass per batch: 4 replicas with
a 2s interval took 27s on the test VM. A crash-looping release is rolled back
after about three restart cycles (23s in the test), well before the
deadline. Rollout state lives in the workload's status, which the controller
alone writes, so it survives server restarts.
