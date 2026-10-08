"""Acceptance-evidence contract: observations alone must not pass the VM gate."""
import unittest
from vm_mesh import validate_mesh, validate_withdrawal


class WithdrawalEvidenceTests(unittest.TestCase):
    def rows(self):
        return [{'endpoints': ['live'], 'elapsed': 25 + i,
                 'withdrawn_applied': True, 'ok': True} for i in range(5)]

    def test_complete_withdrawal(self):
        validate_withdrawal(self.rows(), 'live', 'dead')

    def test_reject_failure_after_withdrawal(self):
        rows = self.rows(); rows[-1]['ok'] = False
        with self.assertRaises(AssertionError): validate_withdrawal(rows, 'live', 'dead')

    def test_reject_empty_or_insufficient_samples(self):
        for rows in ([], self.rows()[:4]):
            with self.assertRaises(AssertionError): validate_withdrawal(rows, 'live', 'dead')

    def test_reject_late_eviction_only_withdrawal(self):
        rows = self.rows(); rows[0]['elapsed'] = 60
        with self.assertRaises(AssertionError): validate_withdrawal(rows, 'live', 'dead')

    def test_reject_lost_survivor(self):
        rows = self.rows(); rows[0]['endpoints'] = []
        with self.assertRaises(AssertionError): validate_withdrawal(rows, 'live', 'dead')

    def test_reject_withdrawn_endpoint_reappearing(self):
        rows = self.rows(); rows[-1]['endpoints'].append('dead')
        rows[-1]['withdrawn_applied'] = False
        # Five good samples must not hide a later return of the stale endpoint.
        rows.insert(0, dict(rows[0]))
        with self.assertRaises(AssertionError): validate_withdrawal(rows, 'live', 'dead')

    def test_reject_proxy_reverting_after_withdrawal(self):
        rows = self.rows() + [dict(self.rows()[-1], withdrawn_applied=False)]
        with self.assertRaises(AssertionError): validate_withdrawal(rows, 'live', 'dead')

    def test_pre_detection_failures_are_measured_not_hidden(self):
        rows = [{'endpoints': ['live', 'dead'], 'elapsed': 1,
                 'withdrawn_applied': False, 'ok': False}] + self.rows()
        validate_withdrawal(rows, 'live', 'dead')


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
