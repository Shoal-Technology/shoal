# 0023: Projects, RBAC and quotas

Status: Accepted (Phase 3 of ADR 0019). Pass A is implemented. Pass B1 is
implemented on branch `projects-pass-b1` (see "Pass B1 implementation").
The rest of pass B and pass C are planned below and are not implemented yet.

Accepted 2026-10-23 by the project owner, under ADR 0019 Phase 3 (rows OB-36,
OB-38, OB-40, OB-41, OB-42, K-26, K-36, K-41, K-43, CC-01, GL-30, GL-31, GL-44).

## Context

Every kind in Shoal is cluster-wide. The store is keyed by `(kind, name)`
(`internal/store/store.go`), and the API takes `kind` and `name` only
(`api/v1alpha1/api.proto`, `GetRequest`, `ListRequest`, `DeleteRequest`). There
is no tenancy scope, no quota and no per-user permission. ADR 0007 keeps one
admin key with full rights, and the node certificate role `shoal:node` is the
only other identity.

ADR 0019 needs a tenancy scope before builds, the registry and pipelines can be
shared by more than one team. Those features already name Projects as the
future scope: ADR 0020 (repository `<namespace>/<name>`), ADR 0021 (repository
prefix), ADR 0022 (approvals use the admin key until Projects exist).

## Decision

### 1. Project kind

- A `Project` is a cluster-scoped object. `metadata.name` is a DNS label of at
  most 63 characters. `spec` holds `displayName` and `description` (at most
  200 bytes each). Labels follow the usual rules.
- The `default` project always exists. It is written to the store when it is
  first applied. Until then it is implicit: `get project default` and `list
  project` report it anyway, so a fresh cluster has a project with no setup.
- `default` cannot be deleted. Any other Project can be deleted only when no
  object in it remains (`FailedPrecondition`, naming the count).

### 2. Membership: which project an object belongs to

- Project-scoped kinds: Workload, Service, Route, Volume, Database,
  BuildConfig, Build, Secret, Config and ResourceQuota. Cluster-scoped kinds:
  Node, Project, and every internal kind (Replica, JoinToken, Certificate, the
  ACME kinds, RouteClaim, OwnerClaim, VolumeClaim, DatabaseBackup, UIAuth).
- A project is recorded as the reserved label `shoal.dev/project` on the stored
  record, like the existing reserved label `shoal.dev/owned-by` (ADR 0016). The
  label is set by the server. A user may not set it in `metadata.labels`.
- **The label is absent for the default project.** A record with no label is in
  `default`. This means every record written before this change is in `default`
  without a migration, and its bytes and its "unchanged" apply result are the
  same as before. The API still reports `metadata.project` as `default`.
- Apply: an empty `metadata.project` keeps the object in its current project,
  or puts a new object in `default`. An explicit project must match the current
  one. Moving an object between projects is refused: delete it and create it
  again. An explicit project must exist (Project applied first).
- Names stay unique cluster-wide in pass A, because the store key is unchanged.
  Two projects cannot each have a Workload called `web`. See open question 1.
- A Database's owned objects (ADR 0016) take the Database's project, so that
  their quota counts in the right project. A Build takes the project of its
  BuildConfig.

### 3. Mapping existing resources

All existing records belong to `default`, through the rule above. Old clients
and old manifests keep working. The API changes are additions only: a new
`ObjectMeta.project`, new optional `project` fields on Get, List and Delete,
and two new `Object` oneof variants. Old servers ignore the new fields.

### 4. Identities

- Pass A adds no identity types. The admin certificate (`shoal:admin`) is
  cluster-admin. Node certificates keep `shoal:node`.
- Pass B: user certificates from the cluster CA, with `CN` = user name,
  `O` = `shoal:user` and one `OU` per group. Optional API tokens, stored as
  hashes with an expiry and bound to a user. The admin key stays a break-glass
  path (ADR 0007).

