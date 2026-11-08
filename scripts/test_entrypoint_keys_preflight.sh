#!/bin/sh
# Drives entrypoint.sh's field-encryption key pre-flight (#75) outside a
# container, with a fake claviger whose `keys check` verdict is scripted.
#
# What matters: a definite "keys check: FAILED" stops the start BEFORE the
# handover request is written (the previous replica keeps /pb_data), and
# everything else fails open and goes on to the normal handover.
#
# Usage: scripts/test_entrypoint_keys_preflight.sh
# Needs a POSIX sh and flock (`brew install flock dash` on macOS).

# shellcheck disable=SC2015 # the pass/fail helpers always return 0
set -u
ROOT=$(cd "$(dirname "$0")/.." && pwd)
SH=${TEST_SH:-dash}
command -v "$SH" >/dev/null 2>&1 || SH="sh"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/pb-keys-preflight.XXXXXX")
FAILS=0

FAKE_PB="$WORK/fake-claviger"
cat > "$FAKE_PB" <<'FAKE'
#!/bin/sh
# fake claviger. `keys check` verdict comes from FAKE_KEYS_MODE; `serve` records itself.
if [ "${1:-}" = "keys" ] || [ "${2:-}" = "keys" ]; then
    echo "$*" >> "$FAKE_LOG"
    case "${FAKE_KEYS_MODE:-ok}" in
        ok) echo "keys check: OK (no encrypted fields)"; exit 0 ;;
        failed)
            echo "keys check: FAILED: 1 encrypted field key(s) exist but the key-encryption key is unusable" >&2
            exit 1 ;;
        unknown) echo "Error: unknown command \"keys\" for \"claviger\"" >&2; exit 1 ;;
        crash) echo "panic: boom" >&2; exit 2 ;;
        # exec: busybox timeout(1) signals only its direct child, and a sleep left
        # behind would hold the output pipe open (the real binary is one process)
        hang) exec sleep 120 ;;
    esac
