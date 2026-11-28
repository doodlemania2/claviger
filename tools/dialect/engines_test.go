package dialect_test

import (
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dbutils"
	"github.com/doodlemania2/claviger/tools/dialect"
)

func TestCockroachAndMySQLAreSelectedByDriverName(t *testing.T) {
	for driver, want := range map[string]string{
		"cockroach": dialect.CockroachName,
		"mysql":     dialect.MySQLName,
		"mariadb":   dialect.MariaDBName,
		"pgx":       dialect.PostgresName,
	} {
		db := dbx.NewFromDB(nil, driver)
		if got := dialect.Of(db).Name(); got != want {
			t.Errorf("%s: dialect %q, want %q", driver, got, want)
		}
	}

	if !dialect.IsPostgres(dialect.Cockroach) || !dialect.IsPostgres(dialect.Postgres) || dialect.IsPostgres(dialect.MySQL) || dialect.IsPostgres(dialect.SQLite) {
		t.Fatal("IsPostgres must be true for PostgreSQL and CockroachDB only")
	}
}

func TestCockroachDefinitions(t *testing.T) {
	d := dialect.Cockroach

	// no COLLATE "C" (CockroachDB rejects the locale), but the same types
	if strings.Contains(d.TextColumn(), "COLLATE") || strings.Contains(d.IDColumn(), "COLLATE") || strings.Contains(d.TextType(), "COLLATE") {
		t.Fatalf("unexpected collation: %s / %s", d.TextColumn(), d.IDColumn())
	}
	if !strings.Contains(dialect.Postgres.TextColumn(), `COLLATE "C"`) {
		t.Fatal("PostgreSQL must keep the C collation")
	}
	if got := d.HiddenColumns()[dialect.RowIDColumn]; !strings.Contains(got, "CACHE 1") {
		t.Fatalf("row id column: %q", got)
	}
	if !d.Retries() || dialect.Postgres.Retries() || dialect.SQLite.Retries() {
		t.Fatal("only CockroachDB (and MySQL) retry transactions")
	}
}

type sqlStateErr struct{ code string }

func (e sqlStateErr) Error() string    { return "boom" }
func (e sqlStateErr) SQLState() string { return e.code }

func TestCockroachRetryableError(t *testing.T) {
	if !dialect.Cockroach.IsRetryable(sqlStateErr{"40001"}) {
		t.Fatal("40001 must be retryable")
	}
	if dialect.Cockroach.IsRetryable(sqlStateErr{"23505"}) || dialect.Cockroach.IsRetryable(nil) {
		t.Fatal("other errors are not retryable")
	}
	if dialect.Postgres.IsRetryable(sqlStateErr{"40001"}) {
		t.Fatal("PostgreSQL does not retry")
	}
}

func TestMySQLAdaptDDL(t *testing.T) {
	in := "CREATE TABLE {{t}} ([[id]] TEXT PRIMARY KEY DEFAULT ('r') NOT NULL, [[a]] TEXT UNIQUE NOT NULL, [[b]] TEXT DEFAULT 'x' NOT NULL, [[j]] JSON DEFAULT '[]' NOT NULL, [[n]] BOOLEAN DEFAULT FALSE);\n" +
		"CREATE INDEX IF NOT EXISTS idx on {{t}} ([[a]]);"
	got := dialect.MySQL.AdaptDDL(in)

	for _, want := range []string{
		"VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin PRIMARY KEY",
		"VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin UNIQUE",
		"LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT ('x')",
		"JSON DEFAULT ('[]')",
		"BOOLEAN DEFAULT FALSE",
		"CREATE INDEX idx on",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	if dialect.SQLite.AdaptDDL(in) != in || dialect.Postgres.AdaptDDL(in) != in {
		t.Fatal("SQLite and PostgreSQL keep the DDL as written")
	}
}

func TestMySQLIndexWithoutColumnTypes(t *testing.T) {
	idx := dbutils.ParseIndex("CREATE UNIQUE INDEX `idx_e` ON `users` (`email`) WHERE `email` != ''")

	got := dialect.MySQL.BuildIndex(idx)
	for _, want := range []string{"CREATE UNIQUE INDEX `idx_e` ON `users`", "CASE WHEN `email` != '' THEN `email` END", "COMMENT '"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}

func TestMySQLStrftime(t *testing.T) {
	fb := dialect.MySQL.(dialect.FunctionBuilder)

	got, err := fb.StrFTime("%Y-%m", "[[posts.when]]", false, []string{"start of month", "+1 day"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DATE_FORMAT(", "'%Y'", "'%m'", "INTERVAL 1 DAY"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}

	for _, bad := range []string{"%V", "%G", "%U"} {
		if _, err := fb.StrFTime(bad, "", false, nil); err == nil {
			t.Errorf("%s must be rejected", bad)
		}
	}
	if _, err := fb.StrFTime("%Y", "", false, []string{"weekday 1"}); err == nil {
		t.Error("the weekday modifier must be rejected")
	}
}
