#!/usr/bin/env sh
set -e

# Default host and port (can be overridden with PB_HOST and PB_PORT environment variables)
HOST=${PB_HOST:-0.0.0.0}
PORT=${PB_PORT:-8090}

# The data dir and binary are overridable only so scripts/test_entrypoint_handover.sh
# can drive this file outside a container. Production uses the defaults.
DATA_DIR=${PB_DATA_DIR:-/pb_data}
PB_BIN=${PB_BIN:-/usr/local/bin/pocketbase}

# Default serve command arguments
DEFAULT_SERVE_ARGS="serve --http=${HOST}:${PORT} --dir=${DATA_DIR} --publicDir=/pb_public --hooksDir=/pb_hooks"

LITESTREAM_PID=""
PB_PID=""
WATCHER_PID=""

# Single-writer handover across a rollout — see acquire_single_writer() for why.
SINGLE_WRITER_LOCK=$DATA_DIR/.pb_singlewriter.lock
# incoming -> outgoing: "please let go", carrying the requester's id
HANDOVER_FILE=$DATA_DIR/.pb_handover
# outgoing -> incoming: "I have let go", carrying the id it let go FOR
HANDOVER_ACK=$DATA_DIR/.pb_handover_ack
# the id of the replica that currently owns /pb_data
OWNER_FILE=$DATA_DIR/.pb_owner
# ids of replicas that have handed over; they must never serve again
RELEASED_FILE=$DATA_DIR/.pb_released
# durable record of every handover event (#54) — console logs are not retained
EVENT_LOG=$DATA_DIR/.pb_handover.log
# how the watcher tells the main shell who asked (container-local, not shared)
REQUESTER_NOTE=/tmp/.pb_handover_requester
# How long an incoming replica waits for the outgoing one to let go. The normal
# wait is a poll interval plus a graceful drain, a few seconds; this only
# bounds the pathological case. Set PB_HANDOVER_TIMEOUT=0 to disable the whole
# mechanism.
HANDOVER_TIMEOUT=${PB_HANDOVER_TIMEOUT:-60}
HANDOVER_POLL=${PB_HANDOVER_POLL:-2}
# Identifies this REPLICA to the other one. The container hostname is the pod
# name, which is stable across a container restart inside the same replica —
# that is deliberate, see start_handover_watcher and the zombie guard.
INSTANCE_ID="${PB_INSTANCE_ID:-$(hostname 2>/dev/null || echo unknown)}"

# ---------------------------------------------------------------------------
# Durable handover events (#54)
# ---------------------------------------------------------------------------
# The Container Apps environment keeps no console logs, so the #35 alarm used
# to exist only for whoever happened to be tailing the stream during a deploy.
# Every handover event is now appended to $EVENT_LOG on the volume, and WARNs
# are also exported to the OTLP collector, where they can be alerted on.

# Rewrite a file keeping only its last N lines. Best-effort: the event log and
# the released list must never be what stops a replica from starting.
trim_file() {
    [ -f "$1" ] || return 0
    tail -n "$2" "$1" > "$1.tmp.$$" 2>/dev/null && mv -f "$1.tmp.$$" "$1" 2>/dev/null || rm -f "$1.tmp.$$" 2>/dev/null
    return 0
}

# event LEVEL MESSAGE — console, the durable log, and (WARN only) OTLP.
event() {
    echo "[entrypoint] $2"
    printf '%s %s %s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$INSTANCE_ID" "$1" "$2" >> "$EVENT_LOG" 2>/dev/null || true
    trim_file "$EVENT_LOG" 500
    if [ "$1" = "WARN" ]; then
        otlp_event "$2"
    fi
    return 0
}

