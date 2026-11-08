//go:build postgres

package pgtests

import (
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// newSQLiteApp boots a regular (SQLite) app in a temp dir, the reference that
// the Postgres results are compared with.
func newSQLiteApp(t testing.TB) *core.BaseApp {
	t.Helper()

	t.Setenv(core.DBURLEnv, "")

	app := core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir()})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("sqlite bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = app.ClearBootstrap() })

	return app
}

// seedRecords creates the same collections and records in the app. The
// data is chosen to exercise every field type and the NULL/empty/case
// corner cases of the filter language.
func seedRecords(t testing.TB, app core.App) {
	t.Helper()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	open := types.Pointer("") // public API rules

	tags := core.NewBaseCollection("tags")
	tags.Fields.Add(&core.TextField{Name: "name"})
	tags.ListRule, tags.ViewRule = open, open
	must(app.Save(tags))

	authors := core.NewBaseCollection("authors")
	authors.Fields.Add(
		&core.TextField{Name: "name"},
		&core.NumberField{Name: "rank"},
	)
	authors.ListRule, authors.ViewRule = open, open
	must(app.Save(authors))

	posts := core.NewBaseCollection("posts")
	posts.Fields.Add(
		&core.TextField{Name: "title"},
		&core.EditorField{Name: "body"},
		&core.NumberField{Name: "views"},
		&core.BoolField{Name: "active"},
		&core.SelectField{Name: "status", Values: []string{"draft", "published", "archived"}, MaxSelect: 1},
		&core.SelectField{Name: "labels", Values: []string{"a", "b", "c", "d"}, MaxSelect: 4},
		&core.RelationField{Name: "author", CollectionId: authors.Id, MaxSelect: 1},
		&core.RelationField{Name: "tags", CollectionId: tags.Id, MaxSelect: 10},
		&core.JSONField{Name: "meta", MaxSize: 100000},
		&core.DateField{Name: "when"},
		&core.URLField{Name: "site"},
		&core.EmailField{Name: "contact"},
		&core.GeoPointField{Name: "geo"},
		&core.FileField{Name: "cover", MaxSelect: 1, MaxSize: 1000},
		&core.FileField{Name: "files", MaxSelect: 5, MaxSize: 1000},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	posts.ListRule, posts.ViewRule = open, open
	posts.CreateRule, posts.UpdateRule, posts.DeleteRule = open, open, open
	must(app.Save(posts))

	// comments point back to posts (a single and a multiple back relation)
	comments := core.NewBaseCollection("comments")
	comments.Fields.Add(
		&core.TextField{Name: "text"},
		&core.RelationField{Name: "post", CollectionId: posts.Id, MaxSelect: 1},
		&core.RelationField{Name: "seen", CollectionId: posts.Id, MaxSelect: 10},
	)
	comments.ListRule, comments.ViewRule = open, open
	must(app.Save(comments))

	save := func(col string, data map[string]any) {
		t.Helper()
		c, err := app.FindCollectionByNameOrId(col)
		must(err)
		r := core.NewRecord(c)
		for k, v := range data {
			r.Set(k, v)
		}
		if err := app.SaveNoValidate(r); err != nil {
			t.Fatalf("seed %s %v: %v", col, data["id"], err)
		}
	}

	for id, name := range map[string]string{"tag000000000t1": "go", "tag000000000t2": "Rust", "tag000000000t3": "100%_sure"} {
		save("tags", map[string]any{"id": id, "name": name})
	}
	save("authors", map[string]any{"id": "aut000000000a1", "name": "Alice", "rank": 1})
	save("authors", map[string]any{"id": "aut000000000a2", "name": "bob", "rank": 2.5})

	const (
		t1 = "tag000000000t1"
		t2 = "tag000000000t2"
		t3 = "tag000000000t3"
		a1 = "aut000000000a1"
		a2 = "aut000000000a2"
	)

	save("posts", map[string]any{
		"id": "pos000000000p1", "title": "Hello World", "body": "<p>First</p>", "views": 10, "active": true,
		"status": "published", "labels": []string{"a", "b"}, "author": a1, "tags": []string{t1, t2},
		"meta":    `{"n":5,"s":"abc","b":true,"arr":[1,2],"o":{"k":"v"},"z":null}`,
		"when":    "2024-01-15 10:00:00.000Z",
		"site":    "https://example.com",
		"contact": "x@example.com",
		"geo":     map[string]any{"lon": 10, "lat": 20},
		"cover":   "cover_one.png", "files": []string{"f1_aaa.txt", "f2_bbb.png"},
	})
	save("posts", map[string]any{
		"id": "pos000000000p2", "title": "hello again", "views": 20, "active": false,
		"status": "draft", "labels": []string{"b"}, "author": a2, "tags": []string{t2},
		"meta": `{"n":7,"s":"ABC"}`,
		"when": "2024-02-20 08:30:00.000Z",
		"geo":  map[string]any{"lon": 11, "lat": 21},
	})
	save("posts", map[string]any{
		"id": "pos000000000p3", "title": "Third post", "views": 0, "active": true,
		"status": "archived", "labels": []string{},
	})
	save("posts", map[string]any{
		"id": "pos000000000p4", "title": "Fourth_post%", "views": 5.5, "active": false,
		"status": "published", "labels": []string{"c", "d"}, "author": a1, "tags": []string{t1, t3},
		"meta": `{"n":"5"}`,
		"when": "2025-03-01 23:59:59.999Z",
		"geo":  map[string]any{"lon": 0, "lat": 0},
	})
	save("posts", map[string]any{
		"id": "pos000000000p5", "title": "", "views": 100, "active": true,
		"labels": []string{"a"}, "author": a2, "tags": []string{t3},
		"meta": `{"s":"x","n":null}`,
		"when": "2023-12-31 00:00:00.000Z",
	})
	save("posts", map[string]any{
		"id": "pos000000000p6", "title": "Zebra", "views": -3, "active": false,
		"status": "draft", "labels": []string{"a", "d"}, "tags": []string{t1, t2, t3},
		"meta":    `5`,
		"contact": "Zed@Example.COM",
		"files":   []string{"f3_ccc.txt"},
	})
	save("posts", map[string]any{
		"id": "pos000000000p7", "title": "ÉCOLE élan", "views": 7, "active": true,
		"status": "archived", "labels": []string{"b", "c"}, "author": a1,
		"meta": `"plain"`,
	})
	save("posts", map[string]any{
		"id": "pos000000000p8", "title": "Hello World", "views": 10, "active": true,
		"status": "published", "labels": []string{"a", "b", "c"}, "author": a2, "tags": []string{t1},
		"meta": `[1,2,3]`,
		"when": "2024-06-01 12:00:00.000Z",
	})

	save("comments", map[string]any{"id": "com000000000c1", "text": "nice", "post": "pos000000000p1", "seen": []string{"pos000000000p1", "pos000000000p2"}})
	save("comments", map[string]any{"id": "com000000000c2", "text": "NICE one", "post": "pos000000000p1", "seen": []string{"pos000000000p3"}})
	save("comments", map[string]any{"id": "com000000000c3", "text": "meh", "post": "pos000000000p2"})
	save("comments", map[string]any{"id": "com000000000c4", "text": "orphan"})
}
