# 0020: Embedded OCI registry

Status: Accepted under ADR 0019 (Phase 1, items OB-22, OB-23, GL-40). Pass A
is implemented: the registry listener, mTLS authorization, tag rows in the
store and the `shoal registry` commands. Agent pull wiring, garbage collection
and Project scoping are follow-ups (see "Follow-ups").

## Context

Images are plain strings today (`WorkloadSpec.image`, appspec `image:`). Shoal
has no place to push an image built from a repository, and no tag that a
Workload or a build can follow. ADR 0019 makes the registry the third part of
the platform: builds (OB-21) push to it, tags (OB-23) move, and deployments
follow them (OB-45).

The design has to match the existing patterns:

- Identity lives in the certificate (ADR 0007): `O` is `shoal:admin` or
  `shoal:node`, and a Node object that is deleted revokes its certificate.
- The serving certificate for a second HTTPS listener comes from the cluster
  CA, the way the web portal gets one (`Server.PortalTLS`, now sharing
  `Server.servingCert`).
- Writes go through the store as protobuf specs (ADR 0002), so a tag is a
  store record with the same semantics as other objects.

## Decision

### 1. Library and storage

- The registry is `github.com/distribution/distribution/v3` v3.1.2 (Apache-2.0),
  embedded in `shoal server` as an `http.Handler` built with
  `handlers.NewApp` (`internal/registry/registry.go`). It is not a separate
  process.
- Blobs and manifests use the filesystem storage driver under
  `--registry-dir` (default `<data-dir>/registry`). Manifest deletion is
  disabled.
- The HTTP secret that signs upload state is random for each start, so
  uploads that are in progress when the server restarts must be retried.

### 2. Listener

- `shoal server --registry-listen <addr>` starts the registry (`cmd/shoal/server.go`,
  `serveRegistry`). The default is empty, so the registry is off until it is
  enabled.
- The listener is TLS 1.3 with a certificate issued by the cluster CA at each
  start, for `localhost`, the host name, the advertise host, the interface
  addresses, and each `--registry-name` (repeatable). Client certificates are
  verified against the cluster CA when one is presented (`Server.RegistryTLS`).
- The registry is not behind the HTTP ingress. Exposing it through a Route
  (ADR 0015) is a follow-up.

### 3. Authorization (no passwords or tokens in v1)

| Caller | Pull | Push | Catalog and `GET /shoal/v1/tags` |
|---|---|---|---|
| Admin certificate (`shoal:admin`) | yes | yes | yes |
| Node certificate (`shoal:node`), Node object exists | yes | no | no |
| Node certificate, Node object deleted | no | no | no |
| No certificate | only if `--registry-anonymous-pull` (default off) | no | no |

- The policy is `allowed` in `internal/registry/registry.go`. A denied request
  gets `401` with no `WWW-Authenticate` challenge. Docker and containerd
  clients use the client certificate instead of a password.
- Repository names are `<namespace>/<name>`. There is no Project kind yet, so
  the first segment is a convention for the future Project (GL-40). Names are
  not checked against a Project in v1.
- Every node can pull every repository. Image secrecy between nodes is not a
  goal of v1.

### 4. Tags in the store

- A successful manifest `PUT` with a tag reference writes an `ImageTag` record
  in the store: name `<repository>:<tag>`, spec `ImageTagSpec{repository, tag,
  digest, media_type}` (`api/v1alpha1/resources.proto`). The digest is the
  manifest digest from the response. A re-push moves the tag, and the record is
  updated (`Server.recordImageTag`).
- Distribution records the tag in its own index before the response is sent.
  The store write happens in the same request, before the `201`. If the store
  write fails, the client gets `500` while the tag already exists in the
  registry. The client's retry writes the row. Until that retry, a tag can exist
  in the registry without a row.
- A push by digest writes no row. Distribution keeps its own tag index for
  `/v2/<name>/tags/list`. The store row is Shoal's record of which digest a tag
  points to. It is not written through `shoal apply`, and it is not an entry in
  the `Object` oneof.
- Each `ImageTag` write is one store write, so with `--store=raft` a push is one
  Raft log entry per tag.

### 5. Client commands

- `shoal registry ls --registry host:port` prints each repository and tag with
  its digest, from `GET /shoal/v1/tags`, using the admin credentials in
  `admin.conf` (or `--config`).
- `shoal registry login --registry host:port [--to DIR]` writes `ca.crt`,
  `client.cert` and `client.key` (0600) into a directory and prints the
  `certs.d` steps. Docker reads `/etc/docker/certs.d/<host:port>/` for a CA and a
  client certificate, so `docker push` then works without a password.

### 6. Agents

Agents already trust the cluster CA. Containerd still needs a `hosts.toml`
entry for the registry (CA and, for node pulls, the node's client certificate),
and image pull credentials delivered like other secrets. That wiring is Pass B.
Until then, a Workload can pull from the registry only on a node that
containerd is configured for by hand.

### 7. Garbage collection

Not built. Blobs and tags accumulate. `shoal registry gc` (mark from ImageTag
rows, sweep blobs without a reference) is a follow-up under CC-09. It must
never delete a digest that a tag still points to.

## Consequences

- The binary grows by the registry's dependency tree. The size delta is
  reported with the pull request.
- `go.mod` upgrades OpenTelemetry, yaml and genproto packages that the
  registry's module requires. These are minimum-version upgrades, not
  behavioural changes of Shoal.
- Admin certificate holders can push any repository. A stolen node
  certificate can read all images until its Node is deleted (at most 30 days
  of exposure from expiry, as in ADR 0007).
- Registry data lives on the server that runs the listener. With `--store=raft`,
  tag rows replicate but blobs do not. Run the registry on one server until
  blob replication is decided.

## Follow-ups

- Pass B: containerd `hosts.toml` for the registry, pull credentials through
  `image_pull_secrets` (K-19), and the agent side of OB-22.
- `shoal registry gc` (CC-09).
- Project-scoped access and tokens (GL-40 remainder), when Project lands.
- Route exposure through ingress (ADR 0015) for external push over the
  internet.
- ImageStream and ImageStreamTag objects with triggers (OB-45), built on
  `ImageTag`.
