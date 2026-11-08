package tests_test

import (
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// TestPostgresHarness: with CLAVIGER_TEST_DB=postgres the test app really is
// on Postgres, with the whole converted fixture (so a green run of the suites
// cannot be a run on SQLite).
func TestPostgresHarness(t *testing.T) {
	if !tests.IsPostgres() {
		t.Skip("CLAVIGER_TEST_DB is not postgres")
	}

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	if got := core.DialectOf(app).Name(); got != "postgres" {
		t.Fatalf("dialect = %s", got)
	}
	if app.PostgresURL() == "" {
		t.Fatal("expected a Postgres URL")
	}

	// the fixture rows (see tests/data), a view over them, and the signed tokens' secrets
	for table, want := range map[string]int{"demo1": 3, "view1": 3, "_superusers": 4} {
		var n int
		if err := app.DB().NewQuery("SELECT count(*) FROM {{" + table + "}}").Row(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if n != want {
			t.Errorf("%s: %d rows, want %d", table, n, want)
		}
	}

	su, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	token := "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6InN5d2JoZWNuaDQ2cmhtMCIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoicGJjXzMxNDI2MzU4MjMiLCJleHAiOjI1MjQ2MDQ0NjEsInJlZnJlc2hhYmxlIjp0cnVlfQ.UXgO3j-0BumcugrFjbd7j0M4MQvbrLggLlcu_YNGjoY"
	if found, err := app.FindAuthRecordByToken(token, core.TokenTypeAuth); err != nil || found.Id != su.Id {
		t.Fatalf("the fixture token does not verify: %v", err)
	}
}
