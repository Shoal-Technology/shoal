#!/usr/bin/env bash
# End-to-end test of `shoal init` and `shoal uninstall` (ADR 0017, pass E).
#
# Runs on a fresh GitHub-hosted Ubuntu runner only: it refuses to run anywhere
# else, so the shared VPS is never a target. The script removes the runner's
# containerd so that init has to install the pinned one, then:
#   1. shoal init --dry-run changes nothing and lists every step;
#   2. shoal init installs and starts the node; the node is Ready and the edge
#      label makes the edge listen on :80;
#   3. an app deployed with shoal deploy answers over HTTP through the edge;
#   4. a second shoal init is all no-ops;
#   5. shoal uninstall removes the units and binaries and keeps /var/lib/shoal.
# It also records the ADR 0017 section 5 numbers: idle RSS of the server and
# the agent (120 s after init, no workloads), and the time from init start to
# the first HTTP 200 through the edge, excluding that idle wait.
#
# Usage: hack/init-e2e.sh /path/to/shoal-binary  (RESULTS defaults to test-results/init-e2e)
set -euo pipefail

if [ "${GITHUB_ACTIONS:-}" != "true" ]; then
  echo "refusing to run: this script only runs on a GitHub-hosted runner" >&2
  exit 2
fi

BIN_SRC=${1:?usage: hack/init-e2e.sh /path/to/shoal-binary}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
RESULTS=${RESULTS:-$ROOT/test-results/init-e2e}
WORK=$(mktemp -d)
ADMIN=/var/lib/shoal/server/admin.conf
SERVER=127.0.0.1:7443
HOST_NAME=hello.shoal.test
APP_IMAGE=docker.io/library/nginx:1.27-alpine
SHOAL="$WORK/shoal"
mkdir -p "$RESULTS"
: >"$RESULTS/steps.txt"

say() { echo "== $*"; }
pass() {
  echo "PASS: $*"
  echo "PASS: $*" >>"$RESULTS/steps.txt"
}
fail() {
  echo "FAIL: $*" >&2
  echo "FAIL: $*" >>"$RESULTS/steps.txt"
  exit 1
}
now_ms() { date +%s%3N; }

dump_logs() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "::group::journal (exit $rc)"
    sudo journalctl -u shoal-server -u shoal-agent -u containerd --no-pager | tail -n 400 || true
    echo "::endgroup::"
  fi
  sudo systemctl stop shoal-agent shoal-server 2>/dev/null || true
  sudo journalctl -u shoal-server --no-pager >"$RESULTS/server.log" 2>/dev/null || true
  sudo journalctl -u shoal-agent --no-pager >"$RESULTS/agent.log" 2>/dev/null || true
  sudo journalctl -u containerd --no-pager >"$RESULTS/containerd.log" 2>/dev/null || true
  rm -rf "$WORK"
  return "$rc"
}
trap dump_logs EXIT

# 0. Prepare: a runner without containerd, so that init installs the pinned one.
say "remove the runner's containerd (init must install its own)"
sudo systemctl stop docker.socket docker.service containerd.service 2>/dev/null || true
sudo apt-get purge -y -qq containerd.io docker-ce docker-ce-cli >/dev/null 2>&1 || true
[ ! -e /usr/bin/containerd ] || fail "containerd is still installed at /usr/bin/containerd"
[ ! -e /usr/local/bin/shoal ] || fail "/usr/local/bin/shoal exists before init"
sudo touch /etc/shoal-disposable
install -m 0755 "$BIN_SRC" "$SHOAL"
pass "runner prepared: no containerd, no shoal"

# 1. Dry run changes nothing.
say "shoal init --dry-run"
sudo "$SHOAL" init --node-name edge-1 --dry-run | tee "$RESULTS/init-dry-run.txt"
grep -q '^WOULD containerd:' "$RESULTS/init-dry-run.txt" || fail "dry run does not plan the containerd install"
grep -Eq '^(done|ok|WOULD) +(binary|kernel)' "$RESULTS/init-dry-run.txt" || fail "dry run lists no kernel or binary step"
if grep -Eq '^done ' "$RESULTS/init-dry-run.txt"; then fail "dry run performed a step"; fi
[ ! -e /usr/local/bin/shoal ] && [ ! -e /etc/systemd/system/shoal-server.service ] || fail "dry run wrote files"
pass "init --dry-run plans every step and changes nothing"

# 2. Real init.
say "shoal init"
t0=$(now_ms)
sudo "$SHOAL" init --node-name edge-1 | tee "$RESULTS/init-1.txt"
init_ms=$(( $(now_ms) - t0 ))
grep -q '^done  *containerd:' "$RESULTS/init-1.txt" || fail "first init did not install containerd"
pass "init installed the node in ${init_ms} ms"

