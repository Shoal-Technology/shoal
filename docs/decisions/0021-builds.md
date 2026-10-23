# 0021: Builds

Status: Accepted under ADR 0019 (Phase 1, items OB-01 to OB-04, OB-20, OB-21
and OB-45 of the parity matrix). Pass A is implemented: the two kinds, their
validation, the controller with an `Executor` interface and a fake executor,
the API, and the CLI. The server runs without an executor in pass A, so a
started Build ends in phase Error until pass B adds the BuildKit executor. The
follow-ups are in "Follow-ups".

## Context

Images are plain strings today (`WorkloadSpec.image`, appspec `image:`). ADR
0020 gives Shoal a registry to push to. What is missing is a recipe that turns
a git repository into an image, and a record of each run. The parity matrix
(OB-01, OB-02) asks for OpenShift's `BuildConfig` and `Build` shapes, so users
recognise them, without OpenShift's `ImageStream` model. ADR 0019 scopes this
work to git source and Dockerfile builds, built in Shoal.

The design follows the existing kinds: an object with a spec and status, a
store record per object (ADR 0002), a controller that reconciles in passes
(ADR 0005), and secrets referenced by name (ADR 0011).

## Decision

### 1. Kinds

**BuildConfig** is the recipe. It is user-appliable, like Database.

| Field | Meaning and default |
|---|---|
| `metadata.name` | DNS label, at most 40 characters, so a Build name fits |
| `spec.git.url` | `https://host/path`, `ssh://host/path` or `git@host:path`. Required. Other schemes, a leading `-`, and credentials in the URL are refused |
| `spec.git.ref` | Branch, tag or commit. Default `HEAD`, the repository's default branch |
| `spec.git.contextDir` | Build context inside the repository. Default `.` |
| `spec.git.dockerfile` | Dockerfile inside the context. Default `Dockerfile` |
| `spec.credentialsSecret` | Name of a Secret: type `kubernetes.io/basic-auth` for https, `kubernetes.io/ssh-auth` for ssh. Optional |
| `spec.output.repository` | Repository in the embedded registry. Default: the config's name |
| `spec.output.tag` | Template: `{short-sha}` (default), `{sha}` or `{ref}` |
| `spec.buildArgs` | Dockerfile `ARG` values: at most 64, one line each |
| `status.lastBuild`, `status.lastPhase` | The newest Build and its phase. Written by the controller |

**Build** is one run. The server creates it; the controller writes its status.

| Field | Meaning |
|---|---|
| `metadata.name` | `<config>-<5 random characters>` |
| `spec.config` | The BuildConfig the run came from |
| `spec.source` | A copy of the config's spec when the run started. Later edits to the config do not change the run |
| `spec.trigger` | `manual` in pass A. Webhooks (OB-11, OB-12) are pass C |
| `spec.cancelRequested` | Set by `CancelBuild` |
| `status.phase` | See the table below |
| `status.message` | The failure reason, or what the phase means |
| `status.startedAt`, `status.finishedAt` | Times of the start and of the end |
| `status.commit` | The commit the run resolved to. Set on success |
| `status.image`, `status.digest` | The `repository:tag` pushed and its manifest digest. Set on success |
| `status.logRef` | The name the executor keeps the output under. Set when the run starts |

Phases follow OpenShift's `BuildPhase` (OB-02). Terminal phases are Complete,
Failed, Error and Cancelled.

| Phase | Meaning |
|---|---|
| New | Created, not yet looked at by the controller |
| Pending | Waiting: an earlier Build of the same config is running |
| Running | The executor has the job |
| Complete | The build ran and the image was pushed |
| Failed | The build ran and failed (for example, the Dockerfile is missing) |
| Error | An infrastructure failure: no executor, the job did not start, or it was lost |
| Cancelled | Stopped on request, or cancelled before it started |

### 2. Git access and credentials

- Credentials never go in the URL and are never returned by the API. `Get`
  and `List` already redact Secret values, so this holds for the Secret too.
- A Secret is checked when a build starts: it must exist and have the type the
  URL needs. The check does not read the value. A BuildConfig may be applied
  before its Secret.
- Pass A adds two Secret types, `kubernetes.io/basic-auth` (`username`,
  `password`) and `kubernetes.io/ssh-auth` (`ssh-privatekey`, with an optional
  `known_hosts`). They follow the existing typed-secret rules (ADR 0011
  addendum): the keys are checked at apply time, and an error names keys, not
  values.
- The ssh `known_hosts` is optional here. Pass B refuses to connect to an ssh
  host without a host key, so a build never trusts an unknown host.

### 3. Output and tags

The repository is an OCI repository name, so `team/web` is valid. The image is
`<registry>/<repository>:<tag>`, pushed to the embedded registry.

The brief proposed `<project>/<name>:<short-sha>` as the default. There is no
Project kind yet (OB-36), so the default is `<name>:<short-sha>`. The project
prefix arrives with Project scoping.

