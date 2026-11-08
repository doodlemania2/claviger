#!/bin/sh
# Runs the regular test suites of the repository against PostgreSQL (issue #106, M4).
#
#   CLAVIGER_TEST_PG_URL=postgres://user:pass@host:5432/postgres?sslmode=disable \
#     scripts/test_postgres_suites.sh [go test args...]
#
# The fixture (tests/data) is converted to a Postgres template database once per
# package (see tests/app_postgres.go). Tests listed in tests/postgres_skip.txt are
# skipped, each with its reason printed. Any other skipped test fails the run, and
# so does a run that passes too few tests (a green that ran nothing is a failure).
set -eu

cd "$(dirname "$0")/.."

: "${CLAVIGER_TEST_PG_URL:?set CLAVIGER_TEST_PG_URL to a Postgres 16+ server on which the role may CREATE DATABASE}"
export CLAVIGER_TEST_DB=postgres

MIN_PASS="${PG_SUITES_MIN_PASS:-4000}"
PACKAGES="${PG_SUITES_PACKAGES:-./tests ./core ./apis ./oidc ./cmd ./forms ./mails ./plugins/jsvm ./plugins/migratecmd}"
SKIP_FILE=tests/postgres_skip.txt

skip_regex="$(grep -v '^[[:space:]]*#' "$SKIP_FILE" | awk -F'\t' 'NF>=2 {printf "%s%s", sep, $1; sep="|"}')"
skip_regex="^($skip_regex)\$"

echo "skipped on Postgres (known gaps, $SKIP_FILE):"
grep -v '^[[:space:]]*#' "$SKIP_FILE" | awk -F'\t' 'NF>=2 {printf "  %s: %s\n", $1, $2}'

log="$(mktemp)"
trap 'rm -f "$log"' EXIT

# shellcheck disable=SC2086
go test -count=1 -v -p 2 -skip "$skip_regex" "$@" $PACKAGES 2>&1 | tee "$log" | grep -E '^(--- FAIL|FAIL|ok|panic:)' || true

if grep -q -- '^--- FAIL\|^FAIL\|^panic:' "$log"; then
  echo "::error::Postgres suites failed"
  grep -A6 -- '--- FAIL\|^panic:' "$log" | head -200
  exit 1
fi

if grep -- '--- SKIP' "$log" | grep -q .; then
  echo "::error::a test was skipped (only the tests in $SKIP_FILE may be skipped)"
  grep -- '--- SKIP' "$log"
  exit 1
fi

# the harness really ran on Postgres
grep -q -- '--- PASS: TestPostgresHarness' "$log" || { echo "::error::tests ran on SQLite, not Postgres"; exit 1; }

passed="$(grep -c -- '--- PASS' "$log")"
echo "passed: $passed (minimum $MIN_PASS)"
test "$passed" -ge "$MIN_PASS"