say "node is Ready with the edge label"
sudo "$SHOAL" get nodes --config "$ADMIN" --server "$SERVER" | tee "$RESULTS/get-nodes.txt"
awk '$1 == "edge-1" && $2 == "Ready" { ok = 1 } END { exit !ok }' "$RESULTS/get-nodes.txt" || fail "edge-1 is not Ready"
edge_code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1/ || true)
[ "$edge_code" != "000" ] && [ -n "$edge_code" ] || fail "the edge does not listen on :80 (label not applied?)"
pass "edge-1 is Ready and the edge answers on :80 (HTTP $edge_code for an unknown host)"

say "containerd runs without its CRI plugins"
sudo ctr plugins ls >"$RESULTS/ctr-plugins.txt" 2>&1 || true
if grep -E 'io\.containerd\.cri\.v1\.(runtime|images).*ok' "$RESULTS/ctr-plugins.txt" >/dev/null; then
  fail "CRI plugins are running; the disabled_plugins config did not apply"
fi
pass "CRI plugins are not running"

# 3. Idle memory, 120 s after init with no workloads.
say "idle RSS after 120 s with no workloads"
sleep 120
rss_of() {
  local pid
  pid=$(systemctl show -p MainPID --value "$1")
  [ -n "$pid" ] && [ "$pid" != 0 ] || { echo 0; return; }
  awk '/VmRSS/ { print $2 }' "/proc/$pid/status"
}
server_rss_kb=$(rss_of shoal-server.service)
agent_rss_kb=$(rss_of shoal-agent.service)
containerd_rss_kb=$(rss_of containerd.service)
echo "server RSS ${server_rss_kb} kB, agent RSS ${agent_rss_kb} kB, containerd RSS ${containerd_rss_kb} kB"
[ "$server_rss_kb" -gt 0 ] && [ "$agent_rss_kb" -gt 0 ] || fail "could not read RSS of the server or the agent"
pass "idle RSS recorded"

# 4. An app through the edge.
say "deploy a minimal app and curl it through the edge"
cat >"$WORK/app.yaml" <<EOF
name: hello
image: $APP_IMAGE
port: 80
domain: $HOST_NAME
replicas: 1
health:
  path: /
resources:
  cpu: 100m
  memory: 128Mi
EOF
t1=$(now_ms)
sudo "$SHOAL" deploy "$WORK/app.yaml" --config "$ADMIN" --server "$SERVER" --wait 5m | tee "$RESULTS/deploy.txt"
body="$RESULTS/app-response.txt"
ok=0
for _ in $(seq 1 90); do
  code=$(curl -s -o "$body" -w '%{http_code}' --max-time 5 -H "Host: $HOST_NAME" http://127.0.0.1/ || true)
  if [ "$code" = 200 ]; then ok=1; break; fi
  sleep 2
done
[ "$ok" = 1 ] || fail "no HTTP 200 for $HOST_NAME through the edge (last code: ${code:-none})"
deploy_to_200_ms=$(( $(now_ms) - t1 ))
grep -qi 'nginx\|welcome' "$body" || fail "the response is not the app"
first_response_ms=$(( init_ms + deploy_to_200_ms ))
pass "app answers 200 through the edge; init to first response ${first_response_ms} ms (init ${init_ms} ms + deploy to 200 ${deploy_to_200_ms} ms)"

# 5. Second init is all no-ops.
say "second shoal init"
sudo "$SHOAL" init --node-name edge-1 | tee "$RESULTS/init-2.txt"
if grep -Eq '^(done|WOULD|FAIL) ' "$RESULTS/init-2.txt"; then
  fail "second init changed something"
fi
grep -Eq '^ok +' "$RESULTS/init-2.txt" || fail "second init printed no ok lines"
pass "second init is all no-ops"

# 6. Uninstall keeps the data.
say "shoal uninstall"
sudo "$SHOAL" uninstall | tee "$RESULTS/uninstall.txt"
[ -f /var/lib/shoal/init.json ] || fail "uninstall removed /var/lib/shoal/init.json"
[ -f "$ADMIN" ] || fail "uninstall removed the cluster admin config under /var/lib/shoal"
for p in /etc/systemd/system/shoal-server.service /etc/systemd/system/shoal-agent.service \
  /usr/local/bin/shoal /usr/local/bin/containerd /etc/containerd/config.toml; do
  [ ! -e "$p" ] || fail "uninstall left $p"
done
pass "uninstall removed the units and binaries and kept /var/lib/shoal"

# Results for ADR 0017 section 6.
cat >"$RESULTS/benchmark.json" <<EOF
{
  "runner": "$(. /etc/os-release && echo "$PRETTY_NAME")",
  "arch": "$(uname -m)",
  "init_ms": $init_ms,
  "deploy_to_first_200_ms": $deploy_to_200_ms,
  "init_to_first_response_ms": $first_response_ms,
  "server_idle_rss_kb": $server_rss_kb,
  "agent_idle_rss_kb": $agent_rss_kb,
  "containerd_idle_rss_kb": $containerd_rss_kb
}
EOF
say "benchmark (ADR 0017 section 6)"
cat "$RESULTS/benchmark.json"
pass "init-e2e complete"
