# 0022: Pipelines (shoal-ci.yml)

Status: Accepted under ADR 0019 (Phase 2, items GL-03 to GL-06, GL-08, GL-11,
GL-15, GL-27, GL-29, GL-33, GL-37, GL-38, GL-42, GL-43 of the parity matrix).
Builds (ADR 0021) are the prerequisite: a pipeline job that builds an image
starts a Build and never runs a builder itself. Nothing in this ADR is
implemented yet.

Accepted 2026-10-23 by the project owner.

## Context

ADR 0019 makes Shoal a git-to-running-app platform. A push builds an image
(ADR 0021), the image goes to the embedded registry (ADR 0020), and a Workload
rolls out to it. Builds cover one step. Real repositories need several steps
in order: install, test, build, deploy, with a human gate before production.
That is a pipeline.

The parity matrix has no pipeline object today (GL-03, GL-37, GL-38 are "None").
Shoal has no Job kind either. K-06 (Job) is P1 and is needed by GL-05 (run a
script in a container) and OB-47 (pre-deploy hooks). This ADR introduces that
kind for pipeline work, with the subset of K-06 it needs.

The pipeline format is named `shoal-ci.yml` and is its own format. It takes the
job shape from the GitLab CI schema (`app/assets/javascripts/editor/schema/ci.json`
at gitlab-org/gitlab, the reference ADR 0019 used) and keeps only the keys
listed below. It is not GitLab-compatible: a `.gitlab-ci.yml` is refused, not
guessed at (ADR 0019 "Alternatives").

Constraints from earlier ADRs that shape the design:

- Each Shoal node is small (1 GB on the VPS path, ADR 0017). Jobs must not
  starve the control plane, so they run only on nodes that match their tags.
- ADR 0011: secrets are referenced by name and their values are never returned
  by the API. Job environment variables are a new consumer of that rule.
- ADR 0007: the admin key is full admin. Jobs must not receive it.
- ADR 0010: a Workload update goes through the existing rollout and rollback
  path. A deploy job uses that path and adds no new one.
- ADR 0015 rejects wildcard Route hosts. This matters for review apps (pass D).

## Decision

### 1. Scope of v1

In. Stages (sequential, the GitLab default), jobs with `image` and `script`,
`needs` (stage order plus explicit edges to earlier stages), `rules` on branch,
tag, event and changed paths, `variables`, `secrets` (by name), `artifacts`
with `paths` and `expire_in`, keyed `cache`, `timeout`, `retry` on
infrastructure errors only, `tags` (node placement), `when` (`on_success`,
`always`, `manual`, `never`), `allow_failure`, `environment` with a deploy
step, and a manual approval gate for environments.

Out is listed in section 13 with reasons.

### 2. The file

One file at the repository root, `shoal-ci.yml`, read at the commit that
triggered the run. Top-level keys are `stages`, `variables`, and any other
name is a job. Unknown keys are an error that names the key. There is no
`default`, `include`, `extends`, `!reference`, `workflow` or `services` in v1.
YAML anchors and aliases are allowed because they are plain YAML.

```yaml
# shoal-ci.yml
stages: [build, test, deploy]

variables:
  NODE_ENV: ci

test-web:
  stage: test
  image: docker.io/library/node:22-alpine   # pulled through the registry mirror, ADR 0020
  script:
    - npm ci
    - npm test
  needs: [build-web-image]                  # explicit edge, see section 3
  cache:
    key: npm-v1                             # literal key, see section 6
    paths: [node_modules/]
  secrets:
    NPM_TOKEN: { secret: npm-token, key: token }   # env var from a Secret, ADR 0011
  timeout: 15m
  retry: { max: 2, when: [runner_error] }   # infrastructure failures only
  tags: [linux-amd64]
  rules:
    - if: branch == "main"
    - if: event == "pull_request"
    - changes: ["web/**"]

build-web-image:
  stage: build
  build: web                                # starts Build "web" (BuildConfig, ADR 0021)
  rules:
    - if: branch == "main"
    - if: tag == true

deploy-production:
  stage: deploy
  needs: [build-web-image, test-web]
  environment:
    name: production                        # Environment kind, pass C
    approval: required                      # gate before the job starts
  deploy:
    workload: web                           # Workload in the same cluster
    from: build-web-image                   # use the digest that job's Build pushed
  rules:
    - if: branch == "main"
      when: manual
  timeout: 10m
```

