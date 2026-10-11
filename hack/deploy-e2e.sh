#!/usr/bin/env bash
# shoal deploy, end to end (ADR 0017 pass F, ADR 0016 pass F): an app with a
# Postgres database, a Volume and a domain, against pebble, on a disposable
# runner with containerd (.github/workflows/deploy-e2e.yml).
#
# The app is hack/blog: a postgres:16-alpine base with a shell script that
# connects through DATABASE_URL (injected by Shoal from the blog-db-credentials
# Secret), inserts one row every two seconds and serves the count on port 3000.
# The workflow builds the image with docker and saves it to $BLOG_TAR. This
# script imports it into containerd namespace "shoal", so the agent finds it
# locally and never pulls it. The Postgres image comes from the registry, as
# the Database controller pins it.
#
# Evidence goes to test-results/deploy-e2e/. Secrets live under $WORK, which is
# removed on exit.
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"
SHOAL="$ROOT/bin/shoal"
HOST=blog.shoal.test
BLOG_IMAGE=localhost/shoal-e2e/blog:ci
VOLUME_BACKEND=${VOLUME_BACKEND:-loop}
RESULTS="$ROOT/test-results/deploy-e2e"
LOGS="$RESULTS/logs"
WORK=$(mktemp -d)
rm -rf "$RESULTS"
mkdir -p "$LOGS"
: >"$RESULTS/steps.txt"
. "$ROOT/hack/e2e-lib.sh"
APP="$WORK/shoal.yaml"

cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  teardown "$rc" "deploy e2e"
  exit "$rc"
}
trap cleanup EXIT

# current_hits: prints the hits=N count from the blog page, or fails when the
# page does not show a count (no connection to Postgres).
current_hits() {
  local n
  curl -fsS --cacert "$ISSUING" --max-time 10 "https://$HOST/" >"$RESULTS/blog-response.txt" || return 1
  n=$(sed -n 's/.* hits=\([0-9][0-9]*\).*/\1/p' "$RESULTS/blog-response.txt")
  [ -n "$n" ] || return 1
  echo "$n"
}
hits_at_least() {
  local n
  n=$(current_hits) || return 1
  [ "$n" -ge "$1" ]
}
hits_above() {
  local n
  n=$(current_hits) || return 1
  [ "$n" -gt "$1" ]
}

# ---------------------------------------------------------------- setup
say "build shoal"
make build >/dev/null
[ -x "$SHOAL" ] || fail "make build did not produce $SHOAL"

say "import the blog image into containerd namespace shoal"
[ -n "${BLOG_TAR:-}" ] && [ -s "$BLOG_TAR" ] || fail "BLOG_TAR is not set or empty ($BLOG_TAR)"
sudo -n ctr -n shoal images import "$BLOG_TAR" >"$LOGS/import.log" 2>&1 || fail "ctr images import failed (see import.log)"
sudo -n ctr -n shoal images ls -q | grep -Fxq "$BLOG_IMAGE" || fail "$BLOG_IMAGE is not in namespace shoal after import"
pass "imported $BLOG_IMAGE from $BLOG_TAR"

install_pebble
start_pebble
hosts_add "$HOST"
AGENT_ARGS=(--volume-backend "$VOLUME_BACKEND")
start_cluster

cat >"$APP" <<EOF
name: blog
image: $BLOG_IMAGE
port: 3000
domain: $HOST
replicas: 1
database: postgres
volumes:
  - name: uploads
    mountPath: /app/uploads
    size: 1Gi
health:
  path: /
resources:
  cpu: 100m
  memory: 128Mi
EOF
cp "$APP" "$RESULTS/shoal.yaml"

# ---------------------------------------------------------------- a) dry-run
say "a) shoal deploy --dry-run lists creates for Workload, Service, Route, Database and Volume"
cc deploy -f "$APP" --dry-run >"$RESULTS/dry-run.txt" 2>&1 || {
  cat "$RESULTS/dry-run.txt"
  fail "a) deploy --dry-run exited non-zero"
}
cat "$RESULTS/dry-run.txt"
for ref in workload/blog service/blog route/blog database/blog-db volume/blog-uploads; do
  grep -Eq "^create +$ref\$" "$RESULTS/dry-run.txt" || fail "a) dry-run has no 'create $ref'"
done
pass "a) dry-run plans create for workload/blog, service/blog, route/blog, database/blog-db, volume/blog-uploads"

# ---------------------------------------------------------------- b) deploy
say "b) shoal deploy --wait=10m exits 0 and prints the URL"
if ! cc deploy -f "$APP" --wait=10m >"$RESULTS/deploy.txt" 2>&1; then
  cat "$RESULTS/deploy.txt"
  fail "b) deploy --wait=10m exited non-zero"
fi
cat "$RESULTS/deploy.txt"
grep -Eq "^url +https://$HOST" "$RESULTS/deploy.txt" || fail "b) deploy output has no url https://$HOST"
pass "b) deploy exits 0 and prints https://$HOST"

# ---------------------------------------------------------------- c) app connects to Postgres
say "c) the app answers over https and proves the Postgres connection"
wait_for 180 "c) https://$HOST/ shows select=1 from Postgres" hits_at_least 1
cat "$RESULTS/blog-response.txt"
grep -q 'select=1' "$RESULTS/blog-response.txt" || fail "c) response has no select=1"
pass "c) app response: $(cat "$RESULTS/blog-response.txt")"

