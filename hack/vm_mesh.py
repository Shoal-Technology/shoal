"""Real WireGuard acceptance across two guest kernels on one disposable runner.

QEMU SLIRP isolates each underlay; guest pod CIDRs are reachable only through
Shoal WireGuard. UDP host forwards connect peers via 10.0.2.2. No host bridges,
iptables, WireGuard or containerd mutations are required. No physical-host HA.
"""
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / 'test-results'
BASE = Path(tempfile.mkdtemp(prefix='shoal-vm-mesh-'))
PROCESSES = {}
RESULTS = []
SSH_PORTS = {1: 12021, 2: 12022}
WG_PORTS = {1: 15181, 2: 15182}
CONFIG = str(BASE / 'server/admin.conf')
BIN = str(ROOT / 'bin/shoal')


def validate_mesh(evidence):
    ids = evidence.get('boot_ids', [])
    assert len(ids) == 2 and all(ids) and len(set(ids)) == 2, 'independent guest kernels required'
    for kind in ('remote_http', 'service_http'):
        assert evidence.get(kind) == [200, 200], f'{kind} must succeed from both guests'
    handshakes = evidence.get('handshakes', [])
    assert len(handshakes) == 2 and all(h > 0 for h in handshakes), 'both WireGuard handshakes required'
    assert evidence.get('remote_routes') == ['shoal-wg', 'shoal-wg'], 'remote pod routes must use WireGuard'


def validate_withdrawal(rows, survivor, withdrawn, max_seconds=40):
    assert rows, 'nonempty withdrawal evidence required'
    converged = [r for r in rows if r.get('withdrawn_applied')]
    assert len(converged) >= 5, 'at least five requests after proxy withdrawal required'
    assert converged[0]['elapsed'] < max_seconds, 'endpoint withdrawal must precede eviction'
    for row in rows:
        assert survivor in row['endpoints'], 'surviving endpoint must never disappear'
    for row in converged:
        assert withdrawn not in row['endpoints'], 'withdrawn endpoint returned before reconnect'
        assert row['ok'], 'Service failed after proxy withdrawal'


def run(*args, timeout=60):

    try:
        return subprocess.check_output(args, text=True, stderr=subprocess.PIPE, timeout=timeout).strip()
    except subprocess.CalledProcessError as error:
        # Remote systemd-run may contain a join token; keep diagnostics without commands.
        detail = re.sub(r'--token\s+\S+', '--token [REDACTED]', error.stderr or '')
        raise OSError(f'{Path(args[0]).name} exit {error.returncode}: {detail[-2000:]}') from None
    except subprocess.TimeoutExpired:
        raise OSError(f'{Path(args[0]).name} timed out after {timeout}s') from None


def ssh(node, command, timeout=30):
    return run('ssh', '-i', str(BASE / 'key'), '-p', str(SSH_PORTS[node]),
               '-o', 'StrictHostKeyChecking=no', '-o', 'UserKnownHostsFile=/dev/null',
               '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=5',
               'root@127.0.0.1', command, timeout=timeout)


def copy(node, *paths):
    run('scp', '-i', str(BASE / 'key'), '-P', str(SSH_PORTS[node]),
        '-o', 'StrictHostKeyChecking=no', '-o', 'UserKnownHostsFile=/dev/null',
        *map(str, paths), 'root@127.0.0.1:/tmp/', timeout=120)


def cli(*args):
    return run(BIN, *args, '--config', CONFIG, '--server', '127.0.0.1:17443', '--timeout', '5s', timeout=10)


def objects(kind):
    text = cli('get', kind, '-o', 'json')
    result, decoder = [], json.JSONDecoder()
    while text:
        item, end = decoder.raw_decode(text)
        result.append(item)
        text = text[end:].strip()
    return result