### 5. Roles and bindings

- Built-in roles are fixed in code (`internal/authz`), not stored:
  - `cluster-admin`: every verb on every kind, in every project.
  - `admin`: every verb on the project-scoped kinds in one project, and
    manages bindings in that project (pass B).
  - `edit`: get, list, create and update on the project-scoped kinds, and
    delete. No Project or ResourceQuota writes.
  - `view`: get and list on the project-scoped kinds. Secret values are never
    returned to any role (ADR 0011), so `view` sees Secret keys only.
- Verbs: `get`, `list`, `create`, `update`, `delete`, and `admin` for
  cluster-level operations such as backup, join tokens, node labels and
  restarts.
- A binding is a subject (user or group), a role and a project (or cluster-wide
  for cluster-admin). Bindings are not stored in pass A, so there is no
  RoleBinding kind yet. Pass B adds the stored kind, the `shoal role bind`
  command and the portal screen.

### 6. Authorization at the API

- Every Control RPC goes through one place in `internal/server/auth.go`. A table
  maps each method to its verb and kind. The project comes from the request
  when the request has one. Apply checks each object it writes, in the handler,
  because one Apply can name several projects.
- Pass A runs in **audit mode** (`Config.AuthzMode`, default `audit`). The
  decision is computed for every call. A would-deny decision is logged at WARN
  and the call goes on. Mutating calls are logged at INFO with the actor, verb,
  kind, name, project and decision. Pass B switches the default to `enforce`.
- In pass A only the admin certificate exists, so every decision is allow. The
  audit output is then the admin's own record of its writes. That is the
  intended first step: the decision table is tested now, and enforcement waits
  for identities that can be denied.
- The audit records are slog lines. A persistent audit kind is CC-01, later.

### 7. Quotas

- `ResourceQuota` is project-scoped. `spec.hard` holds `cpu` (a CPU quantity,
  for example `4` or `2000m`), `memory` (for example `8Gi`), `workloads` (a
  count) and `storage` (for example `100Gi`, Volume sizes). An empty field is
  unlimited. Several quotas may exist in one project. Each is checked on its
  own.
- Usage: a Workload uses `replicas` times its `resources.cpu` and
  `resources.memory`, which Shoal enforces as hard limits. A Volume uses its
  `size`. A Workload counts once in `workloads`. Replicas and controller
  objects are not counted, except through their Workload or Volume.
- Enforcement is at admission (Apply) only. It compares the usage after the
  batch against `hard`. A batch is refused when the usage is above `hard` and
  the batch raises the usage. A batch that lowers the usage is never refused.
  The refusal is `ResourceExhausted` and names the quota, the resource, the
  amount requested and the hard limit.
- Quotas are not checked at scheduling. A replica that fits no node stays
  Pending (E09), as before.
- Quotas that select projects by label (OB-40) are open question 4.

### 8. Migration and rollback

- Records need no migration (section 2). Rollback is safe: an older server
  reads the label as an ordinary label. Projects and quotas written by the new
  server remain in the store, but an older server cannot list them.
- The first start of the new server writes nothing new. The `default` project
  is written only when someone applies it.

## Consequences

- Multi-team use can start with one admin key and named projects for
  accounting and quotas. Pass A does not restrict anyone yet.
- Names are shared across projects (section 2), so a second team cannot reuse a
  name. This is the largest gap, and it is open question 1.
- The audit records are plain log lines and can be lost with the log.
- The decision table is the one place that must change when pass B adds roles
  and bindings.

## Owner decisions (2026-10-23)

The project owner answered the open questions above on 2026-10-23. The
decisions bind passes B and C.

1. **Names are unique per project.** The store key of a project-scoped object
   includes its project. A one-time migration moves the objects that pass A put
   in a named project (section "Pass B1 implementation"). Objects in `default`
   keep their names and their keys.
2. **Only cluster-admin creates Projects in pass B.** Self-service creation
   with a quota template (OB-37) is pass C.
