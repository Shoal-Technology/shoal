#!/usr/bin/env bash
# shoal pipelines, end to end (ADR 0022, pass B). A node labelled as a runner
# and a builder joins a server with containerd. A protected manual run of the
# fixture in hack/pipeline-fixture checks, in containers: the job's network
# (the node's ports unreachable, the internet reachable), a secret (given to a
# protected run, redacted in the log) and a build step (a Build that the run
# waits for). A signed push webhook then starts a run of the same pipeline,
# which must be created with the push trigger and refused the secret, since
# the branch is not protected. It runs on a disposable runner with containerd
# (.github/workflows/pipeline-e2e.yml). Do not run it on the shared VPS: it
# starts containers.
#
# BUILD_REF is the branch the repository is cloned from (the BuildConfig and
# the Pipeline name it). PIPELINE_SHA is the commit under test; the server
# fetches it by sha, so it must be the head of that branch. Evidence goes to
# test-results/pipeline-e2e/.
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"
SHOAL="$ROOT/bin/shoal"
BUILD_REF=${BUILD_REF:?BUILD_REF must name the branch under test}
PIPELINE_SHA=${PIPELINE_SHA:-$(git rev-parse HEAD)}
REPO_URL=https://github.com/Shoal-Technology/shoal.git
RESULTS="$ROOT/test-results/pipeline-e2e"
LOGS="$RESULTS/logs"
WORK=$(mktemp -d)
rm -rf "$RESULTS"
mkdir -p "$LOGS"
: >"$RESULTS/steps.txt"
. "$ROOT/hack/e2e-lib.sh"

NODE_IP=$(hostname -I | awk '{print $1}')
REGISTRY=$NODE_IP:5443
RUNNER=runner-1
PROBE_PORT=8099
PROBE_PID=""
PIPELINE=web-ci
TOKEN_VALUE=$(openssl rand -hex 12)
HOOK_VALUE=$(openssl rand -hex 16)

cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  [ -n "$PROBE_PID" ] && kill "$PROBE_PID" 2>/dev/null
  teardown "$rc" "pipeline e2e"
  exit "$rc"
}
trap cleanup EXIT

# diagnose: the state of the runs, jobs and builds, for a failed run.
diagnose() {
  {
    echo "== diagnose: runs"
    cc pipeline runs || true
    echo "== diagnose: nodes"
    cc get nodes || true
    echo "== diagnose: builds"
    cc get builds || true
    echo "== diagnose: containers"
    sudo -n ctr -n shoal containers ls || true
  } 2>&1 | tee "$RESULTS/diagnose.txt"
}

# run_phase prints the phase of a run from the runs table.
run_phase() {
  cc pipeline runs "$PIPELINE" | awk -v n="$1" '$1 == n { print $5 }'
}

# wait_run waits for a run to end and returns its phase.
wait_run() {
  local name=$1 deadline phase=""
  deadline=$((SECONDS + 2400))
  while :; do
    phase=$(run_phase "$name")
    case "$phase" in
      Complete | Failed | Error | Cancelled) echo "$phase"; return 0 ;;
    esac
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "still ${phase:-unknown}"
      return 1
    fi
    sleep 10
  done
}

say "build the shoal CLI and server"
(cd "$ROOT" && go build -o "$SHOAL" ./cmd/shoal)
pass "built $SHOAL"

say "start the server with the embedded registry on $REGISTRY"
mkdir -p "$DATA"
start_root server "$LOGS/server.log" "$SHOAL" server \
  --data-dir "$DATA" --listen 0.0.0.0:7443 --advertise-address "$NODE_IP:7443" \
  --ui-listen 0.0.0.0:8443 --registry-listen 0.0.0.0:5443

# A plain HTTP server on the node's address. The job's network must not reach
# it, as it must not reach the Shoal API or the portal.
(cd "$WORK" && exec python3 -m http.server "$PROBE_PORT" --bind 0.0.0.0 >"$LOGS/probe.log" 2>&1) &
PROBE_PID=$!
wait_for 30 "probe server answers on $NODE_IP:$PROBE_PORT" curl -fsS --max-time 2 "http://$NODE_IP:$PROBE_PORT/"
wait_for 90 "server accepts CLI calls" test -s "$CONF"
wait_for 90 "server answers get nodes" cc get nodes

