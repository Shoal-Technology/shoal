# Test gates

## Safe checks on a development host

Requirements: Go from go.mod, Python 3, and a C compiler for the Go race detector.

```sh
python3 -m unittest discover -s hack -p 'test_test_report.py'
make vet
make race
make build
./bin/shoal --help
```

CI runs these checks and compiles the integration suite. Compilation does not mean integration tests ran. Go tests behind the `integration` build tag are excluded from the default suite.

## Real container runtime tests

Run only on a disposable Linux VM with root access, containerd 2.x, CNI plugins in /opt/cni/bin, iptables, iproute2, and registry access. Python 3 is needed for the evidence summary. The suite creates containers, a bridge and host networking rules. Do not run it on a shared production host. Runs must be serialized because the suite uses a fixed namespace and bridge.

```sh
make integration-report
```

This writes JSON events to test-results/integration.jsonl and fails on test failure, package failure, an empty event stream, or skipped tests. A prerequisite skip is not a successful runtime verification. Failed runs may leave host network resources behind; discard the test VM rather than reusing it for production.

## Disposable hosted runner

The manually dispatched `Runtime integration` GitHub Actions workflow provisions containerd 2.4.1 and CNI plugins 1.9.1 on an ephemeral Ubuntu 24.04 runner. Release archives are checked against upstream SHA256 manifests. It runs `make integration-report`, uploads JSON test events and the containerd journal, then stops the runtime. It does not connect to production machines.

```sh
gh workflow run integration.yml --repo kaiserern/shoal
```

The first verified run passed nine tests with no failures or skips, including real container lifecycle, log completion, SIGKILL escalation, resource limits, published ports, networking across restarts, lost network namespace recovery and agent recovery after a container crash. This is single-host runtime evidence, not control-plane failover or multi-host evidence.

## Ingress and automatic certificates (ADR 0015, pass D)

`ingress-e2e.yml` (manually dispatched, runs `hack/ingress-e2e.sh`) starts a server and an edge agent on a disposable runner, with pebble v2.10.1 as the ACME CA. It deploys workload `web`, Service `web` and Route `app` for `app.shoal.test` with `tls: auto`, then checks the result through the edge.

```sh
gh workflow run ingress-e2e.yml --repo kaiserern/shoal
```

Verified run [38124009985](https://github.com/kaiserern/shoal/actions/runs/38124009985) on commit aff0831, 2026-10-11, passed:

- a) Route `app` is `Ready` and `CertIssued`. Pebble validated the host over HTTP-01 through the edge on port 80, finalised the order and issued the certificate.
- b) `https://app.shoal.test` returns the nginx welcome page through the edge. The pebble root was passed to curl, so the chain was verified without `-k`.
- c) The served certificate was issued by `Pebble Intermediate CA`.
- d) Plain HTTP gets a 308 redirect to HTTPS.
- e) An unknown host gets the stock 404.
- f) Skipped: websocket upgrade. The nginx image has no websocket endpoint, and no small echo image is wired into CI.

Two earlier runs on this branch failed at step a) and were fixed:

- [38078590852] failed because `CreateCert` polled the order at an empty URL when pebble omitted the `Location` header on finalize. Fixed in c411989, with a regression test in `internal/server/acme_finalize_test.go`.
- [38123434296] on c411989 failed because the script waited for route state `Accepted`, while the controller reports `Ready`. Fixed in aff0831.

Limits: pebble is a test CA, not Let's Encrypt, so rate limits, real DNS and production behaviour are not covered. `app.shoal.test` resolves to 127.0.0.1 on the runner. The run has one server and one edge, and does not test renewal, failover or revocation. This is one run.

## Deploy end to end (ADR 0017 pass F, ADR 0016 pass F)

