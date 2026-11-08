// Package pgtests holds the PostgreSQL integration tests (issue #106).
//
// The tests are behind the "postgres" build tag and need a PostgreSQL 16+
// server, passed as CLAVIGER_TEST_PG_URL (any database on it; every test
// creates and drops its own). Locally:
//
//	cd scripts/pgembedded && go run .
//
// CI runs them against a postgres service container (the "postgres" job in
// .github/workflows/test.yml).
package pgtests
