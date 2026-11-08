//go:build postgres

package pgtests

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	_ "github.com/doodlemania2/claviger/migrations"
	"github.com/doodlemania2/claviger/tools/dialect"
	"github.com/doodlemania2/claviger/tools/security"
)

func adminURL(t testing.TB) string {
	t.Helper()

	u := os.Getenv("CLAVIGER_TEST_PG_URL")
	if u == "" {
		t.Skip("CLAVIGER_TEST_PG_URL is not set (see tests/pgtests/doc.go)")
	}

	return u
}

// freshDatabase creates an empty database and returns its connection URL.
func freshDatabase(t testing.TB) string {
	t.Helper()

	admin := adminURL(t)

	db, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	name := "pbt_" + strings.ToLower(security.PseudorandomString(10))
	if _, err := db.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("create database: %v", err)
	}

	t.Cleanup(func() {
		db, err := sql.Open("pgx", admin)
		if err != nil {
			return
		}
		defer db.Close()
		_, _ = db.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`)
	})

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name

	return u.String()
}

// newApp boots a Claviger app on a fresh Postgres database,
// selected the way a deployment does it: with CLAVIGER_DB_URL.
func newApp(t testing.TB) *core.BaseApp {
	t.Helper()

	t.Setenv(core.DBURLEnv, freshDatabase(t))

	app := core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir()})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = app.ClearBootstrap() })

	return app
}

func TestDialectSelection(t *testing.T) {
	app := newApp(t)

	if got := core.DialectOf(app).Name(); got != dialect.PostgresName {
		t.Fatalf("data db dialect = %q, want postgres", got)
	}
	// the logs db stays a local SQLite file
	if got := core.AuxDialectOf(app).Name(); got != dialect.SQLiteName {
		t.Fatalf("aux db dialect = %q, want sqlite", got)
	}
	if app.ConcurrentDB() != app.NonconcurrentDB() {
		t.Fatal("expected a single connection pool for reads and writes on Postgres")
	}
	if app.AuxConcurrentDB() == app.AuxNonconcurrentDB() {
		t.Fatal("expected the SQLite aux db to keep its separate write pool")
	}
	if _, err := os.Stat(app.DataDir() + "/auxiliary.db"); err != nil {
		t.Fatalf("auxiliary.db should exist locally: %v", err)
	}
	if _, err := os.Stat(app.DataDir() + "/data.db"); err == nil {
		t.Fatal("data.db must not be created when the data db is Postgres")
	}
}

func TestBootstrapAndSystemMigrations(t *testing.T) {
	app := newApp(t)

	// every system migration is recorded as applied
	var applied int
	if err := app.DB().NewQuery("SELECT count(*) FROM _migrations").Row(&applied); err != nil {
		t.Fatal(err)
	}
	if want := len(core.SystemMigrations.Items()); applied != want {
		t.Fatalf("applied migrations = %d, want %d", applied, want)
	}

	// system tables and collections (incl. the fork ones) exist
	for _, name := range []string{
		"_migrations", "_params", "_collections", "users",
		core.CollectionNameSuperusers, core.CollectionNameMFAs, core.CollectionNameOTPs,
		core.CollectionNameExternalAuths, core.CollectionNameAuthOrigins,
	} {
		if !app.HasTable(name) {
			t.Errorf("missing table %q", name)
		}
	}
	if !app.HasTable("USERS") {
		t.Error("HasTable should be case insensitive")
	}
	if app.HasTable("missing_table") {
		t.Error("HasTable(missing) = true")
	}

	cols, err := app.FindAllCollections()
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) < 10 {
		t.Fatalf("expected the system collections, got %d", len(cols))
	}
	// insertion order is the hidden identity column
	if cols[0].Name != core.CollectionNameMFAs {
		t.Errorf("first collection = %q, want %q", cols[0].Name, core.CollectionNameMFAs)
	}

	// settings round trip through the JSONB _params table
	settings := app.Settings()
	settings.Meta.AppName = "pg-test"
	if err := app.Save(settings); err != nil {
		t.Fatal(err)
	}
	if err := app.ReloadSettings(); err != nil {
		t.Fatal(err)
	}
	if got := app.Settings().Meta.AppName; got != "pg-test" {
		t.Fatalf("settings round trip: %q", got)
	}

	// running the migrations again is a no-op
	runner := core.NewMigrationsRunner(app, core.SystemMigrations)
	again, err := runner.Up()
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("migrations re-applied: %v", again)
	}
}

func TestIntrospection(t *testing.T) {
	app := newApp(t)

	info, err := app.TableInfo("_collections")
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]*core.TableInfoRow{}
	for _, row := range info {
		byName[row.Name] = row
	}
	if _, ok := byName[dialect.RowIDColumn]; ok {
		t.Error("the hidden row id column must not be listed")
	}
	if id := byName["id"]; id == nil || id.PK != 1 || !id.NotNull || id.Type != "text" {
		t.Errorf("unexpected id column info: %+v", id)
	}
	if f := byName["fields"]; f == nil || f.Type != "jsonb" || !f.NotNull {
		t.Errorf("unexpected fields column info: %+v", f)
	}
	if info[0].Name != "id" || info[0].Index != 0 {
		t.Errorf("columns should be ordered: %+v", info[0])
	}

	cols, err := app.TableColumns("_collections")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cols {
		if c == dialect.RowIDColumn {
			t.Error("TableColumns lists the hidden column")
		}
	}
	if len(cols) != len(info) {
		t.Errorf("TableColumns %d != TableInfo %d", len(cols), len(info))
	}

	if _, err := app.TableInfo("missing_table"); err == nil {
		t.Error("expected an error for a missing table")
	}

	// constraint backed indexes (primary key, unique) are not listed
	idx, err := app.TableIndexes("_collections")
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != 1 {
		t.Fatalf("unexpected indexes: %v", idx)
	}
	for name, def := range idx {
		// the portable (backtick) form of the collection "indexes" field
		if name != "idx__collections_type" || !strings.HasPrefix(def, "CREATE INDEX `idx__collections_type` ON `_collections` (") {
			t.Errorf("unexpected index %s: %s", name, def)
		}
	}
}

func TestCollectionLifecycle(t *testing.T) {
	app := newApp(t)

	col := core.NewBaseCollection("posts")
	col.Fields.Add(
		&core.TextField{Name: "title", Required: true},
		&core.NumberField{Name: "views"},
		&core.BoolField{Name: "pub"},
		&core.JSONField{Name: "meta"},
		&core.DateField{Name: "when"},
		&core.SelectField{Name: "tag", Values: []string{"a", "b", "c"}, MaxSelect: 1},
		&core.AutodateField{Name: "created", OnCreate: true},
	)
	col.AddIndex("idx_posts_title", true, "title", "")
	col.AddIndex("idx_posts_title_nocase", false, "title COLLATE NOCASE", "")
	if err := app.Save(col); err != nil {
		t.Fatalf("create: %v", err)
	}

	if !app.HasTable("posts") {
		t.Fatal("posts table was not created")
	}

	// column types
	info, err := app.TableInfo("posts")
	if err != nil {
		t.Fatal(err)
	}
	colTypes := map[string]string{}
	for _, r := range info {
		colTypes[r.Name] = r.Type
	}
	for col, want := range map[string]string{
		"id": "text", "title": "text", "views": "double precision", "pub": "boolean",
		"meta": "jsonb", "when": "text", "tag": "text", "created": "text",
	} {
		if colTypes[col] != want {
			t.Errorf("column %s type = %q, want %q", col, colTypes[col], want)
		}
	}

	// the case insensitive index is a lower(...) index
	indexes, err := app.TableIndexes("posts")
	if err != nil {
		t.Fatal(err)
	}
	var pgDef string
	if err := app.DB().NewQuery("SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_posts_title_nocase'").Row(&pgDef); err != nil || !strings.Contains(pgDef, "lower(") {
		t.Errorf("expected a lower() index in the catalog, got %q (%v)", pgDef, err)
	}
	// TableIndexes returns the portable form of it
	if !strings.Contains(indexes["idx_posts_title_nocase"], "`title` COLLATE NOCASE") {
		t.Errorf("expected the portable NOCASE form, got %q", indexes["idx_posts_title_nocase"])
	}
	if !strings.Contains(indexes["idx_posts_title"], "UNIQUE") {
		t.Errorf("expected a unique index, got %q", indexes["idx_posts_title"])
	}

	// insertion order
	for _, title := range []string{"zeta", "alpha", "mid"} {
		r := core.NewRecord(col)
		r.Set("title", title)
		if err := app.Save(r); err != nil {
			t.Fatalf("save record: %v", err)
		}
	}
	var titles []string
	if err := app.DB().NewQuery("SELECT title FROM posts ORDER BY " + core.DialectOf(app).RowOrder()).Column(&titles); err != nil {
		t.Fatal(err)
	}
	if strings.Join(titles, ",") != "zeta,alpha,mid" {
		t.Fatalf("insertion order lost: %v", titles)
	}

	// a duplicate on the unique index is a validation error and
	// the app (and its transaction machinery) keeps working
	dup := core.NewRecord(col)
	dup.Set("title", "zeta")
	if err := app.Save(dup); err == nil {
		t.Fatal("expected a unique violation")
	}

	// update: rename + add + drop fields and rename the collection
	col, err = app.FindCollectionByNameOrId("posts")
	if err != nil {
		t.Fatal(err)
	}
	col.Fields.GetByName("views").SetName("hits")
	col.Fields.RemoveByName("pub")
	col.Fields.Add(&core.EmailField{Name: "contact"})
	col.Name = "articles"
	if err := app.Save(col); err != nil {
		t.Fatalf("update: %v", err)
	}
	if app.HasTable("posts") || !app.HasTable("articles") {
		t.Fatal("collection table was not renamed")
	}
	cols, _ := app.TableColumns("articles")
	got := strings.Join(cols, ",")
	for _, want := range []string{"hits", "contact"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing column %s in %s", want, got)
		}
	}
	for _, unwanted := range []string{"pub", "views"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("unexpected column %s in %s", unwanted, got)
		}
	}
	// indexes follow the renamed table
	indexes, _ = app.TableIndexes("articles")
	if len(indexes) != 2 {
		t.Errorf("expected 2 indexes after the rename, got %v", indexes)
	}

	// single -> multiple select keeps the value
	col, _ = app.FindCollectionByNameOrId("articles")
	if _, err := app.DB().NewQuery("UPDATE articles SET tag = 'b' WHERE title = 'alpha'").Execute(); err != nil {
		t.Fatal(err)
	}
	col.Fields.GetByName("tag").(*core.SelectField).MaxSelect = 3
	if err := app.Save(col); err != nil {
		t.Fatalf("single to multiple: %v", err)
	}
	var tag string
	if err := app.DB().NewQuery("SELECT tag::text FROM articles WHERE title = 'alpha'").Row(&tag); err != nil {
		t.Fatal(err)
	}
	if tag != `["b"]` {
		t.Errorf("tag after single->multiple = %q", tag)
	}
	if err := app.DB().NewQuery("SELECT tag::text FROM articles WHERE title = 'zeta'").Row(&tag); err != nil {
		t.Fatal(err)
	}
	if tag != `[]` {
		t.Errorf("empty tag after single->multiple = %q", tag)
	}
	// ... and back (keeps the last element)
	col, _ = app.FindCollectionByNameOrId("articles")
	col.Fields.GetByName("tag").(*core.SelectField).MaxSelect = 1
	if err := app.Save(col); err != nil {
		t.Fatalf("multiple to single: %v", err)
	}
	if err := app.DB().NewQuery("SELECT tag FROM articles WHERE title = 'alpha'").Row(&tag); err != nil {
		t.Fatal(err)
	}
	if tag != "b" {
		t.Errorf("tag after multiple->single = %q", tag)
	}

	// a failing index is reported as a validation error (not an aborted transaction)
	col, _ = app.FindCollectionByNameOrId("articles")
	col.AddIndex("idx_bad", false, "does_not_exist", "")
	if err := app.Save(col); err == nil {
		t.Fatal("expected an invalid index error")
	}
	if _, err := app.FindFirstRecordByData("articles", "title", "mid"); err != nil {
		t.Fatalf("app unusable after the failed index: %v", err)
	}

	// delete
	col, _ = app.FindCollectionByNameOrId("articles")
	if err := app.Delete(col); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if app.HasTable("articles") {
		t.Fatal("table still exists after deleting the collection")
	}
	if idx, _ := app.TableIndexes("articles"); len(idx) != 0 {
		t.Fatalf("indexes left behind: %v", idx)
	}
}

func TestTryUsesSavepoints(t *testing.T) {
	app := newApp(t)

	err := app.RunInTransaction(func(txApp core.App) error {
		d := core.DialectOf(txApp)

		// a failed probe inside a transaction would abort it...
		probeErr := d.Try(txApp.DB(), func() error {
			_, err := txApp.DB().NewQuery("SELECT * FROM table_that_does_not_exist").Execute()
			return err
		})
		if probeErr == nil {
			return fmt.Errorf("the probe should have failed")
		}

		// ...but Try rolled back to its savepoint, so the transaction goes on
		var one int
		if err := txApp.DB().NewQuery("SELECT 1").Row(&one); err != nil {
			return fmt.Errorf("transaction aborted: %w", err)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSuperuserRecord(t *testing.T) {
	app := newApp(t)

	superusers, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(superusers)
	r.SetEmail("test@example.com")
	r.SetPassword("1234567890")
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}

	found, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if found.Id != r.Id || len(found.Id) != 15 || !found.ValidatePassword("1234567890") {
		t.Fatalf("unexpected record %+v", found)
	}

	// the hidden column never leaks into the record data
	if _, ok := found.FieldsData()[dialect.RowIDColumn]; ok {
		t.Error("hidden row id column exposed in the record data")
	}
	if _, ok := found.PublicExport()[dialect.RowIDColumn]; ok {
		t.Error("hidden row id column exposed in the record export")
	}
}
