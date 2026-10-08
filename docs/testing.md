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

## Two-VM WireGuard gate

Run the manual `Two VM WireGuard acceptance` workflow (`.github/workflows/vm-mesh.yml`). It boots two Ubuntu 24.04 QEMU guests with separate kernels, disks, containerd, CNI, iptables and WireGuard state. QEMU SLIRP forwards only guest SSH and WireGuard underlay UDP; pod HTTP is not forwarded. Each guest must access the other guest's pod IP through a `shoal-wg` route, both peers must have a real handshake, and the fixed ClusterIP must respond from both guests. Shared boot IDs, missing handshakes, failed HTTP and non-WireGuard routes fail the evidence contract. Guest image and runtime archives are pinned and checksum-verified.

Verified run [37799603752, attempt 2](https://github.com/kaiserern/shoal/actions/runs/37799603752): both KVM guests had distinct boot IDs, bidirectional remote-pod and Service HTTP returned 200, and both remote routes used `shoal-wg`. Killing VM2 entirely produced healthy replacement on VM1 in **63.151 seconds**. Fixed-Service sampling from VM1 recorded **15 failures / 39 requests**. Observations show the dead VM's endpoint remained while its node was NotReady until eviction; no zero-downtime claim is warranted. Samples are sequential SSH calls with a 3-second HTTP timeout and 0.2-second inter-attempt pause, not 5Hz. Endpoint/node snapshots are collected after each attempt and may straddle a transition.

Artifacts contain per-request traffic, endpoint/node phases, guest boot evidence, routes, kernel rules, serial logs and sanitized agent/server logs. Join-token arguments from systemd journals are redacted; credentials, disks and guest keys are deleted in cleanup. An earlier run exposed an ephemeral disposable join token in a journal artifact; those artifacts were removed and the cluster destroyed before regenerating clean evidence. The clean run's first attempt failed on an upstream GitHub release HTTP 500 before guest execution; attempt 2 executed the full gate successfully. Twelve Python tests pass, including six evidence-contract tests; this workflow does not execute the nine-test privileged runtime suite from the separate runtime workflow.

This proves **inter-VM networking on one physical runner**, not two independent physical hosts, ingress/DNS, client-pod-origin traffic, Internet WAN behavior, or host-level HA. Shared VM/service hosts are not modified; all privileged installation and guest execution happens in the disposable runner.

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
