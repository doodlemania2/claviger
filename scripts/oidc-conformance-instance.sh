#!/bin/sh
# Starts a throw-away Claviger instance prepared for the OpenID Foundation
# conformance suite (Basic OP plan) and prints the suite's test plan config.
# See docs/oidc-conformance.md.
#
#   scripts/oidc-conformance-instance.sh            start, print the config, keep running until Ctrl-C
#   scripts/oidc-conformance-instance.sh --check    start, verify the bootstrap, print the config, stop
#
# Environment:
#   ISSUER_BASE  public base URL of the instance (the application URL); default http://127.0.0.1:$PORT.
#                The suite needs an https URL that its JVM trusts, see the runbook.
#   SUITE_BASE   base URL of the conformance suite; default https://localhost.emobix.co.uk:8443
#   ALIAS        test plan alias, part of the redirect URIs; default claviger
#   PORT         local port; default 8090
#   WORKDIR      scratch directory; default a new mktemp directory
set -eu

MODE=run
case "${1:-}" in
  --check) MODE=check ;;
  "") ;;
  *) echo "usage: $0 [--check]" >&2; exit 2 ;;
esac

PORT="${PORT:-8090}"
ISSUER_BASE="${ISSUER_BASE:-http://127.0.0.1:$PORT}"
SUITE_BASE="${SUITE_BASE:-https://localhost.emobix.co.uk:8443}"
ALIAS="${ALIAS:-claviger}"
WORKDIR="${WORKDIR:-$(mktemp -d)}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

ADMIN_EMAIL="conformance-admin@example.com"
ADMIN_PASS="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
USER_EMAIL="conformance-user@example.com"
USER_PASS="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
export CONFORMANCE_KEY
CONFORMANCE_KEY="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' | cut -c1-32)"

BIN="$WORKDIR/claviger"
DATA="$WORKDIR/pb_data"
LOG="$WORKDIR/claviger.log"
PID=""

cleanup() {
  if [ -n "$PID" ]; then
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

echo "== building ($WORKDIR)" >&2
(cd "$ROOT" && go build -o "$BIN" ./examples/base)

echo "== creating the superuser" >&2
"$BIN" superuser upsert "$ADMIN_EMAIL" "$ADMIN_PASS" --dir "$DATA" --encryptionEnv CONFORMANCE_KEY >/dev/null

echo "== starting on 127.0.0.1:$PORT" >&2
"$BIN" serve --http "127.0.0.1:$PORT" --dir "$DATA" --encryptionEnv CONFORMANCE_KEY >"$LOG" 2>&1 &
PID=$!

LOCAL="http://127.0.0.1:$PORT"
i=0
until curl -fsS "$LOCAL/api/health" >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -gt 60 ]; then
    echo "the instance did not come up, see $LOG" >&2
    exit 1
  fi
  sleep 1
done

field() { # field NAME: the string value of "NAME" in a compact JSON document on stdin
  sed -n "s/.*\"$1\":\"\\([^\"]*\\)\".*/\\1/p" | head -n 1
}

TOKEN="$(curl -fsS -X POST "$LOCAL/api/collections/_superusers/auth-with-password" \
  -H 'Content-Type: application/json' \
  -d "{\"identity\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}" | field token)"
[ -n "$TOKEN" ] || { echo "superuser sign-in failed" >&2; exit 1; }

api() { # api METHOD PATH BODY
  curl -fsS -X "$1" "$LOCAL$2" -H "Authorization: $TOKEN" -H 'Content-Type: application/json' -d "$3"
}

echo "== application URL $ISSUER_BASE" >&2
api PATCH /api/settings "{\"meta\":{\"appUrl\":\"$ISSUER_BASE\"}}" >/dev/null

echo "== test user $USER_EMAIL" >&2
api POST /api/collections/users/records \
  "{\"email\":\"$USER_EMAIL\",\"password\":\"$USER_PASS\",\"passwordConfirm\":\"$USER_PASS\",\"verified\":true,\"name\":\"Conformance User\"}" >/dev/null

# the suite's redirect URI, and the variant with query parameters that some tests send
CALLBACK="$SUITE_BASE/test/a/$ALIAS/callback"
REDIRECTS="[\"$CALLBACK\",\"$CALLBACK?dummy1=lorem&dummy2=ipsum\"]"

register() { # register NAME: first party, confidential
  api POST /api/oidc/clients \
    "{\"name\":\"$1\",\"authCollection\":\"users\",\"redirectUris\":$REDIRECTS,\"postLogoutUris\":[\"$SUITE_BASE/test/a/$ALIAS/post_logout_redirect\"],\"firstParty\":true}"
}

echo "== registering the two clients the plan needs" >&2
C1="$(register "conformance client")"
C2="$(register "conformance client 2")"
ID1="$(printf '%s' "$C1" | field clientId)"
SECRET1="$(printf '%s' "$C1" | field clientSecret)"
ID2="$(printf '%s' "$C2" | field clientId)"
SECRET2="$(printf '%s' "$C2" | field clientSecret)"
[ -n "$ID1" ] && [ -n "$SECRET1" ] && [ -n "$ID2" ] && [ -n "$SECRET2" ] || { echo "client registration failed" >&2; exit 1; }

echo "== checking discovery" >&2
DISCOVERY="$(curl -fsS "$LOCAL/oidc/.well-known/openid-configuration")"
for want in '"response_types_supported":["code"]' '"jwks_uri"' '"userinfo_endpoint"'; do
  case "$DISCOVERY" in
    *"$want"*) ;;
    *) echo "discovery lacks $want" >&2; exit 1 ;;
  esac
done
case "$DISCOVERY" in
  *"\"issuer\":\"$ISSUER_BASE/oidc\""*) ;;
  *) echo "the issuer is not $ISSUER_BASE/oidc" >&2; exit 1 ;;
esac

echo "== checking an authorize request and the key set (served by the real binary)" >&2
LOC="$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' -G "$LOCAL/oidc/authorize" \
  --data-urlencode "client_id=$ID1" --data-urlencode "redirect_uri=$CALLBACK" \
  --data-urlencode response_type=code --data-urlencode "scope=openid" --data-urlencode state=conformance \
  --data-urlencode code_challenge=E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM --data-urlencode code_challenge_method=S256)"
case "$LOC" in
  "302 "*"/oidc/login?authRequestID="*) ;;
  *) echo "authorize did not redirect to the sign-in page: $LOC" >&2; exit 1 ;;
esac
curl -fsS "$LOCAL/oidc/keys" | grep -q '"keys"' || { echo "the JWKS is empty" >&2; exit 1; }

CONFIG="$WORKDIR/suite-config.json"
cat >"$CONFIG" <<JSON
{
  "alias": "$ALIAS",
  "description": "Claviger Basic OP",
  "server": {
    "discoveryUrl": "$ISSUER_BASE/oidc/.well-known/openid-configuration"
  },
  "client": {
    "client_id": "$ID1",
    "client_secret": "$SECRET1"
  },
  "client2": {
    "client_id": "$ID2",
    "client_secret": "$SECRET2"
  },
  "login": {
    "email": "$USER_EMAIL",
    "password": "$USER_PASS"
  }
}
JSON

echo "== ready" >&2
echo "suite config: $CONFIG" >&2
echo "issuer:       $ISSUER_BASE/oidc" >&2
cat "$CONFIG"

if [ "$MODE" = check ]; then
  exit 0
fi

echo "== running, Ctrl-C stops it (log: $LOG)" >&2
wait "$PID"
