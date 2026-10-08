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

ROOT = Path(__file__).resolve().parents[1]
BIN = str(ROOT / 'bin/shoal')
OUT = ROOT / 'test-results'
OUT.mkdir(exist_ok=True)
BASE = Path(tempfile.mkdtemp(prefix='shoal-cluster-'))
CONFIG = str(BASE / 'server/admin.conf')
PROCESSES = {}
RESULTS = []


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
    if os.geteuid() != 0 or os.environ.get('SHOAL_DISPOSABLE_TEST') != '1':
        raise RuntimeError('requires root and SHOAL_DISPOSABLE_TEST=1 on a disposable host')
    server_args = ['server', '--data-dir', str(BASE / 'server'), '--listen', '127.0.0.1:17443', '--advertise-address', '127.0.0.1:17443', '--ui-listen', '']
    agent_args = {}
    try:
        start('server', server_args)
        wait('server credentials created', lambda: Path(CONFIG).exists(), 30)
        for number in (1, 2):
            name = f'node{number}'
            token = cli('token', 'create', '--max-uses', '1')
            join = re.search(r'--token\s+(\S+).*--ca-hash\s+(\S+)', token)
            if not join:
                raise AssertionError('join command missing token or CA hash')
            args = ['agent', '--server', '127.0.0.1:17443', '--node-name', name, '--state-dir', str(BASE / name), '--containerd-namespace', 'fault-' + name, '--bridge-name', 'fault' + str(number), '--wireguard-interface', '']
            agent_args[name] = args
            start(name, args + ['--token', join[1], '--ca-hash', join[2]])
        wait('two nodes Ready', lambda: len(objects('nodes')) == 2 and all(n.get('status', {}).get('phase') == 'Ready' for n in objects('nodes')), 45)
        manifest = BASE / 'web.yaml'
        manifest.write_text('kind: Workload\nmetadata: {name: web}\nspec:\n  image: nginx:1.28-alpine\n  replicas: 2\n  resources: {cpu: 100m, memory: 64Mi}\n  healthCheck: {http: {path: /, port: 80}, intervalSeconds: 1, timeoutSeconds: 1}\n  update: {maxUnavailable: 1, progressDeadlineSeconds: 20}\n')
        cli('apply', '-f', str(manifest))
        wait('two real HTTP replicas healthy', healthy)
        assert {r['spec']['node'] for r in replicas()} == {'node1', 'node2'}, 'initial replicas not spread'
        old_ids = runtime_ids('node1')
        stop('node1')
        wait('replacement replicas healthy on surviving node', lambda: healthy() and all(r['spec']['node'] == 'node2' for r in replicas()), 150)
        assert old_ids <= runtime_ids('node1'), 'agent termination unexpectedly stopped containers'
        start('node1', agent_args['node1'])
        wait('stale containers removed after reconnect', lambda: not (old_ids & runtime_ids('node1')), 45)
        wait('cluster healthy after reconnect', healthy, 45)
        addresses = [r['status']['ip'] for r in replicas()]
        stop('server')
        # Verify HTTP independently of the unavailable control API.
        for address in addresses:
            with urllib.request.urlopen(f'http://{address}:80/', timeout=2) as response:
                assert response.status == 200, 'HTTP failed while server down'
        RESULTS.append({'assertion': 'HTTP survives control server outage', 'requests': len(addresses)})
        for name in ('node1', 'node2'):
            assert PROCESSES[name].poll() is None, 'agent died with server'
        start('server', server_args)
        wait('server restart restores cluster and HTTP', healthy, 60)
        bad = manifest.read_text().replace('  replicas: 2', '  command: ["/bin/sh", "-c", "exit 1"]\n  replicas: 2')
        manifest.write_text(bad)
        cli('apply', '-f', str(manifest))
        wait('broken release rolled back', lambda: objects('workloads')[0].get('status', {}).get('rollout', {}).get('phase') == 'RolledBack', 90)
        wait('HTTP healthy after rollback', healthy, 60)
    finally:
        for name in list(PROCESSES):
            stop(name)
        (OUT / 'cluster-fault.json').write_text(json.dumps(RESULTS, indent=2))
    print(json.dumps(RESULTS, indent=2))


if __name__ == '__main__':
    main()
