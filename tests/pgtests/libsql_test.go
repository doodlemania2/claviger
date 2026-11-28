//go:build postgres

package pgtests

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/dialect"
	"github.com/doodlemania2/claviger/tools/security"
)

// freshLibSQLNamespace creates an empty namespace on the libSQL test server and
// returns the URL that opens it (the namespace is the first label of the host
// name, the "proxy" parameter is the address that is connected to).
func freshLibSQLNamespace(t testing.TB) string {
	t.Helper()

	server, admin := os.Getenv("CLAVIGER_TEST_LIBSQL_URL"), os.Getenv("CLAVIGER_TEST_LIBSQL_ADMIN_URL")
	if admin == "" {
		t.Skip("CLAVIGER_TEST_LIBSQL_ADMIN_URL is not set (see docs/datastores.md)")
	}

	name := "pbt" + strings.ToLower(security.PseudorandomString(10))

	call := func(method string) {
		req, err := http.NewRequest(method, strings.TrimRight(admin, "/")+"/v1/namespaces/"+name+suffix(method), strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode >= 300 && method == http.MethodPost {
			t.Fatalf("create namespace: %d", res.StatusCode)
		}
	}

	call(http.MethodPost)
	t.Cleanup(func() { call(http.MethodDelete) })

	return "http://" + name + ".libsql.test?proxy=" + url.QueryEscape(server)
}

func suffix(method string) string {
	if method == http.MethodPost {
		return "/create"
	}
	return ""
}

// skipOnLibSQL skips a test that asserts a PostgreSQL specific outcome.
func skipOnLibSQL(t testing.TB, why string) {
	t.Helper()
	if isLibSQLServer(t) {
		t.Skip("known_gap(libsql): " + why)
	}
}

// Several replicas need the LISTEN/NOTIFY bus and the advisory locks of PostgreSQL:
// libSQL (one sqld primary) refuses the mode, loudly.
func TestMultiReplicaRefusedOnLibSQL(t *testing.T) {
	if !isLibSQLServer(t) {
		t.Skip("libSQL only")
	}

	t.Setenv(core.DBURLEnv, freshDatabase(t))
	t.Setenv(core.MultiReplicaEnv, "true")

	app := core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir()})
	err := app.Bootstrap()
	if err == nil {
		_ = app.ClearBootstrap()
		t.Fatal("expected the start to be refused")
	}
	if !strings.Contains(err.Error(), "not supported on libsql") || !strings.Contains(err.Error(), "PostgreSQL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// The logs stay in a local SQLite file next to the app.
func TestLibSQLUsesTheLocalAuxiliaryDatabase(t *testing.T) {
	if !isLibSQLServer(t) {
		t.Skip("libSQL only")
	}

	app := newApp(t)

	if got := core.DialectOf(app).Name(); got != dialect.LibSQLName {
		t.Fatalf("data dialect %q", got)
	}
	if got := core.AuxDialectOf(app).Name(); got != dialect.SQLiteName {
		t.Fatalf("aux dialect %q", got)
	}
}
