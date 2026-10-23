# 0019: Platform scope: Kubernetes, OpenShift and GitLab parity

Status: Accepted. The feature-by-feature comparison is in
`docs/platform/parity-matrix.md`, which cites the upstream files it read.

Accepted 2026-10-23 by the project owner.

## Context

Shoal is a single-binary orchestrator for clusters of 1 to 20 nodes. The
README reports Phase 5 of 6 done and the alpha label. It runs OCI containers
through containerd, with Services, Routes, Volumes, managed Postgres, Secrets,
Configs, rolling updates and rollback, a WireGuard mesh and a web portal.

The owner wants Shoal to become the combination of Kubernetes, OpenShift
(OKD) and GitLab: a place to push code, build it, run it, and govern it with
projects and roles. This ADR decides how far that goes, and what Shoal will
not do.

Facts that bound the decision (each is in the parity matrix):

- Shoal has no git integration, no image build, no image registry, no Job or
  CronJob kind, no namespace or project, no RBAC, no quotas, no pull secrets,
  no security context, and no label-based scheduling. The appspec `build:` key
  is rejected in v1 (ADR 0017, open question 2), and image building is deferred
  to a "git-builds roadmap item".
- ADR 0007 states that RBAC is a non-goal and the admin key is full admin.
- ADR 0016 keeps CSI-like storage plugins, cron expressions, Postgres HA and
  TLS to databases out of scope.
- Shoal's own proto, not Kubernetes, is the API. Its object model (a fixed set
  of kinds in `api/v1alpha1`) is small on purpose.
- Upstream surface is large. Of the 158 concepts compared, 9 exist in Shoal
  today. 17 are P0 (Phase 0 or 1). The rest are P1 to P3 or not planned.

Full parity with the three systems is not a realistic or useful target. Each
of them is a large product, and a single VPS (1 GB RAM in the ADR 0017 path)
must still work. The decision below picks the integrated slice that makes
Shoal a complete git-to-running-app platform for small clusters.

## Decision

Shoal becomes an integrated platform in four parts, built in this order:

1. **Orchestrator** (exists). Workloads, Services, Routes, Volumes, Databases,
   Secrets and Configs, scheduling, rollouts, health checks, HA control plane.
2. **Git-driven build and deploy** (new). A push to a configured repository
   builds an image and rolls it out to a Workload. The build runs as a Job on
   a node labelled for builds.
3. **Registry and pipelines** (new). An embedded OCI registry stores the
   images. A small pipeline file, `shoal-ci.yml`, runs jobs in stages.
4. **Tenancy and governance** (new). A Project scopes names, quotas, network
   policy and roles. Audit records writes.

Key choices, each with the alternative it rejects (see "Alternatives"):

- **Git: integrate, do not host.** Shoal receives webhooks from GitHub and
  GitLab (and later Gitea), verifies their signatures, and clones with a token
  held in a Secret. It reports commit status back to the host. Shoal does not
  build its own git server in this plan.
- **Builds: BuildKit for Dockerfiles first.** A build is a Job on a node with
  a build label (K-15, node selector, pulled into Phase 1). Cloud Native
  Buildpacks come later as a second builder for repositories without a
  Dockerfile. Source-to-Image is not implemented. The Docker daemon is not used,
  because it needs privileged access.
- **Registry: embedded, per Project.** The image registry is CNCF Distribution,
  run by the server, with one repository namespace per Project. Tags are rows in
  the store and point at digests. A tag change can update a Workload (OB-45),
  through the existing rollout and rollback path (ADR 0010). External registries
  stay supported through `image_pull_secrets` (K-19).
- **Multi-tenancy: Project plus RBAC plus quotas.** A Project is the scope for
  names, Routes, Volumes, Secrets, Builds, quotas and network default-deny. Roles
  are bound to Project scopes. Quotas can select Projects by label, because
  labels already exist on every object (ADR 0017, open question 1). ADR 0007
  stays in force: the admin key remains a break-glass path.
- **Pipeline spec: `shoal-ci.yml`, a subset inspired by GitLab CI.** The subset:
  stages, jobs with image and script, rules limited to branch, tag, event and
  changed paths, artifacts with paths and expiry, cache, variables, environment,
  and timeout. It is not GitLab-compatible, and it reads no `include`, `extends`
  or `!reference` in v1. The subset is chosen from the `job_template` keys in the
  GitLab CI schema, and the matrix marks what is skipped.
