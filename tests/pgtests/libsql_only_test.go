//go:build postgres && libsql

package pgtests

// Tests that only mean something against a libSQL server. They are behind
// their own build tag, not a t.Skip, so that a PostgreSQL run (whose CI step
// fails on any skip that is not a known gap) does not compile them:
// go test -tags postgres,libsql ./tests/pgtests/... with CLAVIGER_TEST_DB=libsql.

import (
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/dialect"
)

// Several replicas need the LISTEN/NOTIFY bus and the advisory locks of PostgreSQL:
// libSQL (one sqld primary) refuses the mode, loudly.
func TestMultiReplicaRefusedOnLibSQL(t *testing.T) {
	if !isLibSQLServer(t) {
		t.Fatal("the libsql build tag is for a run against a libSQL server (CLAVIGER_TEST_DB=libsql)")
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
		t.Fatal("the libsql build tag is for a run against a libSQL server (CLAVIGER_TEST_DB=libsql)")
	}

	app := newApp(t)

	if got := core.DialectOf(app).Name(); got != dialect.LibSQLName {
		t.Fatalf("data dialect %q", got)
	}
	if got := core.AuxDialectOf(app).Name(); got != dialect.SQLiteName {
		t.Fatalf("aux dialect %q", got)
	}
}
