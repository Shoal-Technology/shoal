# Architecture

`shoal` is a lightweight container orchestrator for small clusters (1–20
nodes). One static binary runs as **server** (control plane), **agent**
(worker node) or **CLI**. Containers run on containerd; the system
continuously reconciles actual state toward declared desired state.

Status: Phases 1 to 5 and a web portal are implemented:
1. agent and runtime;
2. server, store, API and node registration;
3. scheduling, reconciliation and recovery;
4. health checks, rolling updates, rollback and logs;
5. Secrets/Configs, Services with DNS, and the WireGuard mesh.

High availability (Raft) is Phase 6. Anything marked *(planned)* is design,
not code yet.

```
                 ┌──────────────────────────── shoal server ────────────────────┐
  shoal CLI ──mTLS─▶ Control API (gRPC)                                          │
                 │      ▼                                                     │
                 │  Store (SQLite → Raft) ◀── controller (replicas, scheduler,│
                 │      ▲                     eviction), node tracker, token GC
                 │      │                                                     │
                 │  Agent API (gRPC) ◀─ Join API                              │
                 └──────▲───────────────▲─────────────────────────────────────┘
         snapshots ▼ ▲  │ mTLS stream   │ token + CSR, pinned CA hash
          heartbeats + statuses
                 ┌──────┴───────────────┴──── shoal agent ──────────────────────┐
                 │  sync (or --standalone file) ─▶ Reconciler ─▶ Runtime ─▶ containerd
                 └────────────────────────────────────────────────────────────┘
```

## Key decisions

| # | Decision | ADR |
|---|---|---|
| 1 | Protobuf is the single schema for API, storage and YAML (via protojson) | [0001](decisions/0001-protobuf-single-schema.md) |
| 2 | Store: generic objects, single-object CAS, global revision, watch as a trigger only | [0002](decisions/0002-store-semantics.md) |
| 3 | Agents dial the server; full-snapshot assignment sync; no inbound ports on agents | [0003](decisions/0003-agent-initiated-connections.md) |
| 4 | Bootstrap trust: join token + pinned CA hash, node keys never leave the node | [0004](decisions/0004-bootstrap-trust.md) |
| 5 | Two-level reconciliation: server places replicas, agent runs them | [0005](decisions/0005-two-level-reconciliation.md) |
| 6 | containerd integration: own namespace, labels as state, in-binary log driver, CNI bridge | [0006](decisions/0006-containerd-runtime.md) |
| 7 | Identity and role in the certificate subject; fixed server name `shoal-server`; revocation by deleting the Node | [0007](decisions/0007-certificate-identity.md) |
| 8 | Pure-Go SQLite (`modernc.org/sqlite`), gRPC and protobuf | [0008](decisions/0008-phase2-dependencies.md) |
| 9 | Replica objects with a template copy; one controller pass; spread scheduling; eviction after 60s; statuses in heartbeats; cached snapshots on agents | [0009](decisions/0009-replicas-scheduling-eviction.md) |
| 10 | Agent-side health checks (readiness + restart); in-place rolling updates gated by health; automatic rollback without touching the spec; logs via Sync + UploadLogs | [0010](decisions/0010-health-rollouts-logs.md) |
| 11 | Secrets/Configs: AES-256-GCM at rest, never returned by the API, resolved into snapshots; files update in place, env on restart | [0011](decisions/0011-secrets-configs.md) |
| 12 | Per-node subnets from the server; Service VIPs load-balanced by iptables on every node; DNS `<svc>.shoal` on each node; WireGuard mesh | [0012](decisions/0012-services-mesh.md) |
| 13 | Web portal embedded in the server: HTTPS, admin password (bcrypt), same code paths as the CLI | [0013](decisions/0013-web-portal.md) |

Resolved Phase 0 questions (accepted defaults):

- Networking before Phase 5: CNI bridge with per-node subnet (`--pod-cidr`), portmap for host ports.
- `resources` are both the scheduling request and the cgroup hard limit (memory limit, CFS quota).
- Scheduler: spread (most free resources), tiebreak on fewest replicas of the same workload.
- Partitioned nodes keep running their containers; on reconnect the snapshot decides.
- Single-node mode: `server --with-agent` (implemented in Phase 3).
- Targets: linux/amd64 and linux/arm64.
- Private registries: Phase 5 via Secrets.
- Secret encryption key: file in the data dir (0600), overridable with `--encryption-key-file`.

