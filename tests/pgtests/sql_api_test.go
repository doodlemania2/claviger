//go:build postgres

package pgtests

import (
	"fmt"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
)

// TestSQLEndpoint covers the superuser SQL endpoint (/api/sql) on Postgres:
// statements run as written, writes are classified (and rolled back together),
// and the append-only audit log guard cannot be bypassed by the ways Postgres
// lets a table name be written differently.
func TestSQLEndpoint(t *testing.T) {
	app := newApp(t)
	seedRecords(t, app)
	c := newAPIClient(t, app)

	run := func(who, query string) apiResult {
		t.Helper()
		return c.do("POST", "/api/sql", who, map[string]any{"query": query})
	}
	rows := func(r apiResult) [][]any {
		t.Helper()
		m, _ := r.Body.(map[string]any)
		out := [][]any{}
		for _, row := range m["rows"].([]any) {
			out = append(out, row.([]any))
		}
		return out
	}
	count := func(query string) string {
		t.Helper()
		r := run("super", query)
		if r.Status != 200 {
			t.Fatalf("%s: %d %v", query, r.Status, r.Body)
		}
		return fmt.Sprint(rows(r)[0][0])
	}

	t.Run("access", func(t *testing.T) {
		if r := run("", "select 1"); r.Status != 401 {
			t.Fatalf("guest: %d", r.Status)
		}
		if r := run("user", "select 1"); r.Status != 403 {
			t.Fatalf("user: %d", r.Status)
		}
	})

	t.Run("read, as written", func(t *testing.T) {
		r := run("super", `select title, "views" from posts where id = 'pos000000000p1'`)
		if r.Status != 200 {
			t.Fatalf("%d %v", r.Status, r.Body)
		}
		got := rows(r)
		if len(got) != 1 || fmt.Sprint(got[0]) != "[Hello World 10]" {
			t.Fatalf("unexpected rows %v", got)
		}
		cols := r.Body.(map[string]any)["columns"].([]any)
		if len(cols) != 2 || cols[0].(map[string]any)["name"] != "title" {
			t.Fatalf("unexpected columns %v", cols)
		}
	})

	t.Run("unquoted camelCase folds to lower case", func(t *testing.T) {
		if r := run("super", `select createdAt from posts`); r.Status != 400 || !strings.Contains(fmt.Sprint(r.Body), "SQLSTATE") {
			t.Fatalf("expected the Postgres error, got %d %v", r.Status, r.Body)
		}
		if r := run("super", `select "created" from posts limit 1`); r.Status != 200 {
			t.Fatalf("%d %v", r.Status, r.Body)
		}
	})

	t.Run("invalid query", func(t *testing.T) {
		if r := run("super", "invalid"); r.Status != 400 || !strings.Contains(fmt.Sprint(r.Body), "Raw error") {
			t.Fatalf("%d %v", r.Status, r.Body)
		}
	})

	t.Run("write and affected rows", func(t *testing.T) {
		r := run("super", `update posts set views = 1 where id in ('pos000000000p1','pos000000000p2')`)
		if r.Status != 200 || r.Body.(map[string]any)["affectedRows"].(float64) != 2 {
			t.Fatalf("%d %v", r.Status, r.Body)
		}
		if got := count(`select count(*) from posts where views = 1`); got != "2" {
			t.Fatalf("got %s", got)
		}
	})

	t.Run("multiple write statements are one transaction", func(t *testing.T) {
		before := count(`select count(*) from tags`)
		r := run("super", `delete from tags; this is not sql`)
		if r.Status != 400 {
			t.Fatalf("%d %v", r.Status, r.Body)
		}
		if after := count(`select count(*) from tags`); after != before {
			t.Fatalf("the first statement was not rolled back: %s -> %s", before, after)
		}
	})

	t.Run("a write that looks like a read does not run as one", func(t *testing.T) {
		before := count(`select count(*) from tags`)
		// a writable CTE: the first keyword is WITH
		r := run("super", `with gone as (delete from tags returning id) select count(*) from gone`)
		if r.Status != 200 {
			t.Fatalf("%d %v", r.Status, r.Body)
		}
		if after := count(`select count(*) from tags`); after != "0" || before == "0" {
			t.Fatalf("the writable CTE should have taken the write path: %s -> %s", before, after)
		}
	})

	t.Run("reads cannot write", func(t *testing.T) {
		// a function call cannot write inside the read only transaction either
		before := count(`select count(*) from authors`)
		r := run("super", `select * from (select 1) x, lateral (select count(*) from authors) y`)
		if r.Status != 200 {
			t.Fatalf("%d %v", r.Status, r.Body)
		}
		if count(`select count(*) from authors`) != before {
			t.Fatal("authors changed")
		}
	})

	audit := core.CollectionNameAuditLog
	t.Run("audit log is append only", func(t *testing.T) {
		n := count(`select count(*) from "` + audit + `"`)
		if n == "0" {
			t.Fatal("expected audit entries from the seed")
		}

		blocked := []string{
			`delete from "` + audit + `"`,
			`DELETE FROM "` + strings.ToUpper(audit) + `"`,
			`delete from ` + audit, // folded to lower case
			`delete from public."` + audit + `"`,
			`update "` + audit + `" set action = 'x'`,
			`truncate "` + audit + `"`,
			`/* c */ delete /* "x" */ from "` + audit + `"`,
			`delete from U&"\005f\0061uditLog"`,
			`delete from U&"!005f!0061uditLog" UESCAPE '!'`,
			`delete from U&"\+00005faudit\+00004cog"`,
			`with d as (delete from "` + audit + `" returning 1) select count(*) from d`,
			`alter table "` + audit + `" rename to gone`,
			`drop table "` + audit + `"`,
			`drop schema public cascade`,
			`DROP   SCHEMA   public CASCADE`,
		}
		for _, q := range blocked {
			r := run("super", q)
			if r.Status != 400 || !strings.Contains(fmt.Sprint(r.Body), "append-only") {
				t.Errorf("%q was not rejected by the guard: %d %v", q, r.Status, r.Body)
			}
		}

		if after := count(`select count(*) from "` + audit + `"`); after != n {
			t.Fatalf("audit entries changed: %s -> %s", n, after)
		}

		// reading it is fine, also by name
		if r := run("super", `select action from "`+audit+`" limit 1`); r.Status != 200 {
			t.Fatalf("%d %v", r.Status, r.Body)
		}
	})
}