# ---------------------------------------------------------------- d) persistence
say "d) data persists: kill the Postgres task and read the rows back"
wait_for 120 "d) the app has written at least 3 rows before the kill" hits_at_least 3
before=$(current_hits)
pg_id=""
for id in $(sudo -n ctr -n shoal containers ls -q); do
  info=$(sudo -n ctr -n shoal containers info "$id" 2>/dev/null || true)
  case "$info" in
    *postgres*) pg_id=$id ;;
  esac
done
[ -n "$pg_id" ] || fail "d) no postgres container found in namespace shoal"
echo "postgres container: $pg_id"
sudo -n ctr -n shoal task ls >"$RESULTS/tasks-before-kill.txt" 2>&1 || true
sudo -n ctr -n shoal task kill -s SIGKILL "$pg_id" || fail "d) ctr task kill $pg_id failed"
pass "d) killed the postgres task $pg_id (rows before kill: $before)"
wait_for 300 "d) the agent restarts Postgres and the app writes rows again" hits_above "$before"
after=$(current_hits)
[ "$after" -ge "$before" ] || fail "d) row count went down after restart: before=$before after=$after"
sudo -n ctr -n shoal task ls >"$RESULTS/tasks-after-restart.txt" 2>&1 || true
pass "d) rows survived the restart: before=$before after=$after"

# ---------------------------------------------------------------- e) second deploy
say "e) a second shoal deploy reports everything unchanged"
if ! cc deploy -f "$APP" --wait=2m >"$RESULTS/second-deploy.txt" 2>&1; then
  cat "$RESULTS/second-deploy.txt"
  fail "e) second deploy exited non-zero"
fi
cat "$RESULTS/second-deploy.txt"
if grep -Eq '^(create|update|delete|orphan) ' "$RESULTS/second-deploy.txt"; then
  fail "e) second deploy changed something"
fi
for ref in workload/blog service/blog route/blog database/blog-db volume/blog-uploads; do
  grep -Eq "^unchanged +$ref\$" "$RESULTS/second-deploy.txt" || fail "e) second deploy does not report unchanged $ref"
done
pass "e) second deploy: every object unchanged"

# ---------------------------------------------------------------- f) status
say "f) shoal app status blog lists every object as Ready"
cc app status blog >"$RESULTS/app-status.txt" 2>&1 || fail "f) app status blog exited non-zero"
cat "$RESULTS/app-status.txt"
for pat in '^workload +blog +Ready' '^service +blog +Ready' '^route +blog +Ready' \
  '^database +blog-db +Ready' '^volume +blog-uploads +Ready'; do
  grep -Eq "$pat" "$RESULTS/app-status.txt" || fail "f) app status has no Ready row matching: $pat"
done
awk 'NR > 1 && NF >= 3 && $1 != "url:" && $3 != "Ready" { print "not Ready: " $0; bad = 1 } END { exit bad }' \
  "$RESULTS/app-status.txt" || fail "f) some objects are not Ready (see app-status.txt)"
pass "f) app status: every object is Ready"

# ---------------------------------------------------------------- g) delete keeps data
say "g) shoal app delete blog keeps the Database and the Volume"
cc app delete blog --yes >"$RESULTS/app-delete.txt" 2>&1 || {
  cat "$RESULTS/app-delete.txt"
  fail "g) app delete blog exited non-zero"
}
cat "$RESULTS/app-delete.txt"
cc get workloads >"$RESULTS/workloads-after-delete.txt" 2>&1 || true
if grep -Eq '(^|[[:space:]])blog([[:space:]]|$)' "$RESULTS/workloads-after-delete.txt"; then
  fail "g) workload blog still exists after app delete"
fi
cc get databases >"$RESULTS/databases-after-delete.txt" 2>&1 || fail "g) get databases failed"
grep -Eq '(^|[[:space:]])blog-db([[:space:]]|$)' "$RESULTS/databases-after-delete.txt" ||
  fail "g) database blog-db is gone after app delete"
cc get volumes >"$RESULTS/volumes-after-delete.txt" 2>&1 || fail "g) get volumes failed"
grep -Eq '(^|[[:space:]])blog-uploads([[:space:]]|$)' "$RESULTS/volumes-after-delete.txt" ||
  fail "g) volume blog-uploads is gone after app delete"
cc get databases -o yaml >"$RESULTS/databases.yaml" 2>/dev/null || true
cc get volumes -o yaml >"$RESULTS/volumes.yaml" 2>/dev/null || true
pass "g) app delete removed the workload and kept database blog-db and volume blog-uploads"

# ---------------------------------------------------------------- evidence
if grep -qiE 'fall(ing)? back|dir backend' "$LOGS/agent.log" 2>/dev/null; then
  backend=dir
else
  backend=loop
fi
echo "volume backend requested: $VOLUME_BACKEND; in use: $backend"
echo "volume backend: requested $VOLUME_BACKEND, in use $backend" >>"$RESULTS/steps.txt"
cc get nodes >"$RESULTS/nodes.txt" 2>/dev/null || true
say "all deploy checks passed"
pass "deploy e2e complete"
