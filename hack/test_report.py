"""Summarize Go test JSON without hiding skips or package failures."""
import argparse
import json
from typing import Any


def summarize(events):
    result: dict[str, Any] = dict(passed=0, failed=0, skipped=0, failed_packages=[], completed_packages=0)
    for event in events:
        action = event.get('Action')
        if event.get('Test'):
            key = {'pass': 'passed', 'fail': 'failed', 'skip': 'skipped'}.get(action)
            if key:
                result[key] += 1
        elif action in ('pass', 'fail', 'skip'):
            result['completed_packages'] += 1
            if action == 'fail':
                result['failed_packages'].append(event.get('Package', 'unknown'))
    return result


def exit_status(report, require_no_skips=False):
    evidence = report['completed_packages'] + report['passed'] + report['failed'] + report['skipped']
    return int(not evidence or bool(report['failed_packages']) or report['failed'] > 0
               or (require_no_skips and report['skipped'] > 0))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('input')
    parser.add_argument('--require-no-skips', action='store_true')
    args = parser.parse_args()
    with open(args.input) as stream:
        report = summarize(json.loads(line) for line in stream if line.strip())
    print(json.dumps(report, indent=2))
    return exit_status(report, args.require_no_skips)


if __name__ == '__main__':
    raise SystemExit(main())