## Packages

```
api/v1alpha1/            .proto schema (resources + services) and generated code
cmd/shoal/                 CLI (cobra): server, agent, apply, get, delete, token, debug
internal/resource/       quantities, YAML manifest decode/encode, Workload defaults + validation
internal/store/          Store interface, watch hub, EncryptingStore; storetest/ conformance suite; sqlite/, memory/ (tests)
internal/security/       CA, certificates, TLS configs, join tokens, client config files
internal/server/         gRPC services (Control, Join, Agent), auth, node tracker, snapshots, portal API
internal/portal/         web portal static files (HTML/CSS/JS, embedded)
internal/network/        node networking: Service iptables, DNS resolver, WireGuard mesh
internal/controller/     replicas, scheduler, eviction, workload status (one pass)
internal/objects/        store record <-> API object conversion
internal/runtime/        Runtime interface and types
internal/runtime/containerd/  containerd + CNI implementation, log driver (linux)
internal/runtime/logfile/     rotating log files, tail/follow reader
internal/runtime/fake/        in-memory runtime for tests
internal/agent/          reconciler, image puller, standalone mode, server mode (join/sync)
```


## Agent (Phase 1)

**Assignment** = one replica the node should run: replica ID (also the
container ID), workload, container spec, restart policy, desired state
(`Running`/`Stopped`).

**Reconciler** — one level-triggered pass:

1. List containers in the agent's containerd namespace.
2. Stop and remove containers that are not assigned.
3. For each assignment (sorted by ID):
   - spec hash changed → once the new image is present, remove and recreate
     (the old container keeps running during the pull);
   - `Stopped` → stop if running (container kept for exit status/logs);
   - missing → create and start once the image is present;
   - exited after the agent stopped it → start again, whatever the policy;
   - exited on its own → restart per policy (`Always`, `OnFailure`, `Never`)
     with exponential backoff (1s doubling to 5m, reset after 10m running).
4. Return a status per replica; failures are collected per replica and never
   block other replicas.

Image pulls run in the background, at most one per image. A pass waits up to
2s for a pull it starts (so local images resolve immediately) and otherwise
reports the replica as Pending ("pulling image") and moves on; a slow
registry never delays restarts of other replicas. Failed pulls are retried
with the restart backoff.

Each runtime call has a timeout; graceful stop is SIGTERM, then SIGKILL after
`--stop-timeout`. Stopping the agent never stops containers. Following logs
ends when the container exits.

**Standalone mode** (`shoal agent --standalone -f replicas.yaml`) feeds the
reconciler from a local file, re-read on change. A missing or invalid file
keeps the last valid desired state. In Phase 2 the source of assignments
becomes the server stream; the reconciler is unchanged.

**State.** The agent has no database. Desired state comes from the file (later:
server). Actual state comes from containerd plus labels the runtime stores on
each container (start count, start time, IP, ports, spec hash, whether the
last exit was a requested stop). Backoff is in memory only.

**Logs.** containerd's shim runs `shoal --shoal-log-driver <path>` per task, which
writes `<RFC3339Nano> <stream> <line>` to `<state-dir>/logs/<id>.log`,
rotated at 10 MiB × 3 files. Logs keep flowing while the agent is down and
persist across container restarts until the container is removed.

**Networking.** Each container gets a pinned network namespace
(`<state-dir>/netns/`). It is created on first start, attached to the `shoal0`
bridge (host-local IPAM from `--pod-cidr`, IP masquerade for egress), and
kept across restarts, so the IP stays the same. `portmap` handles `hostPort`;
the `firewall` plugin lets traffic pass even when the FORWARD policy is DROP
(as with Docker). Remove tears the network down with the netns path, which
removes the veth and the iptables rules (ADR 0006). The container also gets a
private cgroup namespace and read-only `/sys/fs/cgroup`, a generated
`/etc/hosts`, and `/etc/resolv.conf` copied from the host's upstream
resolvers. `--bridge-name` and `--containerd-namespace` let several agents
share a host; each namespace is its own CNI network.

## Server (Phase 2)

