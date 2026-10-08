import importlib.util
import pathlib
import unittest


class ReportTests(unittest.TestCase):
    def load(self):
        path = pathlib.Path(__file__).with_name('test_report.py')
        self.assertTrue(path.exists(), 'test report parser is missing')
        spec = importlib.util.spec_from_file_location('report', path)
        assert spec is not None and spec.loader is not None
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def test_counts_only_terminal_test_events(self):
        report = self.load().summarize([
            {'Action': 'run', 'Test': 'TestA'},
            {'Action': 'pass', 'Test': 'TestA'},
            {'Action': 'skip', 'Test': 'TestB'},
            {'Action': 'pass', 'Package': 'pkg'},
        ])
        self.assertEqual(report['passed'], 1)
        self.assertEqual(report['skipped'], 1)
        self.assertEqual(report['failed'], 0)

    def test_package_failure_is_not_hidden(self):
        report = self.load().summarize([{'Action': 'fail', 'Package': 'pkg'}])
        self.assertEqual(report['failed_packages'], ['pkg'])

    def test_skip_gate_fails_integration(self):
        module = self.load()
        report = module.summarize([{'Action': 'skip', 'Test': 'TestRoot'}])
        self.assertEqual(module.exit_status(report, require_no_skips=True), 1)
        self.assertEqual(module.exit_status(report, require_no_skips=False), 0)

    def test_empty_stream_is_not_success(self):
        module = self.load()
        self.assertEqual(module.exit_status(module.summarize([])), 1)


if __name__ == '__main__':
    unittest.main()
