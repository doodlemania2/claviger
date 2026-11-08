//go:build postgres

package pgtests

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/plugins/jsvm"
	"github.com/doodlemania2/claviger/plugins/migratecmd"
	"github.com/doodlemania2/claviger/tools/dbutils"
	"github.com/spf13/cobra"
)

// userIndexes are written the way users (and the admin UI) write them: the
// portable, backtick quoted form. They must come out of an export unchanged
// on both engines, import on both, and be reported by TableIndexes in the same
// form so the live indexes can be compared with the collection ones.
var userIndexes = []string{
	"CREATE UNIQUE INDEX `idx_ie_title` ON `ie_posts` (`title`)",
	"CREATE INDEX `idx_ie_title_nocase` ON `ie_posts` (`title` COLLATE NOCASE)",
	"CREATE INDEX `idx_ie_multi` ON `ie_posts` (`views` DESC, `title`)",
	"CREATE UNIQUE INDEX `idx_ie_partial` ON `ie_posts` (`slug`) WHERE `slug` != ''",
	"CREATE INDEX `idx_ie_expr` ON `ie_posts` (lower(`title` || `slug`))",
	"CREATE INDEX `idx_ie_Mixed_Case` ON `ie_posts` (`slug`)",
}

func newImportExportCollection(t testing.TB, app core.App) {
	t.Helper()

	c := core.NewBaseCollection("ie_posts")
	c.Fields.Add(
		&core.TextField{Name: "title"},
		&core.TextField{Name: "slug"},
		&core.NumberField{Name: "views"},
	)
	c.Indexes = append([]string{}, userIndexes...)
	if err := app.Save(c); err != nil {
		t.Fatalf("save collection: %v", err)
	}
}

// exportCollections is what GET /api/collections returns, the way the admin UI exports it.
func exportCollections(t testing.TB, app core.App) []map[string]any {
	t.Helper()

	c := newAPIClient(t, app)
	r := c.do("GET", "/api/collections?perPage=500&filter=name~%27ie_%27", "super", nil)
	if r.Status != 200 {
		t.Fatalf("export: %d %v", r.Status, r.Body)
	}

	var out []map[string]any
	for _, item := range r.Body.(map[string]any)["items"].([]any) {
		out = append(out, item.(map[string]any))
	}
	return out
}

func indexesOf(t testing.TB, app core.App, name string) []string {
	t.Helper()

	c, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		t.Fatal(err)
	}
	out := append([]string{}, c.Indexes...)
	sort.Strings(out)
	return out
}

// semantic reduces a portable index to what it means, so the live index (rebuilt from the
// catalog) and the stored one compare equal despite formatting.
func semantic(raw string) string {
	idx := dbutils.ParseIndex(raw)
	idx.Optional = false
	idx.SchemaName = ""
	var cols []string
	for _, c := range idx.Columns {
		name := strings.NewReplacer("`", "", " ", "", "(", "", ")", "").Replace(c.Name)
		cols = append(cols, strings.ToLower(name)+"|"+strings.ToUpper(c.Collate)+"|"+strings.ToUpper(c.Sort))
	}
	// Postgres renders expressions with its own parentheses ("title || slug" is stored as "(title || slug)")
	where := strings.NewReplacer("`", "", " ", "", "(", "", ")", "").Replace(idx.Where)
	return fmt.Sprintf("%s unique=%v on=%s cols=%s where=%s", idx.IndexName, idx.Unique, idx.TableName, strings.Join(cols, ","), where)
}

