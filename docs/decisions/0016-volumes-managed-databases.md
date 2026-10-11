# 0016: Volumes and managed databases

Status: Proposed

## Context
Shoal runs stateless workloads well (ADR 0009, 0010) and has Secrets, Configs
and Services (ADR 0011, 0012), HA for the control plane (ADR 0014) and ingress
with automatic TLS (ADR 0015). It cannot yet run a stateful workload safely:

- Containers get no writable storage. `internal/runtime/runtime.go` declares
  `Mount` as read-only, and `internal/runtime/containerd/containerd_linux.go`
  builds every bind mount with `rbind ro nosuid nodev`. A database cannot
  write its data directory today.
- `WorkloadSpec` (`api/v1alpha1/resources.proto`) has no volume field.
- ADR 0009 replaces replicas on a partitioned node after the eviction
  window. A replica that owns local data must never be replaced on another
  node, because the old copy would keep writing to its own disk. The current
  scheduler has no such exception.
- There are no backups, snapshots or restore paths, and no way to connect an
  application to a database except by hand.

This ADR defines three things, in order: node-local `Volume` objects (v1),
snapshots with S3-compatible export, and a managed `Database` kind for
Postgres and Redis (v1). Each one builds on the one before. Postgres high
availability is out of scope for v1 (see Out of scope).

## Decision

### 1. Volumes v1

#### 1.1 Object model
```yaml
kind: Volume
metadata:
  name: pg-data
spec:
  size: 20Gi            # required, Quantity; enforced on the loop backend
  accessMode: RWO       # only RWO in v1; the only value accepted
  owner: { uid: 999, gid: 999 }   # optional, default 0:0; applied at create
  mode: "0700"          # optional, default 0700
  retention: Retain     # Retain | Delete, default Retain (see 1.6)
  snapshots:            # optional, see section 2
    every: 6h           # interval only in v1; cron is later work
    retain: 7
    export: backups-s3  # optional BackupTarget name
  preSnapshot:          # optional hook, see 1.7
    command: ["sync"]
    timeout: 30s
    onFailure: abort    # abort | proceed, default abort
```
Status: `node` (empty until first bind), `state` (`Pending`, `Bound`,
`NodeLost`, `Failed`), `usedBytes`, `message`, `attachedWorkload`.

A Workload mounts a Volume by name:
```yaml
spec:
  replicas: 1
  volumes:
    - name: pg-data        # Volume name, cluster-wide
      mountPath: /var/lib/postgresql/data
      readOnly: false
```
The field is a new `repeated VolumeMount volumes = 13` on `WorkloadSpec`.
Validation rejects any Workload whose `replicas` is not 0 or 1 when it has a
read-write volume. A Volume can be referenced by at most one Workload; a
second reference is rejected at apply time, not silently queued.

#### 1.2 Storage backends and quotas
Three options were considered:

| Option | Quota | Requirements | Cost |
|---|---|---|---|
| A. Plain directory under `<state-dir>/volumes/<name>` | none (size is advisory and reported only) | none | simplest; a runaway writer can fill the disk |
| B. XFS project quota on the state-dir filesystem | kernel-enforced, per directory | state-dir must be XFS with `prjquota` | not the default on Ubuntu (ext4), so operators must prepare the disk |
| C. Loop device: an ext4 image file of `size`, mounted at `<state-dir>/volumes/<name>.mnt` | kernel-enforced by ext4 size | root, `losetup`, `/dev/loop*`; works on any filesystem | one loop device and one mount per volume; offline resize only |

Decision: v1 ships **A and C**. The agent flag `--volume-backend=loop|dir`
defaults to `loop`. `dir` is for hosts without loop devices (containers in
CI, some VMs); the agent then reports `quota: none` in Volume status and
the `size` field is advisory. We do not ship B in v1, because it needs a
filesystem choice the operator has not made. It stays as a later option
behind the same interface.

Online resize is out of scope: growing a loop volume needs unmount, `resize2fs`
and mount. Document it as a manual, offline operation until a later pass.

#### 1.3 Mounting into containers
`runtime.Mount` gets a `ReadOnly bool` field. The containerd runtime builds
`rbind rw nosuid nodev` for read-write volumes and keeps `rbind ro ...` for
Secret and Config files. `noexec` is not set, so images that run from their
data directory keep working. The bind source is the volume directory only,
never its parent, so a container cannot walk into other volumes.

