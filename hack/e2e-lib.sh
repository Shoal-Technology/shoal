# Shared helpers for the containerd end-to-end scripts: hack/ingress-e2e.sh and
# hack/deploy-e2e.sh. Source this file after setting ROOT, SHOAL, RESULTS, LOGS
# and WORK. Loading it runs nothing. The caller then runs install_pebble,
# start_pebble, hosts_add and start_cluster in order, and calls teardown from
# its EXIT trap.
#
# Runs on a disposable Ubuntu runner with containerd 2.x on
# /run/containerd/containerd.sock, CNI plugins in /opt/cni/bin and passwordless
# sudo. Pebble runs as the current user. The shoal server and agent run as root:
# the agent binds :80 and :443 and manages containers.
# Name resolution: pebble's HTTP-01 validator resolves the test hosts through
# the system resolver, so they are mapped to 127.0.0.1 in /etc/hosts.

PEBBLE_VERSION=v2.10.1 # published 2026-04-20; pinned
ACME_DIR=https://localhost:14000/dir
MGMT=https://localhost:15000
SERVER=127.0.0.1:7443
HOSTS_TAG="# shoal-e2e"
DATA="$WORK/server"
CONF="$DATA/admin.conf"
PEBBLE_BIN="$WORK/bin/pebble"
ISSUING="$WORK/pebble-issuing-root.pem"
PEBBLE_PID=""
PEBBLE_DIR=""
MINICA=""
SERVER_ADDR=""
AGENT_ARGS=()

say() { echo "== $*"; }

pass() {
  echo "PASS: $*"
  echo "PASS: $*" >>"$RESULTS/steps.txt"
}

skip() {
  echo "SKIP: $*"
  echo "SKIP: $*" >>"$RESULTS/steps.txt"
}

fail() {
  echo "FAIL: $*" >&2
  echo "FAIL: $*" >>"$RESULTS/steps.txt"
  exit 1
}

# start_root <name> <log> <cmd...>: runs a command as root in the background.
# The pid of the exec'd command is written to $WORK/<name>.pid.
start_root() {
  local name=$1 log=$2
  shift 2
  sudo -n sh -c 'echo $$ >"$0"; exec "$@"' "$WORK/$name.pid" "$@" >>"$log" 2>&1 &
  say "started $name (log $(basename "$log"))"
}

# stop_root <name>: TERM, then KILL after 15s.
stop_root() {
  local f="$WORK/$1.pid" p i
  [ -s "$f" ] || return 0
  p=$(cat "$f")
  sudo -n kill -TERM "$p" 2>/dev/null || return 0
  for i in $(seq 1 15); do
    sudo -n kill -0 "$p" 2>/dev/null || return 0
    sleep 1
  done
  sudo -n kill -KILL "$p" 2>/dev/null || true
}

# cc <cli args...>: the shoal CLI as root against the e2e server.
cc() {
  sudo -n "$SHOAL" "$@" --config "$CONF" --server "$SERVER" --timeout 20s
}

# wait_for <seconds> <what> <command...>
wait_for() {
  local secs=$1 what=$2 deadline
  shift 2
  deadline=$((SECONDS + secs))
  until "$@" >/dev/null 2>&1; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      fail "timed out after ${secs}s: $what"
    fi
    sleep 2
  done
  pass "$what"
}

hosts_remove() {
  sudo -n sed -i "/$HOSTS_TAG\$/d" /etc/hosts 2>/dev/null || true
}

# hosts_add <host>...: maps each host to 127.0.0.1 (replaces earlier entries).
hosts_add() {
  local h
  hosts_remove
  for h in "$@"; do
    echo "127.0.0.1 $h $HOSTS_TAG" | sudo -n tee -a /etc/hosts >/dev/null
  done
  pass "hosts: $* resolve to 127.0.0.1"
}

install_pebble() {
  say "install pebble $PEBBLE_VERSION"
  (cd "$WORK" && GOBIN="$WORK/bin" go install "github.com/letsencrypt/pebble/v2/cmd/pebble@$PEBBLE_VERSION")
  [ -x "$PEBBLE_BIN" ] || fail "go install did not produce $PEBBLE_BIN"
  PEBBLE_DIR=$(cd "$WORK" && go mod download -json "github.com/letsencrypt/pebble/v2@$PEBBLE_VERSION" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["Dir"])')
  MINICA="$PEBBLE_DIR/test/certs/pebble.minica.pem"
  [ -s "$MINICA" ] || fail "pebble minica root not found at $MINICA"
  pass "installed pebble $PEBBLE_VERSION (module dir $PEBBLE_DIR)"
}

