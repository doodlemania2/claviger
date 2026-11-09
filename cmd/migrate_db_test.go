package cmd

// Fork-local (Claviger #106, M5): tests of `migrate-db`.
//
// They need a PostgreSQL server (CLAVIGER_TEST_PG_URL, a role that may CREATE
// DATABASE) and are skipped without one. scripts/pgembedded starts one.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
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
	t.Cleanup(func() { _ = app.ClearBootstrap() })

	return app
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
func compareThroughAPI(t *testing.T, srcDir, pgURL string) {
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

		if !reflect.DeepEqual(sb, db) {
			sj, _ := json.MarshalIndent(sb, "", " ")
			dj, _ := json.MarshalIndent(db, "", " ")
			t.Errorf("%s: the records differ\nSQLite:   %s\nPostgres: %s", c.Name, clipTest(string(sj)), clipTest(string(dj)))
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
	if len(s) > 3000 {
		return s[:3000] + "..."
	}
	return s
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
	_ = app.ClearBootstrap()
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