Rules are read top to bottom. The first rule that matches sets the job's
`when`: the rule's own `when` if it has one, otherwise `on_success`. If no rule
matches, the job is not created in that run. A job without `rules` runs when
its stage runs.

`if` is a small expression. Its grammar in v1 is `branch == "<name>"`,
`tag == true`, `event == "<push|pull_request|manual>"`, combined with `&&`.
No other operators, functions or variable references. `changes` is a list of
glob patterns over the paths changed in the commit range.

### 3. Stages, needs and ordering

- Stages run in the order listed. A job in stage N starts when every job it
  `needs` and every job in stage N-1 that is not `allow_failure` has finished
  with Complete.
- `needs` names jobs in an earlier stage in pass A and B. A `needs` edge inside
  the same stage or to a later stage is a validation error. Free-form DAG
  scheduling (GL-10) is pass D or later.
- If a job fails (not `allow_failure`), no job in a later stage starts. Jobs
  that have not started end as Skipped. The run ends Failed.
- `allow_failure: true` lets the run continue. The job still shows Failed.

### 4. Kinds

Three kinds are added to `api/v1alpha1` as Build was (ADR 0021). Their status
has one writer, the pipeline controller, as with Build.

**Pipeline** is the definition for one repository. It is user-appliable.

| Field | Meaning |
|---|---|
| `metadata.name` | DNS label, at most 40 characters |
| `spec.repo` | A git URL, validated as BuildConfig's `spec.git.url` is (ADR 0021) |
| `spec.ref` | Default branch the pipeline reads for manual runs. Default `HEAD` |
| `spec.path` | File inside the repository. Default `shoal-ci.yml` |
| `spec.credentialsSecret` | Name of a Secret for the clone, as in ADR 0021 |
| `status.lastRun`, `status.lastPhase` | Written by the controller |

**PipelineRun** is one execution. The server creates it. The controller writes
its status.

| Field | Meaning |
|---|---|
| `spec.pipeline` | The Pipeline it came from |
| `spec.source` | A copy of the parsed file, taken when the run was created. Later commits do not change a run |
| `spec.trigger` | `manual`, `push`, `pull_request` or `tag` |
| `spec.commit`, `spec.ref` | The commit that was read |
| `spec.trust` | `protected`, `same-repo` or `fork`. Section 7 |
| `spec.cancelRequested` | Set by `CancelPipelineRun` |
| `status.phase` | Build phases: New, Pending, Running, Complete, Failed, Error, Cancelled |
| `status.message` | The failure reason |
| `status.startedAt`, `status.finishedAt` | Times |

**Job** is one job of one run. It generalises K-06 to what this ADR needs.
Its `spec.pipelineRun` is required. A `build:` step does not create a Job's
own builder: the Job references a Build object (ADR 0021) through `status.buildRef`.

| Field | Meaning |
|---|---|
| `spec.pipelineRun`, `spec.name`, `spec.stage` | Ownership and position |
| `spec.image`, `spec.script`, `spec.env` | As in the file. `spec.env` holds the resolved variables. Secret values are not stored here, only references |
| `spec.artifacts`, `spec.cache`, `spec.timeout`, `spec.retry`, `spec.tags` | As in the file |
| `spec.when`, `spec.allowFailure` | As in the file, after rules |
| `spec.deploy`, `spec.build`, `spec.environment` | Optional blocks, one kind of step each |
| `status.phase` | Build phases, plus Manual (waiting for `play` or approval) and Skipped (a stage above failed) |
| `status.node`, `status.exitCode`, `status.message` | Where it ran and how it ended |
| `status.attempt` | Number of tries so far (`retry`) |
| `status.artifactRefs`, `status.logRef` | Artifacts and log name, set on start |
| `status.buildRef`, `status.image`, `status.digest` | Set by a build step, read by a deploy step |

