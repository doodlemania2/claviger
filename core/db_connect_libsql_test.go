//go:build !no_default_driver

package core

import (
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dialect"
)

func TestIsLibSQLURL(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]bool{
		"libsql://db.example.com":          true,
		"LIBSQL://db.example.com?tls=0":    true,
		"http://127.0.0.1:8080":            true,
		"https://db.example.com":           true,
		"postgres://u:p@h/db":              false,
		"mysql://u:p@h/db":                 false,
		"cockroach://u:p@h/db":             false,
		"file:data.db":                     false,
		"":                                 false,
		"  libsql://db.example.com  ":      true,
		"httpx://not.a.libsql.example.com": false,
	} {
		if got := isLibSQLURL(raw); got != want {
			t.Errorf("isLibSQLURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestParseLibSQLURL(t *testing.T) {
	t.Parallel()

	off := false

	scenarios := []struct {
		name     string
		raw      string
		envToken string
		want     libsqlParams
		wantErr  string
	}{
		{"plain", "libsql://db.example.com", "", libsqlParams{url: "libsql://db.example.com"}, ""},
		{"token in the url", "libsql://db.example.com?authToken=abc", "", libsqlParams{url: "libsql://db.example.com", token: "abc"}, ""},
		{"token from the env", "libsql://db.example.com", "envtoken", libsqlParams{url: "libsql://db.example.com", token: "envtoken"}, ""},
		{"the url token wins", "libsql://db.example.com?authToken=abc", "envtoken", libsqlParams{url: "libsql://db.example.com", token: "abc"}, ""},
		{"auth_token alias", "https://db.example.com?auth_token=abc", "", libsqlParams{url: "https://db.example.com", token: "abc"}, ""},
		{"tls off", "libsql://127.0.0.1:8080?tls=0", "", libsqlParams{url: "libsql://127.0.0.1:8080", tls: &off}, ""},
		{"proxy", "http://ns.example.test?proxy=http://127.0.0.1:8080", "", libsqlParams{url: "http://ns.example.test", proxy: "http://127.0.0.1:8080"}, ""},
		{"credentials", "libsql://user:pass@db.example.com", "", libsqlParams{}, "credentials are not part of the URL"},
		{"unknown param", "libsql://db.example.com?foo=1", "", libsqlParams{}, `unknown query parameter "foo"`},
		{"tls off on http", "http://db.example.com?tls=0", "", libsqlParams{}, "tls=0 is only for libsql://"},
		{"bad proxy", "http://db.example.com?proxy=ftp://x", "", libsqlParams{}, "proxy must be an http"},
		{"no host", "libsql://", "", libsqlParams{}, "the host is missing"},
		{"bad scheme", "postgres://db.example.com", "", libsqlParams{}, "the scheme must be"},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			got, err := parseLibSQLURL(s.raw, s.envToken)

			if s.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), s.wantErr) {
					t.Fatalf("expected an error containing %q, got %v", s.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}

			if got.url != s.want.url || got.token != s.want.token || got.proxy != s.want.proxy {
				t.Fatalf("got %+v, want %+v", got, s.want)
			}
			if (got.tls == nil) != (s.want.tls == nil) || (got.tls != nil && *got.tls != *s.want.tls) {
				t.Fatalf("tls: got %v, want %v", got.tls, s.want.tls)
			}
			// the token never stays in the URL that is handed to the driver
			if strings.Contains(got.url, "?") {
				t.Fatalf("the query string must be stripped, got %q", got.url)
			}
		})
	}
}

func TestRedactDBURL(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3t-token-value"

	for _, raw := range []string{
		"libsql://db.example.com?authToken=" + secret,
		"https://db.example.com?auth_token=" + secret + "&tls=1",
		"http://127.0.0.1:8080?jwt=" + secret,
		"postgres://user:" + secret + "@db.example.com:5432/claviger?sslmode=require",
		"mysql://user:" + secret + "@db.example.com:3306/claviger",
	} {
		got := RedactDBURL(raw)
		if strings.Contains(got, secret) {
			t.Errorf("RedactDBURL(%q) leaked the secret: %q", raw, got)
		}
		if !strings.Contains(got, "db.example.com") && !strings.Contains(got, "127.0.0.1") {
			t.Errorf("RedactDBURL(%q) lost the host: %q", raw, got)
		}
	}

	// nothing to hide
	if got := RedactDBURL("libsql://db.example.com"); got != "libsql://db.example.com" {
		t.Errorf("unexpected change: %q", got)
	}
}

func TestLibSQLConnectErrorsDoNotLeakTheToken(t *testing.T) {
	// not parallel: sets an env var
	const urlToken = "tok-in-url-0123456789"
	const envToken = "tok-in-env-0123456789"

	t.Setenv(DBAuthTokenEnv, envToken)

	// nothing listens on the port of a closed listener
	for _, raw := range []string{
		"http://127.0.0.1:1?authToken=" + urlToken,
		"http://127.0.0.1:1",
	} {
		_, err := LibSQLDBConnect(raw)("")
		if err == nil {
			t.Fatalf("expected a connection error for %q", raw)
		}

		msg := err.Error()
		for _, secret := range []string{urlToken, envToken, "authToken=", url.QueryEscape(urlToken)} {
			if strings.Contains(msg, secret) {
				t.Fatalf("the error leaks %q: %s", secret, msg)
			}
		}
	}

	// an invalid URL is reported as is, without a token
	if _, err := LibSQLDBConnect("libsql://u:p@h")(""); err == nil || strings.Contains(err.Error(), ":p@") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRedactLibSQLErr(t *testing.T) {
	t.Parallel()

	err := redactLibSQLErr(errors.New("Get http://h?authToken=abc: Bearer abc rejected"), "http://h?authToken=abc", "abc")
	if strings.Contains(err.Error(), "abc") {
		t.Fatalf("leaked: %v", err)
	}

	if redactLibSQLErr(nil, "x", "y") != nil {
		t.Fatal("expected nil")
	}
}

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
		t.Skip("CLAVIGER_TEST_LIBSQL_URL is not set")
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
		t.Skip("CLAVIGER_TEST_LIBSQL_AUTH_URL and CLAVIGER_TEST_LIBSQL_AUTH_TOKEN are not set")
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