func TestIndexesRoundTripAcrossEngines(t *testing.T) {
	pg := newApp(t)
	lite := newSQLiteApp(t)
	newImportExportCollection(t, pg)
	newImportExportCollection(t, lite)

	// 1. the stored (and exported) form is the same on both engines, as written
	want := append([]string{}, userIndexes...)
	sort.Strings(want)
	for name, app := range map[string]core.App{"postgres": pg, "sqlite": lite} {
		if got := indexesOf(t, app, "ie_posts"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: stored indexes changed:\n got  %v\n want %v", name, got, want)
		}
	}

	// 2. the live indexes, reported in the portable form, mean what the stored ones mean
	for name, app := range map[string]core.App{"postgres": pg, "sqlite": lite} {
		live, err := app.TableIndexes("ie_posts")
		if err != nil {
			t.Fatal(err)
		}
		if len(live) != len(userIndexes) {
			t.Errorf("%s: expected %d live indexes, got %v", name, len(userIndexes), live)
		}
		for _, stored := range userIndexes {
			idxName := dbutils.ParseIndex(stored).IndexName
			def, ok := live[idxName]
			if !ok {
				t.Errorf("%s: missing live index %s (have %v)", name, idxName, live)
				continue
			}
			if semantic(def) != semantic(stored) {
				t.Errorf("%s: live index %s differs from the stored one:\n live   %s\n stored %s", name, idxName, def, stored)
			}
		}
	}

	// 3. export from Postgres, import into a fresh Postgres and SQLite app, and back
	export := exportCollections(t, pg)
	if len(export) != 1 {
		t.Fatalf("unexpected export: %v", export)
	}
	raw, _ := json.Marshal(export)

	pg2 := newApp(t)
	lite2 := newSQLiteApp(t)
	for name, app := range map[string]core.App{"postgres": pg2, "sqlite": lite2} {
		if err := app.ImportCollectionsByMarshaledJSON(raw, false); err != nil {
			t.Fatalf("%s: import: %v", name, err)
		}
		if got := indexesOf(t, app, "ie_posts"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: imported indexes changed:\n got  %v\n want %v", name, got, want)
		}
		live, _ := app.TableIndexes("ie_posts")
		if len(live) != len(userIndexes) {
			t.Errorf("%s: imported collection has %d live indexes: %v", name, len(live), live)
		}
	}

	// the export of the imported Postgres app is identical (no drift)
	again := exportCollections(t, pg2)
	for _, e := range [][]map[string]any{export, again} {
		for _, c := range e {
			delete(c, "id") // generated per app
			for _, f := range c["fields"].([]any) {
				delete(f.(map[string]any), "id")
			}
			delete(c, "created")
			delete(c, "updated")
		}
	}
	if !reflect.DeepEqual(export, again) {
		a, _ := json.MarshalIndent(export, "", " ")
		b, _ := json.MarshalIndent(again, "", " ")
		t.Errorf("export drifted after an import:\n%s\n---\n%s", a, b)
	}

	// 4. importing the same snapshot again changes nothing (the diff is empty)
	if err := pg2.ImportCollectionsByMarshaledJSON(raw, true); err == nil {
		// deleteMissing=true with a snapshot that lacks the users collection etc. is fine:
		// system collections are never deleted
		if got := indexesOf(t, pg2, "ie_posts"); !reflect.DeepEqual(got, want) {
			t.Errorf("re-import changed the indexes: %v", got)
		}
	} else {
		t.Fatalf("re-import: %v", err)
	}
}