Phases match ADR 0021's Build phases, so the portal and the CLI show one set
of names. The only additions are Manual and Skipped on Job.

**Environment** (pass C) is a named deploy target. Its status holds the
digest currently deployed, the run that deployed it, and the previous digest
for rollback. It is written by the controller only.

### 5. Job execution model on Shoal nodes

The controller decides, the agent runs. Pass A does not start containers. The
controller hands a Job to an `Executor` (the same seam ADR 0021 uses for Builds)
and reads the result.

Placement. A job's `tags` must all match a node's labels (K-15 is pulled
forward to Phase 1 by ADR 0019). A job with no tags runs on any node labelled
`shoal.dev/runner=true`. A job never runs on a node without that label, so a
single-VPS install runs no jobs until a runner node is added.

Container runtime (pass B). Each job is a containerd task from the agent,
the runtime ADR 0006 already uses. The image is pulled from the registry
(ADR 0020), or from a public registry through a configured pull secret.

Isolation, the baseline for every job:

- No `privileged`, no `CAP_SYS_ADMIN`, all capabilities dropped, no new
  privileges, RuntimeDefault seccomp. These match the K-11 baseline in ADR 0019.
- The job's user is non-root where the image allows it. A job that needs root
  inside the container still has no host privilege.
- The root filesystem is read-only except for the workspace volume and `/tmp`.
  Each run gets an empty workspace; nothing is shared between runs.
- No host network, no host paths, no device access, no Docker or containerd
  socket mounted. No Docker-in-Docker (GL-45): a job that builds an image
  uses `build:`.
- CPU, memory and PID limits come from the cluster default. A job may ask for
  less, not more, up to a configured ceiling. The default is set in the server
  config, not in the file.
- The job has egress to the internet and to the registry. It has no ingress,
  and it does not join the service mesh in v1, so it cannot reach Services by
  name.

Rootless. The goal is rootless containers where the node's containerd allows
them. The runtime code in this repository does not set rootless or privileged
options today (checked in `internal/runtime`), so pass B must decide whether
the agent's containerd can run rootless on the build and runner nodes. Until
that is decided, the non-root and no-privilege rules above are the hard
requirement, and "rootless" is a target, not a claim.

Job token. A job that deploys needs to call the control API. It gets a token
scoped to one run, valid for the run's duration plus a short grace, and revoked
when the job ends. The token can call only the deploy action on the Workload
named in that job. It is not the admin key (ADR 0007). Tokens are created by the
server and written to the job environment as a Secret reference, never in
`spec.env`.

Timeout and retry. `timeout` is enforced by the agent, which kills the
container. A timed-out job ends Failed with message `timeout`. `retry` applies
only to `runner_error` (the agent lost the job or the node) and
`node_lost` (the node was marked NotReady). A script that exits non-zero is
never retried. `max` is capped at 2 by the server.

Concurrency. Jobs in one run start in parallel only when their stage allows it.
A node runs at most N jobs at once, N from its config. Runs of different
pipelines are not limited to one at a time, unlike Builds (ADR 0021).

### 6. Variables, secrets, artifacts and cache

- `variables` are plain strings in the file. They are visible in the run
  record and in logs.
- `secrets` map an environment variable name to `{secret, key}`. The Secret is
  checked when the job starts: it must exist and have the key. Its value is
  resolved on the node at job start, as ADR 0011 resolves env secrets, and is
  never written to the Job object, the run record or an artifact.
- Log redaction. The agent replaces each secret value (exact match, and its
  base64 form) with `***` before a log line is stored or streamed. This is
  best-effort: a script that transforms a secret (for example, splitting it)
  can leak parts of it. The docs say so, and secrets are not printed by design.
