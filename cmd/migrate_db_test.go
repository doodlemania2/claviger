package cmd

// Fork-local (Claviger #106, M5): tests of `migrate-db`.
//
// They need a PostgreSQL server (CLAVIGER_TEST_PG_URL, a role that may CREATE
// DATABASE) and are skipped without one. scripts/pgembedded starts one.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/security"
)

func migratePGAdminURL(t testing.TB) string {
	t.Helper()
	u := os.Getenv(tests.TestPGURLEnv)
	if u == "" {
		t.Skip(tests.TestPGURLEnv + " is not set")
	}
	return u
}

// newMigrateTarget creates an empty database and returns its URL.
func newMigrateTarget(t testing.TB) string {
	t.Helper()

	admin := migratePGAdminURL(t)
	name := "pbmig_" + strings.ToLower(security.PseudorandomString(10))

	db, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db, err := sql.Open("pgx", admin)
		if err != nil {
			return
		}
		defer db.Close()
		_, _ = db.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`)
	})

	u, _ := url.Parse(admin)
	u.Path = "/" + name
	return u.String()
}

// sqliteFixtureDir returns a copy of the test fixture whose database is up to date
// with the system migrations (the fixture predates the fork's).
func sqliteFixtureDir(t testing.TB) string {
	t.Helper()

	dir, err := tests.TempDirClone(filepath.Join("..", "tests", "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	migrateSourceUp(t, dir)

	return dir
}

// migrateSourceUp opens the SQLite database of dir with the current system migrations.
func migrateSourceUp(t testing.TB, dir string) {
	t.Helper()

	app := core.NewBaseApp(core.BaseAppConfig{DataDir: dir, DBConnect: core.DefaultDBConnect})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if err := app.ClearBootstrap(); err != nil {
		t.Fatal(err)
	}
}

func openMigrateApp(t testing.TB, dir, pgURL string) core.App {
	t.Helper()

	cfg := core.BaseAppConfig{DataDir: dir, DBConnect: core.DefaultDBConnect}
	if pgURL != "" {
		cfg.DBConnect = core.PostgresDBConnect(pgURL)
		cfg.AuxDBConnect = core.DefaultDBConnect
	}

	app := core.NewBaseApp(cfg)
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminateMigrateApp(app) })

	return app
}

// terminateMigrateApp stops what OnServe started (the webhook worker) before the
// databases are closed, like TestApp.Cleanup does.
func terminateMigrateApp(app core.App) {
	event := new(core.TerminateEvent)
	event.App = app
	_ = app.OnTerminate().Trigger(event, func(e *core.TerminateEvent) error {
		return e.App.ClearBootstrap()
	})
}

func serveHandler(t testing.TB, app core.App) http.Handler {
	t.Helper()

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}

	var handler http.Handler
	ev := &core.ServeEvent{App: app, Router: router}
	if err := app.OnServe().Trigger(ev, func(e *core.ServeEvent) error {
		mux, err := e.Router.BuildMux()
		handler = mux
		return err
	}); err != nil {
		t.Fatal(err)
	}

	return handler
}

func apiGet(h http.Handler, token, path string) (int, any) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var body any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	return rec.Code, body
}

// compareThroughAPI lists every collection through the records API of a SQLite
// app (the source) and of a Postgres app (the migrated copy), as the same
// superuser, and requires the same status and the same records.
func compareThroughAPI(t *testing.T, srcDir, pgURL string, normalizeSource ...func(item map[string]any)) {
	t.Helper()

	src := openMigrateApp(t, srcDir, "")
	dstDir := t.TempDir()
	dst := openMigrateApp(t, dstDir, pgURL)

	superuser, err := src.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		superusers, qerr := src.FindAllRecords(core.CollectionNameSuperusers)
		if qerr != nil || len(superusers) == 0 {
			t.Fatalf("no superuser in the source: %v", err)
		}
		superuser = superusers[0]
	}
	// signed with the secret of the source collection: it only works on the
	// copy when the token secrets travelled with the collections
	token, err := superuser.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	srcH, dstH := serveHandler(t, src), serveHandler(t, dst)

	collections, err := src.FindAllCollections()
	if err != nil {
		t.Fatal(err)
	}

	compared, records := 0, 0
	for _, c := range collections {
		path := "/api/collections/" + c.Name + "/records?perPage=500&skipTotal=1"
		if !c.IsView() {
			path += "&sort=@rowid"
		}

		sc, sb := apiGet(srcH, token, path)
		dc, db := apiGet(dstH, token, path)

		if sc != dc {
			t.Errorf("%s: status %d on SQLite, %d on Postgres", c.Name, sc, dc)
			continue
		}
		if sc != http.StatusOK {
			continue
		}

		if sm, ok := sb.(map[string]any); ok {
			items, _ := sm["items"].([]any)
			for _, it := range items {
				if im, ok := it.(map[string]any); ok {
					for _, n := range normalizeSource {
						n(im)
					}
				}
			}
		}

		if !reflect.DeepEqual(sb, db) {
			t.Errorf("%s: the records differ: %s", c.Name, firstDifference(sb, db))
			continue
		}
		compared++
		if m, ok := sb.(map[string]any); ok {
			if items, ok := m["items"].([]any); ok {
				records += len(items)
			}
		}
	}

	if compared < 5 || records < 20 {
		t.Fatalf("only %d collections and %d records were compared", compared, records)
	}
	t.Logf("compared %d collections and %d records through the API", compared, records)
}

func clipTest(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// firstDifference names the first record and field that differ between two list responses.
func firstDifference(a, b any) string {
	am, _ := a.(map[string]any)
	bm, _ := b.(map[string]any)
	ai, _ := am["items"].([]any)
	bi, _ := bm["items"].([]any)
	if len(ai) != len(bi) {
		return fmt.Sprintf("%d records on SQLite, %d on Postgres", len(ai), len(bi))
	}
	for i := range ai {
		ar, _ := ai[i].(map[string]any)
		br, _ := bi[i].(map[string]any)
		for k, av := range ar {
			if !reflect.DeepEqual(av, br[k]) {
				aj, _ := json.Marshal(av)
				bj, _ := json.Marshal(br[k])
				return fmt.Sprintf("record %v field %s: SQLite %s, Postgres %s", ar["id"], k, clipTest(string(aj)), clipTest(string(bj)))
			}
		}
	}
	return "other keys"
}

func fileSums(t testing.TB, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		out[rel] = fmt.Sprintf("%x", sha256.Sum256(b))
		return nil
	})
	return out
}

func diffSums(a, b map[string]string) []string {
	var d []string
	for k, v := range a {
		if b[k] != v {
			d = append(d, "changed or removed: "+k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			d = append(d, "added: "+k)
		}
	}
	sort.Strings(d)
	return d
}

func TestMigrateDBFixture(t *testing.T) {
	target := newMigrateTarget(t)
	dir := sqliteFixtureDir(t)
	before := fileSums(t, dir)

	var out bytes.Buffer
	report, err := MigrateDB(context.Background(), MigrateDBOptions{Dir: dir, TargetURL: target, BatchSize: 7, Out: &out})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}

	if !report.Verified {
		t.Fatal("the migration was not verified")
	}
	if len(report.Tables) < 10 {
		t.Fatalf("expected the system and fixture tables in the report, got %d", len(report.Tables))
	}
	for _, tb := range report.Tables {
		if !tb.IDsMatch || tb.SourceRows != tb.TargetRows && tb.Name != "_collections" {
			t.Errorf("%s: %+v", tb.Name, tb)
		}
	}

	// the source is untouched (nothing was added next to it either)
	after := fileSums(t, dir)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the source directory changed: %v", diffSums(before, after))
	}

	compareThroughAPI(t, dir, target)

	// the marker table is gone and the target is a regular database
	pg := openMigrateApp(t, t.TempDir(), target)
	if pg.HasTable(migrateMarkerTable) {
		t.Fatal("the progress marker table was not dropped")
	}
}

// the verification must notice a copy that is not faithful
func TestMigrateDBVerificationCatchesDamage(t *testing.T) {
	damage := map[string]func(app core.App) error{
		"a deleted row": func(app core.App) error {
			_, err := app.DB().NewQuery("DELETE FROM {{_superusers}} WHERE [[id]] = (SELECT [[id]] FROM {{_superusers}} LIMIT 1)").Execute()
			return err
		},
		"a changed value": func(app core.App) error {
			_, err := app.DB().NewQuery("UPDATE {{demo1}} SET [[text]] = [[text]] || 'x'").Execute()
			return err
		},
		"a changed flag": func(app core.App) error {
			_, err := app.DB().NewQuery("UPDATE {{demo1}} SET [[bool]] = NOT [[bool]]").Execute()
			return err
		},
		"an extra row": func(app core.App) error {
			_, err := app.DB().NewQuery("INSERT INTO {{demo1}} ([[id]]) VALUES ('extraextraextr')").Execute()
			return err
		},
	}

	for name, fn := range damage {
		t.Run(name, func(t *testing.T) {
			target := newMigrateTarget(t)
			dir := sqliteFixtureDir(t)

			_, err := MigrateDB(context.Background(), MigrateDBOptions{
				Dir: dir, TargetURL: target,
				beforeVerify: func(dst core.App) error { return fn(dst) },
			})
			if err == nil || !strings.Contains(err.Error(), "verification FAILED") {
				t.Fatalf("expected the verification to fail, got %v", err)
			}
		})
	}
}

func TestMigrateDBDryRun(t *testing.T) {
	target := newMigrateTarget(t)
	dir := sqliteFixtureDir(t)
	before := fileSums(t, dir)

	report, err := MigrateDB(context.Background(), MigrateDBOptions{Dir: dir, TargetURL: target, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !report.DryRun || len(report.Tables) == 0 {
		t.Fatalf("unexpected report: %+v", report)
	}

	if !reflect.DeepEqual(before, fileSums(t, dir)) {
		t.Fatal("the dry run changed the source")
	}

	db, err := sql.Open("pgx", target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the dry run created %d table(s)", n)
	}
}

func TestMigrateDBRefusesNonEmptyTarget(t *testing.T) {
	target := newMigrateTarget(t)
	dir := sqliteFixtureDir(t)

	db, err := sql.Open("pgx", target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE something (id TEXT)`); err != nil {
		t.Fatal(err)
	}

	_, err = MigrateDB(context.Background(), MigrateDBOptions{Dir: dir, TargetURL: target})
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("expected a non-empty target error, got %v", err)
	}

	// --resume does not make it acceptable: nothing is in progress there
	_, err = MigrateDB(context.Background(), MigrateDBOptions{Dir: dir, TargetURL: target, Resume: true})
	if err == nil || !strings.Contains(err.Error(), "no migration is in progress") {
		t.Fatalf("expected a nothing-to-resume error, got %v", err)
	}

	// and it left the table alone
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected the target to be untouched, it has %d tables", n)
	}
}

