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

# diagnose: cluster state for a failed run (printed to the log and saved).
diagnose() {
  {
    echo "== diagnose: app status"
    cc app status blog || true
    echo "== diagnose: replicas"
    cc get replicas -o yaml || true
    echo "== diagnose: secrets"
    cc get secrets || true
    echo "== diagnose: databases"
    cc get databases -o yaml || true
    echo "== diagnose: app logs (last 200 lines per replica)"
    cc app logs blog --tail 200 || true
    echo == diagnose: database backups
    cc db backups blog-db || true
    echo "== diagnose: containerd containers"
    sudo -n ctr -n shoal containers ls || true
    sudo -n ctr -n shoal task ls || true
  } 2>&1 | tee "$RESULTS/diagnose.txt"
}

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
say "f) shoal app status blog shows every object in a healthy state"
# The state vocabulary of app status: workload "1/1 ready", database "Ready",
# route "Ready", volume "Bound", secret "empty" (created, no values set), and a
# service has no state (shown as "-"). Anything that is not one of these fails.
cc app status blog >"$RESULTS/app-status.txt" 2>&1 || fail "f) app status blog exited non-zero"
cat "$RESULTS/app-status.txt"
for pat in '^workload +blog +1/1 ready' '^service +blog +-' '^route +blog +Ready' \
  '^database +blog-db +Ready' '^volume +blog-uploads +Bound' '^secret +blog-secrets +empty'; do
  grep -Eq "$pat" "$RESULTS/app-status.txt" || fail "f) app status has no healthy row matching: $pat"
done
if grep -Eiq 'pending|failed|degraded|provisioning|noendpoints|not ready|nodelost|0/1' "$RESULTS/app-status.txt"; then
  fail "f) app status shows an object that is not healthy (see app-status.txt)"
fi
pass "f) app status: workload 1/1 ready, database Ready, route Ready, volume Bound, secret empty, service without state"

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

# ---------------------------------------------------------------- h) backup
# pg_scalar WORKLOAD DATABASE SQL: runs SQL with psql in the Postgres container
# of WORKLOAD (through ctr, outside Shoal) and prints the single value.
pg_scalar() {
  local wl=$1 db=$2 sql=$3 id="" cid info out
  for cid in $(sudo -n ctr -n shoal containers ls -q); do
    info=$(sudo -n ctr -n shoal containers info "$cid" 2>/dev/null || true)
    if printf '%s' "$info" | grep -Eq "\"shoal.workload\": *\"$wl\""; then
      id=$cid
    fi
  done
  [ -n "$id" ] || return 1
  out=$(sudo -n ctr -n shoal task exec --exec-id "psql-$RANDOM-$RANDOM" "$id" psql -U app -d "$db" -tAc "$sql") || return 1
  printf '%s' "$out" | tr -d '[:space:]'
}
# backup_ok ID: the backup is listed with status ok.
backup_ok() {
  cc db backups blog-db >"$RESULTS/db-backups.txt" 2>&1 || return 1
  grep -Eq "^$1[[:space:]]+[^[:space:]]+[[:space:]]+ok[[:space:]]" "$RESULTS/db-backups.txt"
}
# restore_done: the restored Database reports restore state done.
restore_done() {
  cc get databases blog-db-restored -o yaml >"$RESULTS/restored-db.yaml" 2>&1 || return 1
  grep -Eq 'restore(State|_state)"?:[[:space:]]*"?done' "$RESULTS/restored-db.yaml"
}

say "h) shoal db backup blog-db takes a logical backup that verifies"
rows_before=$(pg_scalar blog-db blog_db 'select count(*) from hits') || fail "h) cannot query blog-db"
[ "$rows_before" -ge 3 ] || fail "h) blog-db has $rows_before rows, want at least 3"
if ! cc db backup blog-db >"$RESULTS/db-backup.txt" 2>&1; then
  cat "$RESULTS/db-backup.txt"
  fail "h) shoal db backup blog-db exited non-zero"
fi
cat "$RESULTS/db-backup.txt"
backup_id=$(awk '/^backup /{print $2; exit}' "$RESULTS/db-backup.txt")
[ -n "$backup_id" ] || fail "h) shoal db backup printed no backup ID"
wait_for 300 "h) backup $backup_id is ok" backup_ok "$backup_id"
cat "$RESULTS/db-backups.txt"
pass "h) backup $backup_id verified (blog-db had $rows_before rows)"

# ---------------------------------------------------------------- i) restore
say "i) shoal db restore loads the backup into a new database, and the source keeps its rows"
if ! cc db restore blog-db --from "$backup_id" --into blog-db-restored >"$RESULTS/db-restore.txt" 2>&1; then
  cat "$RESULTS/db-restore.txt"
  fail "i) shoal db restore exited non-zero"
fi
cat "$RESULTS/db-restore.txt"
wait_for 600 "i) blog-db-restored finished its restore" restore_done
cat "$RESULTS/restored-db.yaml"
restored=$(pg_scalar blog-db-restored blog_db_restored 'select count(*) from hits') || fail "i) cannot query blog-db-restored"
[ "$restored" -ge "$rows_before" ] || fail "i) blog-db-restored has $restored rows, the backup had $rows_before"
src=$(pg_scalar blog-db blog_db 'select count(*) from hits') || fail "i) cannot query blog-db"
[ "$src" -ge "$rows_before" ] || fail "i) blog-db has $src rows, fewer than the $rows_before it had at backup"
echo "rows at backup: $rows_before; blog-db-restored: $restored; blog-db: $src" | tee "$RESULTS/restore-rows.txt"
pass "i) blog-db-restored has $restored rows (the backup had $rows_before); blog-db kept $src"

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