- Artifacts. `paths` are collected from the workspace after the script ends,
  whatever its exit code, and stored in `internal/objstore` (ADR 0016). They
  expire after `expire_in` (default 7 days, maximum 30 days). A later job that
  lists the producer in `needs` receives them. An expired artifact is removed
  by the same sweep that the registry garbage collection uses (CC-09).
- Cache. A cache is node-local and keyed by a literal `key`. It is a Volume
  bound to one node, so a job that wants the cache is placed on that node
  (ADR 0016 notes Volumes are node-bound). A cache miss is not a failure. Caches
  never cross trust levels (section 7): a fork run cannot read or write a cache
  used by a protected run. Templated keys (for example, a hash of a lock file)
  are a later addition.

### 7. Security: untrusted code and secrets

Pull requests and forks run code that the repository owner did not write.
The rules:

- `spec.trust` is set by the server, not by the file.
  - `protected`: a push to a branch listed as protected in the server config, a
    tag, or a manual run by the admin key.
  - `same-repo`: a pull request from a branch in the same repository.
  - `fork`: a pull request from a fork.
- `protected` runs get secrets, the job token, environment deploys and the
  cache for protected branches.
- `same-repo` runs get no secrets, no job token and no environment, by default.
  A Pipeline can opt in per secret name, and the server still refuses any
  deploy from them.
- `fork` runs get no secrets, no job token, no environment and no cache from
  protected runs. Their pipeline file is read from the base branch, not from
  the pull request, so a fork cannot add a job that uses a secret.
- A deploy job runs only in a `protected` run. This is enforced by the server
  when the job is created, not by the file.
- Artifacts from a `same-repo` or `fork` run cannot feed a deploy step.
- Limits: at most 100 jobs and 10 stages per run, a maximum timeout set by the
  server (default 2 hours), and a maximum artifact size per job set by the
  server. These bound a hostile file.

The webhook receiver is ADR 0021 pass C. A pipeline run from a webhook is
checked by that receiver's signature check before the server reads the file.

### 8. Logs

Each job's output is an ordered list of lines, stored under the job's `logRef`.
The agent streams them through the `LogChunk` path (OB-20). `shoal pipeline
logs RUN JOB` and the portal (GL-38) read the same stream. A finished job's
log is kept with the run and removed when the run is removed, or after a
retention set in the server config. Logs are redacted (section 6) before they
are stored.

### 9. Status reporting (pass D)

When a run starts and ends, the controller posts a commit status to the git
host (GL-27): GitHub's commit statuses or GitLab's commit status API, with
context `shoal/<pipeline-name>` and a link to the run in the portal. The token
is a Secret the Pipeline names. A failed post is retried with backoff and does
not change the run's result.

### 10. Environments, approvals and deploys (pass C)

- An Environment is created by its first deploy job, or by `shoal env create`.
  Its spec sets `requireApproval` (bool) and the approver rule.
- A job whose `environment.approval` is `required` stops in Manual with
  `status.message: waiting for approval`. An approver calls `shoal pipeline
  approve JOB`. The approval records the approver's name and time on the Job.
  A rejection ends the job Cancelled.
- Until Projects and roles exist (ADR 0019, Phase 3, K-41), the approver must
  be the admin key. The approval is recorded so that it can be checked later.
- A deploy step sets the Workload's image to `<repo>@<digest>` taken from the
  build step's `status.digest`, then waits for the rollout under ADR 0010. A
  failed health check rolls back and the job ends Failed. The Environment keeps
  the previous digest so an operator can roll back by hand (OB-24, later).
- `resource_group` (GL-35) is a per-Environment lock: one deploy at a time. A
  second deploy waits in Pending.

### 11. Review apps (pass D)

Review apps are in scope for pass D, for pull requests from a branch in the
same repository only. ADR 0019 listed them as out of scope. The owner moved
them in scope on 2026-10-23 (ADR 0019 Amendment 2026-10-23), and accepted the
per-PR hostname rule in ADR 0015 (Amendment 2026-10-23).

- A same-repo pull request gets one Workload and one Environment named
  `review/<branch-slug>`. The Environment stops after a set time, or when the
  pull request closes.
