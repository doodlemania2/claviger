package dialect_test

import (
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/tools/dbutils"
	"github.com/doodlemania2/claviger/tools/dialect"
)

// The SQLite search layer must keep producing the historic SQL.
func TestSQLiteSearchSQLIsUnchanged(t *testing.T) {
	d := dialect.SQLite

	for got, want := range map[string]string{
		d.BoolLiteral(true):                   "1",
		d.BoolLiteral(false):                  "0",
		d.IsOp(false):                         "IS",
		d.IsOp(true):                          "IS NOT",
		d.AsText("[[a]]"):                     "[[a]]",
		d.LikeOp(false):                       "LIKE",
		d.LikeOp(true):                        "NOT LIKE",
		d.OrderTerm("[[a]]", "DESC"):          "[[a]] DESC",
		d.NoCaseEqual("[[email]]", "{:e}"):    "[[email]] = {:e} COLLATE NOCASE",
		d.JSONEach("t.c", true):               dbutils.JSONEach("t.c"),
		d.JSONEach("t.c", false):              dbutils.JSONEach("t.c"),
		d.JSONEachParam("p"):                  "json_each({:p})",
		d.JSONArrayLength("t.c", true):        dbutils.JSONArrayLength("t.c"),
		d.JSONExtract("t.c", "a.b[0]"):        dbutils.JSONExtract("t.c", "a.b[0]"),
		d.ColumnCast("{:p}", "NUMERIC"):       "{:p}",
		d.JoinOn():                            "",
		d.SubqueryAlias():                     "",
		d.ScalarCast(dialect.ScalarBool, "x"): "x",
	} {
		if got != want {
			t.Errorf("want %q, got %q", want, got)
		}
	}

	if d.NativeJSON() || d.StrictTypes() || d.ColumnKind("INTEGER") != dialect.ColumnUnknown {
		t.Error("SQLite must stay dynamically typed")
	}

	if _, ok := d.(dialect.FunctionBuilder); ok {
		t.Error("SQLite uses the filter functions of tools/search")
	}
}

func TestPostgresSearchSQL(t *testing.T) {
	d := dialect.Postgres

	for got, want := range map[string]string{
		d.BoolLiteral(true):                       "TRUE",
		d.IsOp(false):                             "IS NOT DISTINCT FROM",
		d.IsOp(true):                              "IS DISTINCT FROM",
		d.AsText("[[a]]"):                         "CAST([[a]] AS TEXT)",
		d.LikeOp(true):                            "NOT ILIKE",
		d.OrderTerm("[[a]]", "ASC"):               "[[a]] ASC NULLS FIRST",
		d.OrderTerm("[[a]]", "DESC"):              "[[a]] DESC NULLS LAST",
		d.NoCaseEqual("[[email]]", "{:e}"):        "lower([[email]]) = lower({:e})",
		d.JSONExtract("t.c", ""):                  "NULLIF([[t.c]], 'null'::jsonb)",
		d.JSONExtract("t.c", "a.b[0]"):            `NULLIF([[t.c]] #> '{"a","b","0"}', 'null'::jsonb)`,
		d.JSONExtract("t.c", "[1].k"):             `NULLIF([[t.c]] #> '{"1","k"}', 'null'::jsonb)`,
		d.JSONText("x"):                           "(x #>> '{}')",
		d.JSONFromScalar(dialect.ScalarText, "x"): "to_jsonb(CAST(x AS TEXT))",
		d.ColumnCast("{:p}", `DOUBLE PRECISION DEFAULT 0 NOT NULL`):    "CAST({:p} AS DOUBLE PRECISION)",
		d.ColumnCast("{:p}", `TEXT COLLATE "C" PRIMARY KEY DEFAULT x`): "CAST({:p} AS TEXT)",
		d.ColumnCast("{:p}", `JSONB DEFAULT '[]' NOT NULL`):            "CAST({:p} AS JSONB)",
		d.JoinOn(): "TRUE",
	} {
		if got != want {
			t.Errorf("want %q, got %q", want, got)
		}
	}

	if !d.NativeJSON() || !d.StrictTypes() {
		t.Error("Postgres is strictly typed with native JSON")
	}

	for typ, want := range map[string]dialect.ColumnKind{
		"text": dialect.ColumnText, "character varying(10)": dialect.ColumnText,
		"double precision": dialect.ColumnFloat, "numeric": dialect.ColumnFloat,
		"bigint": dialect.ColumnInt, "integer": dialect.ColumnInt,
		"boolean": dialect.ColumnBool, "jsonb": dialect.ColumnJSON,
		"timestamp with time zone": dialect.ColumnUnknown,
	} {
		if got := d.ColumnKind(typ); got != want {
			t.Errorf("ColumnKind(%q) = %v, want %v", typ, got, want)
		}
	}
}