# Export one WARN log record to the OTLP collector PocketBase already uses,
# over OTLP/HTTP JSON. Fire-and-forget with a short timeout: telemetry must
# never delay or block the handover.
otlp_event() {
    endpoint=${OTEL_EXPORTER_OTLP_LOGS_ENDPOINT:-}
    if [ -z "$endpoint" ] && [ -n "${OTEL_EXPORTER_OTLP_ENDPOINT:-}" ]; then
        endpoint="${OTEL_EXPORTER_OTLP_ENDPOINT%/}/v1/logs"
    fi
    [ -n "$endpoint" ] || return 0
    command -v wget >/dev/null 2>&1 || return 0

    # the message and ids are ours, but keep the JSON valid whatever they hold
    msg=$(printf '%s' "$1" | tr -d '"\\')
    now_ns="$(date +%s)000000000"

    # OTEL_RESOURCE_ATTRIBUTES is k=v,k=v — the same resource PocketBase reports
    res=""
    has_name=0
    old_ifs=$IFS
    IFS=','
    for kv in ${OTEL_RESOURCE_ATTRIBUTES:-}; do
        k=$(printf '%s' "${kv%%=*}" | tr -d '"\\ ')
        v=$(printf '%s' "${kv#*=}" | tr -d '"\\')
        [ -n "$k" ] || continue
        if [ "$k" = "service.name" ]; then has_name=1; fi
        res="${res:+$res,}{\"key\":\"$k\",\"value\":{\"stringValue\":\"$v\"}}"
    done
    IFS=$old_ifs
    if [ "$has_name" = "0" ]; then
        res="{\"key\":\"service.name\",\"value\":{\"stringValue\":\"stfoa-auth\"}}${res:+,$res}"
    fi

    body="{\"resourceLogs\":[{\"resource\":{\"attributes\":[$res]},\"scopeLogs\":[{\"scope\":{\"name\":\"entrypoint.sh\"},\"logRecords\":[{\"timeUnixNano\":\"$now_ns\",\"severityNumber\":13,\"severityText\":\"WARN\",\"body\":{\"stringValue\":\"[entrypoint] $msg\"},\"attributes\":[{\"key\":\"instance\",\"value\":{\"stringValue\":\"$INSTANCE_ID\"}},{\"key\":\"source\",\"value\":{\"stringValue\":\"single-writer-handover\"}}]}]}]}]}"

    # OTEL_EXPORTER_OTLP_HEADERS is k=v,k=v with URL-encoded values (the spec);
    # decode the few escapes an auth header can carry. Never echo these.
    set -- --header="Content-Type: application/json"
    IFS=','
    for kv in ${OTEL_EXPORTER_OTLP_HEADERS:-}; do
        k=${kv%%=*}
        v=$(printf '%s' "${kv#*=}" | sed 's/%20/ /g; s/%3D/=/g; s/%3d/=/g; s/%2C/,/g; s/%2c/,/g; s/%3A/:/g; s/%3a/:/g')
        [ -n "$k" ] && set -- "$@" --header="$k: $v"
    done
    IFS=$old_ifs

    (wget -q -T 5 -t 1 -O /dev/null "$@" --post-data="$body" "$endpoint" >/dev/null 2>&1 || true) &
    return 0
}

# ---------------------------------------------------------------------------
# Graceful shutdown
# ---------------------------------------------------------------------------
# Drain PocketBase first so in-flight HTTP requests complete and SQLite WAL is
# checkpointed, THEN signal Litestream so it can replicate the final WAL frames
# to the blob replica before exiting. The Container App must set
# terminationGracePeriodSeconds high enough (>= 60s) for this to run to
# completion.
shutdown() {
    echo "[entrypoint] received shutdown signal — stopping pocketbase + litestream..."
    if [ -n "$WATCHER_PID" ]; then
        kill -TERM "$WATCHER_PID" 2>/dev/null || true
    fi
    if [ -n "$PB_PID" ]; then
        kill -TERM "$PB_PID" 2>/dev/null || true
        wait "$PB_PID" 2>/dev/null || true
    fi
    if [ -n "$LITESTREAM_PID" ]; then
        kill -TERM "$LITESTREAM_PID" 2>/dev/null || true
        wait "$LITESTREAM_PID" 2>/dev/null || true
    fi
    echo "[entrypoint] shutdown complete."
    exit 0
}
trap shutdown TERM INT

