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
