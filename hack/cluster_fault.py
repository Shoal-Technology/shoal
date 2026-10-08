"""Single-host process fault acceptance. Run as root on disposable hosts only."""
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import tempfile
import time
import urllib.request
from fault_observer import PartitionProxy, TrafficSampler

ROOT = Path(__file__).resolve().parents[1]
BIN = str(ROOT / 'bin/shoal')
OUT = ROOT / 'test-results'
OUT.mkdir(exist_ok=True)
BASE = Path(tempfile.mkdtemp(prefix='shoal-cluster-'))
CONFIG = str(BASE / 'server/admin.conf')
PROCESSES = {}
RESULTS = []
SAMPLER = None


def cli(*args):
    return subprocess.check_output([BIN, *args, '--config', CONFIG, '--timeout', '5s'], text=True, timeout=10)


def objects(kind):
    text = cli('get', kind, '-o', 'json').strip()
    decoder = json.JSONDecoder()
    result = []
    while text:
        item, end = decoder.raw_decode(text)
        result.append(item)
        text = text[end:].lstrip()
    return result


def start(name, args):
    log = open(OUT / (name + '.log'), 'a')
    process = subprocess.Popen([BIN, *args], stdout=log, stderr=log)
    log.close()
    PROCESSES[name] = process


def stop(name):
    process = PROCESSES.pop(name)
    process.kill()
    process.wait(timeout=15)


def wait(label, predicate, timeout=150):
    begin = time.monotonic()
    last = None
    while time.monotonic() - begin < timeout:
        try:
            if predicate():
                RESULTS.append({'assertion': label, 'seconds': round(time.monotonic() - begin, 3)})
                return
        except (OSError, ValueError, subprocess.SubprocessError) as error:
            last = str(error)
        time.sleep(1)
    raise AssertionError(f'{label} timed out; last error: {last}')


def replicas():
    return objects('replicas')


def healthy():
    items = replicas()
    if SAMPLER is not None:
        SAMPLER.update(addresses=[r['status']['ip'] for r in items if r.get('status', {}).get('ip')])
    if len(items) != 2 or not all(r.get('status', {}).get('ready') for r in items):
        return False
    for replica in items:
        address = replica['status']['ip']
        with urllib.request.urlopen(f'http://{address}:80/', timeout=2) as response:
            if response.status != 200:
                return False
    return True


def runtime_ids(node):
    output = subprocess.check_output(['/usr/local/bin/ctr', '-n', 'fault-' + node, 'containers', 'list', '-q'], text=True)
    return set(output.split())


