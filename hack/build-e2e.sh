#!/usr/bin/env bash
# shoal build, end to end (ADR 0021, pass B): a BuildConfig for the fixture in
# hack/build-fixture, built on a builder node by the rootless BuildKit daemon,
# pushed to the embedded registry with the Build's own certificate, and then
# deployed from shoal.local and checked over HTTP. It runs on a disposable
# runner with containerd (.github/workflows/build-e2e.yml). Do not run it on
# the shared VPS: it starts containers.
#
# BUILD_REF is the branch the fixture is read from (the repository is cloned
# from GitHub at that branch). Evidence goes to test-results/build-e2e/.
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"
SHOAL="$ROOT/bin/shoal"
BUILD_REF=${BUILD_REF:?BUILD_REF must name the branch that holds hack/build-fixture}
RESULTS="$ROOT/test-results/build-e2e"
LOGS="$RESULTS/logs"
WORK=$(mktemp -d)
rm -rf "$RESULTS"
mkdir -p "$LOGS"
: >"$RESULTS/steps.txt"
. "$ROOT/hack/e2e-lib.sh"

# The build container reaches the node through its own network, not loopback,
# so the server and the registry listen on the node's address (ADR 0021).
NODE_IP=$(hostname -I | awk '{print $1}')
REGISTRY=$NODE_IP:5443
BUILDER=builder-1
WORKLOAD_PORT=18080
PROBE_PORT=8099
PROBE_PID=""

cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  [ -n "$PROBE_PID" ] && kill "$PROBE_PID" 2>/dev/null
  teardown "$rc" "build e2e"
  exit "$rc"
}
trap cleanup EXIT

# diagnose: the state of the build and the cluster for a failed run.
diagnose() {
  {
    echo "== diagnose: builds"
    cc get builds -o yaml || true
    echo "== diagnose: nodes"
    cc get nodes || true
    echo "== diagnose: containers"
    sudo -n ctr -n shoal containers ls || true
  } 2>&1 | tee "$RESULTS/diagnose.txt"
}

say "build the shoal CLI and server"
(cd "$ROOT" && go build -o "$SHOAL" ./cmd/shoal)
pass "built $SHOAL"

say "start the server with the embedded registry on $REGISTRY"
mkdir -p "$DATA"
start_root server "$LOGS/server.log" "$SHOAL" server \
  --data-dir "$DATA" --listen 0.0.0.0:7443 --advertise-address "$NODE_IP:7443" \
  --ui-listen 0.0.0.0:8443 --registry-listen 0.0.0.0:5443

# A plain HTTP server on the node's address. The build's RUN steps must not
# reach it: it stands for the node's own services, such as the Shoal API.
(cd "$WORK" && exec python3 -m http.server "$PROBE_PORT" --bind 0.0.0.0 >"$LOGS/probe.log" 2>&1) &
PROBE_PID=$!
wait_for 30 "probe server answers on $NODE_IP:$PROBE_PORT" curl -fsS --max-time 2 "http://$NODE_IP:$PROBE_PORT/"
wait_for 90 "server accepts CLI calls" test -s "$CONF"
wait_for 90 "server answers get nodes" cc get nodes

say "join one agent with containerd: $BUILDER"
token_out=$(cc token create --max-uses 1 --ttl 1h)
join=$(printf '%s\n' "$token_out" | grep -o 'shoal agent .*' || true)
[ -n "$join" ] || fail "token create printed no join command"
field() { echo "$join" | awk -v k="$1" '{ for (i = 1; i < NF; i++) if ($i == k) { print $(i + 1); exit } }'; }
token=$(field --token)
cahash=$(field --ca-hash)
SERVER_ADDR=$(field --server)
[ -n "$token" ] && [ -n "$cahash" ] && [ -n "$SERVER_ADDR" ] || fail "could not read the join command"
start_root agent "$LOGS/agent.log" "$SHOAL" agent \
  --server "$SERVER_ADDR" --token "$token" --ca-hash "$cahash" --node-name "$BUILDER"
unset token cahash
node_ready() { cc get nodes | awk -v n="$BUILDER" '$1 == n && $2 == "Ready" { ok = 1 } END { exit !ok }'; }
wait_for 120 "node $BUILDER is Ready" node_ready