A tag template is checked on apply by expanding it with a sample commit. The
tag must match the OCI tag grammar. `{ref}` turns characters outside
`[A-Za-z0-9._-]` into `-`.

### 4. Controller

The controller (`internal/controller/builds.go`) runs one pass per reconcile:

1. Builds are listed before configs. A Build is created after its config, so
   a config listed later is never missing for one of them.
2. A Build whose config is gone is removed. If it is running, it is cancelled
   first, and if the cancel fails it stays for the next pass.
3. Per config, Running Builds are read from the executor. Success records the
   commit, image and digest. A failure records the message. An unknown job is
   Error.
4. A Running Build that has `cancelRequested` is cancelled and set to
   Cancelled. A waiting Build with that flag is set to Cancelled and never
   starts.
5. Of the waiting Builds, the oldest starts when none of the config runs. The
   others are Pending. One Build runs at a time per config. Builds of different
   configs run in parallel.
6. The config's status is written, then finished Builds beyond the newest 10
   are removed. Builds that have not finished are never removed.

Only the controller writes Build status (ADR 0002: a status has one writer).
The controller sets `finishedAt` on every terminal phase.

### 5. Executor interface

`internal/build` defines the seam:

```go
type Executor interface {
    Start(ctx context.Context, job Job) error
    Status(ctx context.Context, name string) (Status, error) // ErrUnknownJob if unknown
    Cancel(ctx context.Context, name string) error            // ok for a finished job
    Logs(ctx context.Context, name string) ([]Line, error)
}
```

`Config.Executor` is nil in pass A. The controller then sets each started
Build to Error with the message "no build executor is configured on this
server". `internal/build.Fake` is an in-memory executor for the tests. It
records starts and cancels and lets a test finish a job.

### 6. API and CLI

- `Apply` accepts BuildConfig only. A Build cannot be applied.
- `StartBuild(config)` returns the new Build's name. It refuses a missing
  config and a missing or wrongly typed Secret (FAILED_PRECONDITION).
- `CancelBuild(build)` sets the flag. A finished Build is refused.
- `BuildLogs(build)` returns the executor's lines. It is unary in pass A.
- `Delete` of a Build refuses one that has not finished: cancel it first.
  Deleting a BuildConfig removes its Builds through the controller.
- CLI: `shoal build create-config -f FILE`, `shoal build start CONFIG`,
  `shoal build ls [CONFIG]`, `shoal build logs BUILD`, `shoal build cancel
  BUILD`. `shoal get bc` and `shoal get builds` also work.

### 7. Triggers (pass C)

- Webhooks (OB-11, OB-12): a GitHub or GitLab push, or a generic call with a
  per-config token in a header.
- On success, update a Workload's image to the pushed digest through the
  existing rollout (OB-45). The Workload's rollback path (ADR 0010) covers a
  bad image.
- The appspec `build:` section, mapped to a BuildConfig. The appspec stays
  rejecting `build:` until then.

## Pass B plan (not in this change)

The BuildKit executor needs decisions that this ADR does not settle:

- **Placement.** Builders run on nodes with the label `shoal.dev/builder=true`.
  This needs node selection (K-15), which the scheduler does not do yet.
- **Push identity.** ADR 0020 gives node certificates pull access only. A
  builder must push. The recommended route is a third certificate role,
  `shoal:builder`, issued to a builder node and revoked with its Node object
  as the node role is. Pass B needs this decided before it starts.
- **Runtime.** `buildkitd`, rootless, as a pinned containerd task with the
  versions of ADR 0006. The context comes from a git clone in a separate step.
  The credentials Secret is resolved on the node from the snapshot, as env
  secrets are.
- **Logs.** Output goes through the `LogChunk` path (OB-20), with secret values
  redacted.

## Consequences

- A Build records what it built: the commit, the image and the digest. A
  rollback can name a digest (OB-24 and OB-26 later).
- A running build does not change when its config changes. The config is a
  template, and a run is a record.
- Pass A ships with no executor, so the server cannot build yet. That is
  deliberate: the state machine, the queue and the API are tested with a fake,
  and no build runs on the shared VPS until pass B has a verified executor.
- The phase names differ from the brief's Pending, Running, Succeeded, Failed,
  Cancelled. OB-02 asks for OpenShift's names, so the success phase is
  Complete. The names are constants in `internal/resource/build.go`, so
  changing one is a small edit.

## Follow-ups

- Pass B: the BuildKit executor, builder placement, the push identity, and
  streamed logs (see the plan above).
- Pass C: webhooks, the on-success Workload update (OB-45), and the appspec
  `build:` section.
- Later: a run policy with latest-only (OB-16), resources and timeouts (OB-18),
  rerun (OB-19), Dockerfile-less builds (OB-08), and Project scoping (OB-36).
