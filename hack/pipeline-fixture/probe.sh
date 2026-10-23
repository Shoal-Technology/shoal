#!/bin/sh
# Probes for hack/pipeline-e2e.sh (ADR 0022, section 5). Runs inside a pipeline
# job. The job's default route is the node's own bridge address, so the node's
# services are reached at that address. Each of them must refuse or time out.
# The internet must answer.
set -eu

gw=$(ip route | awk '/^default/ {print $3; exit}')
[ -n "$gw" ] || { echo "probe FAIL: no default route"; exit 1; }
echo "probe: node address $gw"

# blocked URL: the URL must not answer, and the failure must be a refusal or a
# timeout. Any other failure means the probe did not test the network.
blocked() {
  if wget -q -T 5 -O /dev/null "$1" 2>/tmp/probe.err; then
    echo "probe FAIL: $1 answered"
    exit 1
  fi
  if ! grep -Eq "can.t connect|timed out" /tmp/probe.err; then
    echo "probe FAIL: $1 failed for another reason: $(cat /tmp/probe.err)"
    exit 1
  fi
  echo "probe ok: $1 is unreachable"
}

blocked "http://$gw:8099/"
blocked "http://$gw:7443/"
blocked "http://$gw:8443/"

wget -q -T 60 -O /dev/null "http://example.com/"
echo "probe ok: the internet is reachable"