#### 1.4 Node pinning (first-bind)
A Volume has no node until it is first needed. The scheduler (ADR 0009
`schedule`) handles a Workload that mounts an unbound Volume as follows:
1. it picks a node exactly as today, using CPU and memory fit;
2. in the same store transaction that binds the replica, it sets
   `Volume.status.node` to that node and `state: Bound`.

After that, every replica of that Workload is constrained to that node. A
replica whose volume node is not Ready stays `Pending` with the message
`volume pg-data is on node n2, which is not Ready`. It never moves. There is
no "re-bind" in v1; a bound Volume stays bound to its node until it is deleted.

#### 1.5 Node loss (explicit)
v1 does **not** move volume data or replicas off a dead node. When the
volume's node is lost (no heartbeat past the eviction window):
- the Volume becomes `NodeLost`; the Workload's replica stays `Pending`;
- the eviction path in ADR 0009 is changed so volume-bound replicas are
  skipped. This is enforced in the scheduler, not by a user setting, because a
  second writer on another node would corrupt the data;
- recovery is one of: bring the node back (its disk and data are intact); or
  restore the latest snapshot (section 2) into a new Volume on a healthy node,
  which is what `Database` restore does (section 3).

Data-loss window: everything written since the last successful snapshot or
export. Users who cannot accept that window must run a managed Database with
backups configured, or wait for replicated volumes (later work).

#### 1.6 Deletion and retention
- `shoal delete volume X` is refused while a Workload references it.
- `retention: Retain` (default): the object is removed, but the directory or
  loop image moves to `<state-dir>/volumes/.retained/<name>-<timestamp>` and
  stays until the operator removes it with `shoal volume purge <name>`.
- `retention: Delete`: the directory or image is removed at once.
- The agent does not delete data unless the Volume object says so, and a
  deleted Volume with data still on disk is reported by the agent until it is
  purged.

#### 1.7 Permissions, UIDs and the backup hook
- At create, the agent chowns the directory (or the ext4 root) to
  `owner.uid:owner.gid` with `owner.mode`. Default root 0700 suits images that
  run as root. Images that run as another user must set `owner` to that user;
  Shoal does not rewrite image users in v1.
- The backup hook, `preSnapshot`, runs a command through the runtime's exec
  support in the first Ready replica that mounts the Volume, with timeout and
  `onFailure`. Example: `psql -c CHECKPOINT` or `redis-cli BGSAVE` (with a wait)
  for a database volume. If the replica has no exec support or is not Ready, the
  snapshot fails with a clear message unless `onFailure: proceed`. Exec in the
  runtime is a new capability in pass A; the database backups in section 3 do
  not depend on it.

### 2. Snapshots and S3 export
- **Local snapshot.** `shoal snapshot create volume/X` runs the `preSnapshot`
  hook, freezes the filesystem with `fsfreeze` (loop backend) or skips the
  freeze (dir backend, documented as not crash-consistent), then writes a
  tar archive to `<state-dir>/snapshots/<volume>/<id>.tar` and records a
  `Snapshot` object. Reflink copies are not used in v1 because the loop
  backend is ext4, which has no reflink. A compressed archive is a later option.
- **Schedule.** `snapshots.every` and `snapshots.retain`. The leader keeps the
  next-due time in the store, so only one node takes a scheduled snapshot,
  and the agent runs it.
- **Export.** A `BackupTarget` object (`endpoint`, `bucket`, `prefix`,
  `region`, `pathStyle`, `credentialsSecret`). The agent that holds the
  snapshot uploads it with a multipart S3 client. Upload runs on the node
  that has the data, so the server never reads volume data.
- **Credential model.** `credentialsSecret` names a Secret that holds
  `accessKeyId` and `secretAccessKey`. The server stores that Secret encrypted
  at rest (ADR 0011, AES-256-GCM, AAD = kind/name). For an upload job, the
  server sends the two values inside the job assignment over the existing
  mTLS session. The agent keeps them in memory only, passes them to the
  uploader, never writes them to disk, and never puts them in logs or
  Snapshot status. Operators should give the key a policy that limits it to
  one bucket prefix.
- **Restore.** A snapshot is restored into a new Volume with
  `shoal volume create --from-snapshot <id>`. The agent extracts into an empty
  directory with the Volume's owner and mode, then the Volume is bound as in
  1.4.

### 3. Managed databases v1

