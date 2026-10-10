#!/usr/bin/env bash
# HA chaos scenario (ADR 0014, pass 2g).
#
# Runs the real shoal binary as three `shoal server --store=raft` processes on
# 127.0.0.1 (node-1 bootstraps, node-2 and node-3 join with a join bundle),
# then a restored cluster, and drives everything through the CLI.
#
# Control plane only: no agents, no containerd, no root. Each server is
# started as the current user. Run it on a disposable runner (see
# .github/workflows/ha-chaos.yml). Evidence goes to test-results/ha-chaos/.
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"
SHOAL="$ROOT/bin/shoal"
RESULTS="$ROOT/test-results/ha-chaos"
LOGS="$RESULTS/logs"
WORK=$(mktemp -d)
rm -rf "$RESULTS"
mkdir -p "$LOGS"
: >"$RESULTS/steps.txt"
declare -A PID=()
CONF=""

# Node N: API 18440+N, raft 18450+N. The restored cluster uses 18461/18471.
api_of() { echo $((18440 + $1)); }
raft_of() { echo $((18450 + $1)); }

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

cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  for name in "${!PID[@]}"; do
    kill -CONT "${PID[$name]}" 2>/dev/null
    kill -TERM "${PID[$name]}" 2>/dev/null
  done
  sleep 2
  for name in "${!PID[@]}"; do
    kill -KILL "${PID[$name]}" 2>/dev/null
  done
  if [ "$rc" -ne 0 ]; then
    echo "::group::shoal server logs (scenario failed with exit $rc)"
    for f in "$LOGS"/*.log; do
      echo "----- $(basename "$f")"
      tail -n 400 "$f"
    done
    echo "::endgroup::"
  fi
  # The join bundle and the backup hold cluster secrets: not evidence.
  rm -rf "$WORK"
  exit "$rc"
}
trap cleanup EXIT

# run_server <name> <data-dir> <api-port> <raft-port> [flags...]
run_server() {
  local name=$1 dir=$2 api=$3 raft=$4
  shift 4
  "$SHOAL" server --data-dir "$dir" --listen "127.0.0.1:$api" \
    --store raft --raft-node-id "$name" --raft-bind "127.0.0.1:$raft" \
    --ui-listen "" "$@" >>"$LOGS/$name.log" 2>&1 &
  PID[$name]=$!
  say "started $name (pid ${PID[$name]}, api 127.0.0.1:$api, raft 127.0.0.1:$raft)"
}

node_run() { # <node-number> [flags...]
  local n=$1
  shift
  run_server "node-$n" "$WORK/node-$n" "$(api_of "$n")" "$(raft_of "$n")" "$@"
}

# cc <api-port> <cli args...>: run the CLI against one server, with the
# admin credentials in $CONF (all nodes share one CA, so one file works).
cc() {
  local port=$1
  shift
  "$SHOAL" "$@" --config "$CONF" --server "127.0.0.1:$port" --timeout 20s
}

# cluster_view <api-port>: "<voters> <leaders> <leader-node-id>". Prints
# "0 0 " when the server does not answer, so a view never passes by accident.
cluster_view() {
  cc "$1" cluster status 2>/dev/null | awk '
    NF == 4 && ($3 == "yes" || $3 == "no") {
      if ($3 == "yes") v++
      if ($4 == "yes") { l++; id = $1 }
    }
    END { print v + 0, l + 0, id }' || true
}

# status_line <api-port> <label>: value of a "Label: value" line.
status_line() {
  cc "$1" cluster status 2>/dev/null | awk -v k="$2:" 'index($0, k) == 1 { print $NF }' || true
}

leader_of() { cluster_view "$1" | awk '{ print $3 }'; }

view_is() { # <api-port> <voters> <leaders>
  local v
  v=$(cluster_view "$1")
  [ "${v% *}" = "$2 $3" ]
}

all_views_are() { # <voters> <leaders>: every node in the first three answers the same
  local n
  for n in 1 2 3; do
    view_is "$(api_of "$n")" "$1" "$2" || return 1
  done
}

leader_changed() { # <api-port> <old-leader-id>
  local l
  l=$(leader_of "$1")
  [ -n "$l" ] && [ "$l" != "$2" ]
}

follows() { # <api-port> <leader-id>
  [ "$(leader_of "$1")" = "$2" ]
}

caught_up() { # <api-port> <leader-api-port>
  local applied commit
  applied=$(status_line "$1" "Applied index")
  commit=$(status_line "$2" "Commit index")
  [ -n "$applied" ] && [ -n "$commit" ] && [ "$applied" -ge "$commit" ]
}

has_workload() { # <api-port> <name>
  cc "$1" get workloads "$2" -o yaml >/dev/null 2>&1
}

both_readable() { # <node-a> <node-b> <object>
  has_workload "$(api_of "$1")" "$3" && has_workload "$(api_of "$2")" "$3"
}

workload_yaml() { # <name>
  cat <<EOF
kind: Workload
metadata: { name: $1 }
spec:
  image: nginx:1.27
  replicas: 1
  ports: [{ containerPort: 80 }]
  resources: { cpu: 200m, memory: 128Mi }
EOF
}

put_workload() { # <api-port> <name>
  workload_yaml "$2" >"$WORK/$2.yaml"
  cc "$1" apply -f "$WORK/$2.yaml" >/dev/null
}

wait_for() { # <seconds> <what> <command...>
  local secs=$1 what=$2 deadline
  shift 2
  deadline=$((SECONDS + secs))
  until "$@"; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      fail "timed out after ${secs}s: $what"
    fi
    sleep 1
  done
  pass "$what"
}

# pick_other <excluded-node> [excluded-node]: the lowest node number not excluded.
pick_other() {
  local n
  for n in 1 2 3; do
    if [ "$n" != "$1" ] && [ "$n" != "${2:-}" ]; then
      echo "$n"
      return 0
    fi
  done
  return 1
}

num_of() { echo "${1#node-}"; }

# ---------------------------------------------------------------- build
say "build shoal"
make build >/dev/null
[ -x "$SHOAL" ] || fail "make build did not produce $SHOAL"

# ---------------------------------------------------------------- bootstrap
say "bootstrap node-1 with --store=raft (no join)"
node_run 1
CONF="$WORK/node-1/admin.conf"
wait_for 60 "node-1 bootstrapped as a single voter and leader" view_is "$(api_of 1)" 1 1

cc "$(api_of 1)" cluster join-bundle --out "$WORK/join.bundle" >/dev/null ||
  fail "cluster join-bundle failed"
node_run 2 --raft-join-bundle "$WORK/join.bundle" --raft-join "127.0.0.1:$(api_of 1)"
node_run 3 --raft-join-bundle "$WORK/join.bundle" --raft-join "127.0.0.1:$(api_of 1)"

# ---------------------------------------------------------------- a
say "a) membership"
wait_for 120 "a) cluster status shows 3 voters and 1 leader" all_views_are 3 1
cc "$(api_of 1)" cluster status || true

# ---------------------------------------------------------------- b
say "b) write through a follower"
LEADER=$(leader_of "$(api_of 1)")
LN=$(num_of "$LEADER")
FN=$(pick_other "$LN")
put_workload "$(api_of "$FN")" before-failover || fail "b) apply through follower node-$FN was rejected"
wait_for 30 "b) write through follower node-$FN is readable on leader node-$LN and on the follower" \
  both_readable "$LN" "$FN" before-failover

# ---------------------------------------------------------------- c
say "c) kill -9 the leader"
LEADER=$(leader_of "$(api_of "$(pick_other "$FN")")")
LN=$(num_of "$LEADER")
kill -KILL "${PID[$LEADER]}"
wait "${PID[$LEADER]}" 2>/dev/null || true
pass "c) killed leader $LEADER with kill -9"
SN=$(pick_other "$LN")
SURV_API=$(api_of "$SN")
wait_for 60 "c) survivor node-$SN elects a new leader (was $LEADER)" leader_changed "$SURV_API" "$LEADER"
wait_for 30 "c) object written before the kill is readable on the survivor" \
  has_workload "$SURV_API" before-failover
wait_for 60 "c) write after failover is accepted by the survivor" put_workload "$SURV_API" after-failover

# ---------------------------------------------------------------- d
say "d) restart the killed node on its data dir"
node_run "$LN" --raft-join "127.0.0.1:$SURV_API"
wait_for 120 "d) restarted $LEADER rejoins: 3 voters and 1 leader on every node" all_views_are 3 1
wait_for 60 "d) restarted $LEADER serves the object written while it was down" \
  has_workload "$(api_of "$LN")" after-failover

# ---------------------------------------------------------------- e
say "e) freeze the leader with SIGSTOP"
LEADER=$(leader_of "$SURV_API")
LN=$(num_of "$LEADER")
OLD_API=$(api_of "$LN")
SN=$(pick_other "$LN")
SURV_API=$(api_of "$SN")
kill -STOP "${PID[$LEADER]}"
pass "e) froze leader $LEADER with SIGSTOP"
wait_for 60 "e) survivor elects a new leader while $LEADER is frozen" leader_changed "$SURV_API" "$LEADER"
NEW=$(leader_of "$SURV_API")
NEWN=$(num_of "$NEW")
put_workload "$SURV_API" during-partition || fail "e) write during the freeze was rejected"
pass "e) write during the freeze accepted by $NEW"
sleep 6 # well past the default 1s election timeout
kill -CONT "${PID[$LEADER]}"
pass "e) resumed $LEADER with SIGCONT"
wait_for 60 "e) $LEADER steps down and follows $NEW" follows "$OLD_API" "$NEW"
wait_for 60 "e) $LEADER catches up (applied index reaches $NEW commit index)" \
  caught_up "$OLD_API" "$(api_of "$NEWN")"
wait_for 30 "e) object written during the freeze is readable on $LEADER" has_workload "$OLD_API" during-partition
wait_for 60 "e) all nodes agree on 3 voters and 1 leader after the freeze" all_views_are 3 1

# ---------------------------------------------------------------- f
say "f) backup and restore into a fresh data dir"
LEADER=$(leader_of "$SURV_API")
LAPI=$(api_of "$(num_of "$LEADER")")
cc "$LAPI" backup --out "$WORK/backup.json" || fail "f) backup on leader $LEADER failed"
cc "$LAPI" get workloads after-failover -o yaml >"$WORK/src-after-failover.yaml"
[ -s "$WORK/backup.json" ] || fail "f) backup file is empty"
pass "f) backup written from leader $LEADER"

mkdir -p "$WORK/restored"
cp "$WORK/node-1/secret.key" "$WORK/restored/secret.key"
run_server restored "$WORK/restored" 18461 18471 --restore-from "$WORK/backup.json"
RCONF="$WORK/restored/admin.conf"
CONF="$RCONF" wait_for 60 "f) restored cluster is up as a single voter" view_is 18461 1 1
CONF="$RCONF" cc 18461 get workloads after-failover -o yaml >"$RESULTS/restored-after-failover.yaml"
diff -u "$WORK/src-after-failover.yaml" "$RESULTS/restored-after-failover.yaml" ||
  fail "f) restored object differs from the source"
pass "f) object after-failover is identical in the restored cluster"
CONF="$RCONF" wait_for 30 "f) object written during the freeze is in the restored cluster" \
  has_workload 18461 during-partition

# ---------------------------------------------------------------- g
say "g) remove node-3 from the cluster"
SN=$(pick_other 3)
cc "$(api_of "$SN")" cluster remove node-3 || fail "g) cluster remove node-3 failed"
pass "g) cluster remove node-3 accepted by node-$SN"
kill -TERM "${PID[node-3]}" 2>/dev/null || true
wait "${PID[node-3]}" 2>/dev/null || true
wait_for 60 "g) status on node-$SN shows 2 voters and 1 leader" view_is "$(api_of "$SN")" 2 1

say "all HA chaos steps passed"
echo "all HA chaos steps passed" >>"$RESULTS/steps.txt"