def wait(label, predicate, timeout=120):
    begin, last = time.monotonic(), None
    while time.monotonic() - begin < timeout:
        try:
            value = predicate()
            if value:
                RESULTS.append({'assertion': label, 'seconds': round(time.monotonic() - begin, 3)})
                return value
        except (subprocess.SubprocessError, OSError, ValueError) as error:
            last = type(error).__name__  # never expose join credentials via exception commands
        time.sleep(2)
    raise AssertionError(f'{label} timed out ({last})')


def boot(node, image):
    disk = BASE / f'node{node}.qcow2'
    run('qemu-img', 'create', '-f', 'qcow2', '-F', 'qcow2', '-b', str(image), str(disk), '8G')
    seed = BASE / f'seed{node}'
    seed.mkdir()
    public_key = (BASE / 'key.pub').read_text().strip()
    (seed / 'user-data').write_text('#cloud-config\ndisable_root: false\nssh_pwauth: false\nusers:\n  - name: root\n    ssh_authorized_keys:\n      - ' + public_key + '\n')
    (seed / 'meta-data').write_text(f'instance-id: shoal-vm-{node}\nlocal-hostname: node{node}\n')
    iso = BASE / f'seed{node}.iso'
    run('genisoimage', '-output', str(iso), '-volid', 'cidata', '-joliet', '-rock',
        str(seed / 'user-data'), str(seed / 'meta-data'))
    accel = 'kvm' if os.access('/dev/kvm', os.R_OK | os.W_OK) else 'tcg'
    log = open(OUT / f'vm{node}-serial.log', 'w')
    PROCESSES[node] = subprocess.Popen([
        'qemu-system-x86_64', '-accel', accel, '-m', '1536', '-smp', '2',
        '-drive', f'file={disk},if=virtio', '-drive', f'file={iso},media=cdrom,readonly=on',
        '-netdev', f'user,id=net0,hostfwd=tcp:127.0.0.1:{SSH_PORTS[node]}-:22,hostfwd=udp:0.0.0.0:{WG_PORTS[node]}-:51820',
        '-device', 'virtio-net-pci,netdev=net0', '-display', 'none', '-serial', 'stdio', '-monitor', 'none',
    ], stdout=log, stderr=log)
    log.close()
    RESULTS.append({'observation': f'VM {node} accelerator', 'value': accel})


def provision(node):
    wait(f'VM {node} SSH ready', lambda: ssh(node, 'true') == '', 300)
    ssh(node, 'cloud-init status --wait >/dev/null', timeout=120)
    copy(node, ROOT / 'bin/shoal', Path('/tmp/shoal-vm-downloads/containerd.tar.gz'),
         Path('/tmp/shoal-vm-downloads/cni.tgz'))
    ssh(node, 'DEBIAN_FRONTEND=noninteractive apt-get update -qq && '
        'DEBIAN_FRONTEND=noninteractive apt-get install -y runc iptables iproute2 wireguard-tools curl && '
        'tar -xzf /tmp/containerd.tar.gz -C /usr/local && mkdir -p /opt/cni/bin && '
        'tar -xzf /tmp/cni.tgz -C /opt/cni/bin && install /tmp/shoal /usr/local/bin/shoal && '
        'sysctl -w net.ipv4.ip_forward=1 && systemd-run --unit=shoal-test-containerd /usr/local/bin/containerd', timeout=300)
    wait(f'VM {node} runtime ready', lambda: 'Server:' in ssh(node, 'ctr version'), 30)


def http(node, address):
    return int(ssh(node, f'curl --noproxy "*" --max-time 3 -s -o /dev/null -w "%{{http_code}}" http://{address}/'))


def ready():
    replicas = objects('replicas')
    return len(replicas) == 2 and all(r.get('status', {}).get('ready') and
        r.get('status', {}).get('templateGeneration') == r['spec'].get('templateGeneration') for r in replicas)


