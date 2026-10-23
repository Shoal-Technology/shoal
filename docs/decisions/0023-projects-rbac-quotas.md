# 0023: Projects, RBAC and quotas

Status: Accepted (Phase 3 of ADR 0019). Pass A is implemented in the same change.
Passes B and C are planned below and are not implemented yet.

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
  their quota counts in the right project.

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