3. **Registry blobs and build cache count toward a storage quota.** The
   accounting is implemented in pass C. Until then they are not counted.
4. **Label-selected quotas (OB-40) are deferred** to P2.
5. **Cross-project traffic is denied by default in pass C.** Pass B1 does not
   yet refuse a new cross-project reference (see "Known gaps").
6. **The admin key becomes break-glass after pass B2.** Every use of it is
   audited. It stays a full certificate until then.
7. **API tokens:** default lifetime 30 days, maximum 1 year, rotatable and
   revocable. Implemented in pass B2.
8. **Pipeline, PipelineRun and Job are project-scoped.** Their Jobs and Builds
   take the project of the run. Implemented in pass B1.


## Phases

- **Pass A (this change)**: Project kind; `default` project; membership label;
  Get, List and Delete scoped by project; Apply moves refused; ResourceQuota
  kind and admission check; `internal/authz` with the built-in roles and a
  decision table; API authorization in audit mode with an audit record for
  mutating calls. Tests: scoping, quota accept and reject, the decision table,
  and the label rules.
- **Pass B**: enforce mode by default; user certificates and optional API
  tokens (section 4); stored RoleBinding kind and `shoal role bind`; `--project`
  on the CLI and `metadata.project` in manifests (the manifest field is already
  in pass A); Secret value reads limited to the edit and admin roles; project-
  scoped join tokens (GL-44).
- **Pass C**: portal screens for projects, roles and quotas; registry
  repositories and build names prefixed by project (ADR 0020, ADR 0021; GL-40);
  pipeline and approval identity from the user, not the admin key (ADR 0022
  pass C follow-up); self-service project creation (OB-37).

## Open questions (owner input needed)

1. **Names per project.** Should a name be unique only within a project, as in a
   Kubernetes namespace? That changes the store key to `(kind, project, name)`.
   It is a migration of every record, and it changes the route and service
   names used in URLs. Proposed: decide before pass B. Pass A keeps names
   cluster-unique.
2. **Who creates Projects.** Only cluster-admin (proposed for pass A and B), or
   self-service with a quota template (OB-37, pass C)?
3. **Quotas for images and build storage.** The registry (ADR 0020) and the
   build cache are not counted in pass A. Count them in pass C, or keep them out?
4. **Label-selected quotas (OB-40).** Do we want one quota across projects
   selected by label, in pass B or later?
5. **Cross-project references.** A Route or Service in project A can name a
   Workload in project B. Pass A allows it. Should Phase 3 default-deny
   cross-project traffic (K-36), and which pass enforces it?
6. **Admin key after pass B.** Keep it as full break-glass (ADR 0007, proposed),
   or limit it to a named break-glass certificate?
7. **API token lifetime.** Default lifetime, rotation and the maximum for pass B.

## Pass B1 implementation

Pass B1 is implemented on branch `projects-pass-b1`. It covers decision 1
(names per project and the migration), decision 8, the CLI `--project`, and
the scoped RPCs. Passes B2 and C are not in this change.

### Keys

- `tenancy.Key(project, name)` (`internal/tenancy/tenancy.go`): the key of a
  project-scoped object is `name` in `default` and `name.project` in any other
  project. Object and project names are DNS labels, which have no dots, so the
  key is unambiguous. It is also a valid DNS name, container ID and file name.
- A record's `Name` is its key. `objects.Meta` shows the bare name and
  `metadata.project`. Anything that uses an object as a store key or an identity
  uses `objects.KeyOf`, which rebuilds the key from the metadata.
- `tenancy.KeyName` and `tenancy.KeyProject` split a key (one dot at most).

### Reference rule

- An object's references to other project-scoped objects (a Workload's Secret,
  Config or Volume; a Route's Service and TLS Secret; a Build's BuildConfig; a
  Pipeline's Secrets) stay bare names. They resolve in the referrer's project.
