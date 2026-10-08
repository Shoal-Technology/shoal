"""Acceptance-evidence contract: observations alone must not pass the VM gate."""
import unittest
from vm_mesh import validate_mesh

class MeshEvidenceTests(unittest.TestCase):
    def evidence(self):
        return {'boot_ids': ['kernel-a', 'kernel-b'], 'remote_http': [200, 200],
                'service_http': [200, 200], 'handshakes': [123, 124],
                'remote_routes': ['shoal-wg', 'shoal-wg']}

    def test_complete_mesh(self):
        validate_mesh(self.evidence())

    def test_reject_shared_kernel(self):
        e = self.evidence(); e['boot_ids'][1] = e['boot_ids'][0]
        with self.assertRaises(AssertionError): validate_mesh(e)

    def test_reject_missing_handshake(self):
        e = self.evidence(); e['handshakes'][1] = 0
        with self.assertRaises(AssertionError): validate_mesh(e)

    def test_reject_http_failure(self):
        e = self.evidence(); e['remote_http'][0] = 503
        with self.assertRaises(AssertionError): validate_mesh(e)

    def test_reject_non_wireguard_route(self):
        e = self.evidence(); e['remote_routes'][1] = 'eth0'
        with self.assertRaises(AssertionError): validate_mesh(e)

    def test_reject_empty_evidence(self):
        with self.assertRaises(AssertionError): validate_mesh({})

if __name__ == '__main__': unittest.main()
