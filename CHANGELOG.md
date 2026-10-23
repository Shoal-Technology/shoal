# Changelog

## v0.2.0

Shoal is alpha. Interfaces, flags and on-disk formats can still change before
1.0. Read [Known limitations](#known-limitations) before you run it.

Full list of changes: `git log v0.1.0..v0.2.0`.

### Added

- **Platform scope.** [ADR 0019](docs/decisions/0019-shoal-platform-scope.md)
  sets the target: parity with Kubernetes, OpenShift and GitLab, feature by
  feature. The comparison is in
  [docs/platform/parity-matrix.md](docs/platform/parity-matrix.md) (PR #1, PR #11).
  It is a plan. Most rows are not built yet.
- **Typed secrets and image pull secrets** (PR #3). Secrets have types, including
  `kubernetes.io/dockerconfigjson` and TLS. An app can name pull secrets, so
  agents can pull images from private registries.
- **Embedded OCI registry** (PR #5, PR #6, [ADR 0020](docs/decisions/0020-embedded-registry.md)).
  The server serves OCI images over HTTPS when started with `--registry-listen`.
  Pushes are admin only. Agents pull from it. It is off by default.
- **Builds** (PR #7, PR #9, [ADR 0021](docs/decisions/0021-builds.md)). A
  `BuildConfig` describes a build from a git repository. Each run is a `Build`
  record with phases. Builds run on nodes labelled `shoal.dev/builder=true`,
  through a rootless BuildKit executor. Each builder has its own identity and
  certificate. Build containers run with network isolation. Only build daemons
  clear `no_new_privs`. Workloads keep it set.
- **Git webhooks and build-to-rollout** (PR #8). Webhooks from GitHub, GitLab,
  Gitea, or a generic HMAC sender start a Build of the pushed commit. A finished
  build can roll out its image. Alpha.
- **Agents with several servers** (PR #2). An agent takes a list of server
  addresses. It follows the leader hint and reconnects when its server goes away.
- **Pipelines design** (PR #10, [ADR 0022](docs/decisions/0022-pipelines.md)).
  The design for `shoal-ci.yml` pipelines. Nothing from it is implemented yet.
- **Health and metrics endpoints.** `/metrics`, `/healthz` and `/readyz` on the
  server and the agent.
- **Error catalogue.** Errors have codes E01 to E15, and `shoal explain` describes
  them. Waits stop at terminal errors instead of retrying.

### Changed

- Go toolchain 1.27.3, and `moby/sys/user` v0.4.1. This fixes GO-2026-6809 and
  GO-2026-6686 (PR #4).
- Raft: a server bound to a wildcard address advertises the `--raft-advertise`
  address when one is set.
- The README states the status of each feature, and the platform scope (PR #11).

### Known limitations

- **Alpha.** Expect changes to interfaces and formats.
- **Builds run only on builder nodes.** Nodes labelled `shoal.dev/builder=true`
  must exist. Without one, a build ends in phase `Error`.
- **Builds are proven for public https git only.** Private https and ssh
  repositories are covered by unit tests only.
- **DNS from builds.** A build step can resolve names, including through a
  resolver on a private address.
- **Webhook redelivery is not deduplicated.** A redelivered or replayed push starts
  another build of the same commit
  ([issue #13](https://github.com/Shoal-Technology/shoal/issues/13)).
- **Registry blobs are not replicated under Raft.** Tags replicate, blobs do not.
  Run the registry on one server until blob replication is decided.
- **Review apps and pipelines are not implemented.** ADR 0022 is a design only.
- **Multi-host is not verified end to end.** The WireGuard mesh, agent failover and
  scheduling across machines are covered by unit tests and by single-host or
  loopback checks. The Raft control plane was verified on one hosted runner with
  loopback addresses.
- **Postgres has no high availability.** A managed database runs as one instance.
- **No S3 backup export yet.** The S3 client exists, but backups do not use it.
  Copy backups off the host yourself.