func TestMigrateDBRefusesLiveSource(t *testing.T) {
	target := newMigrateTarget(t)
	dir := sqliteFixtureDir(t)

	// a rollout handover is in progress
	if err := os.WriteFile(filepath.Join(dir, handoverFileName), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := MigrateDB(context.Background(), MigrateDBOptions{Dir: dir, TargetURL: target})
	if err == nil || !strings.Contains(err.Error(), "refusing to run") {
		t.Fatalf("expected a refusal, got %v", err)
	}

	// the same through a held single-writer lock
	_ = os.Remove(filepath.Join(dir, handoverFileName))
	lock := filepath.Join(dir, singleWriterLockName)
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	held, release, err := tryLockFile(lock)
	if err != nil || held {
		t.Skipf("cannot take the lock here: %v", err)
	}
	defer release()

	_, err = MigrateDB(context.Background(), MigrateDBOptions{Dir: dir, TargetURL: target})
	if err == nil || !strings.Contains(err.Error(), "single-writer lock") {
		t.Fatalf("expected a refusal because of the lock, got %v", err)
	}

	// nothing was written to the target
	db, err := sql.Open("pgx", target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).Scan(&n)
	if n != 0 {
		t.Fatalf("the refused run created %d table(s)", n)
	}

	// --offline-ok is the operator's statement that no server runs
	release()
	if err := os.WriteFile(filepath.Join(dir, handoverFileName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateDB(context.Background(), MigrateDBOptions{Dir: dir, TargetURL: target, OfflineOK: true}); err != nil {
		t.Fatalf("--offline-ok: %v", err)
	}
}

func TestMigrateDBResume(t *testing.T) {
	target := newMigrateTarget(t)
	dir := sqliteFixtureDir(t)

	errStop := errors.New("simulated interruption")
	stopAfter := 3
	batches := 0

	_, err := MigrateDB(context.Background(), MigrateDBOptions{
		Dir: dir, TargetURL: target, BatchSize: 2,
		afterBatch: func(table string, batch int) error {
			batches++
			if batches == stopAfter {
				return errStop
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), errStop.Error()) {
		t.Fatalf("expected the simulated interruption, got %v", err)
	}

	// without --resume the half-filled target is refused, with a pointer to --resume
	_, err = MigrateDB(context.Background(), MigrateDBOptions{Dir: dir, TargetURL: target})
	if err == nil || !strings.Contains(err.Error(), "--resume") {
		t.Fatalf("expected a refusal that names --resume, got %v", err)
	}

	// a different source cannot be resumed into it
	other := sqliteFixtureDir(t)
	app := openMigrateApp(t, other, "")
	if _, err := app.DB().NewQuery("INSERT INTO _params (id, value, created, updated) VALUES ('zz', '1', '', '')").Execute(); err != nil {
		t.Fatal(err)
	}
	terminateMigrateApp(app)
	_, err = MigrateDB(context.Background(), MigrateDBOptions{Dir: other, TargetURL: target, Resume: true})
	if err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("expected a changed source error, got %v", err)
	}

	// interrupt a second time, later, then finish
	batches, stopAfter = 0, 9
	_, err = MigrateDB(context.Background(), MigrateDBOptions{
		Dir: dir, TargetURL: target, BatchSize: 2, Resume: true,
		afterBatch: func(table string, batch int) error {
			batches++
			if batches == stopAfter {
				return errStop
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), errStop.Error()) {
		t.Fatalf("expected the second interruption, got %v", err)
	}

	report, err := MigrateDB(context.Background(), MigrateDBOptions{Dir: dir, TargetURL: target, BatchSize: 2, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified || !report.Resumed {
		t.Fatalf("unexpected report %+v", report)
	}

	compareThroughAPI(t, dir, target)
}

func TestMigrateDBRefusesOldSource(t *testing.T) {
	target := newMigrateTarget(t)

	// the raw fixture predates the system migrations of this version
	dir, err := tests.TempDirClone(filepath.Join("..", "tests", "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	_, err = MigrateDB(context.Background(), MigrateDBOptions{Dir: dir, TargetURL: target})
	if err == nil || !strings.Contains(err.Error(), "older than this version") {
		t.Fatalf("expected an outdated source error, got %v", err)
	}
}

func TestMigrateDBArguments(t *testing.T) {
	dir := t.TempDir()

	for name, opts := range map[string]MigrateDBOptions{
		"no target":     {Dir: dir},
		"not a pg url":  {Dir: dir, TargetURL: "mysql://x"},
		"no database":   {Dir: dir, TargetURL: "postgres://u:p@localhost:1/x"},
		"no dir at all": {Dir: filepath.Join(dir, "missing"), TargetURL: "postgres://u:p@localhost:1/x"},
	} {
		if _, err := MigrateDB(context.Background(), opts); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	// the password stays out of the error
	_, err := MigrateDB(context.Background(), MigrateDBOptions{Dir: sqliteFixtureDirNoPG(t), TargetURL: "postgres://u:s3cretpw@127.0.0.1:1/x"})
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "s3cretpw") {
		t.Fatalf("the error leaks the password: %v", err)
	}
}

// sqliteFixtureDirNoPG is sqliteFixtureDir for tests that need no server.
func sqliteFixtureDirNoPG(t testing.TB) string {
	return sqliteFixtureDir(t)
}

func TestAdaptSQLiteValue(t *testing.T) {
	cases := []struct {
		in   any
		typ  string
		want any
	}{
		{nil, "boolean", nil},
		{int64(1), "boolean", true},
		{int64(0), "boolean", false},
		{"1", "boolean", true},
		{"", "jsonb", nil},
		{"[1]", "jsonb", "[1]"},
		{[]byte(`{"a":1}`), "jsonb", `{"a":1}`},
		{"12.5", "double precision", 12.5},
		{int64(3), "double precision", int64(3)},
		{"abc", "double precision", "abc"},
		{int64(42), "text", "42"},
		{3.5, "text", "3.5"},
		{[]byte("x"), "text", "x"},
		{"2024-01-02 03:04:05.000Z", "text", "2024-01-02 03:04:05.000Z"},
	}
	for _, c := range cases {
		if got := adaptSQLiteValue(c.in, c.typ); !reflect.DeepEqual(got, c.want) {
			t.Errorf("adapt(%#v, %s) = %#v, want %#v", c.in, c.typ, got, c.want)
		}
	}
}

func TestSampleIndexes(t *testing.T) {
	if got := sampleIndexes(0, 20); got != nil {
		t.Fatal(got)
	}
	if got := sampleIndexes(3, 20); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Fatal(got)
	}
	got := sampleIndexes(1000, 20)
	if len(got) != 20 || got[0] != 0 || got[19] != 999 || !sort.IntsAreSorted(got) {
		t.Fatal(got)
	}
}

var _ = io.Discard

// ---- a database with every fork feature populated ----------------------------------------

func randomKEK(t testing.TB) {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAVIGER_KEK", base64.StdEncoding.EncodeToString(b))
	t.Setenv("CLAVIGER_KEK_FILE", "")
}

// fillerValue returns a valid-looking value for the field, or false to leave it out.
func fillerValue(f core.Field, n int) (any, bool) {
	switch ff := f.(type) {
	case *core.TextField:
		if f.GetName() == "id" {
			return nil, false
		}
		return fmt.Sprintf("%s %d", f.GetName(), n), true
	case *core.EditorField:
		return fmt.Sprintf("<p>%s %d</p>", f.GetName(), n), true
	case *core.NumberField:
		return float64(n) + 0.5, true
	case *core.BoolField:
		return n%2 == 0, true
	case *core.EmailField:
		return fmt.Sprintf("filler%d@example.com", n), true
	case *core.URLField:
		return fmt.Sprintf("https://example.com/%d", n), true
	case *core.DateField:
		return fmt.Sprintf("2024-0%d-1%d 10:00:00.000Z", n%9+1, n%9), true
	case *core.SelectField:
		if len(ff.Values) > 0 {
			return ff.Values[n%len(ff.Values)], true
		}
	case *core.JSONField:
		return map[string]any{"n": n, "list": []any{1, "two", nil, true}, "é": "ü"}, true
	case *core.GeoPointField:
		return map[string]any{"lon": 1.5 + float64(n), "lat": 2.5}, true
	case *core.RelationField:
		if ff.Required {
			return security.RandomString(15), true
		}
	case *core.EncryptedTextField:
		return fmt.Sprintf("secret text %d", n), true
	case *core.EncryptedJSONField:
		return map[string]any{"secret": n}, true
	}
	return nil, false
}

// populateForkDB fills a freshly bootstrapped SQLite app with data in every
// table: real flows where there is a constructor (users, field encryption,
// sessions), and filler rows for the rest of the system collections (audit log
// entries are written by the record hooks).
func populateForkDB(t *testing.T, app core.App) {
	t.Helper()

	// a user, with a session
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	user := core.NewRecord(users)
	user.SetEmail("member@example.com")
	user.SetPassword("1234567890")
	if err := app.Save(user); err != nil {
		t.Fatal(err)
	}
	if _, err := core.CreateSession(app, user, "password", "agent/1.0", "10.0.0.1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// field encryption: ciphertext and the wrapped data keys
	people := core.NewBaseCollection("people")
	people.Fields.Add(
		&core.TextField{Name: "name"},
		&core.EncryptedTextField{Name: "phone", Max: 50},
		&core.EncryptedJSONField{Name: "notes"},
	)
	if err := app.Save(people); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		r := core.NewRecord(people)
		r.Set("name", fmt.Sprintf("person %d", i))
		r.Set("phone", fmt.Sprintf("+1 555 000 %04d", i))
		r.Set("notes", map[string]any{"i": i, "ü": "é"})
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	// edge values of a user collection
	edge := core.NewBaseCollection("edge")
	edge.Fields.Add(
		&core.TextField{Name: "txt"},
		&core.NumberField{Name: "num"},
		&core.BoolField{Name: "flag"},
		&core.JSONField{Name: "js"},
		&core.SelectField{Name: "tags", Values: []string{"a", "b", "c"}, MaxSelect: 3},
		&core.DateField{Name: "day"},
		&core.GeoPointField{Name: "pt"},
	)
	edge.AddIndex("idx_edge_txt", true, "txt", "txt != ''")
	if err := app.Save(edge); err != nil {
		t.Fatal(err)
	}
	for i, txt := range []string{"plain", "ünïcödé 日本語 🙂", "", "quote ' and \" and \\ and `tick`", strings.Repeat("long ", 900)} {
		r := core.NewRecord(edge)
		r.Set("txt", txt+fmt.Sprint(i))
		r.Set("num", []float64{0, -1.25, 1e21, 3, 0.1}[i])
		r.Set("flag", i%2 == 0)
		r.Set("js", []any{map[string]any{"a": []any{}}, "text", 12, nil, true}[i])
		r.Set("tags", []string{"a", "c"}[:i%3])
		r.Set("day", fmt.Sprintf("2020-0%d-15 08:30:00.000Z", i+1))
		r.Set("pt", map[string]any{"lon": 10.5, "lat": -20.25})
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	// SQLite's loose typing: values the typed fields would never write
	for _, q := range []string{
		"UPDATE edge SET js = '' WHERE rowid = 1",
		"UPDATE edge SET num = '12.50' WHERE rowid = 2",
		"UPDATE edge SET txt = 77 WHERE rowid = 3",
	} {
		if _, err := app.DB().NewQuery(q).Execute(); err != nil {
			t.Fatal(err)
		}
	}

	// the rest of the system collections: filler rows
	collections, err := app.FindAllCollections()
	if err != nil {
		t.Fatal(err)
	}
	filled := 0
	for _, c := range collections {
		if c.IsView() || c.Name == core.CollectionNameAuditLog || c.Name == core.CollectionNameFieldKeys {
			continue
		}
		var n int
		_ = app.DB().Select("count(*)").From(c.Name).Row(&n)
		if n > 0 {
			continue
		}
		for i := 1; i <= 2; i++ {
			r := core.NewRecord(c)
			for _, f := range c.Fields {
				if v, ok := fillerValue(f, i); ok {
					r.Set(f.GetName(), v)
				}
			}
			if c.IsAuth() {
				r.SetEmail(fmt.Sprintf("%s%d@example.com", strings.TrimPrefix(c.Name, "_"), i))
				r.SetPassword("1234567890")
			}
			if err := app.SaveNoValidate(r); err != nil {
				t.Logf("filler row of %s: %v", c.Name, err)
				continue
			}
			filled++
		}
	}
	t.Logf("%d filler rows", filled)

	// every fork table must now hold data
	for _, name := range []string{
		core.CollectionNameAuditLog, core.CollectionNameSessions, core.CollectionNameFieldKeys,
		core.CollectionNameAPIKeys, core.CollectionNameWebhooks, core.CollectionNameOAuthClients,
		core.CollectionNameOrgs, core.CollectionNameOrgMembers, core.CollectionNameInvitations,
	} {
		var n int
		if err := app.DB().Select("count(*)").From(name).Row(&n); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n == 0 {
			t.Errorf("the populated database has no rows in %s", name)
		}
	}
}

func TestMigrateDBForkFeatures(t *testing.T) {
	randomKEK(t)

	dir := t.TempDir()
	{
		app := openMigrateApp(t, dir, "")
		populateForkDB(t, app)
		terminateMigrateApp(app)
	}

	target := newMigrateTarget(t)

	var out bytes.Buffer
	report, err := MigrateDB(context.Background(), MigrateDBOptions{Dir: dir, TargetURL: target, BatchSize: 3, Out: &out})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !report.Verified {
		t.Fatal("not verified")
	}

	// the one documented adaptation: an empty string in a JSON column (only
	// possible by writing SQLite directly) becomes NULL
	compareThroughAPI(t, dir, target, func(item map[string]any) {
		if v, ok := item["js"]; ok && v == "" && item["collectionName"] == "edge" {
			item["js"] = nil
		}
	})

	// the encrypted values: the wrapped data keys travelled, the KEK stays configured
	t.Setenv(core.DBURLEnv, target)
	msg, err := CheckFieldKeys(context.Background(), dir, "", os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(msg, "OK (") || strings.Contains(msg, "no encrypted fields") {
		t.Fatalf("unexpected keys check result on the migrated database: %s", msg)
	}

	pg := openMigrateApp(t, t.TempDir(), target)
	people, err := pg.FindAllRecords("people")
	if err != nil || len(people) != 3 {
		t.Fatalf("people: %v %d", err, len(people))
	}
	for i, p := range people {
		if got, want := p.GetString("phone"), fmt.Sprintf("+1 555 000 %04d", i); got != want {
			t.Fatalf("decrypted phone %q, want %q", got, want)
		}
	}

	// the user can sign in and the session is valid on the copy
	user, err := pg.FindAuthRecordByEmail("users", "member@example.com")
	if err != nil || !user.ValidatePassword("1234567890") {
		t.Fatalf("the user did not travel: %v", err)
	}
	var sid string
	if err := pg.DB().Select("id").From(core.CollectionNameSessions).Limit(1).Row(&sid); err != nil {
		t.Fatal(err)
	}
	if err := core.ValidateSession(pg, sid, user.Collection().Id, user.Id); err != nil {
		t.Fatalf("session: %v", err)
	}

	// the identity columns work after the copy: new rows come after the copied ones
	r := core.NewRecord(mustCollection(t, pg, "edge"))
	r.Set("txt", "after the migration")
	if err := pg.Save(r); err != nil {
		t.Fatal(err)
	}
	last, err := pg.FindRecordsByFilter("edge", "1=1", "-@rowid", 1, 0)
	if err != nil || len(last) != 1 || last[0].Id != r.Id {
		t.Fatalf("a new record must come last in insertion order: %v", err)
	}
}

func mustCollection(t testing.TB, app core.App, name string) *core.Collection {
	t.Helper()
	c, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMigrateDBSkipViews(t *testing.T) {
	target := newMigrateTarget(t)
	dir := sqliteFixtureDir(t)

	report, err := MigrateDB(context.Background(), MigrateDBOptions{Dir: dir, TargetURL: target, SkipViews: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SkippedViews) == 0 {
		t.Fatal("the fixture has view collections: expected them in the report")
	}

	pg := openMigrateApp(t, t.TempDir(), target)
	for _, name := range report.SkippedViews {
		if _, err := pg.FindCollectionByNameOrId(name); err == nil {
			t.Errorf("view %s must not be in the target", name)
		}
	}
}

func TestMigrateDBCommand(t *testing.T) {
	target := newMigrateTarget(t)
	dir := sqliteFixtureDir(t)

	app := core.NewBaseApp(core.BaseAppConfig{DataDir: dir})
	command := NewMigrateDBCommand(app)

	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"--to", target, "--dry-run", "--verbose"})
	if err := command.Execute(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "dry run: nothing was written") || !strings.Contains(out.String(), "would copy") {
		t.Fatalf("unexpected dry run output:\n%s", out.String())
	}

	out.Reset()
	command = NewMigrateDBCommand(app)
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"--to", target, "--batch", "50"})
	if err := command.Execute(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "verified") || strings.Contains(out.String(), target) {
		t.Fatalf("unexpected output:\n%s", out.String())
	}

	// the target is now used: a second run refuses
	command = NewMigrateDBCommand(app)
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"--to", target})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("expected a refusal, got %v", err)
	}
}
