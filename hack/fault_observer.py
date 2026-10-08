"""TCP control-plane partition and real HTTP sampling, with no firewall changes."""
import json
import socket
import threading
import time
import urllib.request


class PartitionProxy:
    def __init__(self, listen, upstream):
        self.upstream = upstream
        self.blocked = threading.Event()
        self.closed = threading.Event()
        self.lock = threading.Lock()
        self.connections = set()
        self.listener = socket.socket()
        self.listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.listener.bind(listen)
        self.address = self.listener.getsockname()
        self.listener.listen()
        self.listener.settimeout(0.2)
        self.thread = threading.Thread(target=self._accept, daemon=True)
        self.thread.start()

    def _accept(self):
        while not self.closed.is_set():
            try:
                client, _ = self.listener.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            with self.lock:
                if self.blocked.is_set():
                    client.close()
                    continue
                try:
                    upstream = socket.create_connection(self.upstream, timeout=2)
                    upstream.settimeout(None)
                except OSError:
                    client.close()
                    continue
                self.connections.update((client, upstream))
            for source, target in ((client, upstream), (upstream, client)):
                threading.Thread(target=self._relay, args=(source, target), daemon=True).start()

    def _relay(self, source, target):
        try:
            while not self.closed.is_set():
                data = source.recv(65536)
                if not data:
                    break
                target.sendall(data)
        except OSError:
            pass
        finally:
            with self.lock:
                self.connections.discard(source)
                self.connections.discard(target)
            self._close_socket(source)
            self._close_socket(target)

    @staticmethod
    def _close_socket(sock):
        try:
            sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        sock.close()

    def partition(self):
        with self.lock:
            self.blocked.set()
            for sock in list(self.connections):
                self._close_socket(sock)
            self.connections.clear()

    def heal(self):
        self.blocked.clear()

    def close(self):
        self.closed.set()
        self.partition()
        self.listener.close()
        self.thread.join(timeout=3)


class TrafficSampler:
    """Round-robin direct pod requests to cached control-plane endpoints, 5Hz.

    This is not a Shoal Service/LB probe. Removed/stale endpoints can fail until
    the caller refreshes discovery; every attempt records its exact endpoint.
    """
    def __init__(self, output):
        self.output = output
        self.lock = threading.Lock()
        self.addresses = []
        self.phase = 'startup'
        self.rows = []
        self.closed = threading.Event()
        self.thread = threading.Thread(target=self._run, daemon=True)
        self.thread.start()

    def update(self, addresses=None, phase=None):
        with self.lock:
            if addresses is not None:
                self.addresses = sorted(set(addresses))
            if phase is not None:
                self.phase = phase

    def _run(self):
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        index = 0
        while not self.closed.is_set():
            begin = time.monotonic()
            with self.lock:
                addresses, phase = list(self.addresses), self.phase
            if addresses:
                address = addresses[index % len(addresses)]
                index += 1
                row = {'monotonic': begin, 'phase': phase, 'address': address}
                try:
                    with opener.open(f'http://{address}/', timeout=0.5) as response:
                        row['status'] = response.status
                        row['ok'] = response.status == 200
                except Exception as error:
                    row.update(ok=False, error=str(error))
                row['seconds'] = round(time.monotonic() - begin, 4)
                self.rows.append(row)
            self.closed.wait(max(0, 0.2 - (time.monotonic() - begin)))

    def close(self):
        self.closed.set()
        self.thread.join(timeout=3)
        self.output.write_text(''.join(json.dumps(row) + '\n' for row in self.rows))
        summary = {}
        for row in self.rows:
            item = summary.setdefault(row['phase'], {'requests': 0, 'failed': 0})
            item['requests'] += 1
            item['failed'] += int(not row['ok'])
        for phase, item in summary.items():
            streak_start = None
            longest = 0.0
            for row in (r for r in self.rows if r['phase'] == phase):
                if not row['ok']:
                    if streak_start is None:
                        streak_start = row['monotonic']
                    longest = max(longest, row['monotonic'] + row['seconds'] - streak_start)
                else:
                    streak_start = None
            item['longest_observed_failure_streak_seconds'] = round(longest, 3)
        return summary
