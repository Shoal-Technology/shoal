import json
from pathlib import Path
import socket
import socketserver
import tempfile
import threading
import time
import unittest
from fault_observer import PartitionProxy, TrafficSampler


class Echo(socketserver.BaseRequestHandler):
    def handle(self):
        while True:
            data = self.request.recv(1024)
            if not data:
                return
            self.request.sendall(data)


class ObserverTests(unittest.TestCase):
    def test_partition_severs_active_and_rejects_new_then_heals(self):
        server = socketserver.ThreadingTCPServer(('127.0.0.1', 0), Echo)
        server.daemon_threads = True
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        proxy = PartitionProxy(('127.0.0.1', 0), server.server_address)
        try:
            with socket.create_connection(proxy.address, timeout=2) as client:
                client.sendall(b'before')
                self.assertEqual(client.recv(1024), b'before')
                proxy.partition()
                self.assertEqual(client.recv(1024), b'')
            with socket.create_connection(proxy.address, timeout=2) as client:
                self.assertEqual(client.recv(1024), b'')
            proxy.heal()
            with socket.create_connection(proxy.address, timeout=2) as client:
                client.sendall(b'after')
                self.assertEqual(client.recv(1024), b'after')
        finally:
            proxy.close()
            server.shutdown()
            server.server_close()
            thread.join()

    def test_failed_http_attempts_are_recorded_not_zero_filled(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'traffic.jsonl'
            sampler = TrafficSampler(path)
            sampler.update(['127.0.0.1:0'], 'unavailable')
            time.sleep(0.7)
            summary = sampler.close()
            self.assertGreater(summary['unavailable']['requests'], 0)
            self.assertEqual(summary['unavailable']['requests'], summary['unavailable']['failed'])
            rows = [json.loads(line) for line in path.read_text().splitlines()]
            self.assertTrue(all(not row['ok'] and 'error' in row for row in rows))


if __name__ == '__main__':
    unittest.main()