`deploy-e2e.yml` (manually dispatched, runs `hack/deploy-e2e.sh`) starts pebble, a server and one edge agent (`--volume-backend loop`) on a disposable runner. It imports the test app `hack/blog` (a `postgres:16-alpine` base with a shell loop: it connects with `DATABASE_URL`, inserts one row every two seconds and serves the row count on port 3000), then runs `shoal deploy` on this `shoal.yaml`: `name: blog`, `domain: blog.shoal.test`, `database: postgres`, one volume `uploads`, `replicas: 1`. The shared cluster setup is in `hack/e2e-lib.sh`, which `ingress-e2e.sh` also uses.

```sh
gh workflow run deploy-e2e.yml --repo kaiserern/shoal
```

Verified run [38141941992](https://github.com/kaiserern/shoal/actions/runs/38141941992) on commit 2b223f9, 2026-10-11, passed every step:

- a) `deploy --dry-run` plans `create` for `workload/blog`, `service/blog`, `route/blog`, `database/blog-db`, `volume/blog-uploads` (and `secret/blog-secrets`).
- b) `deploy --wait=10m` exits 0 and prints `url https://blog.shoal.test`.
- c) `curl --cacert <pebble root> https://blog.shoal.test/` returns `connected to postgres: select=1 hits=N`, so the app ran `SELECT 1` against the Database through `DATABASE_URL`.
- d) The app writes one row per request cycle. With 4 rows written, the Postgres container's task was killed with `ctr -n shoal task kill -s SIGKILL`. The agent restarted it on the same volume, and the row count read back as 6 (rows before the kill: 4). No data was lost.
- e) A second `deploy` reports all six objects `unchanged`, with no `create`, `update`, `delete` or `orphan` lines.
- f) `app status blog` shows workload `1/1 ready`, database `Ready`, route `Ready`, volume `Bound`, secret `empty` (created, no values set), and service with no state (shown as `-`).
- g) `app delete blog --yes` removes the `blog` workload (asserted). `get databases` still lists `blog-db`, and `get volumes` still lists `blog-uploads` (both asserted). The service, route and secret removal is not asserted.

Earlier runs on this branch, each fixed on the branch:

- [38138016916] failed at b): the workload never became ready. This run had no failure diagnostics, so its cause was not captured.
- [38138904662] failed at b): the app had no HTTP server. The image has no busybox `httpd`, so nothing listened on port 3000, the health check got connection refused, and the agent restarted the container every 30 s. Fixed in 6d8b4f9 by a netcat fallback in `hack/blog/serve.sh`. The failure path now prints app status, replicas, secrets, Databases, app logs and containerd state.
- [38140270243] passed a) to d) and failed at e): `database/blog-db already exists and is not labelled app=blog`. Fixed in 138521e: `metadataOf` in `internal/resource/labels.go` had no case for Database, so the label was dropped on apply. `TestObjectLabelsEveryKind` walks every member of the `Object` oneof.
- [38140939413] passed a) to d) and failed at e): the Database's own children (`service/blog-db`, `workload/blog-db`, `secret/blog-db-credentials`, `volume/blog-db-data`) were reported as orphans on every deploy. With `--prune` they would have been deleted. Fixed in 7e82265: owned children carry `shoal.dev/owned-by=database`, and deploy's orphan check and app listing skip them. Tests `TestOwnedChildrenAreNotOrphans` and `TestUnownedAppObjectIsOrphan` in `internal/deploy`.
- [38141479523] passed a) to e) and failed at f): the assertions expected the words `Ready` on every row. Fixed in 2b223f9 to the states `app status` actually prints.

The Ingress run [38141944451] on 2b223f9 also passed, which confirms the shared setup in `hack/e2e-lib.sh` still works for `ingress-e2e.sh`.