# Sit still until Container Apps drains this replica. Never returns.
#
# Exiting instead looks like a crash, and ACA restarts the container; that
# restart then races the genuine incoming replica for /pb_data (observed
# 2026-09-06 22:49). Health probes fail while parked. Readiness failing is
# correct (traffic belongs to the incoming replica), but LIVENESS failing makes
# ACA restart the container anyway after ~60-90s — which is why a restarted
# container checks $RELEASED_FILE first and parks again (the zombie guard).
park_forever() {
    while :; do
        sleep 3600 &
        wait $! 2>/dev/null || true
    done
}

# Hand /pb_data to an incoming replica WITHOUT exiting.
handover_release() {
    requester=$(cat "$REQUESTER_NOTE" 2>/dev/null || true)
    echo "[entrypoint] handover: stopping pocketbase and releasing /pb_data..."
    if [ -n "$PB_PID" ]; then
        kill -TERM "$PB_PID" 2>/dev/null || true
        wait "$PB_PID" 2>/dev/null || true
        PB_PID=""
    fi
    if [ -n "$LITESTREAM_PID" ]; then
        kill -TERM "$LITESTREAM_PID" 2>/dev/null || true
        wait "$LITESTREAM_PID" 2>/dev/null || true
        LITESTREAM_PID=""
    fi
    # Closing fd 9 is what actually releases the single-writer lock.
    exec 9>&- 2>/dev/null || true

    # Record that this replica is done BEFORE acknowledging, so that a
    # liveness restart of this container can never serve again.
    printf '%s\n' "$INSTANCE_ID" >> "$RELEASED_FILE" 2>/dev/null || true
    trim_file "$RELEASED_FILE" 50
    tmp="$HANDOVER_ACK.$$"
    if printf '%s\n' "$requester" > "$tmp" 2>/dev/null && mv -f "$tmp" "$HANDOVER_ACK" 2>/dev/null; then :; else rm -f "$tmp" 2>/dev/null || true; fi

    event INFO "handover: /pb_data released to '$requester'. Parking until this replica is drained."
    park_forever
}
trap handover_release USR1

# ---------------------------------------------------------------------------
# Single-writer handover
# ---------------------------------------------------------------------------
# Azure Container Apps rolls a revision transition: the incoming replica is
# started and made READY before the outgoing one is drained. Two PocketBase
# processes therefore write the same NFS /pb_data for ~40s, which is what
# corrupted auxiliary.db five times (#35). `maxReplicas: 1` does not prevent it
# — that bounds replicas per revision, not across a transition.
#
# The obvious fix does NOT work and must not be reintroduced: having the
# incoming replica block until the outgoing one is drained deadlocks, because
# readiness gates the handover in both directions. ACA will not drain the
# outgoing replica until the incoming one reports ready, and the incoming one
# cannot report ready while it waits. That shipped on 2026-09-06 (b7642f75) and
# was reverted the same day.
#
# So the dependency is inverted — the incoming replica *asks*, and the outgoing
# replica leaves of its own accord rather than waiting to be drained:
#
#   1. incoming writes its id to .pb_handover
#   2. the outgoing replica's watcher reads an id that is not its own (and not
#      a stale one, see is_stale_requester), stops PocketBase, releases the
#      lock, records itself in .pb_released, writes the requester's id to
#      .pb_handover_ack, and parks
#   3. incoming holds the lock AND sees its own id in the ack, writes itself to
#      .pb_owner, and starts serving
#
# The ack, not the lock, is the proof (#54). On 2026-09-28 the lock alone
# failed both ways: flock's blocking wait on NFS re-polls with exponential
# backoff (…15s, 31s, 61s), so a release 31-60s in was never seen; and a
# replica that started without the lock left it free, so the next claimant
# would have been handed it while the previous writer was still running.
# Hence non-blocking polls every second, and an explicit ack.
#
# It FAILS OPEN. If the wait expires, the incoming replica starts anyway with a
# loud WARN. A brief overlap is recoverable; a container app that can no
# longer be deployed is not.