say "join one agent with containerd: $RUNNER"
token_out=$(cc token create --max-uses 1 --ttl 1h)
join=$(printf '%s\n' "$token_out" | grep -o 'shoal agent .*' || true)
[ -n "$join" ] || fail "token create printed no join command"
field() { echo "$join" | awk -v k="$1" '{ for (i = 1; i < NF; i++) if ($i == k) { print $(i + 1); exit } }'; }
token=$(field --token)
cahash=$(field --ca-hash)
SERVER_ADDR=$(field --server)
[ -n "$token" ] && [ -n "$cahash" ] && [ -n "$SERVER_ADDR" ] || fail "could not read the join command"
start_root agent "$LOGS/agent.log" "$SHOAL" agent \
  --server "$SERVER_ADDR" --token "$token" --ca-hash "$cahash" --node-name "$RUNNER"
unset token cahash
node_ready() { cc get nodes | awk -v n="$RUNNER" '$1 == n && $2 == "Ready" { ok = 1 } END { exit !ok }'; }
wait_for 120 "node $RUNNER is Ready" node_ready

say "label $RUNNER as a runner, a builder and linux-amd64"
cc label node "$RUNNER" shoal.dev/runner=true
cc label node "$RUNNER" shoal.dev/builder=true
cc label node "$RUNNER" linux-amd64=true
pass "labelled $RUNNER"

say "create the secrets: the pipeline token and the webhook secret"
cc secret create pipeline-token "token=$TOKEN_VALUE" >"$RESULTS/secret-create.txt" 2>&1 ||
  fail "secret create pipeline-token refused"
# The webhook key is webhook-secret, the key the README and `shoal build
# webhook-url` name, so the e2e creates it with that same command.
cc secret create hook-secret "webhook-secret=$HOOK_VALUE" >>"$RESULTS/secret-create.txt" 2>&1 ||
  fail "secret create hook-secret refused (see secret-create.txt)"
pass "created pipeline-token and hook-secret"

say "create the BuildConfig web (the build step of the pipeline) with a push webhook"
cat >"$WORK/buildconfig.yaml" <<EOF
kind: BuildConfig
metadata: { name: web }
spec:
  git:
    url: $REPO_URL
    ref: $BUILD_REF
    contextDir: hack/build-fixture
    dockerfile: Dockerfile
  output: { repository: web, tag: "{short-sha}" }
  buildArgs:
    PROBE_NODE: "http://$NODE_IP:$PROBE_PORT/"
    PROBE_API: "http://$NODE_IP:7443/"
    PROBE_PORTAL: "http://$NODE_IP:8443/"
    PROBE_PUBLIC: "http://example.com/"
  webhook: { provider: github, secret: hook-secret, branch: $BUILD_REF }
EOF
cc build create-config -f "$WORK/buildconfig.yaml" >"$RESULTS/create-config.txt" 2>&1 ||
  fail "create-config refused the BuildConfig (see create-config.txt)"
pass "created BuildConfig web"

say "create the pipeline $PIPELINE from hack/pipeline-fixture"
cat >"$WORK/pipeline.yaml" <<EOF
kind: Pipeline
metadata: { name: $PIPELINE }
spec:
  repo: $REPO_URL
  ref: $BUILD_REF
  path: hack/pipeline-fixture/shoal-ci.yml
EOF
cc pipeline create -f "$WORK/pipeline.yaml" >"$RESULTS/pipeline-create.txt" 2>&1 ||
  fail "pipeline create refused the Pipeline (see pipeline-create.txt)"
pass "created Pipeline $PIPELINE"

say "start a protected manual run and wait for it"
run_out=$(cc pipeline run "$PIPELINE" --ref HEAD)
printf '%s\n' "$run_out" >"$RESULTS/run.txt"
run=$(printf '%s\n' "$run_out" | sed -n 's#^pipelinerun/\([^ ]*\) created.*#\1#p')
[ -n "$run" ] || fail "pipeline run printed no run name: $run_out"
pass "started run $run"
phase=$(wait_run "$run") || { diagnose; fail "run $run is $phase after 40 minutes"; }
cc pipeline runs "$PIPELINE" >"$RESULTS/runs.txt" 2>&1 || true
[ "$phase" = Complete ] || { diagnose; fail "run $run ended $phase (see runs.txt and the job logs)"; }
pass "run $run is Complete"