**Data model** ([api/v1alpha1](../api/v1alpha1)). User kinds: Workload
(applied with `shoal apply`) and Node (created by Join, read-only to users
except delete). Internal kind: JoinToken (never returned by the API).
Metadata: name, uid, generation (bumped on spec change), revision (global,
last write), timestamps. YAML uses the protobuf JSON names and rejects
unknown fields. A missing `apiVersion` means `v1alpha1`, and numeric
quantities (`cpu: 0.5`) are accepted.

**Apply.** The server defaults (replicas 1, restartPolicy Always,
maxUnavailable 1, health check 10s/2s/3) and validates every object before
storing any of them, then upserts each one with a compare-and-swap retry loop.
The result is `created`, `configured` or `unchanged` (the encoded spec is
compared byte for byte). Server-managed metadata and status are ignored on
input, so `get -o yaml` output can be applied again. Objects in one request
are not applied atomically.

**gRPC services** ([api.proto](../api/v1alpha1/api.proto)) on one TLS 1.3
port (default `:7443`): `Control` (CLI, admin cert: Apply, Get, List,
Delete, CreateJoinToken), `Join` (no client cert: token + CSR → node
cert), `Agent` (node cert: `Sync` stream, `RenewCertificate`). See ADR 0007.

**Joining.** `shoal token create` stores the token as `JoinToken{sha256(secret),
expiry, maxUses, uses}` (default 24h, unlimited uses) and prints the join
command with the CA hash. Join checks the CSR first, then consumes one use
with a compare-and-swap on the token, creates the Node (NotReady) and returns
a 30-day certificate. An unknown id and a wrong secret give the same error.
Expired and used-up tokens are deleted hourly.

**Agent in server mode.** On first start the agent generates a key, joins
through a TLS connection pinned to the CA hash (a mismatch fails at once,
with no retry) and saves its credentials to
`<state-dir>/agent/credentials.conf` (0600). Later starts need no token, and
`--server` can change the saved address. It keeps one `Sync` stream open
and sends a heartbeat (capacity: CPUs, MemTotal; version, OS, arch) at the
interval the server sends first (5s). It reconnects with backoff (1s doubling
to 30s; 1m after an authorization error) and renews its certificate at two
thirds of its lifetime, reconnecting to present the new one. Since Phase 3 it
also runs its assigned replicas (see below).

**Node tracker** (single writer of Node status). Heartbeats are kept in
memory. The status is written when something other than the heartbeat time
changes, or every 30s, so heartbeats don't become a stream of store writes
(and later Raft entries). Every 2s, a node whose last heartbeat is older than
20s is marked NotReady; after a server restart the stored heartbeat time is
used. `get nodes` overlays the live heartbeat time, so it is current.
Deleting a Node also locks out its agent (ADR 0007).

**Shutdown.** On SIGINT/SIGTERM the server closes agent streams (agents
reconnect), lets in-flight calls finish for up to 10s, then closes the store.

**State on disk** (`--data-dir`, default `/var/lib/shoal/server`, 0700):
`state.db` (SQLite, WAL), `pki/ca.{crt,key}` (key 0600), `admin.conf` (0600).

## Scheduling and recovery (Phase 3)

See ADR 0009 for the reasoning. In short:

- **Replicas.** A Workload with `replicas: N` gets N Replica objects (`get
  replicas`). Each holds a copy of the container template and, once
  scheduled, a node. Deleting a replica makes the controller replace it.
- **Controller pass** (on changes and every 5s): remove replicas of deleted
  workloads and of nodes lost for 60s, match counts and templates, schedule
  oldest first, write workload status (`READY 3/3`). It is idempotent: a pass
  over a converged state writes nothing (tested). Partial failures skip only
  the object concerned.
- **Scheduler:** Ready nodes where the requests fit and no host port
  clashes; most free resources wins, then fewest replicas of the workload.
  When nothing fits, the replica stays Pending with "unschedulable: …".
- **Agent in server mode** runs the reconciler (2s) against the latest
  snapshot. It caches each snapshot on disk, keeps restarting crashed
  containers while the server is unreachable, and reports statuses in
  heartbeats, immediately after a change.
- **Recovery.** A crashed container is restarted by its agent (seconds). A
  lost node is NotReady after 20s; its replicas are replaced on other nodes
  60s after its last heartbeat. When it returns, it removes the old containers.
- **Single node:** `shoal server --with-agent` runs the server and an agent
  for the same machine in one process, without a join token.

