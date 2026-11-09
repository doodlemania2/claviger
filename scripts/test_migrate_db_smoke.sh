#!/bin/sh
# End to end smoke test of `claviger migrate-db` with the real binary (#106, M5).
#
#   CLAVIGER_TEST_PG_URL=postgres://user:pass@host:5432/dbname?sslmode=disable \
#     scripts/test_migrate_db_smoke.sh [path/to/claviger]
#
# 1. makes a SQLite pb_data with a superuser, a collection and two records,
# 2. migrates it into a fresh database created on the server,
# 3. starts the binary on CLAVIGER_DB_URL and reads the same records back through
#    the API, signing in with the superuser that travelled.
#
# The role needs CREATEDB. Needs curl and go. No jq: the answers are checked with grep.
set -eu

: "${CLAVIGER_TEST_PG_URL:?set CLAVIGER_TEST_PG_URL to a Postgres 16+ server on which the role may CREATE DATABASE}"

cd "$(dirname "$0")/.."
ROOT=$(pwd)

WORK=$(mktemp -d "${TMPDIR:-/tmp}/claviger-migrate-smoke.XXXXXX")
PID=""
DBNAME="smoke_$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')"
PORT="${SMOKE_PORT:-18099}"

admin_db() {
  (cd "$ROOT" && go run ./scripts/pgdb "$1" "$DBNAME")
}

cleanup() {
  [ -n "$PID" ] && kill "$PID" 2>/dev/null || true
  admin_db drop >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

BIN="${1:-}"
if [ -z "$BIN" ]; then
  BIN="$WORK/claviger"
  (cd "$ROOT" && CGO_ENABLED=0 go build -o "$BIN" ./examples/base)
fi

# the URL of the new database: same server, other database name
TARGET=$(printf '%s' "$CLAVIGER_TEST_PG_URL" | sed -E "s#(://[^/]+)/[^?]*#\\1/$DBNAME#")
admin_db create

wait_healthy() {
  i=0
  while [ "$i" -lt 60 ]; do
    if curl -fs "http://127.0.0.1:$PORT/api/health" >/dev/null 2>&1; then return 0; fi
    i=$((i + 1))
    sleep 1
  done
  { grep -v "SELECT" "$WORK"/serve*.log 2>/dev/null | head -20; fail "the server did not become healthy"; }
}

stop_server() {
  kill "$PID"
  wait "$PID" 2>/dev/null || true
  PID=""
}

auth_token() {
  curl -fs -X POST "http://127.0.0.1:$PORT/api/collections/_superusers/auth-with-password" \
    -H 'content-type: application/json' \
    -d '{"identity":"smoke@example.com","password":"smoke-pass-12345"}' |
    sed -n 's/.*"token":"\([^"]*\)".*/\1/p'
}

DIR="$WORK/pb_data"

# settings encryption, as in a real deployment (it also keeps the binary out of
# dev mode, which it enters when it runs from the temp directory)
PB_ENCRYPTION_KEY="smoke0123456789abcdefghijklmno12"
export PB_ENCRYPTION_KEY
ENC="--encryptionEnv=PB_ENCRYPTION_KEY"

echo "== 1. a SQLite pb_data"
"$BIN" superuser upsert smoke@example.com smoke-pass-12345 --dir="$DIR" $ENC >/dev/null
"$BIN" serve --dir="$DIR" $ENC --http="127.0.0.1:$PORT" >"$WORK/serve1.log" 2>&1 &
PID=$!
wait_healthy
TOKEN=$(auth_token)
[ -n "$TOKEN" ] || fail "no superuser token from the SQLite server"
curl -fs -X POST "http://127.0.0.1:$PORT/api/collections" -H "authorization: $TOKEN" -H 'content-type: application/json' \
  -d '{"name":"notes","type":"base","fields":[{"name":"title","type":"text"},{"name":"done","type":"bool"},{"name":"meta","type":"json"}]}' >/dev/null
for t in first second; do
  curl -fs -X POST "http://127.0.0.1:$PORT/api/collections/notes/records" -H "authorization: $TOKEN" -H 'content-type: application/json' \
    -d "{\"title\":\"$t note\",\"done\":true,\"meta\":{\"k\":[1,2]}}" >/dev/null
done
stop_server

echo "== 2. the server is gone; migrate-db"
"$BIN" migrate-db --dir="$DIR" $ENC --to="$TARGET" --dry-run | grep -q 'would copy' || fail "dry run"
"$BIN" migrate-db --dir="$DIR" $ENC --to="$TARGET" --verbose | tee "$WORK/migrate.log"
grep -q 'verified' "$WORK/migrate.log" || fail "migrate-db did not verify"
if "$BIN" migrate-db --dir="$DIR" $ENC --to="$TARGET" >/dev/null 2>&1; then
  fail "a second run into the same target must be refused"
fi

echo "== 3. the same data through the API, on Postgres"
CLAVIGER_DB_URL="$TARGET" "$BIN" serve --dir="$DIR" $ENC --http="127.0.0.1:$PORT" >"$WORK/serve2.log" 2>&1 &
PID=$!
wait_healthy
TOKEN=$(auth_token)
[ -n "$TOKEN" ] || fail "the superuser did not travel (no token)"
BODY=$(curl -fs "http://127.0.0.1:$PORT/api/collections/notes/records?sort=@rowid" -H "authorization: $TOKEN")
echo "$BODY" | grep -q '"totalItems":2' || fail "expected 2 records, got: $BODY"
echo "$BODY" | grep -q 'first note' || fail "missing the first record: $BODY"
echo "$BODY" | grep -q 'second note' || fail "missing the second record: $BODY"
curl -fs "http://127.0.0.1:$PORT/api/health" -H "authorization: $TOKEN" | grep -q postgres || fail "the server is not on Postgres"
stop_server

echo "migrate-db smoke test: OK"