// TestMigrateCommandsOnPostgres: automigrate and `migrate collections` write the
// collections the same way on Postgres, and the generated JS migrations apply on
// both engines (a snapshot taken on Postgres runs on SQLite and the other way round).
func TestMigrateCommandsOnPostgres(t *testing.T) {
	pg := newApp(t)

	dir := t.TempDir()
	if err := migratecmd.Register(pg, nil, migratecmd.Config{Automigrate: true, Dir: dir, TemplateLang: migratecmd.TemplateLangJS}); err != nil {
		t.Fatal(err)
	}

	// automigrate runs on the collection API requests and writes *_created_ie_posts.js
	api := newAPIClient(t, pg)
	if r := api.do("POST", "/api/collections", "super", map[string]any{
		"name": "ie_posts",
		"type": "base",
		"fields": []map[string]any{
			{"name": "title", "type": "text"},
			{"name": "slug", "type": "text"},
			{"name": "views", "type": "number"},
		},
		"indexes": userIndexes,
	}); r.Status != 200 {
		t.Fatalf("create collection: %d %v", r.Status, r.Body)
	}

	root := &cobra.Command{Use: "app"}
	if err := migratecmd.Register(pg, root, migratecmd.Config{Dir: dir, TemplateLang: migratecmd.TemplateLangJS}); err != nil {
		t.Fatal(err)
	}
	root.SetArgs([]string{"migrate", "collections"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	// the command asks for a confirmation on stdin
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = stdinW.WriteString("y\n")
	_ = stdinW.Close()
	oldStdin := os.Stdin
	os.Stdin = stdinR
	err = root.Execute()
	os.Stdin = oldStdin
	if err != nil {
		t.Fatalf("migrate collections: %v", err)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "*.js"))
	var created, snapshot string
	for _, f := range files {
		switch {
		case strings.HasSuffix(f, "_created_ie_posts.js"):
			created = f
		case strings.HasSuffix(f, "_collections_snapshot.js"):
			snapshot = f
		}
	}
	if created == "" || snapshot == "" {
		t.Fatalf("expected the automigrate and snapshot files, got %v", files)
	}
	for _, f := range []string{created, snapshot} {
		raw, _ := os.ReadFile(f)
		for _, idx := range userIndexes {
			// the JS template escapes the backticks with the JSON encoder
			enc, _ := json.Marshal(idx)
			if !strings.Contains(string(raw), strings.Trim(string(enc), `"`)) {
				t.Errorf("%s does not hold the portable index %s", filepath.Base(f), idx)
			}
		}
	}

	// both files were written within the same second; the runner orders by name
	// and "collections_snapshot" sorts before "created_*", so put the snapshot last
	late := filepath.Join(dir, "9999999999_collections_snapshot.js")
	if err := os.Rename(snapshot, late); err != nil {
		t.Fatal(err)
	}

	// apply the migrations on a fresh Postgres app and on a SQLite one
	// (the JS migrations are registered once, in the global list that the runner of each app uses)
	targets := map[string]core.App{"postgres": newApp(t), "sqlite": newSQLiteApp(t)}
	jsvm.MustRegister(targets["postgres"], jsvm.Config{MigrationsDir: dir, HooksDir: t.TempDir(), TypesDir: t.TempDir()})
	for name, app := range targets {
		if _, err := core.NewMigrationsRunner(app, core.AppMigrations).Up(); err != nil {
			t.Fatalf("%s: apply migrations: %v", name, err)
		}

		want := append([]string{}, userIndexes...)
		sort.Strings(want)
		if got := indexesOf(t, app, "ie_posts"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: indexes after the migrations:\n got  %v\n want %v", name, got, want)
		}
		if live, _ := app.TableIndexes("ie_posts"); len(live) != len(userIndexes) {
			t.Errorf("%s: live indexes %v", name, live)
		}
	}
}

// The collections list (used by the export) filters and sorts the _collections
// table on Postgres: bool columns take true/false, and the default order is creation order.
func TestCollectionsListFilter(t *testing.T) {
	app := newApp(t)
	newImportExportCollection(t, app)
	c := newAPIClient(t, app)

	r := c.do("GET", "/api/collections?filter=system%3Dfalse&perPage=500", "super", nil)
	if r.Status != 200 {
		t.Fatalf("%d %v", r.Status, r.Body)
	}
	var names []string
	for _, it := range r.Body.(map[string]any)["items"].([]any) {
		names = append(names, it.(map[string]any)["name"].(string))
	}
	if len(names) == 0 || names[len(names)-1] != "ie_posts" || slices.Contains(names, "_superusers") {
		t.Fatalf("unexpected non system collections (in creation order): %v", names)
	}

	r = c.do("GET", "/api/collections?filter=system%3Dtrue&perPage=1", "super", nil)
	if r.Status != 200 || len(r.Body.(map[string]any)["items"].([]any)) != 1 {
		t.Fatalf("%d %v", r.Status, r.Body)
	}
}