Measured on a 2-vCPU VM with real containerd, two agents: a SIGKILLed
container was back in 2s; after SIGKILLing an agent, its replicas ran on the
other node 64s later; the returning node removed its stale containers within
2s.

Per-container overhead: containerd's shim runs one log-driver process (our
binary) per container, about 3 MB of private memory each. Its RSS of ~18 MB
is mostly the binary's pages, shared between all copies.

## Health checks, rolling updates and logs (Phase 4)

See ADR 0010.

- **Health checks** run on the agent against the replica's IP (HTTP or TCP).
  A replica is ready after its first passing probe. After `failureThreshold`
  failures in a row it is restarted. `READY` in `get workloads` counts
  replicas that are ready *on the template they are supposed to run*.
- **Rolling update.** A template change updates replicas in place, at most
  `maxUnavailable` unavailable at any time. Each batch waits for the
  previous one to pass its health checks. `get workloads` shows `UP-TO-DATE`
  and `ROLLOUT` (Progressing, Complete, RolledBack, Failed). `describe` shows
  the stable generation and why a rollout failed.
- **Automatic rollback.** An updated replica that isn't ready within
  `update.progressDeadlineSeconds` (default 120s), or that restarts 3 times,
  fails the rollout. Replicas go back to the stable template; the spec is
  left as applied, and a new apply starts a new rollout.
- **Logs.** `shoal logs WORKLOAD [--replica R] [-f] [--tail N]`. The server
  asks each replica's node over `Sync`; the agent streams the lines back on
  `UploadLogs`.

Measured with real containerd, two agents and an HTTP health check every 2s:
- the 1.27 → 1.28 update of 4 replicas took 27s and never dropped below 3/4
  ready;
- a release whose process never served HTTP was rolled back 23s after the
  apply, with only one replica down at any time.

## Secrets, Services and networking (Phase 5)

See ADRs 0011 and 0012.

- **Secrets and Configs** hold key-value data, used by workloads as env
  (`valueFrom`) or files (`files: [{path, secret|config}]`).
  - Secrets are encrypted at rest (AES-256-GCM, key in
    `<data-dir>/secret.key`), and their values never come back through the
    API or the portal.
  - Files update in place.
  - Env changes take effect on `shoal restart workload NAME`, a rolling
    restart that also retries a rolled-back release.
- **Container subnets.** Each node gets a `/24` of the cluster CIDR from the
  server.
- **Services** get a virtual IP and `<name>.shoal`. Every node load-balances
  them over the ready replicas with iptables, and rejects connections at once
  while there are none. Each node runs a resolver on its bridge address that
  answers Service names and forwards the rest.
- **WireGuard mesh.** Nodes exchange public keys through the server and route
  each other's subnets through the tunnel.

Measured on the test VM (two agents on one host):
- secret env and config files inside the containers;
- no plaintext secret in `state.db`;
- `web.shoal`, `web` and external names resolved from a container;
- requests spread evenly over the replicas on both nodes;
- Config edits reached the served page without a restart;
- both WireGuard interfaces configured as each other's peers.

Traffic through the tunnel between two *separate* hosts was not tested (a
single VM).

## Web portal

See ADR 0013. `https://<server>:7444/`, log in as admin with the password
from `<data-dir>/ui-initial-password` (or `shoal ui reset-password`). Pages:
- overview;
- nodes, including "add node" with a ready join command;
- workloads: create by form or YAML, scale, restart, edit, delete, replicas,
  rollout state, live logs;
- services;
- secrets and configs;
- apply YAML.

It is embedded in the binary (no Node.js or CDN) and calls the same Control
API code as the CLI.

## Planned components (Phase 6)

- **HA:** replace SQLite with embedded Raft (hashicorp/raft), three servers,
  leader election, failover tests. The Store contract and its conformance
  suite are designed for this (ADR 0002); the encrypting store and portal
  password already live in the store.
## Risks

- Split brain during partitions can duplicate a replica until the cut-off
  node returns (ADR 0009); fine for stateless workloads only.
- The Store contract must stay Raft-implementable; the shared conformance
  suite (`internal/store/storetest`) must pass for both implementations.
- A join token plus network access is enough to add a node until the token
  expires; keep TTLs short or use `--max-uses`.
- containerd and CNI plugins are host prerequisites, so "one binary" means one
  binary *of ours*.
- The CA key lives on the server's disk.
