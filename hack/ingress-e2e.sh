#!/usr/bin/env bash
# Ingress and automatic TLS, end to end against pebble (ADR 0015, pass D).
#
# Runs on a disposable Ubuntu runner (.github/workflows/ingress-e2e.yml), with
# containerd already running and passwordless sudo. The daemons and the cluster
# setup live in hack/e2e-lib.sh, which hack/deploy-e2e.sh shares. Evidence goes
# to test-results/ingress-e2e/.
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"
SHOAL="$ROOT/bin/shoal"
HOST=app.shoal.test
RESULTS="$ROOT/test-results/ingress-e2e"
LOGS="$RESULTS/logs"
WORK=$(mktemp -d)
rm -rf "$RESULTS"
mkdir -p "$LOGS"
: >"$RESULTS/steps.txt"
. "$ROOT/hack/e2e-lib.sh"

cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  teardown "$rc" "ingress e2e"
  exit "$rc"
}
trap cleanup EXIT

# ---------------------------------------------------------------- build, pebble, cluster
say "build shoal"
make build >/dev/null
[ -x "$SHOAL" ] || fail "make build did not produce $SHOAL"

install_pebble
start_pebble
hosts_add "$HOST"
start_cluster

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
say "a) route becomes Ready with a certificate issued by pebble"
route_ready() {
  local y
  y=$(cc get route app -o yaml 2>/dev/null) || return 1
  grep -Eq 'state"?: *"?Ready' <<<"$y" && grep -Eq 'certState"?: *"?CertIssued' <<<"$y"
}
wait_for 300 "a) route app is Ready and CertIssued" route_ready
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