# The azd revision epoch embedded in a pod name
# (ca-auth--azd-1790612504-747c78d9f4-98cd2 -> 1790612504), or empty.
revision_epoch() {
    case "$1" in
        *--azd-*) ;;
        *) return 0 ;;
    esac
    n=${1##*--azd-}
    n=${n%%-*}
    case "$n" in
        ''|*[!0-9]*) return 0 ;;
    esac
    printf '%s' "$n"
}

# A request from a replica that has already handed over (a liveness restart of
# a parked container), or from an OLDER azd revision, must never be answered:
# the serving replica would hand the volume back to the one being retired. The
# revision check covers outgoing replicas running the pre-#54 entrypoint, which
# never recorded themselves in .pb_released.
is_stale_requester() {
    if [ -f "$RELEASED_FILE" ] && grep -qxF "$1" "$RELEASED_FILE" 2>/dev/null; then
        return 0
    fi
    mine=$(revision_epoch "$INSTANCE_ID")
    theirs=$(revision_epoch "$1")
    if [ -n "$mine" ] && [ -n "$theirs" ] && [ "$theirs" -lt "$mine" ]; then
        return 0
    fi
    return 1
}

request_handover() {
    tmp="$HANDOVER_FILE.$$"
    if printf '%s\n' "$INSTANCE_ID" > "$tmp" 2>/dev/null && mv -f "$tmp" "$HANDOVER_FILE" 2>/dev/null; then
        return 0
    fi
    rm -f "$tmp" 2>/dev/null || true
    event WARN "could not write $HANDOVER_FILE; the previous replica will not be asked to leave."
    return 0
}

# Poll for a handover request from a replica that is not us, and hand the main
# shell a USR1 so it releases /pb_data.
start_handover_watcher() {
    (
        ignored=""
        while :; do
            if [ -f "$HANDOVER_FILE" ]; then
                requester=$(cat "$HANDOVER_FILE" 2>/dev/null || true)
                # A request carrying our own id is this replica's container
                # restarting, not a new replica. Answering it would hand the
                # volume to ourselves and flap.
                if [ -n "$requester" ] && [ "$requester" != "$INSTANCE_ID" ]; then
                    if is_stale_requester "$requester"; then
                        if [ "$requester" != "$ignored" ]; then
                            event WARN "ignoring a handover request from '$requester', a replica that is being retired. This replica keeps /pb_data."
                            ignored=$requester
                        fi
                    else
                        event INFO "handover requested by '$requester' — releasing /pb_data."
                        printf '%s\n' "$requester" > "$REQUESTER_NOTE" 2>/dev/null || true
                        kill -USR1 "$MAIN_PID" 2>/dev/null || true
                        exit 0
                    fi
                fi
            fi
            sleep "$HANDOVER_POLL"
        done
    ) &
    WATCHER_PID=$!
}