Verified run [38162883534](https://github.com/kaiserern/shoal/actions/runs/38162883534) on commit e37c5f3 (ADR 0016 pass D) passed every step on the first attempt, including the two new steps:

- h) `shoal db backup blog-db` records a backup, and `shoal db backups blog-db` shows it `ok` (the table had 11 rows when the step started). The backup ID is `blog-db-20261011t181738-f5617ae6f3`.
- i) `shoal db restore blog-db --from <id> --into blog-db-restored` creates the new Database. Once it is Ready, its `restoreState` is `done`. A `psql` count inside its container (through `ctr task exec`) gives 16 rows, and blog-db still has 16. The step asserts the restored count is at least the count before the backup, and that blog-db is at least that too. It does not assert equality, so an exact match is seen in the log, not checked by the script.

Final run on main at 0af8381 (after the upstream e2e change in cd0fe1d): [38163520082](https://github.com/kaiserern/shoal/actions/runs/38163520082) also passed every step, with the same counts (blog-db-restored 16 rows, blog-db 16).

Not covered by this run:

- The volume backend is recorded as `loop`, inferred from the absence of a fallback message in the agent log. The Volume status was not read to confirm it. The `dir` backend was not run.
- One unexplained event: in run 38141479523 the blog container was restarted once with exit code 137 and no health-check failure logged. Steps d) and e) still passed. The cause is not known.
- Redis, S3 export and the Redis repeat (ADR 0016 pass F steps 5 to 7) are not covered. A minor upgrade is covered by unit tests only (`TestMinorUpgradeUpdatesImage`, `TestMinorUpgradeWaitsForAVerifiedBackup`); the e2e does not upgrade. Backups and restore are covered by the e2e and by unit tests.
- `shoal init`, `shoal uninstall` and the onboarding benchmark (ADR 0017 section 6) are not covered. The e2e uses the server and agent started directly, as the ingress e2e does.
- The test app image is built on `postgres:16-alpine` without a digest pin, and the domain is `blog.shoal.test` on pebble, not a real DNS record.

## Ingress hardening (ADR 0015, pass E)