func TestPostgresStrFTime(t *testing.T) {
	fb := dialect.Postgres.(dialect.FunctionBuilder)

	scenarios := []struct {
		name        string
		format      string
		timeValue   string
		isNumber    bool
		modifiers   []string
		contains    []string
		expectError string
	}{
		{name: "now", format: "%Y", contains: []string{"to_char(pb_ts.ts, 'YYYY')", "NOW() AT TIME ZONE 'UTC'"}},
		{name: "merged template", format: "%Y-%m-%d %H:%M:%S", timeValue: "[[c]]", contains: []string{`to_char(pb_ts.ts, 'YYYY"-"MM"-"DD" "HH24":"MI":"SS')`, "SELECT CAST([[c]] AS TEXT) AS t"}},
		{name: "fraction and day of year", format: "%f/%j", timeValue: "[[c]]", contains: []string{`'SS.MS"/"DDD'`}},
		{name: "composites", format: "%F %T %R", timeValue: "[[c]]", contains: []string{`'YYYY-MM-DD" "HH24:MI:SS" "HH24:MI'`}},
		{name: "percent and quotes", format: `100%% "it's"`, timeValue: "[[c]]", contains: []string{`'"100% \"it''s\""'`}},
		{name: "weekday and epoch", format: "%w %s", timeValue: "[[c]]", contains: []string{"EXTRACT(DOW FROM pb_ts.ts)", "EXTRACT(EPOCH FROM"}},
		{name: "modifiers", format: "%Y", timeValue: "[[c]]", modifiers: []string{"start of month", "+1 month", "-2 days", "start of year"}, contains: []string{"date_trunc('year', ((date_trunc('month'", "INTERVAL '+1 month'", "INTERVAL '-2 day'"}},
		{name: "weekday modifier", format: "%Y", timeValue: "[[c]]", modifiers: []string{"weekday 3"}, contains: []string{"3 - CAST(EXTRACT(DOW FROM"}},
		{name: "number is a julian day", format: "%Y", timeValue: "{:p}", isNumber: true, contains: []string{"2440587.5", "CAST({:p} AS DOUBLE PRECISION)"}},
		{name: "unixepoch", format: "%Y", timeValue: "{:p}", isNumber: true, modifiers: []string{"unixepoch"}, contains: []string{"to_timestamp(pb_in.t)"}},
		{name: "ignored modifiers", format: "%Y", timeValue: "[[c]]", modifiers: []string{"utc", "subsec"}, contains: []string{"to_char"}},
		{name: "unsupported format", format: "%J", timeValue: "[[c]]", expectError: "%J is not supported"},
		{name: "dangling percent", format: "%Y%", timeValue: "[[c]]", expectError: "dangling"},
		{name: "unsupported modifier", format: "%Y", timeValue: "[[c]]", modifiers: []string{"localtime"}, expectError: `"localtime" is not supported`},
		{name: "invalid modifier", format: "%Y", timeValue: "[[c]]", modifiers: []string{"+1 fortnight"}, expectError: "is not supported"},
		{name: "no sql injection through modifiers", format: "%Y", timeValue: "[[c]]", modifiers: []string{"+1 day'; DROP TABLE x; --"}, expectError: "is not supported"},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			sql, err := fb.StrFTime(s.format, s.timeValue, s.isNumber, s.modifiers)

			if s.expectError != "" {
				if err == nil || !strings.Contains(err.Error(), s.expectError) {
					t.Fatalf("expected an error with %q, got %v (%s)", s.expectError, err, sql)
				}
				return
			}

			if err != nil {
				t.Fatal(err)
			}

			for _, c := range s.contains {
				if !strings.Contains(sql, c) {
					t.Errorf("missing %q in\n%s", c, sql)
				}
			}
		})
	}
}

func TestPostgresGeoDistance(t *testing.T) {
	got := dialect.Postgres.(dialect.FunctionBuilder).GeoDistance("a", "b", "c", "d")
	if !strings.Contains(got, "LEAST(1, GREATEST(-1,") || strings.Contains(got, "min(") {
		t.Fatalf("unexpected SQL: %s", got)
	}
}