def observe(vip, begin, survivor, withdrawn, path):
    """Snapshot controller and real survivor DNAT before each new connection."""
    start = time.monotonic()
    endpoints = objects('services')[0].get('status', {}).get('endpoints', [])
    phases = {n['metadata']['name']: n.get('status', {}).get('phase') for n in objects('nodes')}
    rules = ssh(1, 'iptables-save -t nat')
    applied = (survivor in endpoints and withdrawn not in endpoints and
               f'--to-destination {survivor}:80' in rules and
               f'--to-destination {withdrawn}:80' not in rules)
    if applied and not path.exists():
        path.write_text(rules)
    row = {'monotonic': start, 'elapsed': round(start - begin, 3),
           'endpoints': endpoints, 'node_phases': phases, 'withdrawn_applied': applied}
    try:
        row['status'] = http(1, vip)
        row['ok'] = row['status'] == 200
    except (subprocess.SubprocessError, OSError, ValueError) as error:
        row.update(ok=False, error=type(error).__name__)
    row['seconds'] = round(time.monotonic() - start, 3)
    return row


def main():
    if os.geteuid() != 0 or os.environ.get('SHOAL_DISPOSABLE_TEST') != '1':
        raise RuntimeError('requires root with SHOAL_DISPOSABLE_TEST=1 on disposable runner')
    OUT.mkdir(exist_ok=True)
    evidence = {}
    try:
        run('ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(BASE / 'key'))
        image = Path('/tmp/shoal-vm-downloads/ubuntu.img')
        for node in (1, 2): boot(node, image)
        for node in (1, 2): provision(node)
        log = open(OUT / 'mesh-server.log', 'w')
        PROCESSES['server'] = subprocess.Popen([BIN, 'server', '--data-dir', str(BASE / 'server'),
            '--listen', '0.0.0.0:17443', '--advertise-address', '10.0.2.2:17443', '--ui-listen', ''], stdout=log, stderr=log)
        log.close()
        wait('server credentials created', lambda: Path(CONFIG).exists(), 30)
        for node in (1, 2):
            join = re.search(r'--token\s+(\S+).*--ca-hash\s+(\S+)', cli('token', 'create'))
            assert join, 'missing join credential'
            args = ['shoal', 'agent', '--server', '10.0.2.2:17443', '--node-name', f'node{node}',
                    '--state-dir', '/var/lib/shoal', '--wireguard-endpoint', f'10.0.2.2:{WG_PORTS[node]}',
                    '--token', join[1], '--ca-hash', join[2]]
            ssh(node, 'systemd-run --unit=shoal-agent ' + shlex.join(args))
        wait('two guest nodes Ready', lambda: len(objects('nodes')) == 2 and all(n.get('status', {}).get('phase') == 'Ready' for n in objects('nodes')), 60)
        manifest = BASE / 'workload.yaml'
        manifest.write_text('kind: Workload\nmetadata: {name: mesh-web}\nspec:\n  image: nginx:1.28-alpine\n  replicas: 2\n  resources: {cpu: 100m, memory: 64Mi}\n  healthCheck: {http: {path: /, port: 80}, intervalSeconds: 2, timeoutSeconds: 1}\n')
        cli('apply', '-f', str(manifest))
        wait('two guest HTTP workloads ready', ready, 150)
        replicas = objects('replicas')
        assert {r['spec']['node'] for r in replicas} == {'node1', 'node2'}, 'replicas must be spread'
        ips = {r['spec']['node']: r['status']['ip'] for r in replicas}
        service = BASE / 'service.yaml'
        service.write_text('kind: Service\nmetadata: {name: mesh-web}\nspec:\n  selector: mesh-web\n  ports: [{port: 80, targetPort: 80}]\n')
        cli('apply', '-f', str(service))
        wait('Service has two ready endpoints', lambda: len(objects('services')[0].get('status', {}).get('endpoints', [])) == 2, 30)
        vip = objects('services')[0]['status']['clusterIp']
        evidence['boot_ids'] = [ssh(n, 'cat /proc/sys/kernel/random/boot_id') for n in (1, 2)]
        evidence['remote_http'] = []
        evidence['service_http'] = []
        evidence['handshakes'] = []
        evidence['remote_routes'] = []
        for node in (1, 2):
            remote = ips[f'node{3-node}']
            evidence['remote_http'].append(wait(f'VM {node} remote HTTP via mesh', lambda: http(node, remote), 30))
            evidence['service_http'].append(wait(f'VM {node} ClusterIP HTTP', lambda: http(node, vip), 30))
            route = ssh(node, f'ip -j route get {remote}')
            evidence['remote_routes'].append(json.loads(route)[0]['dev'])
            # Public peer keys are omitted; never collect wg showconf/private keys.
            handshake = ssh(node, "wg show shoal-wg latest-handshakes | cut -f2")
            evidence['handshakes'].append(int(handshake))
            (OUT / f'vm{node}-route.json').write_text(route)
            (OUT / f'vm{node}-rules.txt').write_text(ssh(node, 'iptables-save'))
        validate_mesh(evidence)
        RESULTS.append({'assertion': 'independent kernels and bidirectional WireGuard HTTP validated', 'evidence': evidence})
        # Control-plane-only partition: UDP mesh and local runtime stay live.
        manifest.write_text(manifest.read_text().replace('  replicas: 2\n', '  replicas: 2\n  partitionPolicy: Hold\n'))
        cli('apply', '-f', str(manifest))
        wait('Hold template settled before partition', lambda: ready() and all(
             r['spec']['template'].get('partitionPolicy') == 'Hold' for r in objects('replicas')), 60)
        ips = {r['spec']['node']: r['status']['ip'] for r in objects('replicas')}
        wait('Hold endpoints settled', lambda: set(objects('services')[0].get('status', {}).get('endpoints', [])) == set(ips.values()), 30)
        original = {r['metadata']['name'] for r in objects('replicas')}
        blocked = 'iptables -I OUTPUT 1 -p tcp --dport 17443 -j REJECT --reject-with tcp-reset'
        unblock = blocked.replace('-I OUTPUT 1', '-D OUTPUT')
        ssh(2, blocked)
        begin = time.monotonic()
        partition_rows = []
        try:
            while time.monotonic() - begin < 75:
                row = observe(vip, begin, ips['node1'], ips['node2'], OUT / 'partition-withdrawn-rules.txt')
                # The uncertain replica must still be real and reachable over WireGuard.
                row['remote_http'] = http(1, ips['node2'])
                row['reverse_http'] = http(2, ips['node1'])
                row['partitioned_service_http'] = http(2, vip)
                assert row['remote_http'] == row['reverse_http'] == row['partitioned_service_http'] == 200
                assert {r['metadata']['name'] for r in objects('replicas')} == original, 'Hold changed assignment during partition'
                partition_rows.append(row)
                (OUT / 'partition-traffic.json').write_text(json.dumps(partition_rows, indent=2))
                time.sleep(0.2)
            validate_withdrawal(partition_rows, ips['node1'], ips['node2'])
            RESULTS.append({'assertion': 'Hold control-plane partition retains runtime with bidirectional data traffic and early endpoint withdrawal',
                            'seconds': round(time.monotonic() - begin, 3), 'requests': len(partition_rows),
                            'failed': sum(not r['ok'] for r in partition_rows),
                            'withdrawal_seconds': next(r['elapsed'] for r in partition_rows if r['withdrawn_applied'])})
        finally:
            ssh(2, unblock)
        begin_heal = time.monotonic()
        wait('reconnected node Ready and original endpoints restored', lambda:
             all(n.get('status', {}).get('phase') == 'Ready' for n in objects('nodes')) and
             set(objects('services')[0].get('status', {}).get('endpoints', [])) == set(ips.values()), 60)
        wait('survivor proxy restores remote endpoint', lambda:
             f"--to-destination {ips['node2']}:80" in ssh(1, 'iptables-save -t nat'), 30)
        assert {r['metadata']['name'] for r in objects('replicas')} == original
        assert http(1, vip) == http(2, vip) == 200
        RESULTS.append({'assertion': 'reconnect restores original assignments and Service', 'seconds': round(time.monotonic() - begin_heal, 3)})
        manifest.write_text(manifest.read_text().replace('  partitionPolicy: Hold\n', ''))
        cli('apply', '-f', str(manifest))
        wait('Replace template settled before VM loss', lambda: ready() and all(
             not r['spec']['template'].get('partitionPolicy') for r in objects('replicas')), 60)
        assert {r['spec']['node'] for r in objects('replicas')} == {'node1', 'node2'}
        ips = {r['spec']['node']: r['status']['ip'] for r in objects('replicas')}
        wait('Replace endpoints settled', lambda: set(objects('services')[0].get('status', {}).get('endpoints', [])) == set(ips.values()), 30)
        for node in (1, 2):
            wait(f'VM {node} proxy settled before loss', lambda: all(
                 f'--to-destination {ip}:80' in ssh(node, 'iptables-save -t nat') for ip in ips.values()), 30)
        # Fixed Service from the surviving guest during complete worker VM loss.
        PROCESSES[2].kill()
        PROCESSES[2].wait(timeout=15)
        begin = time.monotonic()
        rows = []
        deadline = begin + 150
        while time.monotonic() < deadline:
            rows.append(observe(vip, begin, ips['node1'], ips['node2'], OUT / 'loss-withdrawn-rules.txt'))
            if rows[-1]['withdrawn_applied']:
                assert rows[-1]['ok'], 'new Service connection failed after proxy withdrawal'
            (OUT / 'mesh-traffic.json').write_text(json.dumps(rows, indent=2))
            current = objects('replicas')
            if ready() and all(r['spec']['node'] == 'node1' for r in current):
                break
            time.sleep(0.2)
        else:
            raise AssertionError('replacement after VM loss timed out')
        validate_withdrawal(rows, ips['node1'], ips['node2'])
        assert http(1, vip) == 200
        converged = [r for r in rows if r['withdrawn_applied']]
        RESULTS.append({'assertion': 'real VM loss replacement healthy on survivor with early endpoint withdrawal', 'seconds': round(time.monotonic() - begin, 3),
                        'requests': len(rows), 'failed': sum(not r['ok'] for r in rows),
                        'withdrawal_seconds': converged[0]['elapsed'], 'after_withdrawal_requests': len(converged),
                        'after_withdrawal_failed': sum(not r['ok'] for r in converged),
                        'sample_interval': 'SSH and snapshot overhead plus 0.2 seconds; bounded 3s HTTP timeout'})
        (OUT / 'mesh-traffic.json').write_text(json.dumps(rows, indent=2))
    finally:
        for node in (1, 2):
            if node in PROCESSES and PROCESSES[node].poll() is None:
                for command, suffix in [('journalctl -u shoal-agent --no-pager', 'agent.log'), ('ip -j route', 'routes.json')]:
                    try:
                        content = ssh(node, command)
                        content = re.sub(r'--token\s+\S+', '--token [REDACTED]', content)
                        (OUT / f'vm{node}-{suffix}').write_text(content)
                    except (subprocess.SubprocessError, OSError): pass
        (OUT / 'mesh-evidence.json').write_text(json.dumps({'results': RESULTS, 'mesh': evidence}, indent=2))
        for process in PROCESSES.values():
            if process.poll() is None:
                process.kill()
                process.wait(timeout=15)
        # Delete join credentials, guest disks, and temporary SSH key; artifacts contain none.
        import shutil
        shutil.rmtree(BASE)
    print(json.dumps(RESULTS, indent=2))


if __name__ == '__main__': main()