- Each review app gets a generated hostname under an owner-configured
  review-app base domain, such as `pr-<n>.<base-domain>`. Each hostname is an
  explicit Route record. There is no wildcard Route and no wildcard certificate
  (ADR 0015).
- The record scheme, hostname naming, per-host certificate issuance and rate
  limits are decided in pass D design, not here.
- Forks never get review apps (section 7).

### 12. API and CLI

API (`api.proto`, same pattern as ADR 0021):

- `ApplyPipeline` accepts Pipeline only.
- `StartPipelineRun(pipeline, ref)` creates a run, reads the file at the commit,
  checks the rules, and refuses a missing Secret or a bad file.
- `CancelPipelineRun(run)` sets the flag. A finished run is refused.
- `PlayJob(job)` starts a job in Manual. `ApproveJob(job)` and `RejectJob(job)`
  are for the approval gate (pass C).
- `PipelineRunLogs(run, job)` is unary in pass A and streamed from pass B.

CLI: `shoal pipeline create -f FILE`, `shoal pipeline run PIPELINE [--ref R]`,
`shoal pipeline ls [PIPELINE]`, `shoal pipeline show RUN`, `shoal pipeline logs
RUN JOB`, `shoal pipeline cancel RUN`, `shoal pipeline play JOB`, `shoal
pipeline approve JOB`, `shoal pipeline lint FILE` (parses and checks the file
without a server, with errors that name the key).

### 13. Not supported, and why

| GitLab feature (ci.json key or doc feature) | Status in v1 | Why |
|---|---|---|
| `include`, `extends`, `!reference`, `inherit`, `default` | Refused | Reuse across files needs a resolver and cache; GL-17 is P3 |
| `services` (sidecars) | Refused | One image per Workload (ADR 0009). Each service would need its own Workload (GL-07, P2) |
| `parallel`, `parallel:matrix` | Refused | Copies of a job with variables; GL-16 is P3 |
| `trigger` (child and downstream pipelines) | Refused | GL-18 is P3; needs run nesting |
| `workflow` (name, rules, auto_cancel) | Refused | GL-09 and GL-22 are P2; a run is created by one trigger |
| `before_script`, `after_script`, `hooks` | Refused | `script` is a list and covers the case. `after_script` running on failure needs a separate state model (GL-23 is P3) |
| `only`, `except` | Refused | Deprecated upstream in favour of `rules`; not worth supporting |
| `run:` (step-based jobs) | Refused | Needs the CI steps runtime, which is not in scope |
| `dependencies`, `start_in`, `interruptible` | Refused | GL-25, GL-24, GL-22: P2 or P3 |
| `coverage` (regex), `release`, `pages`, reports (`junit`, `coverage`) | Refused | GL-21, GL-39, GL-19, GL-12 are P3 |
| `id_tokens`, `identity` (OIDC) | Refused | GL-32 needs workload identity (K-42, P2) |
| `dast_configuration`, security scanners | Refused | GL-20 is P3; scanners run as ordinary jobs |
| `secrets` from external vaults | Refused | Shoal Secrets only (ADR 0011). Vault integration is not planned |
| `image` with `entrypoint`, `pull_policy` | Refused | Keep the image default |
| Merge request approvals, merge gating | Out (ADR 0019) | Decided out of scope; commit statuses (section 9) are the integration |
| Packages, Pages, Git hosting | Out (ADR 0019) | Decided out of scope |
| Runners registered per project (GL-44) | Later | Needs Projects (ADR 0019, Phase 3) |
| Pipeline schedules (GL-36) | Later | Needs K-07 cron |
| `needs` as a full DAG, `rulesNeeds` | Later | GL-10 is P2; v1 uses stage order plus edges to earlier stages |
| Cache keys from file hashes, templated keys | Later | Needs a key expression engine |
| `rules: exists` (GL-08) | Later | Needs a file-existence check at run start |

### 14. Keywords

