//go:build postgres

package pgtests

import (
	"net/url"
	"slices"
	"testing"
)

// View queries are user SQL: they run as written, in the SQL dialect of the
// database (docs/datastores.md), so each engine gets its own spelling.
type viewDef struct {
	name   string
	sqlite string
	pg     string
}

var testViews = []viewDef{
	{
		name:   "v_plain",
		sqlite: "SELECT id, title, views, active, status, created FROM posts",
		pg:     `SELECT id, title, views, active, status, created FROM posts`,
	},
	{
		name:   "v_join",
		sqlite: "SELECT p.id, p.title, a.name AS author_name, a.rank AS author_rank FROM posts p LEFT JOIN authors a ON a.id = p.author",
		pg:     `SELECT p.id, p.title, a.name AS author_name, a.rank AS author_rank FROM posts p LEFT JOIN authors a ON a.id = p.author`,
	},
	{
		name:   "v_agg",
		sqlite: "SELECT a.id, a.name, COUNT(p.id) AS total, SUM(p.views) AS sum_views FROM authors a LEFT JOIN posts p ON p.author = a.id GROUP BY a.id, a.name",
		pg:     `SELECT a.id, a.name, COUNT(p.id) AS total, SUM(p.views) AS sum_views FROM authors a LEFT JOIN posts p ON p.author = a.id GROUP BY a.id, a.name`,
	},
	{
		name:   "v_expr",
		sqlite: "SELECT id, (views * 2) AS doubled, (views > 5) AS big, (title || '!') AS shout, json_extract(meta, '$.n') AS n, CAST(views AS INTEGER) AS whole FROM posts",
		pg:     `SELECT id, (views * 2) AS doubled, (views > 5) AS big, (title || '!') AS shout, (meta -> 'n') AS n, CAST(views AS INTEGER) AS whole FROM posts`,
	},
	{
		// the quoted camelCase alias is the Postgres spelling of an identifier with capitals
		name:   "v_camel",
		sqlite: "SELECT id, title AS postTitle FROM posts",
		pg:     `SELECT id, title AS "postTitle" FROM posts`,
	},
	{
		name:   "v_rowid",
		sqlite: "SELECT (ROW_NUMBER() OVER()) AS id, title FROM posts",
		pg:     `SELECT (ROW_NUMBER() OVER ()) AS id, title FROM posts`,
	},
}

