"""Three-VM multi-host acceptance: raft control plane, agents, edge, failover, partition.

Three QEMU guests run on one disposable runner. Each guest runs a shoal server
(raft store) and an agent. Guests reach each other only through SLIRP host
forwards at 10.0.2.2, so every peer address is 10.0.2.2:<host port>. Pod traffic
crosses the WireGuard mesh, as in hack/vm_mesh.py. Nothing here touches a
machine other than the disposable runner (SHOAL_DISPOSABLE_TEST=1 is required).
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
BASE = Path(tempfile.mkdtemp(prefix='shoal-multihost-'))
BIN = str(ROOT / 'bin/shoal')
NODES = (1, 2, 3)
SSH_PORT = {n: 12020 + n for n in NODES}
API_PORT = {n: 17440 + n for n in NODES}
RAFT_PORT = {n: 17450 + n for n in NODES}
WG_PORT = {n: 15180 + n for n in NODES}
EDGE_PORT = 18080  # host port -> node1:80, where the edge label is set
HOST = 'mesh.multihost.test'
SERVICE = 'mesh-web'
ADMIN = BASE / 'admin.conf'
PROCESSES = {}
RESULTS = []
EVIDENCE = {'phases': {}}
LIVE_SERVERS = set(NODES)
RUNNING_AGENTS = set(NODES)
SSH_KEY = str(BASE / 'key')
SSH_OPTS = ['-i', SSH_KEY, '-o', 'StrictHostKeyChecking=no', '-o', 'UserKnownHostsFile=/dev/null',
            '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=5']


def record(**row):
    RESULTS.append(row)
    print('RESULT', json.dumps(row), flush=True)


def redact(text):
    # Join tokens and bundles must never reach logs or artifacts.
    return re.sub(r'--token\s+\S+', '--token [REDACTED]', text or '')


def run(*args, timeout=60):
    try:
        return subprocess.check_output(args, text=True, stderr=subprocess.PIPE, timeout=timeout).strip()
    except subprocess.CalledProcessError as error:
        raise OSError(f'{Path(args[0]).name} exit {error.returncode}: {redact(error.stderr)[-1500:]}') from None
    except subprocess.TimeoutExpired:
        raise OSError(f'{Path(args[0]).name} timed out after {timeout}s') from None


def ssh(node, command, timeout=30):
    return run('ssh', *SSH_OPTS, '-p', str(SSH_PORT[node]), 'root@127.0.0.1', command, timeout=timeout)


def scp_to(node, *paths):
    run('scp', *SSH_OPTS, '-P', str(SSH_PORT[node]), *map(str, paths), 'root@127.0.0.1:/tmp/', timeout=120)


def scp_from(node, remote, local):
    run('scp', *SSH_OPTS, '-P', str(SSH_PORT[node]), f'root@127.0.0.1:{remote}', str(local), timeout=60)


def cli(*args, node=None):
    """Run the shoal CLI on the runner against a live server (the leader or any member)."""
    live = sorted(LIVE_SERVERS)
    if node is not None:
        live = [node]
    last = None
    for n in live:
        try:
            return run(BIN, *args, '--config', str(ADMIN), '--server', f'127.0.0.1:{API_PORT[n]}',
                       '--timeout', '5s', timeout=15)
        except OSError as error:
            last = error
    raise last or OSError('no live server')


def objects(kind):
    text = cli('get', kind, '-o', 'json')
    result, decoder = [], json.JSONDecoder()
    while text:
        item, end = decoder.raw_decode(text)
        result.append(item)
        text = text[end:].strip()
    return result


def status_rows():
    """(node_id, is_leader) from cluster status, plus the Leader API line."""
    text = cli('cluster', 'status')
    members = []
    for line in text.splitlines():
        parts = line.split()
        if len(parts) == 4 and parts[0].startswith('node'):
            members.append((parts[0], parts[3] == 'yes'))
    leader_api = re.search(r'Leader API:\s+(\S+)', text)
    return members, (leader_api.group(1) if leader_api else 'none')


def leader_node():
    members, _ = status_rows()
    leaders = [m for m, is_leader in members if is_leader]
    return leaders[0] if len(leaders) == 1 else None


def wait(label, predicate, timeout=120, sample=None):
    """Poll predicate until truthy. sample() runs between polls and returns a row list."""
    begin, last = time.monotonic(), None
    while time.monotonic() - begin < timeout:
        try:
            value = predicate()
            if value:
                record(assertion=label, seconds=round(time.monotonic() - begin, 3))
                return value
        except (subprocess.SubprocessError, OSError, ValueError, KeyError, IndexError) as error:
            last = type(error).__name__
        if sample:
            sample()
        else:
            time.sleep(2)
    raise AssertionError(f'{label} timed out ({last})')


def edge_code():
    try:
        return subprocess.run(['curl', '-s', '-o', '/dev/null', '-w', '%{http_code}', '--max-time', '2',
                               '-H', f'Host: {HOST}', f'http://127.0.0.1:{EDGE_PORT}/'],
                              capture_output=True, text=True, timeout=5).stdout.strip()
    except subprocess.SubprocessError:
        return 'error'


def guest_http(node, address):
    return int(ssh(node, f'curl --noproxy "*" --max-time 3 -s -o /dev/null -w "%{{http_code}}" http://{address}/'))


def boot(node, image):
    disk = BASE / f'node{node}.qcow2'
    run('qemu-img', 'create', '-f', 'qcow2', '-F', 'qcow2', '-b', str(image), str(disk), '8G')
    seed = BASE / f'seed{node}'
    seed.mkdir()
    public_key = (BASE / 'key.pub').read_text().strip()
    (seed / 'user-data').write_text('#cloud-config\ndisable_root: false\nssh_pwauth: false\nusers:\n  - name: root\n    ssh_authorized_keys:\n      - ' + public_key + '\n')
    (seed / 'meta-data').write_text(f'instance-id: shoal-multihost-{node}\nlocal-hostname: node{node}\n')
    iso = BASE / f'seed{node}.iso'
    run('genisoimage', '-output', str(iso), '-volid', 'cidata', '-joliet', '-rock',
        str(seed / 'user-data'), str(seed / 'meta-data'))
    accel = 'kvm' if os.access('/dev/kvm', os.R_OK | os.W_OK) else 'tcg'
    forwards = [f'hostfwd=tcp:127.0.0.1:{SSH_PORT[node]}-:22',
                f'hostfwd=tcp:127.0.0.1:{API_PORT[node]}-:7443',
                f'hostfwd=tcp:127.0.0.1:{RAFT_PORT[node]}-:7445',
                f'hostfwd=udp:0.0.0.0:{WG_PORT[node]}-:51820']
    if node == 1:
        forwards.append(f'hostfwd=tcp:127.0.0.1:{EDGE_PORT}-:80')
    log = open(OUT / f'vm{node}-serial.log', 'w')
    PROCESSES[node] = subprocess.Popen([
        'qemu-system-x86_64', '-accel', accel, '-m', '1536', '-smp', '2',
        '-drive', f'file={disk},if=virtio', '-drive', f'file={iso},media=cdrom,readonly=on',
        '-netdev', 'user,id=net0,' + ','.join(forwards),
        '-device', 'virtio-net-pci,netdev=net0', '-display', 'none', '-serial', 'stdio', '-monitor', 'none',
    ], stdout=log, stderr=log)
    log.close()
    record(observation=f'VM {node} accelerator', value=accel)


def provision(node):
    wait(f'VM {node} SSH ready', lambda: ssh(node, 'true') == '', 300)
    ssh(node, 'cloud-init status --wait >/dev/null', timeout=180)
    scp_to(node, ROOT / 'bin/shoal', Path('/tmp/shoal-vm-downloads/containerd.tar.gz'),
           Path('/tmp/shoal-vm-downloads/cni.tgz'))
    ssh(node, 'DEBIAN_FRONTEND=noninteractive apt-get update -qq && '
        'DEBIAN_FRONTEND=noninteractive apt-get install -y runc iptables iproute2 wireguard-tools curl && '
        'tar -xzf /tmp/containerd.tar.gz -C /usr/local && mkdir -p /opt/cni/bin && '
        'tar -xzf /tmp/cni.tgz -C /opt/cni/bin && install /tmp/shoal /usr/local/bin/shoal && '
        'sysctl -w net.ipv4.ip_forward=1 && systemd-run --unit=shoal-test-containerd /usr/local/bin/containerd', timeout=400)
    wait(f'VM {node} runtime ready', lambda: 'Server:' in ssh(node, 'ctr version'), 60)


def server_args(node, join):
    args = ['shoal', 'server', '--data-dir', '/var/lib/shoal/server', '--listen', '0.0.0.0:7443',
            '--advertise-address', f'10.0.2.2:{API_PORT[node]}', '--store', 'raft',
            '--raft-node-id', f'node{node}', '--raft-bind', '0.0.0.0:7445',
            '--raft-advertise', f'10.0.2.2:{RAFT_PORT[node]}', '--ui-listen', '', '--acme-disable']
    if join:
        args += ['--raft-join-bundle', '/tmp/join.bundle', '--raft-join', f'10.0.2.2:{API_PORT[1]}']
    return args


def agent_args(node, token=None, ca_hash=None, server_node=None):
    # Joins must reach the leader ("join: not the leader" otherwise); afterwards the
    # agent talks to its own server, which forwards to the leader.
    args = ['shoal', 'agent', '--server', f'10.0.2.2:{API_PORT[server_node or node]}', '--node-name', f'node{node}',
            '--state-dir', '/var/lib/shoal', '--wireguard-endpoint', f'10.0.2.2:{WG_PORT[node]}']
    if token:
        args += ['--token', token, '--ca-hash', ca_hash]
    return args


def start_unit(node, unit, args):
    ssh(node, f'systemd-run --unit={unit} ' + shlex.join(args))


def stop_agent(node):
    ssh(node, 'systemctl stop shoal-agent; systemctl stop shoal-test-containerd; true')


def start_agent(node, token=None, ca_hash=None):
    if token is None:
        # Restart with the same identity: containerd first, then the agent.
        ssh(node, 'systemd-run --unit=shoal-test-containerd /usr/local/bin/containerd >/dev/null 2>&1 || true')
    start_unit(node, 'shoal-agent', agent_args(node, token, ca_hash))


def workload(image, replicas=3):
    return ('kind: Workload\nmetadata: {name: ' + SERVICE + '}\nspec:\n'
            f'  image: {image}\n  replicas: {replicas}\n  ports: [{{containerPort: 80}}]\n'
            '  resources: {cpu: 100m, memory: 64Mi}\n'
            '  healthCheck: {http: {path: /, port: 80}, intervalSeconds: 2, timeoutSeconds: 1}\n')


def ready_replicas():
    replicas = objects('replicas')
    return [r for r in replicas if r.get('status', {}).get('ready') and
            r.get('status', {}).get('templateGeneration') == r['spec'].get('templateGeneration')]


def all_ready(count=3):
    ready = ready_replicas()
    return len(ready) == count and len(objects('replicas')) == count


def endpoints():
    return set(objects('services')[0].get('status', {}).get('endpoints', []))


def node_phases():
    return {n['metadata']['name']: n.get('status', {}).get('phase') for n in objects('nodes')}


def dump_diagnostics(tag):
    """Write sanitized logs and state from every guest and the cluster for the artifact."""
    OUT.mkdir(exist_ok=True)
    (OUT / f'{tag}-cluster.txt').write_text(redact(_safe(lambda: cli('cluster', 'status'))))
    (OUT / f'{tag}-objects.txt').write_text(redact(_safe(lambda: json.dumps(
        {k: objects(k) for k in ('nodes', 'replicas', 'services', 'routes')}, indent=1))))
    for n in NODES:
        for name, cmd in {
            'server': 'journalctl -u shoal-server -n 400 --no-pager',
            'agent': 'journalctl -u shoal-agent -n 400 --no-pager',
            'containerd': 'journalctl -u shoal-test-containerd -n 200 --no-pager',
            'iptables': 'iptables-save',
            'containers': 'ctr -n shoal containers ls',
            'routes': 'ip route; wg show shoal-wg latest-handshakes',
        }.items():
            (OUT / f'{tag}-vm{n}-{name}.txt').write_text(redact(_safe(lambda: ssh(n, cmd, timeout=60))))


def _safe(fn):
    try:
        return fn()
    except Exception as error:  # diagnostics must never mask the original failure
        return f'unavailable: {type(error).__name__}'


def main():
    if os.geteuid() != 0 or os.environ.get('SHOAL_DISPOSABLE_TEST') != '1':
        raise RuntimeError('requires root with SHOAL_DISPOSABLE_TEST=1 on disposable runner')
    OUT.mkdir(exist_ok=True)
    try:
        scenario()
    except BaseException:
        dump_diagnostics('failure')
        raise
    finally:
        for n in NODES:
            try:
                ssh(n, 'iptables -F OUTPUT; iptables -F INPUT; true', timeout=20)
            except OSError:
                pass
        (OUT / 'multihost-evidence.json').write_text(json.dumps({'results': RESULTS, 'evidence': EVIDENCE}, indent=2))
        for p in PROCESSES.values():
            p.kill()


def scenario():
    run('ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', SSH_KEY)
    image = Path('/tmp/shoal-vm-downloads/ubuntu.img')
    for n in NODES:
        boot(n, image)
    for n in NODES:
        provision(n)
    EVIDENCE['vm_count'] = len(NODES)

    # a) three-server raft cluster joined by bundle, one agent per guest
    ssh(1, 'systemd-run --unit=shoal-server ' + shlex.join(server_args(1, join=False)))
    wait('node1 bootstraps raft and writes admin config', lambda: (
        ssh(1, 'test -s /var/lib/shoal/server/admin.conf && echo ok') == 'ok'), 120)
    scp_from(1, '/var/lib/shoal/server/admin.conf', ADMIN)
    wait('single-member raft has a leader', lambda: leader_node() == 'node1', 60)
    bundle = BASE / 'join.bundle'
    cli('cluster', 'join-bundle', '--out', str(bundle), node=1)
    for n in (2, 3):
        scp_to(n, bundle)
        ssh(n, 'mv /tmp/join.bundle /tmp/join.bundle.tmp && install -m 0600 /tmp/join.bundle.tmp /tmp/join.bundle')
        start_unit(n, 'shoal-server', server_args(n, join=True))
    wait('three raft voters, one leader', lambda: len(status_rows()[0]) == 3 and leader_node() is not None, 240)
    for n in (2, 3):
        ssh(n, 'systemctl is-active shoal-server >/dev/null')

    tokens = []
    for n in NODES:
        m = re.search(r'--token\s+(\S+).*--ca-hash\s+(\S+)', cli('token', 'create', '--description', f'multihost node{n}'))
        assert m, 'missing join credential'
        tokens.append(m.groups())
    for n in NODES:
        # First start joins through the leader (node1), then the agent is switched to its local server.
        start_unit(n, 'shoal-agent', agent_args(n, *tokens[n - 1], server_node=1))
        wait(f'node{n} agent enrolled', lambda n=n: ssh(n, 'test -s /var/lib/shoal/agent/credentials.conf && echo ok') == 'ok', 120)
    wait('three nodes Ready', lambda: len(node_phases()) == 3 and all(p == 'Ready' for p in node_phases().values()), 240)
    cli('label', 'node', 'node1', 'shoal.dev/edge=true')
    record(assertion='node1 labelled as edge; three-server raft and three agents up', nodes=node_phases())

    # b) 3 replicas + Service + edge Route, spread over the nodes
    manifest = BASE / 'workload.yaml'
    manifest.write_text(workload('nginx:1.27-alpine'))
    cli('apply', '-f', str(manifest))
    service = BASE / 'service.yaml'
    service.write_text(f'kind: Service\nmetadata: {{name: {SERVICE}}}\nspec: {{selector: {SERVICE}, ports: [{{port: 80, targetPort: 80}}]}}\n')
    cli('apply', '-f', str(service))
    route = BASE / 'route.yaml'
    route.write_text(f'kind: Route\nmetadata: {{name: {SERVICE}}}\nspec: {{host: {HOST}, service: {SERVICE}, port: 80}}\n')
    cli('apply', '-f', str(route))
    wait('three ready replicas', lambda: all_ready(3), 300)
    spread = {r['spec']['node'] for r in objects('replicas')}
    EVIDENCE['replica_nodes'] = sorted(spread)
    assert spread == {'node1', 'node2', 'node3'}, f'replicas must spread over all nodes, got {spread}'
    vip = objects('services')[0]['status']['clusterIp']
    wait('service has three endpoints', lambda: len(endpoints()) == 3, 60)
    wait('edge answers through node1', lambda: edge_code() == '200', 120)
    ips = {r['spec']['node']: r['status']['ip'] for r in objects('replicas')}
    cross = {}
    for n in NODES:
        for name, addr in [('service_vip', vip)] + [(f'replica_{m}', ips[f"node{m}"]) for m in NODES if m != n]:
            cross[f'node{n}->{name}'] = guest_http(n, addr)
    EVIDENCE['cross_node_http'] = cross
    assert all(v == 200 for v in cross.values()), f'cross-node HTTP failed: {cross}'
    record(assertion='cross-node pod and Service HTTP over the WireGuard mesh', evidence=cross)
    # Service DNS is not implemented as a name lookup in this harness; the ClusterIP is used.
    EVIDENCE['service_dns'] = 'not tested: ClusterIP used'

    # c) hard failure of node2's agent and containerd (its server stays up so raft keeps quorum)
    samples = []
    t0 = time.monotonic()

    def sample():
        samples.append({'t': round(time.monotonic() - t0, 2), 'code': edge_code(),
                        'endpoints': sorted(endpoints())})
        time.sleep(0.5)

    stop_agent(2)
    RUNNING_AGENTS.discard(2)
    t_stop = time.monotonic()
    node2_ip = ips['node2']
    wait('node2 NotReady', lambda: node_phases().get('node2') == 'NotReady', 150, sample)
    t_notready = time.monotonic() - t0
    wait('node2 endpoints withdrawn', lambda: node2_ip not in endpoints(), 150, sample)
    t_withdrawn = time.monotonic() - t0
    wait('node2 replicas rescheduled to node1/node3', lambda: all_ready(3) and
         not any(r['spec']['node'] == 'node2' for r in objects('replicas')), 300, sample)
    t_rescheduled = time.monotonic() - t0
    while time.monotonic() - t0 < (t_rescheduled + 15):
        sample()
    window = [s for s in samples if s['t'] >= t_stop - t0]
    failed = [s for s in window if s['code'] != '200']
    after_withdrawn = [s for s in window if s['t'] >= t_withdrawn and s['code'] != '200']
    EVIDENCE['failover'] = {
        'notready_seconds': round(t_notready, 1), 'endpoints_withdrawn_seconds': round(t_withdrawn, 1),
        'rescheduled_seconds': round(t_rescheduled, 1), 'requests': len(window), 'failed_requests': len(failed),
        'failed_after_withdrawal': len(after_withdrawn)}
    record(assertion='agent failover measured', **EVIDENCE['failover'])
    assert not after_withdrawn, 'edge failed after endpoints were withdrawn'
    EVIDENCE['failover_samples'] = samples

    # d) leader failover: kill the leader's server; its agent is re-pointed at a surviving server
    old = leader_node()
    leader_vm = int(old.removeprefix('node'))
    t_kill = time.monotonic()
    ssh(leader_vm, 'systemctl kill -s KILL shoal-server; true')
    LIVE_SERVERS.discard(leader_vm)
    survivor = min(LIVE_SERVERS)
    # The agent on the killed VM would otherwise lose its only server; re-point it (identity kept on disk).
    new = wait(f'new raft leader after killing {old}', lambda: (lambda x: x if x not in (None, old) else None)(leader_node()), 120)
    repoint_agents(int(new.removeprefix('node')))
    EVIDENCE['leader_failover_seconds'] = round(time.monotonic() - t_kill, 1)
    record(assertion='new leader elected on another VM', old=old, new=new, seconds=EVIDENCE['leader_failover_seconds'])
    manifest.write_text(workload('nginx:1.28-alpine'))
    cli('apply', '-f', str(manifest))
    wait('rolling update after leader failover ready', lambda: all_ready(3) and
         objects('workloads')[0]['spec']['image'] == 'nginx:1.28-alpine', 300)
    wait('edge still 200 after leader failover', lambda: edge_code() == '200', 60)
    # Restore three voters before the partition step (two servers alone could not keep quorum).
    ssh(leader_vm, 'systemctl reset-failed shoal-server 2>/dev/null; true')
    start_unit(leader_vm, 'shoal-server', server_args(leader_vm, join=True))
    LIVE_SERVERS.add(leader_vm)
    wait('killed server rejoins as a voter', lambda: len(status_rows()[0]) == 3 and leader_node() is not None, 240)

    # e) partition node3 from the other VMs (all traffic to 10.0.2.2 except ssh) for 60s
    rules = [('OUTPUT', ['-p', 'tcp', '--sport', '22', '-j', 'ACCEPT']),
             ('INPUT', ['-p', 'tcp', '--dport', '22', '-j', 'ACCEPT']),
             ('OUTPUT', ['-d', '10.0.2.2', '-j', 'DROP']),
             ('INPUT', ['-s', '10.0.2.2', '-j', 'DROP'])]
    split_rows = []
    ssh(3, 'true')
    for chain, spec in rules[:2]:
        ssh(3, f'iptables -I {chain} 1 ' + shlex.join(spec))
    for chain, spec in rules[2:]:
        ssh(3, f'iptables -I {chain} 3 ' + shlex.join(spec))
    t_part = time.monotonic()

    def split_sample():
        row = {'t': round(time.monotonic() - t_part, 1), 'edge': edge_code()}
        leaders = []
        for n in sorted(LIVE_SERVERS):
            if n == 3:
                continue  # node3 is cut off; its server must not be consulted
            try:
                leaders.append(leader_node_of(n))
            except OSError as error:
                leaders.append(f'err:{type(error).__name__}')
        row['leaders_seen'] = leaders
        names = [x for x in leaders if x and not x.startswith('err')]
        row['one_leader'] = len(set(names)) == 1 and len(names) == len([x for x in leaders if not x.startswith('err')])
        split_rows.append(row)
        time.sleep(3)

    while time.monotonic() - t_part < 60:
        split_sample()
    try:
        for chain, spec in rules:
            ssh(3, f'iptables -D {chain} ' + shlex.join(spec))
    finally:
        ssh(3, 'iptables -F OUTPUT; iptables -F INPUT; true')
    EVIDENCE['partition_samples'] = split_rows
    assert all(r['one_leader'] for r in split_rows if r['leaders_seen']), 'split brain: leaders disagree during partition'
    assert all(r['edge'] == '200' for r in split_rows), 'edge failed during partition'
    record(assertion='partition: one leader seen by reachable servers, edge kept answering', samples=len(split_rows))

    repoint_agents(int(leader_node().removeprefix('node')))
    wait('node3 Ready after heal', lambda: node_phases().get('node3') == 'Ready', 240)
    wait('replicas ready and Service endpoints match after heal', lambda: all_ready(3) and
         endpoints() == {r['status']['ip'] for r in ready_replicas()}, 240)
    members_after = status_rows()[0]
    record(assertion='partitioned node rejoined; consistent membership after heal', members=members_after)

    # f) bring node2 back: containerd and agent (its server never stopped)
    ssh(2, 'systemd-run --unit=shoal-test-containerd /usr/local/bin/containerd >/dev/null 2>&1 || true')
    start_unit(2, 'shoal-agent', agent_args(2, server_node=int(leader_node().removeprefix('node'))))
    RUNNING_AGENTS.add(2)
    wait('node2 Ready again after restart', lambda: node_phases().get('node2') == 'Ready', 240)
    wait('endpoints restored to three after node2 returns', lambda: all_ready(3) and len(endpoints()) == 3, 240)
    wait('edge answers after node2 returns', lambda: edge_code() == '200', 60)
    record(assertion='node2 rejoined after agent and containerd restart', phases=node_phases())


def repoint_agents(leader_vm):
    """Restart every running agent so it talks to the current leader.

    Agent Sync streams are not forwarded by followers (ADR 0014, pass 2d), and
    an agent keeps only one server address. Until the agent can follow a leader
    hint, the harness restarts agents at the leader. Identity stays on disk.
    """
    for n in sorted(RUNNING_AGENTS):
        ssh(n, 'systemctl stop shoal-agent; true')
        start_unit(n, 'shoal-agent', agent_args(n, server_node=leader_vm))


def leader_node_of(n):
    """Leader id as seen by the server on node n; None when it has none."""
    text = run(BIN, 'cluster', 'status', '--config', str(ADMIN), '--server', f'127.0.0.1:{API_PORT[n]}',
               '--timeout', '5s', timeout=15)
    leaders = [line.split()[0] for line in text.splitlines()
               if len(line.split()) == 4 and line.split()[0].startswith('node') and line.split()[3] == 'yes']
    return leaders[0] if len(leaders) == 1 else None


if __name__ == '__main__':
    main()