- **Kubernetes: manifest import before API compatibility.** Shoal does not
  implement the kube-apiserver API or Kubernetes clients. It adds
  `shoal import k8s`, which converts a subset of Deployment, Service, Ingress,
  ConfigMap, Secret and Job to Shoal objects and reports every dropped field.
  A compatibility layer can be decided after the import has been used on real
  manifests.
- **Security baseline: restricted defaults.** Every Workload gets a restricted
  default profile (non-root where the image allows, no privilege escalation, all
  capabilities dropped, RuntimeDefault seccomp), with an explicit per-Project
  opt-out (K-11, OB-38). Typed Secrets are added (K-28).
- **Identity: OIDC through GitHub or GitLab, not our own OAuth server.** Users
  and groups are stored in Shoal and linked to provider accounts (OB-41, OB-42).

Out of scope (decided, not deferred): git hosting; kube-apiserver compatibility;
CRDs and operators; admission webhooks; multi-cluster federation; CSI and
network storage (ADR 0016 stays); GitLab Pages, packages, merge request
approvals and review apps; Bitbucket; Windows and macOS nodes.

## Alternatives considered

- **Build git hosting first (a Gitea-like server).** Rejected for this plan.
  It is a large product on its own, and the owner's repositories already live
  on GitHub or GitLab. Integration gives the git-to-deploy path without that
  cost. Revisit only if a user needs Shoal to be the only git host.
- **`docker build` or a privileged builder.** Rejected. It needs a daemon or
  privileged containers, which the security baseline rules out. BuildKit runs
  without either (rootless where the host allows; see Consequences).
- **Source-to-Image (OpenShift S2I).** Rejected for v1. Builder images are a
  large maintenance surface. Buildpacks cover the no-Dockerfile case later.
- **External registry only.** Rejected as the default. A single-VPS install must
  work without another service. The embedded registry is optional for teams that
  already run one, and pull secrets cover that case.
- **Multi-container pods and sidecars.** Rejected for v1. Shoal's Replica runs
  one image (ADR 0009). A helper that needs its own lifecycle is a separate
  Workload behind a Service. Init work runs as a Job before rollout (OB-47).
- **GitLab-compatible `.gitlab-ci.yml`.** Rejected. Implementing its semantics
  would be large and would overpromise compatibility. `shoal-ci.yml` is named
  and documented as its own format. Reading `.gitlab-ci.yml` could be added later.
- **Kubernetes API compatibility as the primary API.** Rejected. Shoal's proto
  API (ADR 0001) is what the CLI, portal and agents use. Import (K-49) gives the
  migration path.
- **CRDs and an operator model.** Rejected. The Database kind shows the pattern:
  an expansion inside the server, with fixed, tested kinds (ADR 0016).
- **An in-house OAuth server (OpenShift style).** Rejected. OIDC integration is
  less code and less risk for a small team.

## Consequences

Positive:

- The git-to-running-app path is complete and short: push, build, registry,
  rollout, with rollback already in place (ADR 0010).
- Projects make multi-user use possible without a second control plane.
- Each phase ends with a testable acceptance criterion (below).

Negative and risks:

- Builds run code from repositories. Build jobs need a build label, CPU and
  memory limits, a deadline (K-17), no host networking or host paths, secret
  mounts instead of environment variables, and a network policy for egress.
  This is the largest security surface added by this ADR.
- Webhook endpoints are internet-facing. Signatures are checked before any
  work starts, and the endpoint is rate-limited.
- Registry storage grows. Garbage collection (CC-09) is part of Phase 5, and it
  must never delete a digest that a tag still points to.
- On a 1 GB host, builds would starve the control plane. Builds therefore run
  only on nodes with the build label. The ADR 0017 five-minute path does not
  change.
- Phase 1 needs K-15 (node selector), which the matrix lists as P1. Phase 1 pulls
  it forward.
- Dependencies (BuildKit, CNCF Distribution, the GitHub and GitLab clients) must
  pass `hack/license_check.py` before merge, as the README requires. This has
  not been checked yet.
- Shoal's "have" surface is wider on paper than in practice. The README says
  multi-host HA and agent failover are not verified. Phase 0 is therefore first.

## Roadmap

Each phase has acceptance criteria. A phase is done when every criterion has a
test or a recorded manual check in `docs/testing.md`.

### Phase 0: hardening

Rows: K-51, CC-03, CC-06.

- Agent failover and replica rescheduling are tested on three separate hosts
  (not loopback). The result is recorded in `docs/testing.md`.