Each key in the file is a GitLab name when it has the same meaning in
`ci.json`. Where meaning differs, the key is renamed or narrowed, and the
table says so.

| Key | Source | Note |
|---|---|---|
| `stages`, `stage`, `image`, `script`, `needs`, `rules`, `variables`, `artifacts`, `cache`, `timeout`, `retry`, `tags`, `when`, `allow_failure`, `environment` | `job_template` in ci.json | Same name, narrower meaning (see sections 2 to 6) |
| `secrets` | `job_template` | Narrowed: `{secret, key}` only, Shoal Secrets only |
| `build` | new | Starts a Build from a BuildConfig (ADR 0021) and waits |
| `deploy` | new | Updates a Workload from a build's digest (section 10) |
| `if`, `changes` | `rules` | Narrowed to the grammar in section 2 |

## Alternatives considered

- **Read `.gitlab-ci.yml` directly.** Rejected for v1. Its semantics (includes,
  extends, child pipelines, a large rule grammar) are a large product. Shoal
  would overpromise compatibility. A reader for it can come after this format
  is used on real repositories (ADR 0019 open question 3).
- **Run builds inside pipeline jobs with `docker build`.** Rejected. It needs a
  daemon or privileged containers, which the security baseline rules out. A
  build job starts a Build instead (ADR 0021).
- **Argo Workflows or Tekton as the engine.** Rejected. Both are Kubernetes
  CRDs. Shoal has no CRDs and no operator model (ADR 0019).
- **Jobs run as Workloads.** Rejected. A Workload is long-running and restarts
  on exit (ADR 0009). A job runs once and reports an exit code, which is what
  the Job kind is for.
- **Use the admin key inside jobs.** Rejected. ADR 0007 keeps it as break-glass.
  The per-run job token is the replacement.
- **Trust the pull request's pipeline file.** Rejected for forks. A fork would
  otherwise add a job that reads a secret.

## Consequences

Positive:

- A repository can declare build, test and deploy in one file, and the run is
  recorded: the file, the commit, each job's result, the image digest.
- A deploy goes through the same rollout and rollback path as any other
  change (ADR 0010). There is one way to change a Workload's image.
- Trust levels make the fork case safe by default, not by review.

Negative and risks:

- Jobs run arbitrary code on the runner nodes. Isolation depends on the
  container runtime's settings. Pass B must test the isolation rules in
  section 5 on a real node before any runner is enabled.
- Redaction is best-effort (section 6). Secrets must be scoped to the smallest
  set of jobs that need them.
- A Job kind is new to the store and API. It is the same kind K-06 names, so
  later Job work (backoff, TTL, deadline) extends it rather than adding a second
  kind.
- The file format is a Shoal format. Users who already have GitLab files must
  rewrite them. The lint command (section 12) reports every unsupported key
  with its reason, so the rewrite is mechanical for the supported subset.
- Review apps depend on the ADR 0015 and ADR 0019 amendments of 2026-10-23 (section 11).

## Implementation plan

Each pass ends with acceptance criteria. A pass is done when each criterion has
a test, or a recorded manual check in `docs/testing.md`.

### Pass A: model, controller, fake runner

Scope: the three kinds, the file parser and linter, the rules and stage
evaluation, the controller state machine, the `Executor` seam with a fake, the
API and the CLI `pipeline` commands. No containers run. A started run whose
jobs have no executor ends Error, as ADR 0021 pass A does for Builds.

Acceptance criteria:

- `shoal pipeline lint` accepts the example in section 2 and rejects an unknown
  key, a `needs` edge inside a stage, and an `include:` key. Each error names
  the key and the line.
- The rules tests cover first-match-wins, no-match omission, `when: manual`,
  and every `if` form in section 2. An `if` with an operator outside the grammar
  is refused.
- With the fake runner: a failed job in stage 1 stops stage 2 and marks its jobs
  Skipped. `allow_failure` lets the run continue. `retry` retries only
  `runner_error` and never a non-zero exit.