func TestViewCollections(t *testing.T) {
	sqliteApp := newSQLiteApp(t)
	seedRecords(t, sqliteApp)
	pgApp := newApp(t)
	seedRecords(t, pgApp)

	sqliteClient := newAPIClient(t, sqliteApp)
	pgClient := newAPIClient(t, pgApp)

	createView := func(c *apiClient, name, query string) apiResult {
		return c.do("POST", "/api/collections", "super", map[string]any{
			"name": name, "type": "view", "viewQuery": query, "listRule": "", "viewRule": "",
		})
	}

	for _, v := range testViews {
		t.Run("create/"+v.name, func(t *testing.T) {
			sres := createView(sqliteClient, v.name, v.sqlite)
			pres := createView(pgClient, v.name, v.pg)
			if sres.Status != 200 || pres.Status != 200 {
				t.Fatalf("create view: sqlite=%d %s postgres=%d %s", sres.Status, brief(sres.Body), pres.Status, brief(pres.Body))
			}

			// the generated field types
			sf, pf := fieldTypes(sres.Body), fieldTypes(pres.Body)
			t.Logf("sqlite fields:   %v", sf)
			t.Logf("postgres fields: %v", pf)
		})
	}

	// field types inferred from the view's columns (Postgres only; SQLite
	// views are dynamically typed so unknown expressions are JSON fields)
	t.Run("field types", func(t *testing.T) {
		for view, want := range map[string]map[string]string{
			"v_plain": {"id": "text", "title": "text", "views": "number", "active": "bool", "status": "select", "created": "autodate"},
			"v_agg":   {"id": "text", "name": "text", "total": "number", "sum_views": "number"},
			"v_expr":  {"id": "text", "doubled": "number", "big": "bool", "shout": "text", "n": "json", "whole": "number"},
			"v_camel": {"id": "text", "postTitle": "text"},
		} {
			col, err := pgApp.FindCollectionByNameOrId(view)
			if err != nil {
				t.Fatal(err)
			}
			for name, typ := range want {
				f := col.Fields.GetByName(name)
				if f == nil {
					t.Errorf("%s: missing field %q (have %v)", view, name, col.Fields.FieldNames())
					continue
				}
				if f.Type() != typ {
					t.Errorf("%s.%s type = %q, want %q", view, name, f.Type(), typ)
				}
			}
		}
	})

	cases := []struct {
		name, url string
		// idsOnly: the values of the expression columns are typed by the
		// engine (SQLite 1/true, JSON text vs number, integer casts) so only
		// the matching records are compared
		idsOnly bool
	}{
		{name: "plain list", url: "/api/collections/v_plain/records?sort=title,id"},
		{name: "plain filter", url: "/api/collections/v_plain/records?sort=id&filter=" + url.QueryEscape(`views > 5 && active = true`)},
		{name: "plain filter like", url: "/api/collections/v_plain/records?sort=id&filter=" + url.QueryEscape(`title ~ "hello"`)},
		{name: "plain filter null", url: "/api/collections/v_plain/records?sort=id&filter=" + url.QueryEscape(`status = ""`)},
		{name: "plain paging", url: "/api/collections/v_plain/records?sort=id&page=2&perPage=3"},
		{name: "plain fields", url: "/api/collections/v_plain/records?sort=id&fields=id,title"},
		{name: "plain view", url: "/api/collections/v_plain/records/pos000000000p1"},
		{name: "join list", url: "/api/collections/v_join/records?sort=id"},
		{name: "join filter", url: "/api/collections/v_join/records?sort=id&filter=" + url.QueryEscape(`author_name ~ "ali" && author_rank >= 1`)},
		{name: "join filter null", url: "/api/collections/v_join/records?sort=id&filter=" + url.QueryEscape(`author_name = ""`)},
		{name: "agg list", url: "/api/collections/v_agg/records?sort=-total,id"},
		{name: "agg filter", url: "/api/collections/v_agg/records?sort=id&filter=" + url.QueryEscape(`total >= 2`)},
		{name: "agg sum filter", url: "/api/collections/v_agg/records?sort=id&filter=" + url.QueryEscape(`sum_views > 0`)},
		{name: "expr list", url: "/api/collections/v_expr/records?sort=id", idsOnly: true},
		{name: "expr filter number", url: "/api/collections/v_expr/records?sort=id&filter=" + url.QueryEscape(`doubled > 20`), idsOnly: true},
		{name: "expr filter bool", url: "/api/collections/v_expr/records?sort=id&filter=" + url.QueryEscape(`big = true`), idsOnly: true},
		{name: "expr filter text", url: "/api/collections/v_expr/records?sort=id&filter=" + url.QueryEscape(`shout ~ "world!"`), idsOnly: true},
		{name: "camel list", url: "/api/collections/v_camel/records?sort=id"},
		{name: "camel filter", url: "/api/collections/v_camel/records?sort=id&filter=" + url.QueryEscape(`postTitle ~ "zeb"`)},
		{name: "rownumber list", url: "/api/collections/v_rowid/records?sort=id"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sres := sqliteClient.do("GET", c.url, "super", nil)
			pres := pgClient.do("GET", c.url, "super", nil)

			if sres.Status != 200 {
				t.Fatalf("sqlite %d: %s", sres.Status, brief(sres.Body))
			}
			if pres.Status != 200 {
				t.Fatalf("postgres %d: %s", pres.Status, brief(pres.Body))
			}

			if c.idsOnly {
				if w, g := itemIds(sres), itemIds(pres); !slices.Equal(w, g) {
					t.Fatalf("ids mismatch\n sqlite:   %v\n postgres: %v", w, g)
				}
				return
			}

			want, got := normalize(sres.Body), normalize(pres.Body)
			if brief(want) != brief(got) {
				t.Fatalf("mismatch\n sqlite:   %s\n postgres: %s", brief(want), brief(got))
			}
		})
	}

	t.Run("update view query", func(t *testing.T) {
		col, err := pgApp.FindCollectionByNameOrId("v_plain")
		if err != nil {
			t.Fatal(err)
		}
		col.ViewQuery = `SELECT id, title FROM posts WHERE views > 5`
		if err := pgApp.Save(col); err != nil {
			t.Fatal(err)
		}
		records, err := pgApp.FindRecordsByFilter("v_plain", "id != ''", "", 0, 0)
		if err != nil || len(records) != 6 {
			t.Fatalf("records = %d, err = %v", len(records), err)
		}
	})

	t.Run("invalid view query is rejected", func(t *testing.T) {
		res := createView(pgClient, "v_bad", "SELECT id, nope FROM posts")
		if res.Status != 400 {
			t.Fatalf("status = %d, want 400: %s", res.Status, brief(res.Body))
		}
		if pgApp.HasTable("v_bad") {
			t.Fatal("the invalid view was left behind")
		}
	})
}

func fieldTypes(body any) map[string]string {
	m, _ := body.(map[string]any)
	fields, _ := m["fields"].([]any)
	out := map[string]string{}
	for _, f := range fields {
		fm, _ := f.(map[string]any)
		name, _ := fm["name"].(string)
		typ, _ := fm["type"].(string)
		out[name] = typ
	}
	return out
}