- Control-plane and database backups are exported to an S3-compatible bucket and
  restored to a fresh server in a documented test.
- The ADR 0017 five-minute path still passes on a 1 GB host.

### Phase 1: git to deploy

Rows: K-15 (pulled forward), K-19, K-28, OB-01 to OB-04, OB-11, OB-20 to OB-23,
OB-45, GL-02, GL-40.

- A push to a configured GitHub or GitLab repository starts a Build. A request
  with a bad or missing signature is refused, and a test covers it.
- The build clones the repository at the pushed ref and builds the Dockerfile
  with BuildKit on a build-labelled node. The build does not run on any other
  node.
- The image is stored in the embedded registry under the Project, and a tag row
  points at its digest.
- A Workload with `image_pull_secrets` pulls a private image from that registry.
- The Workload rolls out to the new digest. A failing health check triggers the
  existing rollback (ADR 0010), and a test covers it.
- Build logs are visible with `shoal` and in the portal. A failed build reports
  an errcat code.

### Phase 2: pipelines

Rows: GL-03 to GL-06, GL-08, GL-11, GL-15, GL-27, GL-29, GL-33, GL-37, GL-38,
GL-42, GL-43, K-06, K-07, K-14, K-15.

- A `shoal-ci.yml` with build, test and deploy stages runs on two nodes. A failed
  job stops the later stages.
- Artifacts from one job are downloadable and visible to the next job. They
  expire after `expire_in`, and a test checks the expiry.
- A job with tags runs only on a node whose labels match.
- A manual job holds the deploy stage until an admin approves it.
- Commit status is posted back to the git host.

### Phase 3: multi-tenancy

Rows: OB-36, OB-38, OB-40, OB-41, OB-42, K-26, K-36, K-41, K-43, CC-01, GL-30,
GL-31, GL-44.

- A user in Project A cannot list, read or route to objects in Project B. A test
  covers each kind.
- A user without the role cannot read Secret values. The audit log records the
  write with the actor.
- A create that exceeds a quota is refused with an errcat code. A quota selected
  by label spans two Projects.
- A Workload in Project A cannot reach a Service in Project B unless a policy
  allows it.
- The admin key path (ADR 0007) still works and is documented as break-glass.

### Phase 4: Kubernetes import

Rows: K-02, K-08, K-20, K-31, K-49, OB-30, OB-48, OB-49, OB-50.

- `shoal import k8s` converts a Deployment, Service, Ingress, ConfigMap, Secret
  and Job from a sample app. It prints every dropped field, and a strict mode
  exits non-zero when any field is dropped.
- The imported app reaches Ready with the expected Route and Service.
- Golden tests cover at least five sample manifests.

### Phase 5: operations

Rows: CC-04, CC-05, CC-07, CC-09, K-07 (if not in Phase 2), K-39, K-40, K-42,
K-44, K-45, GL-32, GL-36.

- A NodeNotReady alert fires within the window in ADR 0018, in a test.
- An upgrade from the previous release keeps state and running Workloads, in an
  automated test.
- `shoal drain` moves replicas without breaking a PodDisruptionBudget, in a test.
- Registry garbage collection removes untagged digests past a retention setting,
  and never removes a digest that a tag points to.

## Open questions

1. Which BuildKit mode runs on the build nodes: rootless, or rootful inside a
   dedicated VM? Decide in Phase 1 after a security review.
2. Whether the Project default network policy (K-36) is enforced by the agent's
   iptables rules or by a separate component. Decide in Phase 3.
3. Whether `shoal-ci.yml` should read `.gitlab-ci.yml` as an import format.
   Decide after Phase 2 is used on real repositories.

## Amendment 2026-10-23

Accepted 2026-10-23 by the project owner.

- Review apps move in scope, for pull requests from a branch in the same
  repository only. Fork pull requests never get a review app.
- The work is pass D of ADR 0022: a same-repo pull request gets one Workload
  and one Environment named `review/<branch-slug>`, which stop after a set time
  or when the pull request closes.
- The per-PR hostnames are allowed by ADR 0015 Amendment 2026-10-23, under an
  owner-configured base domain. Routing stays explicit, with no wildcard routes.
- In the Decision section, review apps are removed from the out-of-scope list.
  The rest of that list is unchanged, including fork review apps.
- The parity matrix rows GL-34 (review apps) and GL-33 and GL-35 (environments)
  are updated to match. GL-34 moves from P3 and Skip to P2 and Build, for
  same-repo pull requests only.
