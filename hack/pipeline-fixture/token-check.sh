#!/bin/sh
# Secret check for hack/pipeline-e2e.sh (ADR 0022, section 6). The secret is
# in FIXTURE_TOKEN. The script prints its length and then the value, which the
# runner must replace with [redacted] before the log is stored.
set -eu
echo "token length $(printf %s "$FIXTURE_TOKEN" | wc -c)"
echo "token is $FIXTURE_TOKEN"