acquire_single_writer() {
    if [ "$HANDOVER_TIMEOUT" = "0" ]; then
        echo "[entrypoint] WARN: PB_HANDOVER_TIMEOUT=0 — single-writer handover disabled."
        echo "[entrypoint]       Two replicas sharing /pb_data will corrupt SQLite."
        return 0
    fi

    # PocketBase creates --dir itself, but that happens after this runs, and an
    # image started without a mounted volume has no /pb_data at all.
    if ! mkdir -p "$DATA_DIR" 2>/dev/null; then
        echo "[entrypoint] WARN: cannot create $DATA_DIR; skipping single-writer handover."
        return 0
    fi

    # Zombie guard: this replica already handed /pb_data over and has been
    # restarted by its liveness probe while parked. It must never serve again.
    if [ -f "$RELEASED_FILE" ] && grep -qxF "$INSTANCE_ID" "$RELEASED_FILE" 2>/dev/null; then
        event INFO "this replica already handed /pb_data over; parking instead of serving (container restarted while parked)."
        park_forever
    fi

    if ! command -v flock >/dev/null 2>&1; then
        request_handover
        event WARN "flock not found — cannot confirm the previous replica let go."
        return 0
    fi

    # No lock file yet means no replica has ever run the handover on this
    # volume, so there is no previous owner to wait for (a fresh volume, or a
    # container started without one).
    fresh_volume=0
    [ -e "$SINGLE_WRITER_LOCK" ] || fresh_volume=1

    if ! touch "$SINGLE_WRITER_LOCK" 2>/dev/null; then
        request_handover
        event WARN "cannot create $SINGLE_WRITER_LOCK; skipping the lock wait."
        return 0
    fi

    # fd 9 stays open for the life of the shell, which is what holds the lock.
    # It is inherited by pocketbase and litestream, so the lock outlives even a
    # SIGKILL that skips the shutdown trap, and the kernel releases it once the
    # whole process tree is gone.
    exec 9>>"$SINGLE_WRITER_LOCK"

    owner=$(cat "$OWNER_FILE" 2>/dev/null || true)
    if [ "$fresh_volume" = "1" ]; then
        need_ack=0
        echo "[entrypoint] first start on this volume; taking the lock."
    elif [ "$owner" = "$INSTANCE_ID" ]; then
        # This replica's own container restarting: the previous process is
        # gone with the container, so only the lock is needed, not an ack.
        need_ack=0
        echo "[entrypoint] restarting as the current owner; waiting for the lock (up to ${HANDOVER_TIMEOUT}s)..."
    else
        need_ack=1
        rm -f "$HANDOVER_ACK" 2>/dev/null || true
        request_handover
        echo "[entrypoint] waiting for the previous replica (${owner:-unknown}) to release /pb_data (up to ${HANDOVER_TIMEOUT}s)..."
    fi

    locked=0
    acked=0
    waited=0
    while [ "$waited" -lt "$HANDOVER_TIMEOUT" ]; do
        if [ "$locked" = "0" ] && flock -n -x 9; then
            locked=1
        fi
        if [ "$locked" = "1" ]; then
            if [ "$need_ack" = "0" ]; then
                break
            fi
            if [ "$(cat "$HANDOVER_ACK" 2>/dev/null || true)" = "$INSTANCE_ID" ]; then
                acked=1
                break
            fi
        fi
        sleep 1
        waited=$((waited + 1))
    done

    if [ "$locked" = "1" ] && { [ "$need_ack" = "0" ] || [ "$acked" = "1" ]; }; then
        event INFO "/pb_data is ours after ${waited}s — single writer confirmed."
    elif [ "$locked" = "1" ]; then
        # Expected once, on the deploy that introduces the ack: the outgoing
        # replica runs the pre-#54 entrypoint, which releases but never acks.
        # Also after the previous replica's pod died outright.
        event WARN "lock held, but the previous replica (${owner:-unknown}) never acknowledged within ${HANDOVER_TIMEOUT}s. Starting — if it was still running, two writers overlapped. See DEPLOY.md and #54."
    else
        event WARN "no handover after ${HANDOVER_TIMEOUT}s — the lock is still held. Starting anyway WITHOUT the lock: two writers are sharing /pb_data and auxiliary.db may corrupt. See DEPLOY.md, #35 and #54."
    fi

    tmp="$OWNER_FILE.$$"
    if printf '%s\n' "$INSTANCE_ID" > "$tmp" 2>/dev/null && mv -f "$tmp" "$OWNER_FILE" 2>/dev/null; then :; else rm -f "$tmp" 2>/dev/null || true; fi
    # Clear our own request, and only ours — never a newer replica's.
    if [ "$(cat "$HANDOVER_FILE" 2>/dev/null || true)" = "$INSTANCE_ID" ]; then
        rm -f "$HANDOVER_FILE" 2>/dev/null || true
    fi
    rm -f "$HANDOVER_ACK" 2>/dev/null || true
}