Verified run [38126778237](https://github.com/kaiserern/shoal/actions/runs/38126778237) on commit e519bf9 (2026-10-11), first attempt, passed. It runs the same checks a) to f) as above against pebble, now with `--acme-accept-tos` on the server. Check a) would fail if that flag were missing, because the Route would report CertError.

What the e2e run does not cover: the route cache restart, the Route claim race, the 502/503/504 retry and the applied-revision wait. Those are covered by in-process tests only: `internal/edge/proxy_gateway_test.go`, `internal/server/route_claim_test.go`, `internal/agent/edge_cache_test.go` and the ack tests in `internal/server/certs_test.go`.

The `integration`, `integration-report` and `ingress-e2e` make targets refuse to run unless `CI=true` or `SHOAL_DISPOSABLE=1`.

## Partition policy

Workload manifests accept `spec.partitionPolicy: Hold` or `Replace`. Omitted policy preserves existing Replace behavior. Invalid values are rejected. Hold retains assignments on silent or deleted nodes instead of evicting and replacing them. An unavailable worker can therefore leave the workload unavailable indefinitely. Node record deletion is not proof that containers stopped.

Hold is not fencing and is not an exactly-once execution guarantee. Explicit workload deletion, scaling, rollout changes, or switching back to Replace are not safe takeover mechanisms for an uncertain worker. Operators must verify the old process is stopped before manual takeover. Mixed-version control planes must not be used to enforce Hold: older controllers do not know this policy.

The disposable harness tests Hold with a 75-second live control partition, longer than the default 60-second eviction threshold, and checks that runtime count remains at the desired two. It then tests the existing Replace path separately.

## Control-plane partition and direct HTTP sampling

The same workflow executes `hack/cluster_fault.py` with a real server and two agents. A TCP relay cuts node1's existing control connection and rejects reconnects without stopping its agent or containers. After replacement replicas respond to HTTP, the harness verifies the original replica still responds, records runtime inventory, heals the connection, and waits for stale cleanup. This deliberately documents duplication rather than treating failover as singleton safety.

`test-results/traffic.jsonl` records round-robin direct pod HTTP attempts at a target 5Hz, with 500ms request timeout. Actual rate falls when requests take longer. Addresses are cached and refreshed by health checks, not by a Service or production load balancer. During the rollback-status wait, discovery remains cached; failures can include removed endpoints. The summary distinguishes phases and cannot prove user-facing zero downtime. Full per-request evidence is retained for interpretation.

A duplicate-observation-to-cleanup window is measured from verified replacement health to the first stale-inventory removal observation. Polling and asynchronous removal mean this is not an exact duplicate-serving duration.

Safe local observer checks (no containers or firewall changes):

```sh
python3 -m unittest discover -s hack -p 'test_*.py' -v
```

## Service endpoint eligibility and partition tradeoff

The controller withdraws a replica IP when its assigned node is missing or NotReady, without waiting for workload eviction. The default heartbeat uncertainty threshold remains 20 seconds and replacement threshold remains 60 seconds. This does not change `Hold`/`Replace`, kill containers, fence nodes, or reroute existing conntrack connections. Workload Ready counts still reflect the last replica reports; Service eligibility additionally checks node readiness.

The cross-VM harness separately exercises a 75-second control-only partition under Hold, reconnect, and actual guest destruction under Replace. During partition it asserts unchanged assignment IDs and real bidirectional WireGuard HTTP plus Service HTTP from both guests. It captures API endpoints and survivor guest DNAT rules before each new Service connection. Endpoint withdrawal must reach the connected proxy in under 40 seconds, preserve the healthy survivor throughout, and all sampled new connections after convergence must succeed until reconnect or replacement. Pre-detection failures remain in the evidence rather than being masked.

This conservative rule intentionally removes a still-serving partitioned replica from connected proxies. The disconnected agent retains its last Service snapshot; it cannot receive the withdrawal. An all-node control partition can leave connected proxies with no eligible endpoints even while pods are running. This is a documented availability tradeoff, not a data-plane failure detector or a zero-downtime guarantee. Unit regressions cover both policies, pre-eviction assignment retention, deleted nodes under Hold, reconnect, idempotence, all nodes uncertain, unready replicas, stale template status and missing IPs.

### Verified endpoint-withdrawal runs

Run [37803401825](https://github.com/kaiserern/shoal/actions/runs/37803401825) exercises the persistent-withdrawal gate on two independent guest kernels:

- Control-only partition with Hold lasted 75.488 seconds. Service had 0 failures in 52 requests from the connected guest; every sample also verified remote-pod HTTP in both directions and Service HTTP from the disconnected guest. Assignment IDs were unchanged. Connected-proxy withdrawal was observed at 20.109 seconds. Original endpoint/proxy state and Service were restored 17.145 seconds after healing.
- Actual VM2 destruction under Replace: replacement became healthy on VM1 at 64.556 seconds. Service had 4 failures in 74 requests, all before observed withdrawal at 17.936 seconds. There were 0 failures in the 66 new connections sampled after withdrawal. The threshold measures time since the last heartbeat, not since fault injection, explaining withdrawal earlier than 20 seconds after destruction.
- The healthy survivor endpoint remained in every observation; both endpoint state and guest kernel DNAT rules were checked. These are sampled fresh TCP connections, not continuous zero-downtime or established-flow guarantees. SSH/snapshot overhead and a 3-second curl timeout make rates incomparable to earlier sampling runs.

Runtime regression [37802448792](https://github.com/kaiserern/shoal/actions/runs/37802448792), using the same Go production change, passed all 9 privileged runtime tests without skips. Single-host spread-agent loss retained the survivor with 0/338 Service failures; Hold partition 0/456, Replace partition plus heal 0/455, server outage/restart 0/20, healthy rollout 0/55, broken rollout/rollback 0/178. Deliberate fixture reset is separate (15/16 failed). Critically, full-placement agent loss produced **215/309 Service failures**, with a **42.827-second longest observed failure streak**, while cached direct-pod requests still succeeded. All replica assignments were on the silent worker, so withdrawing its endpoints left none until replacement. This is the explicit control-uncertainty tradeoff; a surviving Ready endpoint is required for availability, and this gate is not two-host evidence.

Local verification: 248 Go race tests, zero failures/skips, ordinary/integration-tagged vet, binary build, integration compilation and 20 Python observer/evidence tests passed. Remote CI and the stronger cross-VM workflow passed. Downloaded artifacts were checked for unredacted join-token arguments and PEM/OpenSSH private-key blocks; none were found. The shared main VM and Kryvora networking were not changed by these disposable runtime tests.

## Two-VM WireGuard gate

Run the manual `Two VM WireGuard acceptance` workflow (`.github/workflows/vm-mesh.yml`). It boots two Ubuntu 24.04 QEMU guests with separate kernels, disks, containerd, CNI, iptables and WireGuard state. QEMU SLIRP forwards only guest SSH and WireGuard underlay UDP; pod HTTP is not forwarded. Each guest must access the other guest's pod IP through a `shoal-wg` route, both peers must have a real handshake, and the fixed ClusterIP must respond from both guests. Shared boot IDs, missing handshakes, failed HTTP and non-WireGuard routes fail the evidence contract. Guest image and runtime archives are pinned and checksum-verified.

Verified run [37799603752, attempt 2](https://github.com/kaiserern/shoal/actions/runs/37799603752): both KVM guests had distinct boot IDs, bidirectional remote-pod and Service HTTP returned 200, and both remote routes used `shoal-wg`. Killing VM2 entirely produced healthy replacement on VM1 in **63.151 seconds**. Fixed-Service sampling from VM1 recorded **15 failures / 39 requests**. Observations show the dead VM's endpoint remained while its node was NotReady until eviction; no zero-downtime claim is warranted. Samples are sequential SSH calls with a 3-second HTTP timeout and 0.2-second inter-attempt pause, not 5Hz. Endpoint/node snapshots are collected after each attempt and may straddle a transition.

Artifacts contain per-request traffic, endpoint/node phases, guest boot evidence, routes, kernel rules, serial logs and sanitized agent/server logs. Join-token arguments from systemd journals are redacted; credentials, disks and guest keys are deleted in cleanup. An earlier run exposed an ephemeral disposable join token in a journal artifact; those artifacts were removed and the cluster destroyed before regenerating clean evidence. The clean run's first attempt failed on an upstream GitHub release HTTP 500 before guest execution; attempt 2 executed the full gate successfully. Twelve Python tests pass, including six evidence-contract tests; this workflow does not execute the nine-test privileged runtime suite from the separate runtime workflow.

This proves **inter-VM networking on one physical runner**, not two independent physical hosts, ingress/DNS, client-pod-origin traffic, Internet WAN behavior, or host-level HA. Shared VM/service hosts are not modified; all privileged installation and guest execution happens in the disposable runner.

## HA chaos gate (ADR 0014, pass 2g)

Run the manual `HA chaos` workflow (`.github/workflows/ha-chaos.yml`). It calls `hack/ha-chaos.sh` on an Ubuntu 24.04 runner. The script builds the real `shoal` binary and starts three `shoal server --store=raft` processes on 127.0.0.1: node-1 bootstraps, and node-2 and node-3 join with a join bundle. Only the control plane runs: no agent, no containerd and no root. Every step is checked through the CLI with a timeout. On failure the script prints all server logs. Logs and `steps.txt` are uploaded as the `ha-chaos-evidence` artifact.

Verified run [38073411873](https://github.com/kaiserern/shoal/actions/runs/38073411873) on commit `9ba61e8` (2026-10-10 UTC), first attempt, every step passed:

- a) `cluster status` shows 3 voters and 1 leader, the same from all three nodes.
- b) A write sent to a follower is readable on the leader and on the follower. A follower answers a write with the leader's address and the CLI retries there (ADR 0014, option B). This is a client-side redirect, not a server-side proxy.
- c) `kill -9` on the leader: a survivor elected a new leader in about 2 seconds. Earlier objects were readable and a new write succeeded.
- d) The killed node restarted on its data dir with `--raft-join`. Status showed 3 voters and 1 leader on every node, and the node served an object written while it was down.
- e) SIGSTOP on the leader for 6 seconds: a survivor elected a new leader and accepted a write. After SIGCONT the old leader stepped down, caught up to the new leader's commit index and read the object written during the freeze.
- f) `shoal backup --out` from the leader, restored with `--restore-from` into a fresh data dir on new ports as a single voter. The restored object is identical to the source.
- g) `cluster remove node-3` on a survivor: status shows 2 voters and 1 leader.