- Cluster-wide records that name a project-scoped object store its key:
  `Replica.spec.workload`, `VolumeClaim` (name and spec), `RouteClaim.spec`,
  `OwnerClaim` (name and spec) and `DatabaseBackupRecord.database`.
- Replica names are qualified from the bare name and the random suffix.

### Apply and lookups

- An apply without `metadata.project` targets `default`. A name in another
  project is another object. The pass-A refusal to move an object by a changed
  project is gone: an object moves only by being deleted and applied again.
- `findObject` (`internal/server/projects.go`): with a project, the object is
  read by key and an absent name fails with `not found in project`. With an
  empty project, the name is searched in every project: one match is used, and
  more than one fails with `FailedPrecondition` naming the projects.
- Scoped RPCs with new `project` request fields (`api/v1alpha1/api.proto`):
  `LogsRequest` 5, `RestartWorkloadRequest` 2, `StartBuildRequest` 2,
  `CancelBuildRequest` 2, `BuildLogsRequest` 2, `StartPipelineRunRequest` 6,
  `CancelPipelineRunRequest` 2, `PlayJobRequest` 2, `JobLogsRequest` 2,
  `DatabaseBackupsRequest` 2, `BackupDatabaseRequest` 2. The existing `project`
  fields of Get, List and Delete are unchanged.
- Pipeline, PipelineRun and Job are project-scoped (`tenancy.scoped`). A run
  takes the project of its Pipeline, and its Jobs and Builds take the run's
  project. The webhook de-duplicates Builds within one project.
- A Build of a non-default project is pushed to `<project>/<repository>`
  (`internal/server/builds.go`, `createBuild`). The default project keeps its
  repository names. Images pushed before this change keep their names.

### Migration

- Version record: kind `StoreMeta`, name `schema`, status `{"version":1}`
  (`internal/projmigrate/projmigrate.go:31`, `Version`, `MetaKind`, `MetaName`).
- `Build` (`projmigrate.go:78`) is a pure function of the records. It returns
  the moves that bring the store to version 1. Project-scoped records of a
  non-default project that are still bare-keyed are moved to their keys. A
  Secret is re-sealed under its new name, because the sealing binds kind and
  name (`internal/store/encrypt.go:162`, `Reseal`). The cluster-wide references
  are rewritten (`rewriteRefs` `projmigrate.go:177`, `refsOf` `:222`). The
  version record is put.
- `Build` refuses a partial state before anything changes, with `ErrPartial`
  (`projmigrate.go:43`). Refused cases: a qualified key with no version record;
  a bare key of a non-default project with version 1; a default name with a dot;
  a store newer than the server; a move onto a key that exists; an unreadable
  version record.
- `Run` (`projmigrate.go:305`): export, then `Build`. If there is anything to
  do and the store has records, it writes a backup
  (`internal/server/projmigrate.go:34`, `backupBeforeMigration`, to
  `DataDir/backups/pre-projects-<UTC time>.shoalbackup`, the usual
  `shoal-backup/v1` format with Secrets sealed). It then calls `Rekey` once.
  A second `Build` must find nothing to do, or the run fails.
- `Rekey` is atomic in every backend: one IMMEDIATE transaction in sqlite
  (`internal/store/sqlite/rekey.go:13`), one raft log entry replicated by the
  FSM (`internal/store/raftstore/raftstore.go:317`, `opRekey`), and a locked
  batch in memory (`internal/store/memory/rekey.go:13`). The conformance suite
  checks it (`internal/store/storetest/rekey.go`).
- Hook: `internal/server/server.go` calls `migrateProjects` after the raft
  store is ready and before the API serves (`server.go:423`), and at the start
  of the leader tasks, for a node elected later (`server.go:558`). A follower
  returns `ErrNotLeader` and leaves the work to the leader
  (`internal/server/projmigrate.go:22`).