#### 3.1 Object model
```yaml
kind: Database
metadata:
  name: app-db
spec:
  engine: postgres        # postgres | redis
  version: "16"           # major.minor selects a pinned image; see 3.6
  size: 20Gi              # data Volume size
  replicas: 1             # fixed at 1 in v1; validation rejects others
  resources: { cpu: 1, memory: 2Gi }
  backup:
    schedule: 24h         # interval in v1
    retain: 7             # number of backups kept per target
    targets: [volume, s3] # volume = a backup Volume; s3 = a BackupTarget
    s3: backups-s3        # required when targets includes s3
  restoreFrom: ""         # optional backup id; creates the DB from it
```
Status: `phase` (`Provisioning`, `Ready`, `BackingUp`, `Restoring`,
`Upgrading`, `Degraded`, `Failed`), `endpoint` (`app-db.shoal:5432` or
`:6379`), `lastBackup`, `nextBackup`, `image` (the resolved digest), and
`message`.

#### 3.2 The controller expands a Database
`internal/controller/databases.go` owns each Database and writes the child
objects. Children carry an owner reference, so deleting the Database removes
them under the same retention rules as Volumes:
- **Volume** `<db>-data`, size from `spec.size`, RWO, first-bind pinned.
- **Workload** `<db>`, one replica, image from section 3.6, mount
  `<db>-data` at the engine's data path (`/var/lib/postgresql/data` for
  Postgres, `/data` for Redis), `restartPolicy: Always`, and the existing
  in-place update with no surge (`workload.go`), so the old replica stops
  before the new one starts and the Volume is never mounted twice.
- **Service** `<db>`, one port, so other workloads reach it at `<db>.shoal`.
- **Secret** `<db>-credentials`, generated (section 3.5).

Users never write the children. The `Database` is the only object they
manage. Children can be read with `shoal get`, but edits to them are
overwritten on the next reconcile.

#### 3.3 Health checks
v1 uses a TCP probe on the service port, run by the agent prober (ADR 0010),
which today only does HTTP. The prober gets a TCP mode. A TCP probe confirms
the port is listening, not that the database answers queries. The stronger
check (`pg_isready`, `redis-cli ping`) needs exec in the container; that
arrives in pass A and is switched on for databases once it is reliable (pass
C). Until then, `Ready` means the port answers.

#### 3.4 Logical backups
Both engines back up over the network from a one-shot container of the same
pinned image, so no exec into the database is needed:
- Postgres: `pg_dump -Fc -h <db>.shoal -U <user> <db>` writes a custom-format
  dump.
- Redis: `redis-cli -h <db>.shoal --rdb <file>` writes an RDB file.

The one-shot container is a Workload with `restartPolicy: Never`. It mounts a
backup Volume (`<db>-backups`, targets `volume`) or streams to S3 through
the uploader (targets `s3`, section 2 credential model). A backup records a
`Backup` object with its id, engine version, size, checksum (SHA-256 of the
file) and target. `retain` deletes the oldest backups beyond the count, after
the new one is verified.

Physical snapshots of `<db>-data` (section 2) are also available, but they
are not crash-consistent for Postgres without a checkpoint. So the logical
dump is the default backup, and a snapshot is an extra for the volume.

#### 3.5 Credentials
- The controller generates the superuser password (Postgres) or the
  `requirepass` password (Redis) with `crypto/rand`, 32 bytes, base64url. It is
  written once into the `<db>-credentials` Secret with fields `username`,
  `password`, `database` and `url`.
- Postgres reads `POSTGRES_PASSWORD` from that Secret. Redis reads
  `requirepass` from a file mounted from the Secret, not from the command line,
  so the password does not show in `ps`.
- The Secret is encrypted at rest by the existing mechanism. `Database` does
  not need its own key handling.
- Rotation is not in v1. Re-creating a Secret would lock the database out
  without a matching server change, so rotation is a later pass that changes
  the server and the Secret together.

#### 3.6 Connection string, exposure and image pinning
- The connection string is the `url` field of `<db>-credentials`
  (`postgres://<user>:<pw>@app-db.shoal:5432/<db>?sslmode=disable`, or
  `redis://:<pw>@app-db.shoal:6379/0`). Applications consume it with the
  existing EnvSource reference:
  ```yaml
  env:
    - name: DATABASE_URL
      valueFrom: { secret: { name: app-db-credentials, key: url } }
  ```
  Nothing is injected automatically. The reference is explicit, and the
  Secret is the only place the value lives.
