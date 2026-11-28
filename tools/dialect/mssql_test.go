package dialect_test

import (
	"errors"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dbutils"
	"github.com/doodlemania2/claviger/tools/dialect"
)

func TestMSSQLIsSelectedByDriverName(t *testing.T) {
	for _, driver := range []string{"mssql", "sqlserver"} {
		if got := dialect.Of(dbx.NewFromDB(nil, driver)).Name(); got != dialect.MSSQLName {
			t.Errorf("%s: dialect %q", driver, got)
		}
	}

	if dialect.IsPostgres(dialect.MSSQL) {
		t.Fatal("SQL Server is not PostgreSQL")
	}
}

func TestMSSQLDefinitions(t *testing.T) {
	d := dialect.MSSQL

	for name, def := range map[string]string{"text": d.TextColumn(), "id": d.IDColumn(), "json": d.JSONColumn("[]")} {
		if !strings.Contains(def, "COLLATE "+dialect.MSSQLCollation) {
			t.Errorf("%s column without the binary collation: %s", name, def)
		}
	}

	if got := d.QuoteIdent("a]b"); got != "[a]]b]" {
		t.Errorf("QuoteIdent = %s", got)
	}

	if !d.Retries() || d.IsRetryable(nil) {
		t.Fatal("SQL Server retries its transactions")
	}

	dialect.MSSQLErrorNumber = func(err error) int32 { return 1205 }
	defer func() { dialect.MSSQLErrorNumber = func(error) int32 { return 0 } }()
	if !d.IsRetryable(errors.New("deadlock")) {
		t.Fatal("a deadlock victim must be retryable")
	}
}

func TestMSSQLAdaptDDL(t *testing.T) {
	got := dialect.MSSQL.AdaptDDL(`CREATE TABLE IF NOT EXISTS {{_x}} (a TEXT PRIMARY KEY NOT NULL, b TEXT UNIQUE, c BOOLEAN DEFAULT FALSE NOT NULL, d TEXT DEFAULT '' NOT NULL, e VARCHAR(100));
		CREATE INDEX IF NOT EXISTS idx_x on {{_x}} (e)`)

	for _, want := range []string{
		"IF OBJECT_ID(N'_x', N'U') IS NULL CREATE TABLE {{_x}} (",
		"a NVARCHAR(255) COLLATE " + dialect.MSSQLCollation + " PRIMARY KEY",
		"b NVARCHAR(255) COLLATE " + dialect.MSSQLCollation + " UNIQUE",
		"c BIT DEFAULT 0 NOT NULL",
		"d NVARCHAR(MAX) COLLATE " + dialect.MSSQLCollation + " DEFAULT ''",
		"e NVARCHAR(100) COLLATE",
		"CREATE INDEX idx_x on",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestMSSQLBuildIndex(t *testing.T) {
	d := dialect.MSSQL.(dialect.IndexBuilder)

	// without column types every key is a plain column
	sql, err := d.BuildIndexOn(nil, dbutils.ParseIndex("CREATE INDEX `idx_a` ON `posts` (`a`, `b` DESC)"))
	if err != nil || !strings.Contains(sql, "CREATE INDEX [idx_a] ON [posts] ([a], [b] DESC)") {
		t.Fatalf("plain index: %v\n%s", err, sql)
	}
	if !strings.Contains(sql, "claviger_index") {
		t.Fatalf("the portable definition is not kept:\n%s", sql)
	}

	// a case insensitive partial unique index is a computed hash key that has a key of its own
	// for the rows that SQLite would skip
	sql, err = d.BuildIndexOn(nil, dbutils.ParseIndex("CREATE UNIQUE INDEX `idx_u` ON `users` (`email` COLLATE NOCASE) WHERE `email` != ''"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ADD [_pbx_idx_u_0] AS (CASE WHEN LOWER([email]) IS NOT NULL AND ([email] != '') THEN CONVERT(BINARY(32), HASHBYTES('SHA2_256'",
		"[_rowid_]",
		"PERSISTED",
		"CREATE UNIQUE INDEX [idx_u] ON [users] ([_pbx_idx_u_0])",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in\n%s", want, sql)
		}
	}

	// an expression key written with || is concatenated with +
	sql, _ = d.BuildIndexOn(nil, dbutils.ParseIndex("CREATE INDEX `idx_e` ON `posts` (lower(`title` || `slug`))"))
	if first, _, _ := strings.Cut(sql, ";"); strings.Contains(first, "||") || !strings.Contains(first, "[title] + [slug]") {
		t.Fatalf("expression key:\n%s", sql)
	}
}

func TestMSSQLStrFTime(t *testing.T) {
	fb := dialect.MSSQL.(dialect.FunctionBuilder)

	if sql, err := fb.StrFTime("%Y-%m-%d %H:%M:%f", "[[posts.when]]", false, []string{"start of month", "+1 day", "weekday 1"}); err != nil || strings.Contains(sql, "@@TS@@") {
		t.Fatalf("supported: %v %s", err, sql)
	}

	for _, tc := range []struct {
		format string
		mods   []string
	}{
		{"%G", nil}, {"%P", nil}, {"%Y", []string{"localtime"}}, {"%Y", []string{"+2.5 days"}}, {"%", nil},
	} {
		if _, err := fb.StrFTime(tc.format, "[[posts.when]]", false, tc.mods); err == nil {
			t.Errorf("%q %v must be rejected", tc.format, tc.mods)
		}
	}
}

func TestMSSQLJSONExtractIsTyped(t *testing.T) {
	sql := dialect.MSSQL.JSONExtract("posts.meta", "a.b[0]")

	for _, want := range []string{"OPENJSON([[posts.meta]], '$.a.b')", "j.[key] = N'0'", "sql_variant", "ISJSON([[posts.meta]]) = 1"} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in\n%s", want, sql)
		}
	}
}
