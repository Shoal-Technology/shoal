# Platform parity matrix: Kubernetes, OpenShift (OKD), GitLab

Status: Accepted (see [ADR 0019](../decisions/0019-shoal-platform-scope.md))

This matrix compares upstream concepts with what Shoal does today, and
proposes one of three outcomes for each: **Build** in Shoal, **Integrate**
with an external system, or **Skip** with a reason. It is the input to ADR
0019. Every row says where its upstream fact came from. Rows marked
*(inferred)* are design guesses that have not been tested. Rows marked
*(docs knowledge)* name upstream features that are not in the fetched
sources, so they are not verified against code.

## 0. Sources and method

Upstream, read from shallow, sparse, blobless clones (no code was run):

| Source | Revision | Files read |
|---|---|---|
| kubernetes/api | `2173a23` | `core/v1/types.go`, `apps/v1/types.go`, `batch/v1/types.go`, `networking/v1/types.go`, `autoscaling/v2/types.go`, `rbac/v1/types.go`, `policy/v1/types.go`, `storage/v1/types.go` |
| openshift/api | `c3d7aef` | `build/v1`, `image/v1`, `route/v1`, `project/v1`, `apps/v1` (DeploymentConfig), `template/v1`, `security/v1`, `quota/v1`, `oauth/v1`, `user/v1` `types.go` |
| gitlab-org/gitlab | `9d3d93f7` | `app/assets/javascripts/editor/schema/ci.json` (the GitLab CI schema) |

Shoal (`main` at `484097e`): `api/v1alpha1/resources.proto`, `api/v1alpha1/api.proto`,
`api/v1alpha1/doc.go`, `internal/resource/kinds.go`, `internal/appspec/spec.go`,
`internal/errcat/errcat.go`, `internal/controller/scheduler.go`,
`internal/objstore/objstore.go`, `internal/store/store.go`, `cmd/shoal/*.go`,
`docs/architecture.md`, `docs/decisions/0001` to `0018`, `README.md`.

Priority scale. It maps to the phases in ADR 0019:

- **P0**: Phase 0 or 1. Hardening, and the git-to-deploy path.
- **P1**: Phase 2 or 3. Pipelines and multi-tenancy.
- **P2**: Phase 4 or 5. Kubernetes import, operations, polish.
- **P3**: not planned. Skip, or revisit after the phases above.
- **have**: Shoal already does this. No priority.

Each row's Shoal column uses exact names: a proto message, an ADR, a CLI
command, or a file. "none" means nothing matching was found in code or docs.

## 1. Shoal baseline

| Shoal kind | Defined in | Notes |
|---|---|---|
| Workload | `api/v1alpha1/resources.proto` `WorkloadSpec` | One image, replicas, env, ports, health check, rolling update, rollback (ADR 0010), volumes, files |
| Replica | `resources.proto` `ReplicaSpec` | Server-made; holds a template copy; runs one DB job at a time (ADR 0016) |
| Node | `resources.proto` `NodeSpec`, `NodeStatus` | Labels set with `shoal label node`; `shoal.dev/edge` marks an edge (ADR 0015) |
| Service | `resources.proto` `ServiceSpec` | Ready replicas of one Workload; cluster IP; DNS `<name>.shoal` (ADR 0012) |
| Route | `resources.proto` `RouteSpec` | host, path_prefix, service, port, tls auto/none/secret, redirect_http (ADR 0015) |
| Volume | `resources.proto` `VolumeSpec` | Node-local; RWO only; retention Retain/Delete; bound to first mount's node (ADR 0016) |
| Database | `resources.proto` `DatabaseSpec` | Postgres only; expands to Volume, Workload, Service, Secret (ADR 0016) |
| Secret / Config | `resources.proto` | data map; Secret encrypted at rest and values never returned (ADR 0011); Config max 256 KiB (`internal/resource/kinds.go`) |
| Certificate, AcmeAccount, AcmeChallenge, JoinToken, UIAuth, DatabaseBackupRecord | `resources.proto`, `doc.go` | Internal kinds |

Not present in Shoal: Namespace or Project, RBAC (ADR 0007: "no RBAC, a non-goal"),
Job, CronJob, Build, image registry, git integration, pipelines, quotas,
audit log, pull secrets, security context, label-based scheduling.

## 2. Kubernetes

### 2a. Workloads and scheduling

Source: `kubernetes-api/core/v1/types.go`, `kubernetes-api/apps/v1/types.go`.

