#!/bin/sh
# Drives entrypoint.sh's single-writer handover (#35, #54) outside a container.
#
# Each "replica" is entrypoint.sh run against a shared temp dir standing in for
# the NFS /pb_data, with a fake pocketbase that records when it is serving. The
# assertion that matters is OVERLAP: two fake servers running at once is two
# SQLite writers on one volume.
#
# Needs a POSIX sh and util-linux-style flock (`brew install flock dash` on
# macOS). Usage: scripts/test_entrypoint_handover.sh [path-to-old-entrypoint]
# The optional old entrypoint enables T3, the pre-#54 -> #54 transition.

set -u
ROOT=$(cd "$(dirname "$0")/.." && pwd)
SH=${TEST_SH:-dash}
command -v "$SH" >/dev/null 2>&1 || SH="sh"
OLD_ENTRYPOINT=${1:-}
WORK=$(mktemp -d "${TMPDIR:-/tmp}/pb-handover.XXXXXX")
FAILS=0

FAKE_PB="$WORK/fake-pocketbase"
cat > "$FAKE_PB" <<'EOF'
#!/bin/sh
# fake pocketbase: `serve --dir=X ...` runs until TERM; everything else is a no-op
[ "${1:-}" = "serve" ] || exit 0
for a in "$@"; do case "$a" in --dir=*) dir=${a#--dir=} ;; esac; done
me=$PB_INSTANCE_ID
mkdir -p "$dir/.serving"
for other in "$dir"/.serving/*; do
    [ -e "$other" ] || continue
    echo "OVERLAP: $me started while $(basename "$other") was serving" >> "$dir/.ledger"
done
: > "$dir/.serving/$me"
echo "start $me" >> "$dir/.ledger"
stop() {
    sleep "${FAKE_PB_STOP_DELAY:-0}"
    rm -f "$dir/.serving/$me"
    echo "stop $me" >> "$dir/.ledger"
    exit 0
}
trap stop TERM INT
while :; do sleep 1 & wait $!; done
EOF
chmod +x "$FAKE_PB"

fail() { echo "  FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "  ok:   $*"; }

# start_replica DATA ID SCRIPT [VAR=VAL ...] — prints the entrypoint shell pid
start_replica() {
    data=$1 id=$2 script=$3
    shift 3
    env PB_DATA_DIR="$data" PB_BIN="$FAKE_PB" PB_INSTANCE_ID="$id" PB_HANDOVER_POLL=1 \
        OTEL_EXPORTER_OTLP_ENDPOINT= PB_ADMIN_EMAIL= "$@" \
        "$SH" "$script" >> "$data/.console.$id" 2>&1 &
    echo $!
}

serving() { [ -e "$1/.serving/$2" ]; }

# wait_until SECONDS CMD... — poll once a second
wait_until() {
    n=$1
    shift
    while [ "$n" -gt 0 ]; do
        "$@" && return 0
        sleep 1
        n=$((n - 1))
    done
    return 1
}

# Kill a replica's whole process tree, the way a container restart would.
kill_replica() {
    pkill -KILL -P "$1" 2>/dev/null
    kill -KILL "$1" 2>/dev/null
    for p in $(pgrep -P "$1" 2>/dev/null); do pkill -KILL -P "$p" 2>/dev/null; kill -KILL "$p" 2>/dev/null; done
    sleep 1
}

no_overlap() {
    if grep -q OVERLAP "$1/.ledger" 2>/dev/null; then
        fail "$(grep OVERLAP "$1/.ledger")"
    else
        pass "no two writers at any point"
    fi
}

cleanup_scenario() {
    for p in $PIDS; do kill_replica "$p"; done
    PIDS=""
}

NEW="$ROOT/entrypoint.sh"
A=ca-auth--azd-1000000100-aaaa-aaaaa
B=ca-auth--azd-1000000200-bbbb-bbbbb

echo "T1: new -> new handover, then a liveness restart of the parked replica"
D="$WORK/t1"; mkdir -p "$D"; PIDS=""
pa=$(start_replica "$D" "$A" "$NEW"); PIDS="$PIDS $pa"
wait_until 15 serving "$D" "$A" && pass "A serving" || fail "A never started"
pb=$(start_replica "$D" "$B" "$NEW"); PIDS="$PIDS $pb"
t0=$(date +%s)
wait_until 30 serving "$D" "$B" && pass "B serving after $(( $(date +%s) - t0 ))s" || fail "B never started"
serving "$D" "$A" && fail "A still serving" || pass "A stopped"
grep -q "single writer confirmed" "$D/.console.$B" && pass "B confirmed by ack" || fail "B did not see an ack: $(tail -2 "$D/.console.$B")"
grep -qxF "$A" "$D/.pb_released" && pass "A recorded in .pb_released" || fail "A not recorded as released"
[ "$(cat "$D/.pb_owner")" = "$B" ] && pass "owner is B" || fail "owner is '$(cat "$D/.pb_owner")'"
kill_replica "$pa"
pa=$(start_replica "$D" "$A" "$NEW"); PIDS="$PIDS $pa"
sleep 5
serving "$D" "$A" && fail "zombie A started serving" || pass "restarted A parked instead of serving"
serving "$D" "$B" && pass "B still serving" || fail "B stopped after the zombie restart"
grep -q "already handed /pb_data over" "$D/.console.$A" && pass "zombie guard fired" || fail "no zombie guard message"
grep -q "INFO" "$D/.pb_handover.log" && pass "events recorded durably" || fail "no durable event log"
no_overlap "$D"
cleanup_scenario

echo "T2: outgoing replica takes 35s to release (the NFS flock-backoff window)"
D="$WORK/t2"; mkdir -p "$D"; PIDS=""
pa=$(start_replica "$D" "$A" "$NEW" FAKE_PB_STOP_DELAY=35); PIDS="$PIDS $pa"
wait_until 15 serving "$D" "$A" || fail "A never started"
pb=$(start_replica "$D" "$B" "$NEW"); PIDS="$PIDS $pb"
t0=$(date +%s)
wait_until 70 serving "$D" "$B" && pass "B serving after $(( $(date +%s) - t0 ))s" || fail "B never started"
grep -q "single writer confirmed" "$D/.console.$B" && pass "B confirmed by ack, not by timeout" || fail "B fell back: $(tail -2 "$D/.console.$B")"
no_overlap "$D"
cleanup_scenario

if [ -n "$OLD_ENTRYPOINT" ] && grep -q HANDOVER_ACK "$OLD_ENTRYPOINT"; then
    echo "T3: skipped — the given old entrypoint already has the #54 ack"
elif [ -n "$OLD_ENTRYPOINT" ]; then
    echo "T3: transition — outgoing runs the pre-#54 entrypoint and holds NO lock (prod 2026-09-28)"
    D="$WORK/t3"; mkdir -p "$D"; PIDS=""
    OLD="$WORK/old-entrypoint.sh"
    sed -e "s#/pb_data#$D#g" -e "s#/usr/local/bin/pocketbase#$FAKE_PB#g" "$OLD_ENTRYPOINT" > "$OLD"
    # A starts while something else holds the lock, times out, and serves unlocked
    : > "$D/.pb_singlewriter.lock"
    flock -x "$D/.pb_singlewriter.lock" sleep 6 &
    holder=$!
    sleep 1
    pa=$(start_replica "$D" "$A" "$OLD" PB_HANDOVER_TIMEOUT=2); PIDS="$PIDS $pa"
    wait_until 15 serving "$D" "$A" && pass "old A serving without the lock" || fail "old A never started"
    wait "$holder" 2>/dev/null
    pb=$(start_replica "$D" "$B" "$NEW" PB_HANDOVER_TIMEOUT=15); PIDS="$PIDS $pb"
    t0=$(date +%s)
    wait_until 40 serving "$D" "$B" && pass "B serving after $(( $(date +%s) - t0 ))s" || fail "B never started"
    grep -q "never acknowledged" "$D/.console.$B" && pass "B waited out the missing ack (expected once)" || fail "B did not take the no-ack path: $(tail -2 "$D/.console.$B")"
    no_overlap "$D"
    # the old replica, restarted by liveness while parked, asks for the volume back
    kill_replica "$pa"
    pa=$(start_replica "$D" "$A" "$OLD" PB_HANDOVER_TIMEOUT=20); PIDS="$PIDS $pa"
    sleep 6
    serving "$D" "$B" && pass "B kept /pb_data" || fail "B handed the volume to the retired replica"
    serving "$D" "$A" && fail "old zombie A is serving" || pass "old zombie A blocked on B's lock"
    grep -q "ignoring a handover request from '$A'" "$D/.console.$B" && pass "B ignored the stale request" || fail "B did not ignore the stale request"
    no_overlap "$D"
    cleanup_scenario
fi

echo "T4: first start on a fresh volume, then the owner's own container restarts"
D="$WORK/t4"; mkdir -p "$D"; PIDS=""
pb=$(start_replica "$D" "$B" "$NEW"); PIDS="$PIDS $pb"
wait_until 5 serving "$D" "$B" && pass "B took a fresh volume without waiting" || fail "B waited on a fresh volume"
kill_replica "$pb"; rm -f "$D/.serving/$B"
t0=$(date +%s)
pb=$(start_replica "$D" "$B" "$NEW"); PIDS="$PIDS $pb"
wait_until 15 serving "$D" "$B" && pass "B back after $(( $(date +%s) - t0 ))s without waiting for an ack" || fail "B did not come back"
no_overlap "$D"
cleanup_scenario

echo
if [ "$FAILS" -eq 0 ]; then
    echo "PASS (work dir $WORK)"
    rm -rf "$WORK"
else
    echo "$FAILS FAILURE(S) — consoles and ledgers kept in $WORK"
    exit 1
fi
