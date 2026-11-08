package dialect_test

import (
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dbutils"
	"github.com/doodlemania2/claviger/tools/dialect"
	_ "modernc.org/sqlite"
)

func TestOf(t *testing.T) {
	sqliteDB, err := dbx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer sqliteDB.Close()

	pgDB := dbx.NewFromDB(nil, "pgx") // no connection is needed to pick the dialect

	scenarios := []struct {
		name string
		db   dbx.Builder
		want string
	}{
		{"nil", nil, dialect.SQLiteName},
		{"sqlite db", sqliteDB, dialect.SQLiteName},
		{"pgx db", pgDB, dialect.PostgresName},
		{"postgres driver alias", dbx.NewFromDB(nil, "postgres"), dialect.PostgresName},
		{"unknown driver", dbx.NewFromDB(nil, "something"), dialect.SQLiteName},
		{"pgx tx builder", &dbx.Tx{Builder: pgDB.Builder}, dialect.PostgresName},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			if got := dialect.Of(s.db).Name(); got != s.want {
				t.Fatalf("want %q, got %q", s.want, got)
			}
		})
	}
}

// The SQLite dialect must keep producing the historic definitions.
func TestSQLiteDefinitionsAreUnchanged(t *testing.T) {
	d := dialect.SQLite

	for got, want := range map[string]string{
		d.IDColumn():            "TEXT PRIMARY KEY DEFAULT ('r'||lower(hex(randomblob(7)))) NOT NULL",
		d.TextColumn():          "TEXT DEFAULT '' NOT NULL",
		d.NumberColumn():        "NUMERIC DEFAULT 0 NOT NULL",
		d.BoolColumn():          "BOOLEAN DEFAULT FALSE NOT NULL",
		d.JSONColumn(""):        "JSON DEFAULT NULL",
		d.JSONColumn("[]"):      "JSON DEFAULT '[]' NOT NULL",
		d.JSONColumn(`{"a":1}`): `JSON DEFAULT '{"a":1}' NOT NULL`,
		d.NowDefault():          "(strftime('%Y-%m-%d %H:%M:%fZ'))",
		d.StringDefault("base"): `"base"`,
		d.Int64Type():           "INTEGER",
		d.RowOrder():            "rowid ASC",
	} {
		if got != want {
			t.Errorf("want %q, got %q", want, got)
		}
	}

	if d.HiddenColumns() != nil || !d.SeparateWritePool() {
		t.Error("unexpected SQLite hidden columns or pool setting")
	}
}

func TestBuildIndex(t *testing.T) {
	scenarios := []struct {
		name       string
		index      string
		wantSQLite string
		wantPG     string
	}{
		{
			"simple",
			"CREATE INDEX `idx_a` ON `t` (`a`)",
			"CREATE INDEX `idx_a` ON `t` (`a`)",
			`CREATE INDEX "idx_a" ON "t" ("a")`,
		},
		{
			"unique optional multi column with sort",
			"CREATE UNIQUE INDEX IF NOT EXISTS idx_a ON t (a DESC, b ASC)",
			"CREATE UNIQUE INDEX IF NOT EXISTS `idx_a` ON `t` (\n  `a` DESC,\n  `b` ASC\n)",
			`CREATE UNIQUE INDEX IF NOT EXISTS "idx_a" ON "t" ("a" DESC, "b" ASC)`,
		},
		{
			"nocase becomes lower()",
			"CREATE UNIQUE INDEX idx_u ON t (username COLLATE NOCASE)",
			"CREATE UNIQUE INDEX `idx_u` ON `t` (`username` COLLATE NOCASE)",
			`CREATE UNIQUE INDEX "idx_u" ON "t" (lower("username"))`,
		},
		{
			"expression and where with backticks",
			"CREATE UNIQUE INDEX idx_e ON t (`email`, (a || b)) WHERE `email` != ''",
			"CREATE UNIQUE INDEX `idx_e` ON `t` (\n  `email`,\n  (a || b)\n) WHERE `email` != ''",
			`CREATE UNIQUE INDEX "idx_e" ON "t" ("email", ((a || b))) WHERE "email" != ''`,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			parsed := dbutils.ParseIndex(s.index)

			if got := dialect.SQLite.BuildIndex(parsed); got != s.wantSQLite {
				t.Errorf("sqlite: want\n%s\ngot\n%s", s.wantSQLite, got)
			}
			if got := dialect.Postgres.BuildIndex(parsed); got != s.wantPG {
				t.Errorf("postgres: want\n%s\ngot\n%s", s.wantPG, got)
			}
		})
	}

	if got := dialect.Postgres.BuildIndex(dbutils.Index{}); got != "" {
		t.Errorf("an invalid index should render as empty, got %q", got)
	}
}

func TestPostgresDefinitions(t *testing.T) {
	d := dialect.Postgres

	for _, got := range []string{d.RandomIDDefault(), d.NowDefault(), d.IDColumn(), d.NumberColumn(), d.JSONColumn("[]")} {
		if strings.Contains(got, "randomblob") || strings.Contains(got, "strftime") || strings.Contains(got, "NUMERIC") {
			t.Errorf("SQLite-ism in the Postgres definition: %q", got)
		}
	}

	if got := d.JSONColumn("[]"); got != "JSONB DEFAULT '[]' NOT NULL" {
		t.Errorf("unexpected json column: %q", got)
	}
	if got := d.StringDefault("it's"); got != "'it''s'" {
		t.Errorf("unexpected string default: %q", got)
	}
	if got := d.HiddenColumns()[dialect.RowIDColumn]; !strings.Contains(got, "GENERATED ALWAYS AS IDENTITY") {
		t.Errorf("missing identity column: %q", got)
	}
	if d.SeparateWritePool() {
		t.Error("Postgres should use a single pool")
	}
}