say "the probe job: the node's ports are unreachable, the internet is reachable"
cc job logs "$run-probe" >"$RESULTS/probe-log.txt" 2>&1 || fail "could not read the probe log"
grep -q "probe ok: the internet is reachable" "$RESULTS/probe-log.txt" || fail "probe did not reach the internet (see probe-log.txt)"
grep -q "probe ok: http://" "$RESULTS/probe-log.txt" || fail "no blocked probe passed (see probe-log.txt)"
if grep -q "probe FAIL" "$RESULTS/probe-log.txt"; then fail "a probe failed (see probe-log.txt)"; fi
pass "the job cannot reach the node's ports and reaches the internet"

say "the token check job: the secret reaches the script and is redacted in the log"
cc job logs "$run-token-check" >"$RESULTS/token-check-log.txt" 2>&1 || fail "could not read the token-check log"
grep -q "token length 24" "$RESULTS/token-check-log.txt" || fail "the script did not see the secret (see token-check-log.txt)"
grep -q "token is \[redacted\]" "$RESULTS/token-check-log.txt" || fail "the secret was not redacted in the log"
pass "the secret reached the job and is redacted"

say "the build step: the run waited for a Build"
cc get builds -o yaml >"$RESULTS/builds.yaml" 2>&1 || true
grep -q "trigger: pipeline" "$RESULTS/builds.yaml" || fail "no Build was started by the pipeline (see builds.yaml)"
pass "a Build was started by the pipeline's build step"

say "the secret value is in no evidence file"
if grep -rqF "$TOKEN_VALUE" "$RESULTS"; then
  fail "the secret value is in the evidence (it must be redacted everywhere)"
fi
pass "the secret value is absent from every log and record"

say "a signed push webhook starts a run of the same pipeline"
before=$(cc pipeline runs "$PIPELINE" | awk 'NR > 1 { print $1 }')
uid=$(cc get bc web -o yaml | grep -m1 'uid:' | awk '{print $2}')
[ -n "$uid" ] || fail "the BuildConfig has no uid"
body="{\"ref\":\"refs/heads/$BUILD_REF\",\"after\":\"$PIPELINE_SHA\",\"commits\":[{\"added\":[],\"modified\":[\"hack/pipeline-fixture/shoal-ci.yml\"],\"removed\":[]}]}"
sig="sha256=$(printf %s "$body" | openssl dgst -sha256 -hmac "$HOOK_VALUE" | awk '{print $NF}')"
hook_code=$(curl -sk -o "$RESULTS/hook-reply.txt" -w '%{http_code}' -X POST \
  -H "X-GitHub-Event: push" -H "X-Hub-Signature-256: $sig" -H "Content-Type: application/json" \
  --data "$body" "https://127.0.0.1:8443/hooks/v1/$uid")
[ "$hook_code" = 202 ] || fail "the webhook answered $hook_code (see hook-reply.txt)"
pass "the webhook accepted the push"

hook_run=""
for _ in $(seq 1 60); do
  now=$(cc pipeline runs "$PIPELINE" | awk 'NR > 1 { print $1 }')
  hook_run=$(comm -13 <(printf '%s\n' "$before" | sort) <(printf '%s\n' "$now" | sort) | head -1)
  [ -n "$hook_run" ] && break
  sleep 5
done
[ -n "$hook_run" ] || { diagnose; fail "the push started no pipeline run"; }
trigger=$(cc pipeline runs "$PIPELINE" | awk -v n="$hook_run" '$1 == n { print $3 }')
[ "$trigger" = push ] || fail "run $hook_run has trigger '$trigger', want push"
pass "the push started run $hook_run with the push trigger"

hook_phase=$(wait_run "$hook_run") || { diagnose; fail "run $hook_run is $hook_phase after 40 minutes"; }
cc pipeline runs "$PIPELINE" >"$RESULTS/runs-after-push.txt" 2>&1 || true
[ "$hook_phase" = Failed ] || fail "run $hook_run ended $hook_phase, want Failed: a branch that is not protected gets no secret"
pass "run $hook_run ended Failed: its secret was refused, as a non-protected run must"

echo "PASS: pipeline end to end" >>"$RESULTS/steps.txt"
echo "pipeline end to end: run $run Complete, push run $hook_run $hook_phase"