Limits: SIGSTOP freezes a process. It does not cut the network, so this is not a network partition test. All addresses are loopback on one runner. Agent failover, multi-host networking and power-loss durability are not covered. No Go code changed in this pass, so no Go tests were added; the only check on the VPS was `bash -n`.

## Worker-loss scenario interpretation

A process crash must be interpreted with its placement precondition. The harness now first crashes one worker while two replicas are spread across two workers, asserting at every recovery poll that the surviving ready replica remains a Service endpoint. It then resets only the disposable workload to restore spread placement before partition tests.

The later `full-placement-agent-crash` scenario deliberately kills the agent on the worker holding **both** replicas after partition failover. Once that worker is evicted, there is no independently healthy surviving assignment: an empty Service endpoint set while replacements start is expected. The earlier run's 15 refused requests and approximately 2.8-second sampled failure window occurred here, not during restart/reconnect and not because a healthy surviving worker endpoint was removed. Retaining uncertain endpoints to hide this gap would change safety semantics; this harness change does not make that production change.

Verified run [37794828711](https://github.com/kaiserern/shoal/actions/runs/37794828711): spread-worker crash produced 0 failures in 334 ClusterIP requests, preserved the surviving IP across all 64 endpoint polls (minimum one endpoint), and reached healthy replacement in 65.969 seconds. Full-placement loss produced 10 failures in 311 requests with a 1.802-second longest observed failure streak. Healthy rollout (61 requests), broken rollout/rollback (153), Hold partition (456), and Replace partition/heal (452) each had zero Service request failures. All nine privileged runtime tests passed with no skips.

`fixture-reset` traffic is reported separately because deleting the test workload intentionally interrupts service (13 failures in 16 requests in this run). Neither same-host scenario proves worker-host failure or cross-host routing.

## Real Service traffic gate

The fault harness also creates a real Service and samples its fixed ClusterIP through Shoal's iptables proxy. `service-traffic.jsonl` records every request, `service-endpoints.json` records API endpoint observations during health checks, and `service-rules.txt` captures final kernel rules. Sampling targets 5Hz with 500ms timeout; longest failure streak is an observed sample window, not an exact outage duration. The healthy rollout changes the actual nginx image, then a crashing command exercises rollback.

Two agents on this single disposable host share the host iptables chains. This gate proves the ClusterIP path on that topology, not independently isolated per-host proxies, DNS, ingress, or cross-host WireGuard. A workflow can pass while traffic failures are recorded: recovery assertions are gates; request counts are measurements, not a zero-error SLA gate.

## Multi-host acceptance backlog

The current runtime integration suite is single-host. The following are required before claiming multi-host reliability:

* Two separate worker VMs with WireGuard traffic passing between containers.
* DNS and Service requests reaching replicas on both workers.
* Container process termination followed by recovery.
* Agent termination followed by reassignment and stale replica cleanup on reconnect.
* Server restart with surviving workloads and agent reconnection.
* Worker reboot and recovery.
* Rolling update with continuously measured request failures.
* Broken release with automatic rollback.
* Network partition with explicit duplicate-replica observations.

Record topology, versions, commands, timestamps, request counts and recovery times. Do not treat two agents on one host as proof of cross-host networking. Partition testing currently must use disposable stateless workloads; singleton and stateful safety is not established.
