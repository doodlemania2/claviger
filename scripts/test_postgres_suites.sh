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
retry_log="$(mktemp)"
trap 'rm -f "$log" "$retry_log"' EXIT

show_failures() {
  grep -A6 -- '--- FAIL\|^panic:' "$1" | head -200
  # parallel subtests print their messages away from their "--- FAIL" line
  echo "failure messages:"
  grep -E '^[[:space:]]+[A-Za-z0-9_]+_test\.go:[0-9]+:' "$1" | head -100
}

# shellcheck disable=SC2086
go test -count=1 -v -p 2 -timeout 60m -skip "$skip_regex" "$@" $PACKAGES 2>&1 | tee "$log" | grep -E '^(--- FAIL|FAIL|ok|panic:)' || true

if grep -q -- '^--- FAIL\|^FAIL\|^panic:' "$log"; then
  # A panic or timeout is never retried.
  if grep -q -- '^panic:' "$log"; then
    echo "::error::Postgres suites panicked"
    show_failures "$log"
    exit 1
  fi

  # Some upstream tests are timing-sensitive (async routines, realtime event
  # counts) and Postgres is slower than SQLite. Rerun the failed top-level tests
  # once: a test that passes on the retry is reported as flaky, not a failure.
  failed="$(grep -E '^--- FAIL: ' "$log" | awk '{print $3}' | sort -u | paste -sd'|' -)"
  echo "::warning::retrying failed Postgres tests once: $failed"
  show_failures "$log"
  # shellcheck disable=SC2086
  go test -count=1 -v -p 2 -timeout 30m -skip "$skip_regex" -run "^($failed)\$" "$@" $PACKAGES >"$retry_log" 2>&1 || true
  if grep -q -- '^--- FAIL\|^panic:' "$retry_log" || ! grep -q -- '^--- PASS' "$retry_log"; then
    echo "::error::Postgres suites failed (also on retry)"
    show_failures "$retry_log"
    exit 1
  fi
  echo "::warning::flaky on Postgres (failed once, passed on retry): $failed"
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