litestream_restore() {
    if [ -n "$LITESTREAM_REPLICA_URL" ] && [ ! -f "$DATA_DIR/data.db" ]; then
        echo "[entrypoint] no database found — attempting Litestream restore..."
        litestream restore -if-replica-exists -config /etc/litestream.yml "$DATA_DIR/data.db" || true
        litestream restore -if-replica-exists -config /etc/litestream.yml "$DATA_DIR/auxiliary.db" || true
    fi
}

litestream_replicate() {
    if [ -n "$LITESTREAM_REPLICA_URL" ]; then
        echo "[entrypoint] starting Litestream replication..."
        litestream replicate -config /etc/litestream.yml &
        LITESTREAM_PID=$!
    fi
}

# Bootstrap superuser on first boot. Uses `create` so an admin-set password is
# preserved across restarts (subsequent boots see "already exists" and no-op).
# Skips on empty/invalid email so a misconfigured secret can't spam SQLite opens
# on every restart.
create_superuser() {
    if [ -z "$PB_ADMIN_EMAIL" ] || [ -z "$PB_ADMIN_PASSWORD" ]; then
        return 0
    fi
    case "$PB_ADMIN_EMAIL" in
        *@*.*) ;;
        *)
            echo "[entrypoint] WARN: PB_ADMIN_EMAIL ('$PB_ADMIN_EMAIL') is not a valid email; skipping superuser bootstrap."
            echo "[entrypoint]       Fix with: azd env set PB_ADMIN_EMAIL <admin@example.com> && azd up"
            return 0
            ;;
    esac
    out=$("$PB_BIN" superuser create "$PB_ADMIN_EMAIL" "$PB_ADMIN_PASSWORD" --dir="$DATA_DIR" 2>&1) || true
    case "$out" in
        ""|*"already exists"*|*"UNIQUE constraint"*|*"Value must be unique"*|*"Successfully"*) ;;
        *) echo "[entrypoint] superuser create: $out" ;;
    esac
}

run_serve() {
    # The main shell's pid, for the watcher subshell to signal.
    MAIN_PID=$$

    # Before anything opens data.db or auxiliary.db — ahead of the Litestream
    # restore and the superuser bootstrap below.
    acquire_single_writer
    start_handover_watcher
    litestream_restore
    litestream_replicate
    # Brief pause so Litestream can read the restored db, match it against the
    # replica, and adopt the existing generation BEFORE PocketBase opens
    # data.db. Without this, the first SQLite write from `superuser create` or
    # `serve` startup can race the initial Litestream sync and look like the
    # start of a new generation in the replica.
    if [ -n "$LITESTREAM_PID" ]; then
        sleep 2
    fi
    create_superuser
    # shellcheck disable=SC2086
    "$PB_BIN" $DEFAULT_SERVE_ARGS "$@" &
    PB_PID=$!
    # `wait` is interruptible — when SIGTERM arrives the shell jumps to the
    # `shutdown` trap, which kills both children in order and exits.
    set +e
    wait "$PB_PID"
    rc=$?
    set -e
    if [ -n "$WATCHER_PID" ]; then
        kill -TERM "$WATCHER_PID" 2>/dev/null || true
    fi
    if [ -n "$LITESTREAM_PID" ]; then
        kill -TERM "$LITESTREAM_PID" 2>/dev/null || true
        wait "$LITESTREAM_PID" 2>/dev/null || true
    fi
    exit "$rc"
}

# Default invocation (no args): supervised serve with Litestream restore + replicate.
if [ $# -eq 0 ]; then
    run_serve
fi

# Global flags pass through directly to pocketbase.
case "$1" in
    --help|-h|--version|-v)
        exec "$PB_BIN" "$@"
        ;;
esac

# First arg starts with '-' = serve flags (run supervised).
if [ "${1#-}" != "$1" ]; then
    run_serve "$@"
fi

# Otherwise: subcommand passthrough (migrate, superuser, …) — no Litestream
# supervision, no graceful trap, because these are short-lived admin commands.
exec "$PB_BIN" "$@"
