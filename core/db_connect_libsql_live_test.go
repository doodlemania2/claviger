//go:build libsql && !no_default_driver

package core

// The live libSQL tests run only with -tags libsql, against a sqld: the
// Postgres CI job fails on any skip, so these are not compiled there.

import (
	"errors"
	"os"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dialect"
)

// the live tests need a sqld (docs/datastores.md "libSQL"):
//
//	CLAVIGER_TEST_LIBSQL_URL=http://127.0.0.1:8080
//
// and, for the authentication ones, a second sqld started with a JWT key:
//
//	CLAVIGER_TEST_LIBSQL_AUTH_URL=http://127.0.0.1:8082
//	CLAVIGER_TEST_LIBSQL_AUTH_TOKEN=<a JWT signed by the matching private key>
func TestLibSQLLive(t *testing.T) {
	raw := os.Getenv("CLAVIGER_TEST_LIBSQL_URL")
	if raw == "" {
		t.Fatal("the libsql build tag needs CLAVIGER_TEST_LIBSQL_URL")
	}

	db, err := LibSQLDBConnect(raw)("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if got := dialect.Of(db).Name(); got != dialect.LibSQLName {
		t.Fatalf("dialect = %q, want %q", got, dialect.LibSQLName)
	}

	table := "claviger_live_" + strings.ToLower(t.Name())
	_, _ = db.NewQuery("DROP TABLE IF EXISTS " + table).Execute()
	if _, err := db.NewQuery("CREATE TABLE " + table + " (id TEXT PRIMARY KEY, n INTEGER)").Execute(); err != nil {
		t.Fatal(err)
	}
	defer db.NewQuery("DROP TABLE IF EXISTS " + table).Execute() //nolint:errcheck

	count := func() int {
		var n int
		if err := db.NewQuery("SELECT count(*) FROM " + table).Row(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// commit
	if err := db.Transactional(func(tx *dbx.Tx) error {
		_, err := tx.NewQuery("INSERT INTO " + table + " VALUES ('a', 1)").Execute()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatal("the committed row is missing")
	}

	// rollback
	_ = db.Transactional(func(tx *dbx.Tx) error {
		if _, err := tx.NewQuery("INSERT INTO " + table + " VALUES ('b', 2)").Execute(); err != nil {
			return err
		}
		return errors.New("rollback")
	})
	if count() != 1 {
		t.Fatal("the rolled back row is still there")
	}

	// unique violation text (the app matches on it)
	_, err = db.NewQuery("INSERT INTO " + table + " VALUES ('a', 3)").Execute()
	if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("unexpected unique violation error: %v", err)
	}

	// introspection
	d := dialect.Of(db)
	if !d.HasTable(db, table) {
		t.Fatal("HasTable")
	}
	cols, err := d.TableColumns(db, table)
	if err != nil || len(cols) != 2 {
		t.Fatalf("TableColumns: %v %v", cols, err)
	}

	// the statements that sqld refuses are no-ops
	for name, fn := range map[string]func(dbx.Builder) error{"vacuum": d.Vacuum, "optimize": d.Optimize, "checkpoint": d.Checkpoint} {
		if err := fn(db); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestLibSQLLiveAuth(t *testing.T) {
	raw, token := os.Getenv("CLAVIGER_TEST_LIBSQL_AUTH_URL"), os.Getenv("CLAVIGER_TEST_LIBSQL_AUTH_TOKEN")
	if raw == "" || token == "" {
		t.Fatal("the libsql build tag needs CLAVIGER_TEST_LIBSQL_AUTH_URL and CLAVIGER_TEST_LIBSQL_AUTH_TOKEN")
	}

	// without a token the server refuses, and says nothing about a token value
	t.Setenv(DBAuthTokenEnv, "")
	if _, err := LibSQLDBConnect(raw)(""); err == nil {
		t.Fatal("expected the server to refuse a connection without a token")
	}

	// a wrong token is refused and not echoed
	const wrong = "eyJ.wrong.token-value-0123456789"
	t.Setenv(DBAuthTokenEnv, wrong)
	if _, err := LibSQLDBConnect(raw)(""); err == nil {
		t.Fatal("expected the server to refuse a wrong token")
	} else if strings.Contains(err.Error(), wrong) {
		t.Fatalf("the error leaks the token: %v", err)
	}

	// the token from the environment
	t.Setenv(DBAuthTokenEnv, token)
	db, err := LibSQLDBConnect(raw)("")
	if err != nil {
		t.Fatalf("env token: %v", err)
	}
	db.Close()

	// the token in the URL
	t.Setenv(DBAuthTokenEnv, "")
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	db, err = LibSQLDBConnect(raw + sep + "authToken=" + token)("")
	if err != nil {
		t.Fatalf("url token: %v", err)
	}
	db.Close()
}
