#!/bin/sh
# Checks entrypoint.sh's opt-in settings encryption: the global
# --encryptionEnv=PB_ENCRYPTION_KEY flag reaches claviger if and only if
# PB_ENCRYPTION_KEY is set and non-empty, on serve, on the superuser bootstrap
# and on subcommand passthrough, and a key that is not 32 characters stops the
# start before anything runs. The key itself must never appear in output or args.
#
# Needs only a POSIX sh. Usage: scripts/test_entrypoint_encryption.sh

set -u
ROOT=$(cd "$(dirname "$0")/.." && pwd)
SH=${TEST_SH:-dash}
command -v "$SH" >/dev/null 2>&1 || SH="sh"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/pb-encryption.XXXXXX")
FAILS=0
KEY=abcdefghijklmnopqrstuvwxyz012345 # 32 characters, test value only

FAKE_PB="$WORK/fake-claviger"
cat > "$FAKE_PB" <<'FAKE'
#!/bin/sh
# fake claviger: record argv, one invocation per line, and exit
echo "$*" >> "$PB_ARGS_LOG"
exit 0
FAKE
chmod +x "$FAKE_PB"

fail() { echo "  FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "  ok:   $*"; }

# run_entry LABEL [VAR=VAL ...] -- ARGS... ; leaves $WORK/args.LABEL and $WORK/out.LABEL
run_entry() {
    label=$1
    shift
    vars=""
    while [ "$1" != "--" ]; do vars="$vars $1"; shift; done
    shift
    : > "$WORK/args.$label"
    # shellcheck disable=SC2086 # vars are simple VAR=VAL words without spaces
    env -u PB_ENCRYPTION_KEY PB_DATA_DIR="$WORK/data.$label" PB_BIN="$FAKE_PB" \
        PB_ARGS_LOG="$WORK/args.$label" PB_INSTANCE_ID=enc-test PB_HANDOVER_TIMEOUT=0 \
        OTEL_EXPORTER_OTLP_ENDPOINT= PB_ADMIN_EMAIL= $vars \
        "$SH" "$ROOT/entrypoint.sh" "$@" > "$WORK/out.$label" 2>&1
    echo $? > "$WORK/rc.$label"
}

has_flag() { grep -q -- '--encryptionEnv=PB_ENCRYPTION_KEY' "$WORK/args.$1"; }
all_lines_have_flag() { [ -s "$WORK/args.$1" ] && ! grep -qv -- '--encryptionEnv=PB_ENCRYPTION_KEY' "$WORK/args.$1"; }
no_flag() { [ -s "$WORK/args.$1" ] && ! grep -q -- 'encryptionEnv' "$WORK/args.$1"; }
no_key_leak() { ! grep -rqF "$KEY" "$WORK"/args.* "$WORK"/out.* 2>/dev/null; }

echo "E1: key unset -> no flag on serve, subcommand or superuser bootstrap"
run_entry unset --  ; # no args = supervised serve
run_entry unset_sub -- migrate up
run_entry unset_su PB_ADMIN_EMAIL=a@b.co PB_ADMIN_PASSWORD=x -- 
if no_flag unset; then pass "serve has no flag"; else fail "serve args: $(cat "$WORK/args.unset")"; fi
if no_flag unset_sub; then pass "migrate has no flag"; else fail "migrate args: $(cat "$WORK/args.unset_sub")"; fi
if no_flag unset_su; then pass "superuser create + serve have no flag"; else fail "args: $(cat "$WORK/args.unset_su")"; fi

echo "E2: key set but empty -> no flag"
run_entry empty PB_ENCRYPTION_KEY= --
if no_flag empty; then pass "empty key is treated as unset"; else fail "args: $(cat "$WORK/args.empty")"; fi

echo "E3: 32-character key -> flag on every command, key never echoed"
run_entry set PB_ENCRYPTION_KEY=$KEY --
run_entry set_flags PB_ENCRYPTION_KEY=$KEY -- --dev
run_entry set_sub PB_ENCRYPTION_KEY=$KEY -- migrate up
run_entry set_su PB_ENCRYPTION_KEY=$KEY PB_ADMIN_EMAIL=a@b.co PB_ADMIN_PASSWORD=x --
if all_lines_have_flag set; then pass "serve has the flag"; else fail "serve args: $(cat "$WORK/args.set")"; fi
if all_lines_have_flag set_flags; then pass "serve with extra flags has the flag"; else fail "args: $(cat "$WORK/args.set_flags")"; fi
if all_lines_have_flag set_sub && grep -q '^--encryptionEnv=PB_ENCRYPTION_KEY migrate up$' "$WORK/args.set_sub"; then pass "migrate passthrough has the flag, args intact"; else fail "args: $(cat "$WORK/args.set_sub")"; fi
if all_lines_have_flag set_su && [ "$(wc -l < "$WORK/args.set_su")" -eq 2 ]; then pass "superuser create and serve both have the flag"; else fail "args: $(cat "$WORK/args.set_su")"; fi
if no_key_leak; then pass "key value not in args or output"; else fail "key value leaked"; fi

echo "E4: wrong-length key -> refuses to start, nothing runs, key not echoed"
for k in short abcdefghijklmnopqrstuvwxyz0123456; do
    run_entry bad PB_ENCRYPTION_KEY=$k --
    run_entry bad_sub PB_ENCRYPTION_KEY=$k -- migrate up
    if [ "$(cat "$WORK/rc.bad")" -ne 0 ] && [ ! -s "$WORK/args.bad" ] && grep -q "must be exactly 32" "$WORK/out.bad"; then pass "serve refused (${#k} chars)"; else fail "serve rc=$(cat "$WORK/rc.bad") out: $(cat "$WORK/out.bad")"; fi
    if [ "$(cat "$WORK/rc.bad_sub")" -ne 0 ] && [ ! -s "$WORK/args.bad_sub" ]; then pass "subcommand refused (${#k} chars)"; else fail "subcommand rc=$(cat "$WORK/rc.bad_sub")"; fi
    if grep -qF "$k" "$WORK/out.bad" "$WORK/out.bad_sub"; then fail "bad key echoed"; else pass "bad key not echoed"; fi
done

echo "E5: --help is not blocked by a bad key"
run_entry help PB_ENCRYPTION_KEY=short -- --help
if [ "$(cat "$WORK/rc.help")" -eq 0 ] && grep -q -- '--help' "$WORK/args.help"; then pass "help passes through"; else fail "help rc=$(cat "$WORK/rc.help")"; fi

echo
if [ "$FAILS" -eq 0 ]; then
    echo "PASS"
    rm -rf "$WORK"
else
    echo "$FAILS FAILURE(S) — kept in $WORK"
    exit 1
fi