def main():
    global SAMPLER
    if os.geteuid() != 0 or os.environ.get('SHOAL_DISPOSABLE_TEST') != '1':
        raise RuntimeError('requires root and SHOAL_DISPOSABLE_TEST=1 on a disposable host')
    server_args = ['server', '--data-dir', str(BASE / 'server'), '--listen', '127.0.0.1:17443', '--advertise-address', '127.0.0.1:17443', '--ui-listen', '']
    agent_args = {}
    proxy = PartitionProxy(('127.0.0.1', 17444), ('127.0.0.1', 17443))
    SAMPLER = TrafficSampler(OUT / 'traffic.jsonl')
    try:
        start('server', server_args)
        wait('server credentials created', lambda: Path(CONFIG).exists(), 30)
        for number in (1, 2):
            name = f'node{number}'
            token = cli('token', 'create', '--max-uses', '1')
            join = re.search(r'--token\s+(\S+).*--ca-hash\s+(\S+)', token)
            if not join:
                raise AssertionError('join command missing token or CA hash')
            args = ['agent', '--server', '127.0.0.1:17444' if number == 1 else '127.0.0.1:17443', '--node-name', name, '--state-dir', str(BASE / name), '--containerd-namespace', 'fault-' + name, '--bridge-name', 'fault' + str(number), '--wireguard-interface', '']
            agent_args[name] = args
            start(name, args + ['--token', join[1], '--ca-hash', join[2]])
        wait('two nodes Ready', lambda: len(objects('nodes')) == 2 and all(n.get('status', {}).get('phase') == 'Ready' for n in objects('nodes')), 45)
        manifest = BASE / 'web.yaml'
        manifest.write_text('kind: Workload\nmetadata: {name: web}\nspec:\n  image: nginx:1.28-alpine\n  replicas: 2\n  resources: {cpu: 100m, memory: 64Mi}\n  healthCheck: {http: {path: /, port: 80}, intervalSeconds: 1, timeoutSeconds: 1}\n  update: {maxUnavailable: 1, progressDeadlineSeconds: 20}\n')
        cli('apply', '-f', str(manifest))
        wait('two real HTTP replicas healthy', healthy)
        assert {r['spec']['node'] for r in replicas()} == {'node1', 'node2'}, 'initial replicas not spread'
        # Hold must retain the existing assignment beyond the eviction timeout.
        original_manifest = manifest.read_text()
        manifest.write_text(original_manifest.replace('  replicas: 2', '  partitionPolicy: Hold\n  replicas: 2'))
        cli('apply', '-f', str(manifest))
        wait('Hold workload healthy before partition', healthy)
        held_ids = runtime_ids('node1')
        SAMPLER.update(phase='hold-partition')
        proxy.partition()
        time.sleep(75)
        assert PROCESSES['node1'].poll() is None
        assert {r['spec']['node'] for r in replicas()} == {'node1', 'node2'}, 'Hold reassigned lost replica'
        assert held_ids == runtime_ids('node1'), 'Hold runtime assignment changed'
        assert len(runtime_ids('node1')) + len(runtime_ids('node2')) == 2, 'Hold created duplicate runtime'
        assert healthy(), 'Hold original HTTP did not survive control partition'
        RESULTS.append({'assertion': 'Hold prevents partition replacement beyond eviction timeout', 'partition_seconds': 75, 'runtime_containers': 2})
        proxy.heal()
        wait('Hold node reconnects', lambda: all(n.get('status', {}).get('phase') == 'Ready' for n in objects('nodes')), 45)
        manifest.write_text(original_manifest)
        cli('apply', '-f', str(manifest))
        wait('Replace workload healthy after policy change', healthy)
        SAMPLER.update(phase='baseline')
        time.sleep(3)
        old_ids = runtime_ids('node1')
        old_address = next(r['status']['ip'] for r in replicas() if r['spec']['node'] == 'node1')
        SAMPLER.update(phase='partition')
        partition_begin = time.monotonic()
        proxy.partition()
        wait('replacement HTTP replicas healthy during live agent partition', lambda: healthy() and all(r['spec']['node'] == 'node2' for r in replicas()), 150)
        assert PROCESSES['node1'].poll() is None, 'partition killed agent'
        assert old_ids <= runtime_ids('node1'), 'partition removed old runtime'
        with urllib.request.urlopen(f'http://{old_address}:80/', timeout=2) as response:
            assert response.status == 200, 'partitioned original stopped serving'
        duplicate_begin = time.monotonic()
        RESULTS.append({'assertion': 'partition duplicates live HTTP replica', 'partition_seconds': round(duplicate_begin - partition_begin, 3), 'runtime_containers': len(runtime_ids('node1')) + len(runtime_ids('node2')), 'desired_replicas': 2})
        time.sleep(5)
        SAMPLER.update(phase='partition-heal')
        proxy.heal()
        wait('partition stale container removed after heal', lambda: not (old_ids & runtime_ids('node1')), 45)
        RESULTS.append({'assertion': 'duplicate observation to observed cleanup window', 'seconds': round(time.monotonic() - duplicate_begin, 3), 'note': 'starts at verified replacement health, ends at observed stale removal; not exact duplication onset'})
        wait('cluster healthy after partition heal', healthy, 45)
        # Retain the process-crash scenario on a node that now has replicas.
        SAMPLER.update(phase='agent-crash')
        old_ids = runtime_ids('node2')
        stop('node2')
        wait('replacement replicas healthy on surviving node', lambda: healthy() and all(r['spec']['node'] == 'node1' for r in replicas()), 150)
        assert old_ids <= runtime_ids('node2'), 'agent termination unexpectedly stopped containers'
        start('node2', agent_args['node2'])
        wait('stale containers removed after reconnect', lambda: not (old_ids & runtime_ids('node2')), 45)
        wait('cluster healthy after reconnect', healthy, 45)
        addresses = [r['status']['ip'] for r in replicas()]
        SAMPLER.update(phase='server-outage')
        stop('server')
        time.sleep(3)
        # Verify HTTP independently of the unavailable control API.
        for address in addresses:
            with urllib.request.urlopen(f'http://{address}:80/', timeout=2) as response:
                assert response.status == 200, 'HTTP failed while server down'
        RESULTS.append({'assertion': 'HTTP survives control server outage', 'requests': len(addresses)})
        for name in ('node1', 'node2'):
            assert PROCESSES[name].poll() is None, 'agent died with server'
        SAMPLER.update(phase='server-restart')
        start('server', server_args)
        wait('server restart restores cluster and HTTP', healthy, 60)
        bad = manifest.read_text().replace('  replicas: 2', '  command: ["/bin/sh", "-c", "exit 1"]\n  replicas: 2')
        manifest.write_text(bad)
        SAMPLER.update(phase='broken-rollout')
        cli('apply', '-f', str(manifest))
        wait('broken release rolled back', lambda: objects('workloads')[0].get('status', {}).get('rollout', {}).get('phase') == 'RolledBack', 90)
        wait('HTTP healthy after rollback', healthy, 60)
        SAMPLER.update(phase='post-rollback')
        time.sleep(3)
    finally:
        traffic = SAMPLER.close()
        RESULTS.append({'assertion': 'direct pod HTTP sampling summary', 'phases': traffic, 'discovery': 'cached endpoints refreshed by health checks; not a service or load balancer'})
        proxy.close()
        for name in list(PROCESSES):
            stop(name)
        (OUT / 'cluster-fault.json').write_text(json.dumps(RESULTS, indent=2))
    print(json.dumps(RESULTS, indent=2))


if __name__ == '__main__':
    main()