- Idempotent: a store at version 1 with consistent keys is an empty plan and
  nothing is written.
- Crash: `Rekey` is all or nothing. A crash before the commit leaves the old
  state and the backup on disk, and the next start runs the migration again. A
  crash after the commit leaves version 1, and the next start does nothing.
- Restore: a backup taken before this change has no version record. Restoring
  it creates a store at version 0, which the next start migrates (the restore
  runs before the hook). A backup taken after it carries the version record.

### Downgrade

There is no in-place downgrade. To run a server from before pass B1 on a store
that has been migrated, stop the server, start the old version on an empty
data directory with the `secret.key`, and restore
`DataDir/backups/pre-projects-*.shoalbackup`. Objects and Projects written after
the migration are not in that backup and are lost. Until then, the old server
reads a non-default object as `name.project`, with its project label.

### CLI

- `--project/-p` is a global flag. Its order of precedence is: the flag, then
  `SHOAL_PROJECT`, then the default written by `shoal project use` to
  `~/.shoal/project`, then none (`cmd/shoal/project.go`, `resolveProject`).
  The choice is made in the root's `PersistentPreRunE`.
- With no project, an apply writes to `default`, and get, delete, logs, restart,
  builds, pipelines and database commands find the name in any project, failing
  when the name is ambiguous. `shoal apply -p X` sets `metadata.project` on
  project-scoped objects that do not name one, and refuses a manifest that names
  another project.
- New commands: `shoal project create|ls|delete|use`, `shoal quota set|ls`.

### Tests

- `internal/store/storetest/rekey.go`: atomic Rekey, identity kept, failure
  changes nothing, put creates or replaces (memory and sqlite).
- `internal/projmigrate/projmigrate_test.go`: fresh store; a store as pass A
  left it, with Secrets and cluster-wide references; the second run changes
  nothing; two projects with one name; a crash before commit; a failed backup;
  each refused partial state.
- `internal/server/projects_b1_test.go`: one name in two projects; scoped RPCs
  (RestartWorkload, Logs, BuildLogs, CancelBuild, CancelPipelineRun, PlayJob,
  JobLogs, BackupDatabase, DatabaseBackups); a start over a pre-migration store
  migrates it and keeps a backup.
- `internal/server/projects_test.go`: pass-A expectations moved to per-project
  names.
- `cmd/shoal/project_test.go`: flag, environment and default-file precedence,
  and the global `-p` flag.

### Known gaps (for review)

1. **Cross-project references are refused (decision 5, pass B1).** A reference
   from an object to an object in another project fails at apply with
   `InvalidArgument`, naming both projects. The references are Route to
   Service and TLS Secret, Service to Workload (selector), Workload to Secret,
   Config, Volume and pull Secret, BuildConfig to Secret and to the Workload
   it updates on success, and Pipeline to Secret (`resource.References`,
   `internal/resource/refs.go`; the apply check is
   `internal/server/crossproject.go`). An object's references resolve only
   within its own project. The migration refuses pass-A data that holds such a
   reference (`projmigrate.ErrCrossProject`, `internal/projmigrate/projmigrate.go`).
   It takes no backup and changes nothing. The refusal lists each reference
   and the remedy: move the object into the referring project, or delete the
   reference, then upgrade. Environment targets (pass C) are not in this
   change, because pass C has not merged. Cross-project service traffic policy
   stays in pass C (decision 5).
2. **Portal.** Reads by name use the any-project lookup. Writes go through the
   default project. Project screens are pass C.
3. **Registry and build-cache quota** is pass C (decision 3).
4. **Replicas of pass-A workloads** keep their names, so containers are not
   replaced. New replicas use the qualified form.
5. **DNS** for a non-default project's Service uses the `name.project` form. This
   is not yet tested end to end.
6. **Not in B1:** user certificates, API tokens, stored role bindings, `shoal
   role bind`, and the admin key as break-glass (decisions 6 and 7; passes B2).