- `CancelPipelineRun` cancels a running job and marks not-started jobs Cancelled.
- A timed-out job (fake clock) ends Failed with message `timeout`.
- Artifacts with `expire_in` are removed after expiry in a test, using the
  objstore fake.
- `StartPipelineRun` refuses a missing Secret or a Secret without the named key,
  without reading the value.
- A run's `spec.source` does not change when the file in git changes.

### Pass B: container runner and triggers

Scope: the containerd executor on agents, tag-based placement, the isolation
baseline, the job token, log streaming with redaction, push and pull_request
triggers through the ADR 0021 webhook receiver (so pass B needs ADR 0021 pass
C first, or a manual trigger until then), and the `trust` rules of section 7.

Acceptance criteria:

- On a three-host test (not loopback), a three-job, two-stage pipeline runs a
  script on two nodes, with the second stage starting only after the first
  finishes.
- A job tagged `linux-amd64` never runs on a node without that label (test).
- The isolation rules of section 5 are checked on a real node: the container
  has no added capabilities, no privilege, no host mount, no socket, and the
  root filesystem is read-only outside the workspace. Each rule is a test.
- A secret set in `secrets` is visible to the script and absent from the logs,
  the run record and the artifacts (test uses a known value).
- A fork run gets no secret and no job token (test with a fake fork event).
- A pipeline file from a fork's pull request is not read: the base branch's
  file is used (test).
- Log lines are streamed while the job runs, not only at the end.
- The decision on rootless containers (section 5) is recorded in this ADR's
  follow-ups with the result of the test.

### Pass C: environments and approvals

Scope: Environment kind, the deploy step, the manual approval gate, the
`resource_group` lock and the approval record.

Acceptance criteria:

- A deploy job with `from: build-web-image` sets the Workload's image to the
  build's digest and waits for the rollout (test with the existing rollout
  path).
- A deploy job whose health check fails rolls back, ends Failed, and leaves the
  Environment at its previous digest (test).
- A deploy job with `approval: required` stays in Manual until `shoal pipeline
  approve`. The approval records the approver and time. A rejection ends the
  job Cancelled (test).
- A second deploy to the same Environment waits in Pending while the first runs
  (test).
- A deploy job in a `same-repo` or `fork` run is refused when the job is
  created (test).
- The job token can perform the deploy action on the named Workload only, and
  is refused for any other action or Workload (test).

### Pass D: commit status and review apps

Scope: commit statuses to GitHub and GitLab (section 9). Review apps (section
11), with the ADR 0015 and ADR 0019 amendments of 2026-10-23 in place. Free-form
`needs` (GL-10) and cache key templates are candidates for this pass or later.

Acceptance criteria:

- A run posts a `pending` status when it starts and a `success` or `failure`
  when it ends, with context `shoal/<pipeline-name>`, to a test repository on
  each host (manual check recorded in `docs/testing.md`, since the hosts are
  external).
- A failed status post is retried and does not change the run's result (test
  with a fake host that fails twice).
- Review apps: a same-repo pull request gets
  one Environment and one Workload, which stop after the set time or when the
  pull request closes (test). A fork pull request gets none (test).

## Open questions

1. Whether the agent's containerd can run jobs rootless (section 5). Decide in
   pass B on a real node.
2. Whether `protected` branches come from the server config alone, or also from
   the git host's protection rules (GL-28). The server config is the decision
   for v1.
3. Whether approvals need an audit record separate from the Job (OB-38, Phase 3).
4. Whether the job token's grace period should be zero. Decide in pass C.
5. Whether `needs` should accept edges to later stages once GL-10 lands, or
   stay limited to earlier stages.

## Follow-ups

- Done with this change: ADR 0019 Amendment 2026-10-23 and ADR 0015 Amendment
  2026-10-23 (review apps and per-PR hostnames).
- K-06 (Job) backoff, deadline and TTL reuse the Job kind from section 4.
- GL-16 (`parallel`), GL-18 (`trigger`) and GL-17 (`include`) are candidates
  for a later ADR once v1 is used on real repositories.
