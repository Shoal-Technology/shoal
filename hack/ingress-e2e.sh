#!/usr/bin/env bash
# Ingress and automatic TLS, end to end against pebble (ADR 0015, pass D).
#
# Runs on a disposable Ubuntu runner (.github/workflows/ingress-e2e.yml), with
# containerd already running and passwordless sudo. Pebble runs as the current
# user. The shoal server and agent run as root: the agent binds :80 and :443
# and manages containers. Evidence goes to test-results/ingress-e2e/.
#
# Name resolution: pebble's HTTP-01 validator resolves app.shoal.test through
# the system resolver (Go's resolver reads /etc/hosts first), so the script maps
# that name to 127.0.0.1 in /etc/hosts. Pebble's -dnsserver flag is not used.
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"
SHOAL="$ROOT/bin/shoal"
PEBBLE_VERSION=v2.10.1 # published 2026-04-20; pinned
HOST=app.shoal.test
HOSTS_TAG="# shoal-ingress-e2e"
SERVER=127.0.0.1:7443
ACME_DIR=https://localhost:14000/dir
MGMT=https://localhost:15000
RESULTS="$ROOT/test-results/ingress-e2e"
LOGS="$RESULTS/logs"
WORK=$(mktemp -d)
DATA="$WORK/server"
CONF="$DATA/admin.conf"
PEBBLE_BIN="$WORK/bin/pebble"
PEBBLE_PID=""
ISSUING="$WORK/pebble-issuing-root.pem"
MINICA=""
SERVER_ADDR=""
rm -rf "$RESULTS"
mkdir -p "$LOGS"
: >"$RESULTS/steps.txt"

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

cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  stop_root agent
  stop_root server
  if [ -n "$PEBBLE_PID" ]; then
    kill -TERM "$PEBBLE_PID" 2>/dev/null
    sleep 1
    kill -KILL "$PEBBLE_PID" 2>/dev/null
  fi
  hosts_remove
  if [ "$rc" -ne 0 ]; then
    echo "::group::logs (ingress e2e failed with exit $rc)"
    for f in "$LOGS"/*.log; do
      echo "----- $(basename "$f")"
      tail -n 400 "$f"
    done
    echo "::endgroup::"
  fi
  # Secrets live under $WORK (admin.conf, CA and encryption keys, join
  # credentials): remove them, keep only logs and step evidence.
  sudo -n rm -rf "$WORK"
  sudo -n chown -R "$(id -u):$(id -g)" "$RESULTS" 2>/dev/null
  exit "$rc"
}
trap cleanup EXIT

# ---------------------------------------------------------------- build and pebble
say "build shoal"
make build >/dev/null
[ -x "$SHOAL" ] || fail "make build did not produce $SHOAL"

say "install pebble $PEBBLE_VERSION"
(cd "$WORK" && GOBIN="$WORK/bin" go install "github.com/letsencrypt/pebble/v2/cmd/pebble@$PEBBLE_VERSION")
[ -x "$PEBBLE_BIN" ] || fail "go install did not produce $PEBBLE_BIN"
PEBBLE_DIR=$(cd "$WORK" && go mod download -json "github.com/letsencrypt/pebble/v2@$PEBBLE_VERSION" |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["Dir"])')
MINICA="$PEBBLE_DIR/test/certs/pebble.minica.pem"
[ -s "$MINICA" ] || fail "pebble minica root not found at $MINICA"
pass "installed pebble $PEBBLE_VERSION (module dir $PEBBLE_DIR)"

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
pebble_ready() { curl -fsS --cacert "$MINICA" "$ACME_DIR" -o /dev/null; }
wait_for 60 "pebble answers its ACME directory" pebble_ready
curl -fsS --cacert "$MINICA" "$MGMT/roots/0" -o "$ISSUING"
[ -s "$ISSUING" ] || fail "pebble issuing root from $MGMT/roots/0 is empty"
pass "fetched pebble issuing root from $MGMT/roots/0"

# ---------------------------------------------------------------- hosts
say "map $HOST to 127.0.0.1 in /etc/hosts (pebble validation and curl use it)"
hosts_remove
echo "127.0.0.1 $HOST $HOSTS_TAG" | sudo -n tee -a /etc/hosts >/dev/null
pass "$HOST resolves to 127.0.0.1"

# ---------------------------------------------------------------- server
say "start shoal server (ACME against pebble)"
mkdir -p "$DATA"
start_root server "$LOGS/server.log" "$SHOAL" server \
  --data-dir "$DATA" --listen 127.0.0.1:7443 --ui-listen "" \
  --acme-directory "$ACME_DIR" --acme-email ops@shoal.test \
  --acme-ca-roots "$MINICA"
wait_for 90 "server accepts CLI calls" test -s "$CONF"
wait_for 90 "server answers get nodes" cc get nodes

# ---------------------------------------------------------------- agent
say "create a join token and start one agent with containerd (edge listeners :80 and :443)"
TOKEN_OUT=$(cc token create --max-uses 1 --ttl 1h)
JOIN=$(printf '%s\n' "$TOKEN_OUT" | grep -o 'shoal agent .*' || true)
[ -n "$JOIN" ] || fail "token create printed no join command"
field() { echo "$JOIN" | awk -v k="$1" '{ for (i = 1; i < NF; i++) if ($i == k) { print $(i + 1); exit } }'; }
TOKEN=$(field --token)
CAHASH=$(field --ca-hash)
SERVER_ADDR=$(field --server)
[ -n "$TOKEN" ] && [ -n "$CAHASH" ] && [ -n "$SERVER_ADDR" ] || fail "could not read the join command"
start_root agent "$LOGS/agent.log" "$SHOAL" agent \
  --server "$SERVER_ADDR" --token "$TOKEN" --ca-hash "$CAHASH" \
  --node-name edge-1 --edge-http :80 --edge-https :443
unset TOKEN CAHASH
node_ready() { cc get nodes | awk '$1 == "edge-1" && $2 == "Ready" { ok = 1 } END { exit !ok }'; }
wait_for 120 "node edge-1 is Ready" node_ready

# ---------------------------------------------------------------- edge label
say "label edge-1 as an ingress edge"
cc label node edge-1 shoal.dev/edge=true
edge_listens() { curl -sS -o /dev/null --max-time 2 http://127.0.0.1:80/; }
wait_for 60 "edge listens on :80 after the label" edge_listens

# ---------------------------------------------------------------- workload
say "deploy nginx workload, Service and Route $HOST (tls: auto)"
cat >"$WORK/workload.yaml" <<'EOF'
kind: Workload
metadata: { name: web }
spec:
  image: nginx:1.27-alpine
  replicas: 1
  ports: [{ containerPort: 80 }]
  resources: { cpu: 100m, memory: 64Mi }
  healthCheck: { http: { path: /, port: 80 }, intervalSeconds: 1, timeoutSeconds: 1 }
EOF
cat >"$WORK/service.yaml" <<'EOF'
kind: Service
metadata: { name: web }
spec: { selector: web, ports: [{ port: 80 }] }
EOF
cat >"$WORK/route.yaml" <<EOF
kind: Route
metadata: { name: app }
spec: { host: $HOST, service: web, port: 80, tls: auto }
EOF
cc apply -f "$WORK/workload.yaml"
cc apply -f "$WORK/service.yaml"
cc apply -f "$WORK/route.yaml"
workload_ready() { cc get workload web -o yaml 2>/dev/null | grep -Eq 'readyReplicas"?: *"?1"?'; }
wait_for 240 "workload web has 1 ready replica" workload_ready

# ---------------------------------------------------------------- a) route status
say "a) route becomes Accepted with a certificate issued by pebble"
route_ready() {
  local y
  y=$(cc get route app -o yaml 2>/dev/null) || return 1
  grep -Eq 'state"?: *"?Accepted' <<<"$y" && grep -Eq 'certState"?: *"?CertIssued' <<<"$y"
}
wait_for 300 "a) route app is Accepted and CertIssued" route_ready
cc get route app -o yaml >"$RESULTS/route-status.yaml" || true
cc get route app >"$RESULTS/route-table.txt" || true

# ---------------------------------------------------------------- b) https
say "b) https://$HOST answers through the edge with the workload response"
https_ok() {
  local body
  body=$(curl -fsS --cacert "$ISSUING" --max-time 10 "https://$HOST/") || return 1
  grep -q 'Welcome to nginx' <<<"$body"
}
wait_for 90 "b) curl --cacert pebble root returns the nginx welcome page" https_ok

# ---------------------------------------------------------------- c) issuer
say "c) served certificate verifies against pebble's root and is not self-signed"
openssl s_client -connect 127.0.0.1:443 -servername "$HOST" -CAfile "$ISSUING" \
  -verify_return_error -verify_hostname "$HOST" </dev/null >"$RESULTS/s_client.txt" 2>&1 || true
grep -q 'Verify return code: 0 (ok)' "$RESULTS/s_client.txt" ||
  fail "c) served chain does not verify against pebble's root (see s_client.txt)"
openssl s_client -connect 127.0.0.1:443 -servername "$HOST" </dev/null 2>/dev/null |
  sed -n '/BEGIN CERTIFICATE/,/END CERTIFICATE/p' |
  openssl x509 -noout -subject -issuer -dates >"$RESULTS/served-cert.txt"
issuer=$(sed -n 's/^issuer=//p' "$RESULTS/served-cert.txt")
subject=$(sed -n 's/^subject=//p' "$RESULTS/served-cert.txt")
case "$issuer" in
  *Pebble*) ;;
  *) fail "c) issuer is not pebble: $issuer" ;;
esac
[ "$issuer" != "$subject" ] || fail "c) certificate is self-signed"
pass "c) issuer: $issuer (subject: $subject)"

# ---------------------------------------------------------------- d) redirect
say "d) http://$HOST redirects to https with 308"
redirect=$(curl -sS -o /dev/null --max-time 10 -w '%{http_code} %{redirect_url}' "http://$HOST/")
[ "$redirect" = "308 https://$HOST/" ] || fail "d) got '$redirect', want '308 https://$HOST/'"
pass "d) http gets 308 to https ($redirect)"

# ---------------------------------------------------------------- e) unknown host
say "e) unknown host gets a plain 404 with no route data"
resp=$(curl -sS --max-time 10 -w '\n%{http_code}' -H 'Host: unknown.example.test' http://127.0.0.1/)
code=$(tail -n 1 <<<"$resp")
body=$(head -n -1 <<<"$resp")
[ "$code" = 404 ] || fail "e) unknown host got status $code, want 404"
[ "$body" = "404 page not found" ] || fail "e) unexpected 404 body: $body"
pass "e) unknown host: 404 with stock body"

# ---------------------------------------------------------------- f) websocket
skip "f) websocket upgrade: the nginx image has no websocket endpoint and no cheap echo image is wired into CI"

# ---------------------------------------------------------------- evidence
cc get workload web -o yaml >"$RESULTS/workload.yaml" 2>/dev/null || true
cc get nodes >"$RESULTS/nodes.txt" 2>/dev/null || true
say "all required checks passed"
pass "ingress e2e complete"