- Traffic stays on the Shoal network (Service VIP, WireGuard mesh). TLS to
  the database is not in v1; `sslmode=disable` is stated here so it is not
  mistaken for a default.
- Images are the official `postgres` and `redis` images, pinned by digest in
  a table in `internal/database/images.go`: `engine + major.minor -> image
  reference@sha256:digest`. The table is updated in Shoal releases. Users pick a
  major.minor and cannot choose an arbitrary image in v1. This keeps the image
  source known and makes a digest change reviewable.

#### 3.7 Restore
- `restoreFrom: <backup id>` creates the Database with an empty `<db>-data`
  Volume. For Postgres, the database starts empty, a restore one-shot runs
  `pg_restore --clean --if-exists -h <db>.shoal` from the backup, and the
  Database goes `Ready` after the restore exits 0. For Redis, the restore job
  mounts the Volume, writes `dump.rdb` into it, and the server starts from it.
- A restore into an existing Database is refused. Restore into a new
  Database, then point applications at the new endpoint. This avoids
  overwriting live data by accident.
- The backup checksum is checked before the restore starts.

#### 3.8 Minor version upgrades
- Only minor upgrades inside one major version are in v1. A new Shoal release
  bumps the pinned digest. Users trigger the upgrade by setting
  `spec.version` to the new minor (or by the release's default), and the
  controller:
  1. takes a logical backup and waits for it to verify;
  2. stops the old replica (the in-place update with no surge);
  3. starts the new image on the same `<db>-data` Volume;
  4. returns the Database to `Ready` or sets `Failed` with the old image still
     recorded.
- Rollback is restarting the previous digest on the same volume. It works for
  minors because the on-disk format does not change within a major. Postgres
  major upgrades and Redis major moves are refused; the procedure is to
  restore a backup into a new Database of the new major.

### 4. Security
- **Credentials**: generated and stored as encrypted Secrets (3.5). They are
  not in the Database spec, the Workload spec or any log line. `get -o yaml`
  shows Secret metadata, not values (existing Secret behavior, to be tested
  for `Database` children too).
- **Volume isolation**: each volume directory is owned by its `owner` and mode
  `0700`. The container sees only its own volume directory (1.3). A Volume
  can be referenced by one Workload (1.1). The loop image is never bind-mounted,
  so the container cannot reach the image file or other volumes.
- **S3 credentials**: Secret-backed, encrypted at rest, sent to the agent only
  for the upload, held in memory, never logged or stored in Snapshot or Backup
  status (section 2).
- **Images**: pinned by digest, from the official images only (3.6).
- **Network**: the database has no published host port. Only Shoal Services
  reach it. Access control between workloads (RBAC, roadmap item 7) is later
  work, so any workload on the network can connect with the credentials it is
  given.

### 5. Out of scope for v1 (later work)
- Network or replicated volumes, for example an integration with a CSI-like
  plugin or a replicated block store. These need the node-loss story in 1.5
  to change.
- Moving a bound Volume to another node, and online resize.
- Postgres HA (streaming replicas or failover). A Database is one replica on
  one node. Its data survives a container restart, but not loss of its node
  except through a restore.
- Redis Cluster and Sentinel.
- Cron expressions for schedules (interval only in v1).
- Credential rotation for databases.
- TLS to databases.
- Exec-based health checks until exec is proven (pass C).

### Consequences
- Shoal gains a real data path. The first stateful workload runs on one node,
  pinned, and is recovered from backups when that node is lost. Users who
  need zero data loss on node failure need replicated storage, which is
  later work, and this must be stated in docs.
- The scheduler and eviction code get a second rule (volume-bound replicas
  stay put) on top of ADR 0009.
- The runtime gets read-write mounts and an exec path. Both are new surface
  and need tests in pass A.
- Backups run as one-shot containers, which makes the Workload restart policy
  `Never` a real mode. Roadmap item 6 (cron jobs) will reuse that mode.
- A pinned image table is a maintenance cost: each engine release needs a
  Shoal release.

### Testing plan
- **Unit tests.** Validation of Volume and Database (`internal/resource`),
  including rejected multi-writer and replicas > 1 with volumes. Secret
  generation (length, character set, uniqueness). Backup retention selection.
  Image table lookups. S3 request signing against fixed vectors. Fuzz the
  Volume and Database decoders alongside the existing manifest fuzzers.
- **In-process controller tests with fakes.** A fake runtime, a fake store
  and a fake clock. Covered: first-bind pinning, a node-not-Ready replica
  staying Pending, eviction skipping volume-bound replicas, Database expansion
  into children with owner references, backup scheduling and retention,
  restore refusal on an existing Database, and minor upgrade failure leaving
  the old image recorded.
- **CI e2e on a disposable runner with containerd.** The sequence:
  1. create a Database (postgres 16) and wait for `Ready`;
  2. write rows with a client container on the Shoal network;
  3. kill the database container and wait for it to restart on the same
     Volume;
  4. check the rows persist;
  5. run a logical backup and an export to a local S3-compatible test server
     (MinIO or similar, on the runner only);
  6. create a new Database with `restoreFrom` that backup;
  7. compare row counts and a checksum of the table dump between the two.
  Repeat the sequence for Redis in pass E.
- **The shared VPS never runs containers.** All integration and e2e runs use
  the GitHub workflow on a disposable runner, as with `integration.yml`,
  `vm-mesh.yml` and `ingress-e2e.yml`. Nothing in this ADR runs on
  `codex-vps`. Results go to `docs/testing.md` with the run ID, as before.

### Implementation passes
Each pass is sized for one agent session and ends with its own tests green.

- **A. Volume kind, agent mounts and node pinning.** `Volume` in
  `resources.proto` and `internal/resource`; `WorkloadSpec.volumes`; the
  `runtime.Mount.ReadOnly` change; exec support in the containerd runtime;
  loop and dir backends behind the agent flag; first-bind pinning in the
  scheduler; the eviction exception; `shoal` create, get and delete for
  volumes. Tests: unit, controller with fakes, and a containerd integration
  test on a runner that writes a file, restarts, and reads it back.
- **B. Snapshots and S3 export.** `Snapshot` and `BackupTarget` kinds; local
  tar snapshots with `fsfreeze` and the pre-snapshot hook; the interval
  schedule and retention; the S3 uploader with in-memory credentials; the
  `from-snapshot` restore into a new Volume. Tests: unit (signing vectors),
  fakes, and e2e against a test S3 server on the runner.
- **C. Database controller for Postgres.** `Database` kind; the controller
  expansion (3.2); generated credentials (3.5); the image table (3.6); TCP
  health, and `pg_isready` once exec is proven; `Ready` and `Degraded` phases.
  Tests: fakes plus the first half of the e2e (create, write, kill, persist).
- **D. Backups and restore.** `pg_dump` and `pg_restore` one-shots; the
  `Backup` object with checksum; scheduled backups with retention to a backup
  Volume and to S3; `restoreFrom`; minor upgrades with the pre-upgrade
  backup and rollback. Tests: fakes plus the full e2e (backup, restore,
  compare).
- **E. Redis.** Engine branch in the controller; `requirepass` from a mounted
  file; `redis-cli --rdb` backup; RDB restore through the restore one-shot;
  same e2e sequence. Tests: as C and D.
- **F. e2e CI.** The workflow file, the runner setup with containerd and a
  test S3 server, and the full sequence for both engines, recorded in
  `docs/testing.md`. This pass also adds the workflow to the quality gate
  (`docs/quality.md`).

## Open questions
1. **Default backend.** `loop` needs root and loop devices on every node. Is
   that acceptable, or should `dir` be the default with quotas off until
   XFS is set up?
2. **Scheduling library.** Interval-only schedules keep v1 small. Do we add a
   cron library (for example `robfig/cron`) now or wait for roadmap item 6?
   Needs an ADR 0008 dependency decision.
3. **S3 client.** Use `minio-go` (new dependency, less code) or a small SigV4
   signer (more code, no dependency)?
4. **Exec in the runtime.** Pass A adds it for the pre-snapshot hook. Confirm
   that exec is acceptable for the runtime's surface and that its logs do not
   leak into the agent log.
5. **Eviction for volume replicas.** Should the operator be able to force a
   move after a node is confirmed dead (for example a `shoal volume
   force-rebind`), knowing the old node may still have a copy? Default is no.
6. **Quota for Database.** Should database volumes count against any node
   disk limit, or is that left to the operator until quotas (roadmap item 7)?
7. **Redis persistence mode.** Use RDB snapshots only, or also AOF? v1 plan
   is RDB only, which means a crash can lose the last minutes of writes.