fi
[ "${1:-}" = "serve" ] || [ "${2:-}" = "serve" ] || exit 0
for a in "$@"; do case "$a" in --dir=*) dir=${a#--dir=} ;; esac; done
echo "serve $*" >> "$FAKE_LOG"
: > "$dir/.serving"
trap 'rm -f "$dir/.serving"; exit 0' TERM INT
while :; do sleep 1 & wait $!; done
FAKE
chmod +x "$FAKE_PB"

fail() { echo "  FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "  ok:   $*"; }

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

# run_entrypoint DIR ID [VAR=VAL ...] — runs entrypoint.sh to completion or
# until killed; prints nothing, leaves the console in DIR/.console and the exit
# code in DIR/.rc
run_entrypoint() {
    d=$1 id=$2
    shift 2
    (
        env PB_DATA_DIR="$d" PB_BIN="$FAKE_PB" PB_INSTANCE_ID="$id" PB_HANDOVER_POLL=1 \
            PB_HANDOVER_TIMEOUT=3 FAKE_LOG="$d/.fake.log" OTEL_EXPORTER_OTLP_ENDPOINT= PB_ADMIN_EMAIL= \
            "$@" "$SH" "$ROOT/entrypoint.sh" > "$d/.console" 2>&1
        echo $? > "$d/.rc"
    ) > /dev/null 2>&1 &
    echo $!
}

# a volume that another replica has owned: the lock and owner files exist, so
# the incoming replica WILL write a handover request
prepare_volume() {
    mkdir -p "$1"
    : > "$1/data.db"
    : > "$1/.pb_singlewriter.lock"
    echo "ca-auth--azd-1000000100-aaaa-aaaaa" > "$1/.pb_owner"
}

serving() { [ -e "$1/.serving" ]; }
finished() { [ -e "$1/.rc" ]; }
PIDS=""
cleanup_scenario() {
    for p in $PIDS; do
        pkill -KILL -P "$p" 2>/dev/null
        kill -KILL "$p" 2>/dev/null
    done
    PIDS=""
}

B=ca-auth--azd-1000000200-bbbb-bbbbb

echo "P1: definite failure stops the start before any handover request"
D="$WORK/p1"; prepare_volume "$D"
p=$(run_entrypoint "$D" "$B" FAKE_KEYS_MODE=failed); PIDS="$p"
wait_until 10 finished "$D" && pass "entrypoint exited" || fail "entrypoint did not exit"
[ "$(cat "$D/.rc" 2>/dev/null)" = "1" ] && pass "exit status 1" || fail "exit status '$(cat "$D/.rc" 2>/dev/null)'"
[ -e "$D/.pb_handover" ] && fail ".pb_handover was written: the previous replica was asked to leave" || pass "no handover request written"
serving "$D" && fail "server started" || pass "server never started"
grep -q "keys check: FAILED" "$D/.console" && pass "reason on the console" || fail "no reason on the console: $(tail -3 "$D/.console")"
grep -q "WARN.*pre-flight failed" "$D/.pb_handover.log" && pass "recorded in the durable event log" || fail "not in the event log"
cleanup_scenario

echo "P2: ok verdict goes on to the normal handover"
D="$WORK/p2"; prepare_volume "$D"
p=$(run_entrypoint "$D" "$B" FAKE_KEYS_MODE=ok); PIDS="$p"
wait_until 15 serving "$D" && pass "server started after the (timed-out) handover" || fail "server never started: $(tail -3 "$D/.console")"
grep -q "keys check" "$D/.fake.log" && pass "keys check ran" || fail "keys check did not run"
grep -q -- "keys check --dir=$D" "$D/.fake.log" && pass "with the data dir" || fail "wrong arguments: $(cat "$D/.fake.log")"
cleanup_scenario

echo "P3: unknown command (exit 1 without the marker) fails open"
D="$WORK/p3"; prepare_volume "$D"
p=$(run_entrypoint "$D" "$B" FAKE_KEYS_MODE=unknown); PIDS="$p"
wait_until 15 serving "$D" && pass "server started" || fail "an unrelated exit 1 blocked the start: $(tail -3 "$D/.console")"
grep -q "pre-flight could not run" "$D/.console" && pass "warning logged" || fail "no warning"
cleanup_scenario

echo "P4: a crash (exit 2) fails open"
D="$WORK/p4"; prepare_volume "$D"
p=$(run_entrypoint "$D" "$B" FAKE_KEYS_MODE=crash); PIDS="$p"
wait_until 15 serving "$D" && pass "server started" || fail "a crash blocked the start: $(tail -3 "$D/.console")"
cleanup_scenario

if command -v timeout >/dev/null 2>&1; then
    echo "P5: a hung check is cut off and fails open"
    D="$WORK/p5"; prepare_volume "$D"
    p=$(run_entrypoint "$D" "$B" FAKE_KEYS_MODE=hang); PIDS="$p"
    wait_until 50 serving "$D" && pass "server started after the timeout" || fail "a hang blocked the start: $(tail -3 "$D/.console")"
    cleanup_scenario
else
    echo "P5: skipped, no timeout(1) on this machine"
fi

echo "P6: PB_KEYS_PREFLIGHT=0 disables it"
D="$WORK/p6"; prepare_volume "$D"
p=$(run_entrypoint "$D" "$B" FAKE_KEYS_MODE=failed PB_KEYS_PREFLIGHT=0); PIDS="$p"
wait_until 15 serving "$D" && pass "server started" || fail "the kill switch did not work"
[ -e "$D/.fake.log" ] && grep -q "keys check" "$D/.fake.log" && fail "keys check ran anyway" || pass "keys check not run"
cleanup_scenario

echo "P7: no database yet (fresh volume) skips the check"
D="$WORK/p7"; mkdir -p "$D"
p=$(run_entrypoint "$D" "$B" FAKE_KEYS_MODE=failed); PIDS="$p"
wait_until 15 serving "$D" && pass "server started" || fail "a fresh volume was blocked"
[ -e "$D/.fake.log" ] && grep -q "keys check" "$D/.fake.log" && fail "keys check ran without a database" || pass "keys check not run"
cleanup_scenario

echo "P8: the encryption env flag is passed to the check"
D="$WORK/p8"; prepare_volume "$D"
p=$(run_entrypoint "$D" "$B" PB_ENCRYPTION_KEY=0123456789abcdef0123456789abcdef); PIDS="$p"
wait_until 15 serving "$D" || fail "server never started"
grep -q -- "--encryptionEnv=PB_ENCRYPTION_KEY keys check" "$D/.fake.log" && pass "flag forwarded" || fail "flag missing: $(cat "$D/.fake.log")"
cleanup_scenario

echo "P9: a parked (already released) replica is not killed by the check"
D="$WORK/p9"; prepare_volume "$D"
echo "$B" > "$D/.pb_released"
p=$(run_entrypoint "$D" "$B" FAKE_KEYS_MODE=failed); PIDS="$p"
sleep 4
finished "$D" && fail "the parked replica exited ($(cat "$D/.rc"))" || pass "it parked instead of exiting"
grep -q "already handed /pb_data over" "$D/.console" && pass "zombie guard reached" || fail "zombie guard not reached: $(tail -3 "$D/.console")"
cleanup_scenario

echo
if [ "$FAILS" -eq 0 ]; then
    echo "PASS (work dir $WORK)"
    rm -rf "$WORK"
else
    echo "$FAILS FAILURE(S) — consoles kept in $WORK"
    exit 1
fi