say "label $BUILDER as a builder"
cc label node "$BUILDER" shoal.dev/builder=true
pass "labelled $BUILDER shoal.dev/builder=true"

say "create the BuildConfig (ref $BUILD_REF) and start one build"
cat >"$WORK/buildconfig.yaml" <<EOF
kind: BuildConfig
metadata: { name: web }
spec:
  git:
    url: https://github.com/Shoal-Technology/shoal.git
    ref: $BUILD_REF
    contextDir: hack/build-fixture
    dockerfile: Dockerfile
  output: { repository: web, tag: "{short-sha}" }
  buildArgs:
    PROBE_NODE: "http://$NODE_IP:$PROBE_PORT/"
    PROBE_API: "http://$NODE_IP:7443/"
    PROBE_PORTAL: "http://$NODE_IP:8443/"
    PROBE_PUBLIC: "http://example.com/"
EOF
cc build create-config -f "$WORK/buildconfig.yaml" >"$RESULTS/create-config.txt" 2>&1 ||
  fail "create-config refused the BuildConfig (see create-config.txt)"
pass "created BuildConfig web"
start_out=$(cc build start web)
echo "$start_out" >"$RESULTS/start.txt"
name=$(printf '%s\n' "$start_out" | sed -n 's#^build/\([^ ]*\) created.*#\1#p')
[ -n "$name" ] || fail "build start printed no build name: $start_out"
pass "started build $name"

build_phase() {
  cc get build "$name" -o yaml 2>/dev/null | grep -m1 -o 'phase: *[A-Za-z]*' | sed 's/phase: *//' || true
}
say "wait for build $name to finish (up to 40 minutes)"
deadline=$((SECONDS + 2400))
phase=""
while :; do
  phase=$(build_phase)
  case "$phase" in
    Complete) break ;;
    Failed | Error | Cancelled)
      cc build logs "$name" >"$RESULTS/build-log.txt" 2>&1 || true
      fail "build $name ended $phase (see build-log.txt)"
      ;;
  esac
  if [ "$SECONDS" -ge "$deadline" ]; then
    cc build logs "$name" >"$RESULTS/build-log.txt" 2>&1 || true
    fail "build $name is still $phase after 40 minutes"
  fi
  sleep 10
done
pass "build $name is Complete"

cc get build "$name" -o yaml >"$RESULTS/build.yaml"
cc build logs "$name" >"$RESULTS/build-log.txt" 2>&1 || true
image=$(grep -m1 -o 'image: *[^ ]*' "$RESULTS/build.yaml" | sed 's/image: *//')
digest=$(grep -m1 -o 'digest: *sha256:[0-9a-f]*' "$RESULTS/build.yaml" | sed 's/digest: *//')
[ -n "$image" ] && [ -n "$digest" ] || fail "a Complete build has no image or digest"
pass "built $image at $digest"

say "the registry holds the tag"
sudo -n "$SHOAL" registry ls --registry "$REGISTRY" --config "$CONF" >"$RESULTS/registry-ls.txt" 2>&1 ||
  fail "registry ls failed (see registry-ls.txt)"
grep -q "${image%%:*}" "$RESULTS/registry-ls.txt" || fail "registry ls does not list ${image%%:*}"
pass "registry lists $image"

say "deploy shoal.local/$image and check it over HTTP"
cat >"$WORK/workload.yaml" <<EOF
kind: Workload
metadata: { name: web }
spec:
  image: shoal.local/$image
  replicas: 1
  ports: [{ containerPort: 8080, hostPort: $WORKLOAD_PORT }]
  resources: { cpu: 100m, memory: 64Mi }
EOF
cc apply -f "$WORK/workload.yaml"
served() {
  curl -fsS --max-time 5 "http://127.0.0.1:$WORKLOAD_PORT/" >"$RESULTS/workload-response.txt" 2>/dev/null &&
    grep -q "shoal build fixture ok" "$RESULTS/workload-response.txt"
}
wait_for 180 "the built image serves its page on :$WORKLOAD_PORT" served
cc get workload web -o yaml >"$RESULTS/workload.yaml" || true

echo "PASS: build end to end" >>"$RESULTS/steps.txt"
echo "build end to end: $name -> $image ($digest)"