# start_pebble: validation enforced, HTTP-01 on port 80 (the edge listener).
start_pebble() {
  say "start pebble (httpPort 80 = edge HTTP listener, no sleeps, validation enforced)"
  cat >"$WORK/pebble-config.json" <<EOF
{
  "pebble": {
    "listenAddress": "127.0.0.1:14000",
    "managementListenAddress": "127.0.0.1:15000",
    "certificate": "$PEBBLE_DIR/test/certs/localhost/cert.pem",
    "privateKey": "$PEBBLE_DIR/test/certs/localhost/key.pem",
    "httpPort": 80,
    "tlsPort": 5001,
    "ocspResponderURL": "",
    "externalAccountBindingRequired": false
  }
}
EOF
  PEBBLE_VA_NOSLEEP=1 PEBBLE_VA_ALWAYS_VALID=0 \
    "$PEBBLE_BIN" -config "$WORK/pebble-config.json" >>"$LOGS/pebble.log" 2>&1 &
  PEBBLE_PID=$!
  wait_for 60 "pebble answers its ACME directory" curl -fsS --cacert "$MINICA" "$ACME_DIR" -o /dev/null
  curl -fsS --cacert "$MINICA" "$MGMT/roots/0" -o "$ISSUING"
  [ -s "$ISSUING" ] || fail "pebble issuing root from $MGMT/roots/0 is empty"
  pass "fetched pebble issuing root from $MGMT/roots/0"
}

stop_pebble() {
  if [ -n "$PEBBLE_PID" ]; then
    kill -TERM "$PEBBLE_PID" 2>/dev/null
    sleep 1
    kill -KILL "$PEBBLE_PID" 2>/dev/null
  fi
  return 0
}

# start_cluster: server (ACME against pebble), one agent with containerd and the
# edge listeners, then the shoal.dev/edge label. Extra agent flags come from
# the AGENT_ARGS array.
start_cluster() {
  mkdir -p "$DATA"
  start_root server "$LOGS/server.log" "$SHOAL" server \
    --data-dir "$DATA" --listen 127.0.0.1:7443 --ui-listen "" \
    --acme-directory "$ACME_DIR" --acme-email ops@shoal.test \
    --acme-ca-roots "$MINICA" --acme-accept-tos
  wait_for 90 "server accepts CLI calls" test -s "$CONF"
  wait_for 90 "server answers get nodes" cc get nodes

  say "create a join token and start one agent with containerd (edge listeners :80 and :443)"
  local token_out join token cahash field
  token_out=$(cc token create --max-uses 1 --ttl 1h)
  join=$(printf '%s\n' "$token_out" | grep -o 'shoal agent .*' || true)
  [ -n "$join" ] || fail "token create printed no join command"
  field() { echo "$join" | awk -v k="$1" '{ for (i = 1; i < NF; i++) if ($i == k) { print $(i + 1); exit } }'; }
  token=$(field --token)
  cahash=$(field --ca-hash)
  SERVER_ADDR=$(field --server)
  [ -n "$token" ] && [ -n "$cahash" ] && [ -n "$SERVER_ADDR" ] || fail "could not read the join command"
  start_root agent "$LOGS/agent.log" "$SHOAL" agent \
    --server "$SERVER_ADDR" --token "$token" --ca-hash "$cahash" \
    --node-name edge-1 --edge-http :80 --edge-https :443 ${AGENT_ARGS[@]+"${AGENT_ARGS[@]}"}
  unset token cahash
  node_ready() { cc get nodes | awk '$1 == "edge-1" && $2 == "Ready" { ok = 1 } END { exit !ok }'; }
  wait_for 120 "node edge-1 is Ready" node_ready

  say "label edge-1 as an ingress edge"
  cc label node edge-1 shoal.dev/edge=true
  edge_listens() { curl -sS -o /dev/null --max-time 2 http://127.0.0.1:80/; }
  wait_for 60 "edge listens on :80 after the label" edge_listens
}

# teardown <exit code> <label>: stops the daemons, removes the hosts entries,
# dumps the logs on failure (containerd included), and removes the secrets
# under $WORK. Evidence under $RESULTS stays.
teardown() {
  local rc=$1 label=$2 f
  stop_root agent
  stop_root server
  stop_pebble
  hosts_remove
  if [ "$rc" -ne 0 ]; then
    sudo -n journalctl -u shoal-test-containerd --no-pager >"$LOGS/containerd.log" 2>&1 || true
    echo "::group::logs ($label failed with exit $rc)"
    for f in "$LOGS"/*.log; do
      [ -e "$f" ] || continue
      echo "----- $(basename "$f")"
      tail -n 400 "$f"
    done
    echo "::endgroup::"
  fi
  sudo -n rm -rf "$WORK"
  sudo -n chown -R "$(id -u):$(id -g)" "$RESULTS" 2>/dev/null || true
}