| Upstream concept | What it does | Shoal today | Gap | Proposed design | Priority |
|---|---|---|---|---|---|
| **K-01** Pod, PodSpec, Container | Smallest schedulable unit; one or more containers sharing network and volumes | Replica runs exactly one container image (`WorkloadSpec.image`) | No init containers, no sidecars, no multi-container pods | Skip sidecars: run the helper as its own Workload plus Service on the mesh *(inferred)*. Build init work as a Job that runs before rollout (see OB-47). | P1 |
| **K-02** Deployment, DeploymentStrategy (RollingUpdateDeployment: maxUnavailable, maxSurge) | Declarative replicas with rolling update and rollback | Workload: `update.max_unavailable`, `progress_deadline_seconds`, rollback to `rollout.stable_template` (ADR 0010) | No `max_surge`, no Recreate strategy, no revision history beyond the stable template | Build: add `max_surge` and a recreate option to `UpdateStrategy`; bounded history of stable templates | P2 |
| **K-03** ReplicaSet (ReplicaSetSpec) | Keeps N pods of one template | Replica objects, one per instance, each with a template copy (ADR 0009) | None of substance | Have. Keep. | have |
| **K-04** StatefulSet (StatefulSetSpec, volumeClaimTemplates) | Ordered pods with stable identity and per-pod storage | Database: one Postgres replica with one Volume (ADR 0016) | No N-replica stateful set | Skip for v1. Revisit only with replicated storage; ADR 0016 lists that as out of scope | P3 |
| **K-05** DaemonSet (DaemonSetSpec) | One pod per matching node | None. Node roles are labels (`shoal.dev/edge`), not per-node workloads | No "every node" placement | Build later: placement "all Ready nodes" on Workload, for agents or log shippers | P3 |
| **K-06** Job (JobSpec: backoffLimit, suspend, ttlSecondsAfterFinished, podFailurePolicy; PodSpec.activeDeadlineSeconds) | Run-to-completion work with retries, deadline and TTL | No Job kind. The DB backup/restore job path (ADR 0016 pass D, `DatabaseJob`) and Workload `restart_policy` Never/OnFailure exist | No Job kind, backoff, deadline, TTL, or per-run status | Build: a Job kind that reuses the Replica job runner. CI jobs (GL-05) and pre-deploy hooks (OB-47) depend on it | P1 |
| **K-07** CronJob (CronJobSpec: schedule, concurrencyPolicy, history limits) | Scheduled Jobs | `DatabaseBackup.schedule` is an interval only, minimum 1h (ADR 0016 section 3.4); cron deferred (ADR 0016 section 5) | No cron; no concurrency policy | Build on K-06 with a cron parser; reuse the backup scheduler | P2 |
| **K-08** Probes: liveness, readiness, startup (Container.LivenessProbe, ReadinessProbe, StartupProbe) | Restart on liveness failure, gate traffic on readiness, allow slow starts | One `HealthCheck` (http, tcp or exec) that both gates readiness and restarts after `failure_threshold` (ADR 0010) | No separate readiness and liveness; no startup probe (`initial_delay_seconds` approximates) | Build: optional separate readiness probe; keep the combined default | P2 |
| **K-09** Lifecycle hooks: postStart, preStop (Lifecycle, LifecycleHandler) | Run code at container start and before stop | None | No hooks | Skip for v1. Pre-deploy work goes through OB-47 | P3 |
| **K-10** Resource requests vs limits (ResourceRequirements) | Scheduling request separate from enforced limit | `Resources.cpu` and `memory` are used for scheduling and enforced as hard limits (`WorkloadSpec.resources`) | No separate requests; no ephemeral storage | Build: split into requests and limits; keep limits as default | P2 |
| **K-11** SecurityContext, PodSecurityContext, Capabilities, SeccompProfile | Per-container and per-pod privilege settings | No security fields in `WorkloadSpec`. The containerd runtime sets none of these; the only capability use is CNI portmap (`internal/runtime/containerd/network_linux.go`) | No Shoal-enforced baseline; containers run with runtime defaults | Build: a restricted default for every Workload (non-root where the image allows, no privilege escalation, drop all capabilities, RuntimeDefault seccomp), with an explicit opt-out per Project. Ties to OB-38 | P1 |
| **K-12** Affinity, PodAntiAffinity | Co-locate or spread pods by rule | Scheduler: free-capacity fraction, then fewer replicas of the same workload, no host-port clash (ADR 0009) | No affinity rules | Skip. The spread tie-break is enough for 1 to 20 nodes *(inferred)* | P3 |
| **K-13** TopologySpreadConstraint | Even spread across topology domains | Same as K-12 | None | Skip | P3 |
| **K-14** Taint, Toleration, NodeSpec.taints | Repel pods from nodes unless tolerated | None. `NodeSpec.labels` exist but the scheduler does not read them (`internal/controller/scheduler.go`) | No taints | Build with K-15: dedicated build and runner nodes | P2 |
| **K-15** PodSpec.nodeSelector | Place pods on labelled nodes | Labels are set with `shoal label node`; scheduler ignores them | No placement by label | Build: `node_selector` on `WorkloadSpec`, checked by the scheduler. Needed for runners (GL-42) and builds (OB-18) | P1 |
| **K-16** PodSpec.priorityClassName, preemption | Important pods preempt others | None | None | Skip | P3 |
| **K-17** PodSpec.activeDeadlineSeconds | Hard deadline for a pod | None | None | Build with K-06 | P1 |
| **K-18** PodSpec.restartPolicy (Always, OnFailure, Never) | What happens on exit | `WorkloadSpec.restart_policy`: Always (default), OnFailure, Never | None | Have. | have |
| **K-19** PodSpec.imagePullSecrets | Credentials to pull private images | `WorkloadSpec.image_pull_secrets` (appspec `imagePullSecrets`) names dockerconfigjson Secrets. The server sends the logins to nodes over mTLS, and the containerd resolver logs in per registry host (ADR 0011 addendum; PR #3) | None | Have. | have |
| **K-20** EnvFromSource (ConfigMapEnvSource, SecretEnvSource) | Import every key as an env var | `EnvVar.value_from` takes one key from a Secret or Config (`EnvSource`) | No bulk import | Build: `env_from` with optional prefix. Small | P2 |

### 2b. Services, networking, config and storage

Source: `kubernetes-api/core/v1/types.go`, `kubernetes-api/networking/v1/types.go`, `kubernetes-api/storage/v1/types.go`.

| Upstream concept | What it does | Shoal today | Gap | Proposed design | Priority |
|---|---|---|---|---|---|
| **K-21** Service, ServiceType ClusterIP | Stable virtual IP and DNS for pods | Service: cluster IP, DNS `<name>.shoal` on every node, ready endpoints (ADR 0012) | None of substance | Have. | have |
| **K-22** ServiceType NodePort | Expose a port on every node | `Port.host_port` publishes on the node that runs the replica (`WorkloadSpec.ports`) | No Service-level NodePort; port follows the replica | Skip. `host_port` covers the common case | P3 |
| **K-23** ServiceType LoadBalancer, LoadBalancerStatus | External L4 address from a provider | None. HTTP ingress is the Route on edge nodes (ADR 0015) | No L4 load balancer | Skip for v1. Integrate a cloud LB later if a user needs it | P3 |
| **K-24** ServiceType ExternalName | DNS alias to an external host | None | None | Skip | P3 |
| **K-25** Endpoints, EndpointSubset | Ready backend addresses | `ServiceStatus.endpoints` (ready replica IPs) | Not a standalone object | Have. | have |
| **K-26** Namespace, NamespaceSpec, NamespaceStatus | Scope for names, RBAC and quotas | None. Every kind is cluster-wide. `RouteSpec.service` is "cluster-wide" (`resources.proto`) | No tenancy scope | Build as a Project (OB-36) | P1 |
| **K-27** ConfigMap (data, binaryData, immutable) | Plain configuration | `Config`: `spec.data`, max 256 KiB (`MaxDataBytes`) | No binaryData; no immutable flag | Keep. Skip binaryData. Immutable is a small later addition | P3 |
| **K-28** Secret types (core/v1 SecretType constants: Opaque, kubernetes.io/tls, kubernetes.io/dockerconfigjson, service-account-token and others) | Typed secrets with validation | `Secret.type` takes Opaque (default), kubernetes.io/tls or kubernetes.io/dockerconfigjson. `resource.ValidateSecret` checks the data against the type on every apply. Values are still never returned (ADR 0011 addendum; PR #3) | service-account-token skipped (K-42) | Have. | have |
| **K-29** PersistentVolume, PersistentVolumeClaim (PersistentVolumeClaimSpec: accessModes, storageClassName, resources) | Storage claimed separately from pods, bound to a volume | `Volume`: size, access_mode RWO only, owner, mode, retention; bound to a node on first mount (ADR 0016 section 1) | No claim object; no RWX; no resize; no move between nodes | Keep Volume. Skip the claim split. RWX only with a networked store (P3) | P3 |
| **K-30** StorageClass, CSIDriver, VolumeAttachment (storage/v1) | Dynamic provisioning through drivers | None. Backends are loop or dir (`internal/volume`) | No provisioning plugins | Skip for v1. ADR 0016 lists CSI-like plugins as out of scope | P3 |
| **K-31** EmptyDirVolumeSource | Pod scratch space | None | No ephemeral volume | Build as a Volume with retention Delete (`VolumeSpec.retention`) | P2 |
| **K-32** HostPathVolumeSource | Mount a host directory | None | Intentional absence | Skip: security risk, and Volume is the managed alternative | P3 |
| **K-33** ProjectedVolumeSource, DownwardAPIVolumeSource | Combined secret, config and token files; pod metadata files | `FileMount` mounts one Secret or Config per path (`resources.proto`) | No projection | Skip. FileMount covers the common case | P3 |
| **K-34** Ingress (IngressSpec, IngressRule, HTTPIngressPath, IngressTLS) | HTTP host and path routing with TLS | `Route`: host, path_prefix (longest wins), service, port, tls, redirect_http (ADR 0015) | No exact or implementation-specific path types | Have. Exact path type is a small later addition | have |
| **K-35** IngressClass (IngressClassSpec) | Choose the controller for an Ingress | None. The edge is a node label (`shoal.dev/edge`, `internal/resource/route_status.go`) | One edge model | Skip. Multi-edge is an operator step (ADR 0017 open question 4) | P3 |
| **K-36** NetworkPolicy (NetworkPolicySpec, ingress and egress rules, NetworkPolicyPeer) | Allow rules between pods | None. Flat WireGuard mesh with service VIPs (ADR 0012) | No isolation between workloads or tenants | Build with Projects: default-deny between Projects, allow lists per Service. Enforcement may reuse the agent's iptables use *(inferred)* | P1 |
| **K-37** ServiceCIDR, IPAddress (networking/v1) | Cluster IP range management | Node `pod_cidr` assigned from the cluster CIDR (`NodeSpec.pod_cidr`) | Fixed range | Skip | P3 |

### 2c. Batch, policy, autoscaling, RBAC, control plane

Source: `kubernetes-api/autoscaling/v2/types.go`, `kubernetes-api/policy/v1/types.go`, `kubernetes-api/rbac/v1/types.go`, `kubernetes-api/core/v1/types.go`.

| Upstream concept | What it does | Shoal today | Gap | Proposed design | Priority |
|---|---|---|---|---|---|
| **K-38** HorizontalPodAutoscaler (HorizontalPodAutoscalerSpec, MetricSpec) | Scale replicas from metrics | `WorkloadSpec.replicas` is a fixed integer. Node metrics exist (`internal/nodestat`, ADR 0018) but are not per Workload | No autoscaling | Skip for v1. Revisit with a CPU target only | P3 |
| **K-39** PodDisruptionBudget (PodDisruptionBudgetSpec: minAvailable, maxUnavailable) | Limit voluntary disruption | `UpdateStrategy.max_unavailable` applies to rollouts only | No budget used by drain or eviction | Build with K-40 | P2 |
| **K-40** Eviction, cordon, drain (Eviction; NodeSpec.unschedulable) | Voluntary eviction and node maintenance | Involuntary only: replicas on a node NotReady for 60s are deleted and re-placed (ADR 0009). `shoal remove` deletes a node (`cmd/shoal/cluster.go`) | No cordon, no drain, no eviction API | Build: `unschedulable` on Node plus `shoal cordon` and `shoal drain`, respecting K-39 | P2 |
| **K-41** Role, ClusterRole, RoleBinding, ClusterRoleBinding (PolicyRule, Subject, RoleRef) | Permissions by verb and resource | Two roles only: `shoal:admin` and `shoal:node` in the certificate subject (ADR 0007). The admin key is full admin | No per-user or per-project permissions | Build: Role and RoleBinding scoped to a Project, with identities from OB-41 and OB-42. Keep the ADR 0007 admin path as the break-glass route | P1 |
| **K-42** ServiceAccount, ServiceAccountTokenProjection | Identity for a pod | None. Workloads have no identity to the API or to external services | No workload identity | Integrate later: short-lived OIDC tokens to workloads (same design as GL-32) | P2 |
| **K-43** ResourceQuota (ResourceQuotaSpec.hard, ResourceQuotaStatus) | Caps total usage per namespace | None. Disk is counted per node for Volumes (`VolumeStatus.quota`, `NodeCapacity.disk_bytes`) | No per-tenant caps | Build per Project, reusing node capacity totals | P1 |
| **K-44** LimitRange (LimitRangeSpec, LimitRangeItem) | Default and maximum per container | Defaults are in code for Database (500m CPU, 384Mi, ADR 0017) only | Defaults not per Project | Build with K-43 | P2 |
| **K-45** Event, EventSource, EventSeries | Human-readable history of object changes | None user-visible. `store.Event` is an internal watch type (`internal/store/store.go`). Status messages and errcat codes exist (`internal/errcat`) | No event list | Build: user-visible events in the store with a TTL; `shoal events` | P2 |
| **K-46** NodeStatus conditions, allocatable, addresses (NodeCondition, NodeAddress) | Node health and capacity | `NodeStatus`: phase Ready or NotReady, capacity (cpu, memory, disk), heartbeat, address, WireGuard fields (`resources.proto`) | No condition list; no allocatable | Keep. Add conditions when alerts land (ADR 0018, "Not yet") | P3 |
| **K-47** Custom resources and operators (apiextensions; not in the fetched set) | Extend the API with new kinds | Fixed kinds in code (`internal/resource/kinds.go`, `api/v1alpha1/doc.go`). Database is a built-in expansion | No extension point | Skip. The Database expansion is the pattern for managed services | P3 |
| **K-48** Admission webhooks (not in the fetched set) | Policy checks at write time | In-process validation only (`internal/resource/kinds.go`, `Prepare`) | No extension point | Skip. Built-in defaults (K-11, OB-38) cover the need | P3 |
| **K-49** Kubernetes manifest import (Deployment, Service, Ingress, ConfigMap, Secret, Job) | Reuse existing YAML | `apply` takes Shoal `Object` messages and `shoal.yaml` only (ADR 0017) | No Kubernetes YAML | Integrate: `shoal import k8s` converts a subset to Shoal objects and reports every dropped field. ADR 0019 puts this before API compatibility | P2 |
| **K-50** kube-apiserver REST API and client compatibility | Any Kubernetes client talks to the cluster | Shoal gRPC Control API (`api/v1alpha1/api.proto`, service `Control`) | No Kubernetes API | Skip. K-49 is the compatibility path | P3 |
| **K-51** Control-plane HA (leader election; etcd is not in the fetched set) | Replicated control plane | Raft store with three servers (ADR 0014). README: verified on one runner with loopback addresses only; multi-host not verified | Multi-host HA unverified | Have, and verify. Phase 0 work | P0 |

## 3. OpenShift (OKD)

### 3a. Builds

Source: `openshift-api/build/v1/types.go` (BuildConfig, Build, BuildSource, BuildStrategy, BuildTriggerPolicy, BuildOutput, BuildRequest, BuildLog).

| Upstream concept | What it does | Shoal today | Gap | Proposed design | Priority |
|---|---|---|---|---|---|
| **OB-01** BuildConfig (BuildConfigSpec, CommonSpec: source, strategy, output, resources, nodeSelector) | Stored recipe that produces images | Have, pass A (ADR 0021): the `BuildConfig` kind with a git URL, ref, context directory, Dockerfile, credentials Secret, output repository and tag template, and build args. Validated on apply. The appspec `build:` key is still rejected (`internal/appspec/spec.go`) | No webhook or workload trigger; no resources or nodeSelector yet | Have. Pass C: on-success trigger (OB-45) and the appspec `build:` section. Pass B: the BuildKit executor | P0 |
| **OB-02** Build run (BuildSpec, BuildStatus; BuildPhase New, Pending, Running, Complete, Failed, Error, Cancelled) | One execution with a phase and logs | Have, pass A (ADR 0021): a `Build` kind with these phases, start and finish times, the resolved commit, the pushed image and digest, and a log reference. One run per config at a time, and the last 10 finished runs kept | Pass A has no executor: a server without one marks each Build Error | Have the records. Pass B supplies the executor | P0 |
| **OB-03** BuildSource type Git (GitBuildSource: uri, ref; contextDir) | Clone a repository at a ref | Have (pass B, ADR 0021): `BuildConfig.spec.git` (url, ref, contextDir, dockerfile). The builder clones with the host's `git`. https uses a basic-auth Secret through `GIT_ASKPASS`. ssh uses an ssh-auth Secret and refuses a host with no `known_hosts`. Credentials stay out of argv, the URL and the log | Proven in CI only for a public https repository (`build-e2e.yml`). Private https and ssh are covered by unit tests only. Builders need `git` on the host | Build (ADR 0021, pass B) | P0 |
| **OB-04** BuildSource Dockerfile; DockerBuildStrategy (buildArgs, dockerfilePath) | Build an image from a Dockerfile | Have (pass B, ADR 0021): a rootless BuildKit daemon (`moby/buildkit:v0.32.2-rootless`, pinned by digest) runs as a container on a node labelled `shoal.dev/builder=true`. `buildctl` builds the Dockerfile with `buildArgs`, `contextDir` and `dockerfile` from the spec | Rootless operation under this containerd is unverified until `build-e2e.yml` passes. Shoal runs the container with containerd's default profile, with no seccomp or AppArmor setting. No layer cache, no resource limits, and one 90-minute timeout | Build (ADR 0021, pass B) | P0 |
| **OB-05** BuildSource Binary (BinaryBuildSource) | Build from uploaded files | None | No upload | Build later: `shoal build --from-dir` uploads an archive through the Control API *(inferred)* | P2 |
| **OB-06** BuildSource Image (ImageSource: copy paths from an image) | Copy files from another image into a build | None | None | Skip. A base image in the Dockerfile covers it | P3 |
| **OB-07** SourceBuildStrategy (Source-to-Image, builder image) | Build from source using a builder image | None | None | Skip. Buildpacks (OB-08) instead | P3 |
| **OB-08** Cloud Native Buildpacks (not in openshift-api; docs knowledge) | Build an image from source with no Dockerfile | None | No Dockerfile-less build | Integrate the buildpacks lifecycle as a second builder | P2 |
| **OB-09** CustomBuildStrategy | Arbitrary builder image | None | None | Skip | P3 |
| **OB-10** JenkinsPipelineBuildStrategy | Jenkins-driven build | None | None | Skip. Replaced by `shoal-ci.yml` (GL-03) | P3 |
| **OB-11** WebHookTrigger: GitHubWebHookCause, GitLabWebHookCause | HTTP call from a git host starts a build | Have, pass C (ADR 0021): `POST /hooks/v1/<config uid>` on the portal starts a Build of the pushed commit. GitHub (`X-Hub-Signature-256`) and GitLab (`X-Gitlab-Token`) checks, constant time. 1 MiB body limit, 30 requests a minute per config | No tag triggers. A redelivered push starts a second Build. The portal needs a certificate the git host trusts | Have. Deduplicating redeliveries later | P0 |
| **OB-12** GenericWebHookEvent (BuildTrigger type Generic) | Generic HTTP call starts a build | Have, pass C (ADR 0021): `provider: generic`, with an HMAC-SHA256 of the body in `X-Shoal-Signature-256`. The secret is per config, not in the URL | The secret is per config, not per build | Have | P1 |
| **OB-13** BitbucketWebHookCause | Bitbucket push starts a build | None | None | Skip. Not a target host | P3 |
| **OB-14** ImageChangeTrigger (ImageChangeBuildTriggerType) | Rebuild when a base image tag changes | None | No base-image tracking | Build once tags exist (OB-23) | P2 |
| **OB-15** ConfigChangeBuildTriggerType | Rebuild when the build config changes | None | None | Skip. A changed build spec is just a new Build | P3 |
| **OB-16** BuildConfigSpec.runPolicy (BuildRunPolicy) | Concurrency of builds per config | None | None | Build: serial and latest-only policies for the build queue | P1 |
| **OB-17** SecretBuildSource (secrets mounted into a build) | Build-time credentials without putting them in the image | None | None | Build: BuildKit secret mounts *(inferred)*. Secret values are redacted from build logs | P1 |
| **OB-18** BuildSpec.resources, nodeSelector, completionDeadlineSeconds | Limits, placement and timeout for builds | None | None | Build with K-15 and K-17. Builds run on nodes labelled for builds | P1 |
| **OB-19** BuildRequest, BuildPhaseCancelled | Cancel or rerun a build | None | None | Build: `shoal build cancel` and `shoal build rerun` | P1 |
| **OB-20** BuildLog, BuildLogOptions | Stream build output | Have, partly (pass B, ADR 0021): the builder ships its log to the server every two seconds through `ReportBuild`. The server keeps it in `builds/<name>.log` under the data dir, and `BuildLogs` serves it. The git password is redacted | `BuildLogs` is unary: the CLI does not follow a running build yet. Only the git password is redacted. The log does not go through the `LogChunk` path | Follow mode in the CLI, and redaction of other Secret values, as later work on ADR 0021 | P0 |
| **OB-21** BuildOutput.to (ObjectReference), pushSecret | Push the result to a named tag | Have (pass B, ADR 0021 and ADR 0020): the image is pushed to `<registry>/<repository>:<tag>`. The repository defaults to the BuildConfig name, and the tag defaults to `{short-sha}`. The builder pushes with its own certificate, scoped to that repository and valid only while the Build runs. The Build records the digest | No `pushSecret`: the certificate is the credential. No Project prefix in the repository name (OB-36) | Build (ADR 0021, pass B) | P0 |

### 3b. Images and registry

Source: `openshift-api/image/v1/types.go` (ImageStream, ImageStreamTag, TagReference, TagEvent, Image, ImageSignature, ImageStreamImport).

| Upstream concept | What it does | Shoal today | Gap | Proposed design | Priority |
|---|---|---|---|---|---|
| **OB-22** ImageStream (ImageStreamSpec, ImageStreamStatus) | Named repository of tags | Have (passes A and B, ADR 0020): embedded registry behind `--registry-listen`, repositories named `<namespace>/<name>`, mTLS authorization for admin and node certificates. Agents pull over mTLS with the cluster CA and their node certificate; the address comes in NodeConfig; `shoal.local/<repo>:<tag>` maps to it on each node. Workloads take plain image strings | No Project scope. No garbage collection. Agents trust only the advertised registry address, not every `--registry-name` | Project scoping when Project lands (GL-40). Garbage collection (CC-09). Other registry names as a list in NodeConfig, if needed | P0 |
| **OB-23** ImageStreamTag (TagReference: from, referencePolicy) | Named tag pointing at an image | Have (passes A and B, ADR 0020): an ImageTag store record per tag, `<repo>:<tag>` pointing at the manifest digest, moved on re-push. An agent pulls a tag by name (`<registry>/<repo>:<tag>` or `shoal.local/<repo>:<tag>`) | Tags do not trigger anything yet. A node that already has the name keeps its image when the tag moves (no re-pull). No history (OB-24). No referencePolicy (skipped by design) | OB-45 follows tags through the rollout path and re-pulls moved tags, resolving to digests; OB-24 history later | P0 |
| **OB-24** TagEvent, NamedTagEventList | History of images a tag pointed to | None. Rollback keeps only `stable_template` (`RolloutStatus`) | No tag history | Build: keep the last N tag events, so rollback can target an earlier digest | P1 |
| **OB-25** TagImportPolicy, ImportModeType (Legacy, PreserveOriginal) | Scheduled import of external images into a stream | None | None | Skip for v1. Pull external images directly by reference | P3 |
| **OB-26** Image, ImageLayer, ImageManifest | Digest and manifest metadata | None. No digest stored per tag | No digest-pinned deploys | Build: record digests in tag events; deploy by digest *(inferred)* | P1 |
| **OB-27** Image signatures (SignatureCondition, SignatureIssuer) | Trust metadata for images | None | None | Integrate cosign verification later (ADR 0017 open question 5) | P3 |
| **OB-28** ImageStreamImport, ImageStreamMapping | API to create tags from images | None | None | Skip the custom API. Registry push (OB-22) is the path | P3 |

### 3c. Routes

Source: `openshift-api/route/v1/types.go` (RouteSpec, TLSConfig, TLSTerminationType, InsecureEdgeTerminationPolicyType, WildcardPolicyType, RouterShard).

| Upstream concept | What it does | Shoal today | Gap | Proposed design | Priority |
|---|---|---|---|---|---|
| **OB-29** Route basics: host, path, to, port | Expose a service on a host and path | `Route`: host, path_prefix, service, port, tls, redirect_http (`RouteSpec`) | None of substance | Have. | have |
| **OB-30** alternateBackends, RouteTargetReference.weight | Split traffic across services (canary, blue/green) | None | No weighted routing | Build: weights in `RouteSpec`. The edge splits by weight *(inferred; the edge owns the proxy, ADR 0015)* | P2 |
| **OB-31** TLSTerminationType: edge, passthrough, reencrypt; TLSConfig.destinationCACertificate | Where TLS ends | `tls`: auto, none or secret, with the edge terminating | No passthrough; no re-encrypt | Skip for v1. Passthrough needs SNI routing in the edge | P3 |
| **OB-32** InsecureEdgeTerminationPolicyType (None, Allow, Redirect) | Plain HTTP handling | `redirect_http` (bool) | Allow and None not explicit | Have. Document the default | have |
| **OB-33** WildcardPolicyType (None, Subdomain) | Wildcard host matching | Host must be a lowercase DNS name; no wildcard (`RouteSpec.host`) | No wildcard | Skip. ADR 0015 decision | P3 |
| **OB-34** RouteHTTPHeaders (set and delete actions) | Add or remove request or response headers | None | None | Skip for v1 | P3 |
| **OB-35** RouterShard | Split routes across router groups | Single edge set, selected by label (`shoal.dev/edge`) | No shards | Skip. ADR 0017 open question 4 covers multi-edge later | P3 |

### 3d. Projects, quotas, security and identity

Source: `openshift-api/project/v1/types.go`, `openshift-api/security/v1/types.go`, `openshift-api/quota/v1/types.go`, `openshift-api/user/v1/types.go`, `openshift-api/oauth/v1/types.go`.

| Upstream concept | What it does | Shoal today | Gap | Proposed design | Priority |
|---|---|---|---|---|---|
| **OB-36** Project (ProjectSpec.finalizers, ProjectStatus.phase, displayName, description) | Tenancy scope for resources | None (see K-26) | No scope | Build: a Project kind. Every Workload, Route, Volume, Build and Secret belongs to one | P1 |
| **OB-37** ProjectRequest | Self-service project creation with a template | None | None | Build: `shoal project create`, with a quota template. Depends on OB-40 | P2 |
| **OB-38** SecurityContextConstraints (allowPrivilegedContainer, allowHostNetwork, allowHostDirVolumePlugin, allowedCapabilities, runAsUser, seLinuxContext, fsGroup, readOnlyRootFilesystem, seccompProfiles, forbiddenSysctls) | Admission policy per user or group | None (see K-11) | No policy objects | Build: one policy object per Project (restricted or privileged), applied when a Workload is created. Only fields Shoal can enforce | P1 |
| **OB-39** SCC.volumes (FSType list) | Limit which volume types a workload may use | Only Shoal Volume, Secret and Config mounts (`WorkloadSpec.volumes`, `files`) | Nothing to restrict | Build as a field of OB-38 | P2 |
| **OB-40** ClusterResourceQuota (ClusterResourceQuotaSelector: labels, annotations; quota; status by namespace) | One quota across projects selected by labels | Labels exist on every object (`ObjectMeta.labels`, ADR 0017 open question 1). No quota | No quota | Build: quotas select Projects by label, on top of K-43 | P1 |
| **OB-41** User, Identity, Group, UserIdentityMapping (user/v1) | Users linked to external identities; groups | None. Admin certificate only (ADR 0007); portal password (`UIAuthSpec`) | No users | Build: users linked to git-provider accounts; groups for K-41 bindings | P1 |
| **OB-42** OAuthClient, OAuthAccessToken, OAuthAuthorizeToken, OAuthClientAuthorization (oauth/v1) | Built-in OAuth server for login and API tokens | Join tokens (`JoinToken`, ADR 0004) and the portal password | No user login; no user API tokens | Skip our own OAuth server. Integrate OIDC login through GitHub or GitLab; user API tokens stored in the store | P1 |
| **OB-43** RangeAllocation (security/v1) | UID and GID ranges per project | None | None | Skip until OB-38 exists | P3 |

### 3e. Deployments and templates

Source: `openshift-api/apps/v1/types.go` (DeploymentConfig), `openshift-api/template/v1/types.go`.

| Upstream concept | What it does | Shoal today | Gap | Proposed design | Priority |
|---|---|---|---|---|---|
| **OB-44** DeploymentConfig (DeploymentConfigSpec: triggers, strategy, replicas, revisionHistoryLimit, selector, template) | Deployment with triggers and hooks | `Workload`: rolling update, automatic rollback, replicas (`resources.proto`) | Triggers and hooks missing | Have the Workload. Add OB-45, OB-46, OB-47 | have |
| **OB-45** DeploymentTriggerImageChangeParams (automatic, containerNames, from) | Roll out when a tag gets a new image | Have, pass C (ADR 0021): `spec.onSuccess.updateWorkload` sets the Workload image to `shoal.local/<repo>@<digest>` when the newest Build completes. The existing rollout and rollback apply | Triggered by Builds only, not by tag moves (OB-23). Needs the executor to report a digest (pass B) | Have. Tag-move triggers come with OB-23 | P0 |
| **OB-46** DeploymentTriggerType ConfigChange | Roll out when the template changes | A Workload spec change rolls out. Secret and Config changes do not restart replicas: env is read at container creation, files update live (`EnvVar`, `FileMount`) | Secret or Config change does not refresh env | Build: optional restart on Secret or Config change, reusing `restarted_at` | P1 |
| **OB-47** LifecycleHook (ExecNewPodHook, failure policy) | Run a pod before or after rollout | None | No pre-deploy step | Build as a Job (K-06) that runs before rollout; failure aborts the rollout | P1 |
| **OB-48** DeploymentStrategyTypeRecreate, Custom | Stop old pods, then start new ones; custom | Rolling only (ADR 0010) | No Recreate | Build with K-02 | P2 |
| **OB-49** DeploymentConfigRollback, DeploymentConfigRollbackSpec | Roll back to a chosen revision | Automatic rollback to `stable_template` only (ADR 0010) | No manual target | Build: keep N stable templates (K-02) | P2 |
| **OB-50** Template (template/v1: parameters, objects) | Parameterized bundle of objects | `shoal.yaml` expansion (`internal/appspec`, ADR 0017 section 2). No parameters | No parameters or generated values | Build: `parameters` in `shoal.yaml`, with `generate` for values such as database passwords | P2 |
| **OB-51** TemplateInstance (requester, objects status) | Track objects made from a template | None | None | Skip for v1. Objects are tracked by owner (`OwnerClaim`, `doc.go`) | P3 |
| **OB-52** BrokerTemplateInstance | Service broker provisioning | None | None | Skip | P3 |
| **OB-53** DeploymentLog, DeploymentLogOptions, DeploymentRequest | Logs of the deployer and instantiate requests | Replica logs (`LogsRequest`) | No deployer logs | Covered by OB-20 for builds. Skip the rest | P3 |

## 4. GitLab

### 4a. Pipeline definition and execution

Source: `gitlab-org/gitlab` `app/assets/javascripts/editor/schema/ci.json` (top-level keys: `stages`, `variables`, `default`, `include`, `workflow`, `before_script`, `after_script`, `image`, `services`, `cache`, `pages`, `spec`; job keys in `job_template`: `script`, `image`, `services`, `rules`, `needs`, `artifacts`, `cache`, `retry`, `timeout`, `parallel`, `environment`, `resource_group`, `trigger`, `when`, `tags`, `variables`, `dependencies`, `release`, `id_tokens`, `secrets`, `coverage`, `interruptible`, `manual_confirmation`, `extends`, `inherit`, `hooks`, `start_in`, `pages`, `dast_configuration`, and others).

| Upstream concept | What it does | Shoal today | Gap | Proposed design | Priority |
|---|---|---|---|---|---|
| **GL-01** Git hosting: repositories, branches, tags, issues, wiki (docs knowledge; not in ci.json) | Git server and project UI | None | No git hosting | Skip. Integrate GitHub, GitLab and Gitea (GL-02) | P3 |
| **GL-02** Git provider integration: webhooks, clone credentials (docs knowledge) | Trigger pipelines and builds from pushes | Partly, passes A and C (ADR 0021): GitHub, GitLab and Gitea webhooks start Builds. Clone credentials are typed Secrets (basic-auth, ssh-auth) | The clone needs the executor (pass B). No commit status back to the host. No merge request triggers | Pass B: clone and build. Status reporting later | P0 |
| **GL-03** Pipeline file `.gitlab-ci.yml` (top-level keys in ci.json, listed above) | Declarative pipeline in the repository | Appspec `shoal.yaml` describes an app, not a pipeline (ADR 0017) | No pipeline file | Build `shoal-ci.yml` (ADR 0022): a subset. Stages, jobs with image and script, rules limited to branch, tag, event and changes, artifacts, cache, variables, environment, timeout. Not GitLab-compatible; no `include` or `extends` in v1 | P1 |
| **GL-04** `stages` (default build, test, deploy) | Sequential groups of jobs | None | None | Build: sequential stages only, same as the GitLab default | P1 |
| **GL-05** Job `script`, `before_script`, `after_script` | Commands run in the job image | None. `Workload.command` replaces an image entrypoint with an argv; it is not a script (`resources.proto`) | No job execution | Build on K-06: run a shell script in a container; logs over OB-20 | P1 |
| **GL-06** Job `image` (and top-level `image`) | Container image a job runs in | `WorkloadSpec.image` | Only for Workloads | Build on K-06 | P1 |
| **GL-07** Job `services` (and top-level `services`) | Sidecar services linked to the job container | None. One image per Workload | No sidecars | Build: run each service as a Workload with a Service name on the mesh *(inferred)* | P2 |
| **GL-08** Job `rules` (conditions: `if`, `changes`, `exists`; `when`, `allow_failure`, `variables`) | Include a job only when conditions match | None | None | Build: `if` on branch, tag and event, and `changes` on paths. `exists` later | P1 |
| **GL-09** Top-level `workflow` (name, auto_cancel, rules) | Pipeline-wide rules and auto-cancel | None | None | Build with GL-08 | P2 |
| **GL-10** Job `needs` (and `rulesNeeds`) | Dependencies between jobs, beyond stage order (DAG) | None | None | Build after stage ordering works | P2 |
| **GL-11** Job `artifacts` (paths, expire_in, exclude, untracked, when, expose_as, public, access, name) | Files kept from a job for later jobs and for download | None | No file handoff | Build: `paths` and `expire_in`. Store in `internal/objstore` (S3-compatible, ADR 0016) or in a Volume *(inferred)* | P1 |
| **GL-12** Artifact `reports` (junit, coverage, codequality, dotenv) | Reports shown in the UI | None | None | Skip for v1. Logs and artifact downloads are enough | P3 |
| **GL-13** Job `cache` (`cache_item`: key, fallback_keys, paths, policy, untracked, when) | Reuse dependencies between pipelines | None | None | Build: a node-local cache Volume keyed by `key`, scheduled to the same node *(inferred; Volume is node-bound, ADR 0016)* | P2 |
| **GL-14** Job `retry` (retry_max, retry_errors) | Retry a failed job | `restart_policy` applies to Workloads only | No job retry | Build on K-06 backoff | P2 |
| **GL-15** Job `timeout` | Job time limit | None | None | Build with K-17 | P1 |
| **GL-16** Job `parallel` and `parallel_matrix` | Run copies of a job with variables | None | None | Skip for v1 | P3 |
| **GL-17** `include`, `extends`, `!reference` | Reuse YAML from other files | None | None | Skip for v1. YAML anchors only; `include` later | P3 |
| **GL-18** Job `trigger` (child and downstream pipelines) | Start another pipeline from a job | None | None | Skip for v1 | P3 |
| **GL-19** Top-level `pages` | Publish a static site | None. A static site could run as a Workload with a Route *(inferred)* | None | Skip for v1 | P3 |
| **GL-20** Job `dast_configuration`, security scanning jobs (docs knowledge for scanners) | Security scans in the pipeline | None | None | Integrate: run scanner images as ordinary jobs | P3 |
| **GL-21** Job `coverage` (regex) | Parse coverage from the log | None | None | Skip for v1 | P3 |
| **GL-22** Job `interruptible`; workflow `auto_cancel` | Cancel older runs when a newer one starts | None | None | Build with GL-09 | P2 |
| **GL-23** Job `hooks` (pre_get_sources_script) | Run a hook before the clone | None | None | Skip | P3 |
| **GL-24** Job `start_in` | Run a job after a delay | None | None | Skip for v1 | P3 |
| **GL-25** Job `dependencies` | Choose which earlier artifacts a job receives | None | None | Build with GL-10 | P2 |

### 4b. Merge requests and status checks

| Upstream concept | What it does | Shoal today | Gap | Proposed design | Priority |
|---|---|---|---|---|---|
| **GL-26** Merge request pipelines (docs knowledge; `rules` with merge request source) | Run on a proposed change | None | No merge request events | Integrate through GL-02 events | P2 |
| **GL-27** Commit status checks and merge gating (docs knowledge) | Pipeline result shown on the change and able to block merge | None | None | Integrate: report status to the git host through its API | P2 |
| **GL-28** Protected branches and tags (docs knowledge) | Restrict who may push or deploy | None | None | Integrate: read the host's protection rules; gate deploys with K-41 roles | P2 |

### 4c. Variables, environments and deploys

| Upstream concept | What it does | Shoal today | Gap | Proposed design | Priority |
|---|---|---|---|---|---|
| **GL-29** Manual jobs and approvals (job `when: manual`, `manual_confirmation`) | Human gate before a job or deploy | None | None | Build: manual job with approver role (K-41) | P2 |
| **GL-30** Plain variables (top-level and job `variables`) | Environment for a pipeline or job | `Config` (plain) and literal `EnvVar.value` (`resources.proto`) | No pipeline-scoped variables | Build: Project-scoped Config and job env | P1 |
| **GL-31** Masked and protected variables (docs knowledge; `secrets` in job_template) | Hide values in logs; limit to protected refs | `Secret`: encrypted at rest, values never returned (ADR 0011) | No log masking; no ref scope | Build: Project-scoped Secret; redact Secret values from job and build logs *(inferred)* | P1 |
| **GL-32** Job `id_tokens`, `identity` (OIDC tokens) | Short-lived tokens to clouds and registries | None | None | Build with K-42 | P2 |
| **GL-33** Job `environment` (name, url, action, deployment_tier, auto_stop_in) | Named deploy target with history | None | None | Build (ADR 0022 pass C): an Environment record in a Project, linking Workloads and Routes *(inferred)* | P2 |
| **GL-34** Review apps and dynamic environments (docs knowledge) | Per-branch deploy with its own URL | None | Route host must be fixed (ADR 0015 rejects wildcards). ADR 0015 Amendment 2026-10-23 allows explicit per-PR hostnames under a base domain | Build for same-repo pull requests only, in pass D of ADR 0022 (ADR 0019 Amendment 2026-10-23). Fork pull requests are skipped by design | P2 |
| **GL-35** Job `resource_group` | Serialize jobs that deploy to one target | None | None | Build (ADR 0022 pass C): a deploy lock per Environment *(inferred)* | P2 |
| **GL-36** Pipeline schedules (docs knowledge) | Run pipelines on a cron | Interval-only backup schedule (ADR 0016) | No cron | Build with K-07 | P2 |
| **GL-37** Pipeline run on demand (API and UI) | Start a pipeline manually | Control API (`api.proto`); no pipeline object | No pipeline object | Build with the pipeline kind | P1 |
| **GL-38** Pipeline and job UI: runs, logs, status, retry | View and act on runs | Web portal for workloads and logs (ADR 0013) | No pipeline view | Build in the portal | P1 |
| **GL-39** Job `release` | Create a release record | None | None | Skip | P3 |

### 4d. Registry, packages and runners

| Upstream concept | What it does | Shoal today | Gap | Proposed design | Priority |
|---|---|---|---|---|---|
| **GL-40** Container registry, per project, with access tokens (docs knowledge; not in ci.json) | Store and serve images per project | Have (partial, ADR 0020): the embedded registry, the same as OB-22, with admin and node certificates | No Project-scoped tokens yet: a node certificate can pull any repository. No garbage collection (CC-09) | Project-scoped tokens when Project lands (ADR 0020 follow-ups) | P0 |
| **GL-41** Package registries: npm, Maven, PyPI, generic (docs knowledge) | Host language packages | None | None | Skip. Integrate external registries | P3 |
| **GL-42** Runners and job `tags` | Execute jobs on hosts that match the tags | Shoal agent runs containers on nodes (ADR 0003, ADR 0006). Labels exist but do not affect placement | No tag matching | Build: map job tags to node labels, using K-15 | P1 |
| **GL-43** Executors (docs knowledge: docker, kubernetes, shell) | How a job runs | containerd only | Container executor only | Build the containerd executor. Skip shell and other executors | P1 |
| **GL-44** Runner registration tokens (docs knowledge) | Attach runners to projects | `JoinToken` for nodes (ADR 0004); scope is cluster-wide | No project scope | Build: project-scoped join tokens | P1 |
| **GL-45** Docker-in-Docker and privileged builds (docs knowledge) | Build images inside a job | None | None | Skip. Use the BuildKit build (OB-04), which avoids privileged jobs | P3 |

## 5. Cross-cutting

| Upstream concept | What it does | Shoal today | Gap | Proposed design | Priority |
|---|---|---|---|---|---|
| **CC-01** Audit log of writes (K8s and OpenShift audit; GitLab audit events; docs knowledge) | Record who changed what | None in code. The word "audit" appears only in README and SECURITY text | No audit trail | Build with Projects and RBAC (K-41) | P1 |
| **CC-02** Secrets encrypted at rest (K8s EncryptionConfiguration; docs knowledge) | Protect secret values in storage | Yes: encrypted at rest; values never returned (ADR 0011) | None | Have. | have |
| **CC-03** Backup and restore of control plane and databases | Recover from loss | `Backup` RPC, `DatabaseBackup`, `restore_from` (`api.proto`; ADR 0014, ADR 0016). `internal/objstore` is an S3 client, but README says "No S3 backup export" | Backups stay on the host | Build: wire S3 export for control-plane and database backups | P0 |
| **CC-04** Metrics, health and alerts | Operator visibility | Metrics and health endpoints (ADR 0018). Alerts planned, not built | No alerts | Build the ADR 0018 plan: alert evaluator and `shoal alerts` | P2 |
| **CC-05** Web console for the platform (OpenShift console; GitLab UI) | Operate from the browser | Web portal (ADR 0013) for workloads, logs and more | No builds, pipelines or projects | Build in the portal, one phase at a time | P2 |
| **CC-06** Multi-host verification and agent failover | Trust on real multi-node clusters | Covered by unit tests and single-host or loopback checks only (README "Limitations") | Unverified | Phase 0 test work on three real hosts | P0 |
| **CC-07** Upgrades and version skew (not in the fetched set) | Safe upgrade of server and agents | Install and version logic (`internal/install/versions.go`) | Partial | Build an automated upgrade test that keeps state | P2 |
| **CC-08** Multi-cluster federation (docs knowledge) | Several clusters under one control plane | None | None | Skip | P3 |
| **CC-09** Image retention and garbage collection (docs knowledge) | Remove old images and tags | None | None | Build with OB-22 | P2 |

## 6. Counts and P0 list

| Area | Rows | have | P0 | P1 | P2 | P3 |
|---|---|---|---|---|---|---|
| Kubernetes (2a to 2c) | 51 | 5 | 3 | 9 | 13 | 21 |
| OpenShift (3a to 3e) | 53 | 3 | 10 | 14 | 9 | 17 |
| GitLab (4a to 4d) | 45 | 0 | 2 | 14 | 16 | 13 |
| Cross-cutting (5) | 9 | 1 | 2 | 1 | 4 | 1 |
| **Total** | **158** | **9** | **17** | **38** | **42** | **52** |

Row counts above were computed from the tables in this file by counting each row's priority cell. Shoal parity today: 9 of 158 rows are marked "have". This is a tally of named concepts, not of the work involved.

**P0 list** (Phase 0 and Phase 1 scope):

1. K-19: `image_pull_secrets` on `WorkloadSpec`, with a dockerconfigjson Secret.
2. K-28: typed Secrets, dockerconfigjson first, then tls with PEM validation.
3. K-51: verify control-plane HA on multiple hosts.
4. OB-01: Build kind (BuildConfig equivalent).
5. OB-02: Build run record with OpenShift phase names.
6. OB-03: git source clone with a token from a Secret.
7. OB-04: Dockerfile build with BuildKit, as a Job on a build-capable node.
8. OB-11: GitHub and GitLab webhook triggers with signature checks.
9. OB-20: build logs over the existing log stream.
10. OB-21: build output pushed to a named tag.
11. OB-22: embedded OCI registry, one repository namespace per Project.
12. OB-23: image tags in the store, pointing at digests.
13. OB-45: a successful build updates the Workload image through the existing rollout.
14. GL-02: git provider integration (webhooks, clone credentials).
15. GL-40: the container registry, same as OB-22.
16. CC-03: S3 export for control-plane and database backups.
17. CC-06: multi-host verification and agent failover tests.

## 7. Not verified and limits

- Rows marked *(docs knowledge)* (GL-01, GL-02, GL-20, GL-26 to GL-28, GL-34, GL-36, GL-40, GL-41, GL-43 to GL-45, OB-08, CC-01, CC-02, CC-08, CC-09, K-47 to K-48, and K-51 for etcd) are not in the fetched sources and were not checked against upstream docs in this pass.
- The GitLab source is the CI schema only. Its job-level keys were read from `job_template`; the schema has no separate `environment`, `trigger` or `needs` definitions, so those keys are read from the `job_template` property list.
- No upstream code was run. Shoal behavior is taken from its proto, ADRs and source files at `main` `484097e`. Runtime behavior was not tested.
- "Have" rows describe the proto and ADR surface. They do not mean the feature is verified; README says multi-host is not verified.
