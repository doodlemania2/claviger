package tests

// Fork-local addition (issue #106, M4): run the regular test apps on PostgreSQL.
//
// With CLAVIGER_TEST_DB=postgres and CLAVIGER_TEST_PG_URL=postgres://... (a
// server on which the role may CREATE DATABASE), NewTestApp and
// NewTestAppWithConfig keep the default fixture (tests/data) but store the app
// data in a throwaway Postgres database instead of data.db.
//
// The SQLite fixture file cannot be used by Postgres, and a hand written copy
// of it would drift from upstream's. So the fixture is converted instead, once
// per test process:
//
//  1. the fixture is opened as a regular SQLite app (the source of truth),
//  2. its collections are imported into a fresh Postgres app (so the import path
//     builds the tables, indexes and views for the Postgres dialect), and
//  3. the rows of every table are copied over, with the values adapted to the
//     column types (SQLite is dynamically typed, 0/1 booleans become booleans...).
//
// The result is a template database. Every test app is then a
// "CREATE DATABASE ... TEMPLATE" clone of it (a file copy, a few ms), dropped
// again in Cleanup.
//
// Tests whose expectations are SQLite specific are skipped on Postgres
// through [SkipOnPostgres] (or listed in docs/datastores.md, not silently).

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/security"
)

// Environment variables that select the Postgres test mode.
const (
	// TestDBEnv selects the engine of the test apps ("postgres" or empty/"sqlite").
	TestDBEnv = "CLAVIGER_TEST_DB"
	// TestPGURLEnv is the admin connection URL of the Postgres test server.
	TestPGURLEnv = "CLAVIGER_TEST_PG_URL"
)

// IsPostgres reports whether the test apps run on PostgreSQL
// (CLAVIGER_TEST_DB=postgres).
func IsPostgres() bool {
	return strings.EqualFold(os.Getenv(TestDBEnv), "postgres")
}

// SkipOnPostgres skips the test when the test apps run on PostgreSQL.
//
// The reason is mandatory: it ends up in the CI log and the skips are
// counted (see the "postgres" job in .github/workflows/test.yml).
// Use it only for documented gaps and SQLite specific expectations.
func SkipOnPostgres(t testing.TB, reason string) {
	t.Helper()

	if reason == "" {
		t.Fatal("SkipOnPostgres needs a reason")
	}

	if IsPostgres() {
		t.Skip("known_gap(postgres): " + reason)
	}
}

type testPostgres struct {
	adminURL string
	tplName  string
	cloneMu  sync.Mutex
}

var (
	testPGOnce sync.Once
	testPG     *testPostgres
	testPGErr  error
)

// pgDBs tracks the databases of the running test apps by data dir, so that a
// restarted app (NewTestApp(app.DataDir())) reuses the database of the
// original one. A database is dropped when its last app is cleaned up.
type pgDBs struct {
	mu    sync.Mutex
	byDir map[string]string // app data dir => database name
	refs  map[string]int    // database name => running apps
}

var testPGDBs = &pgDBs{byDir: map[string]string{}, refs: map[string]int{}}

func (r *pgDBs) acquire(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs[name]++
}

func (r *pgDBs) release(name, dir string) func() {
	return func() {
		r.mu.Lock()
		delete(r.byDir, dir)
		r.refs[name]--
		last := r.refs[name] <= 0
		if last {
			delete(r.refs, name)
		}
		r.mu.Unlock()

		if last {
			dropDatabase(testPG.adminURL, name)
		}
	}
}

// preparePostgres switches config to a fresh clone of the Postgres template
// database (or, for a restart of a running app, to its database). It returns
// a cleanup func that releases it (nil if the app stays on SQLite).
//
// srcDataDir is the data dir that the caller passed (empty for the default
// fixture) and dataDir the temp dir that the app will use.
func preparePostgres(config *core.BaseAppConfig, srcDataDir, dataDir string) (func(), error) {
	if !IsPostgres() || config.DBConnect != nil {
		return nil, nil
	}

	var name string

	if srcDataDir == "" {
		testPGOnce.Do(func() {
			testPG, testPGErr = buildPostgresTemplate()
		})
		if testPGErr != nil {
			// many tests ignore the NewTestApp error and would only fail with a nil pointer panic
			panic(fmt.Sprintf("postgres test template: %v", testPGErr))
		}

		var err error
		if name, err = testPG.clone(); err != nil {
			return nil, err
		}
	} else {
		testPGDBs.mu.Lock()
		name = testPGDBs.byDir[srcDataDir]
		testPGDBs.mu.Unlock()

		if name == "" {
			return nil, nil // a custom data dir of a SQLite app
		}
	}

	testPGDBs.acquire(name)
	testPGDBs.mu.Lock()
	testPGDBs.byDir[dataDir] = name
	testPGDBs.mu.Unlock()

	config.DBConnect = core.PostgresDBConnect(withDatabase(testPG.adminURL, name))
	config.AuxDBConnect = core.DefaultDBConnect
	limitTestPGConns(config)

	return testPGDBs.release(name, dataDir), nil
}

// limitTestPGConns keeps the pool of a test app small: the default (120)
// times the parallel tests would exceed the max_connections of a stock server.
func limitTestPGConns(config *core.BaseAppConfig) {
	if config.DataMaxOpenConns <= 0 {
		config.DataMaxOpenConns = 6
	}
	if config.DataMaxIdleConns <= 0 {
		config.DataMaxIdleConns = 2
	}
}

func withDatabase(rawURL, name string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	u.Path = "/" + name
	return u.String()
}

func randomDBName(prefix string) string {
	return prefix + strings.ToLower(security.PseudorandomString(10))
}

func adminExec(adminURL string, query string) error {
	db, err := sql.Open("pgx", adminURL)
	if err != nil {
		return err
	}
	defer db.Close()

	_, err = db.Exec(query)
	return err
}

func dropDatabase(adminURL, name string) {
	_ = adminExec(adminURL, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
}

func (p *testPostgres) clone() (string, error) {
	// a template can't be accessed by two CREATE DATABASE at the same time
	p.cloneMu.Lock()
	defer p.cloneMu.Unlock()

	name := randomDBName("pbt_")

	var err error
	for range 20 {
		err = adminExec(p.adminURL, fmt.Sprintf(`CREATE DATABASE "%s" TEMPLATE "%s"`, name, p.tplName))
		if err == nil {
			return name, nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	return "", fmt.Errorf("clone template: %w", err)
}

// buildPostgresTemplate converts the tests/data fixture (see the file comment).
func buildPostgresTemplate() (*testPostgres, error) {
	adminURL := os.Getenv(TestPGURLEnv)
	if adminURL == "" {
		return nil, fmt.Errorf("%s is not set", TestPGURLEnv)
	}

	// templates leaked by crashed runs
	dropStaleTemplates(adminURL)

	p := &testPostgres{adminURL: adminURL, tplName: randomDBName("pbtpl_")}

	if err := adminExec(adminURL, `CREATE DATABASE "`+p.tplName+`"`); err != nil {
		return nil, err
	}
	_ = adminExec(adminURL, fmt.Sprintf(`COMMENT ON DATABASE "%s" IS '%d'`, p.tplName, time.Now().Unix()))

	if err := p.fill(); err != nil {
		dropDatabase(adminURL, p.tplName)
		return nil, err
	}

	return p, nil
}

func dropStaleTemplates(adminURL string) {
	db, err := sql.Open("pgx", adminURL)
	if err != nil {
		return
	}
	defer db.Close()

	rows, err := db.Query(`SELECT datname FROM pg_database
		WHERE datname LIKE 'pbtpl\_%' AND shobj_description(oid, 'pg_database') ~ '^[0-9]+$'
		AND shobj_description(oid, 'pg_database')::bigint < $1`, time.Now().Add(-30*time.Minute).Unix())
	if err != nil {
		return
	}
	var stale []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			stale = append(stale, n)
		}
	}
	rows.Close()

	for _, n := range stale {
		dropDatabase(adminURL, n)
	}
}

func (p *testPostgres) fill() (err error) {
	_, currentFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(path.Dir(currentFile), "data")

	// 1. the SQLite fixture
	srcDir, err := TempDirClone(fixtureDir)
	if err != nil {
		return err
	}
	defer os.RemoveAll(srcDir)

	src := core.NewBaseApp(core.BaseAppConfig{
		DataDir: srcDir,
		// no settings encryption: the template must not depend on the env of the first test
		EncryptionEnv: "",
		DBConnect:     core.DefaultDBConnect,
	})
	if err := src.Bootstrap(); err != nil {
		return fmt.Errorf("open fixture: %w", err)
	}
	defer src.ClearBootstrap()
	if err := src.RunAllMigrations(); err != nil {
		return fmt.Errorf("fixture migrations: %w", err)
	}

	// 2. an empty Postgres app (bootstrap runs the system migrations)
	dstDir, err := os.MkdirTemp("", "pb_test_pgtpl_*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dstDir)

	dstConfig := core.BaseAppConfig{
		DataDir: dstDir,
		// no settings encryption: the template must not depend on the env of the first test
		EncryptionEnv: "",
		DBConnect:     core.PostgresDBConnect(withDatabase(p.adminURL, p.tplName)),
		AuxDBConnect:  core.DefaultDBConnect,
	}
	limitTestPGConns(&dstConfig)
	dst := core.NewBaseApp(dstConfig)
	if err := dst.Bootstrap(); err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer dst.ClearBootstrap()

	// import the collections from the raw _collections rows: the public JSON
	// of a collection hides the secrets (token secrets, OAuth2 client secrets)
	// that the fixture's tokens and requests depend on
	data, err := rawCollections(src)
	if err != nil {
		return err
	}
	if err := dst.ImportCollections(data, false); err != nil {
		return fmt.Errorf("import collections: %w", err)
	}

	collections, err := src.FindAllCollections()
	if err != nil {
		return err
	}

	// 3. copy the rows (the raw _collections rows too: the public JSON of a
	// collection hides the token secrets that the fixture's tokens are signed with)
	return copyFixtureRows(src, dst, collections)
}

// rawCollections returns the collections as import payload, built from the
// stored rows (the "options" are flattened like in the collection JSON).
func rawCollections(app core.App) ([]map[string]any, error) {
	rows, err := app.DB().NewQuery("SELECT * FROM _collections ORDER BY rowid").Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	jsonCols := map[string]bool{"fields": true, "indexes": true, "options": true}

	var result []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}

		m := map[string]any{}
		for i, col := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if jsonCols[col] {
				var decoded any
				if err := json.Unmarshal([]byte(fmt.Sprint(v)), &decoded); err != nil {
					return nil, fmt.Errorf("collection %v %s: %w", m["name"], col, err)
				}
				v = decoded
			}
			if col == "system" {
				v = v == int64(1) || v == true
			}
			m[col] = v
		}

		if opts, ok := m["options"].(map[string]any); ok {
			for k, v := range opts {
				m[k] = v
			}
		}
		delete(m, "options")

		result = append(result, m)
	}

	return result, rows.Err()
}

func copyFixtureRows(src, dst core.App, collections []*core.Collection) error {
	d := core.DialectOf(dst)

	tables := []string{"_collections", "_params"}
	for _, c := range collections {
		if c.IsView() {
			continue
		}
		tables = append(tables, c.Name)
	}

	return dst.RunInTransaction(func(txApp core.App) error {
		for _, table := range tables {
			dstCols, err := d.TableInfo(txApp.DB(), table)
			if err != nil {
				return fmt.Errorf("%s: %w", table, err)
			}
			colTypes := make(map[string]string, len(dstCols))
			for _, c := range dstCols {
				colTypes[c.Name] = strings.ToLower(c.Type)
			}

			rows, err := src.DB().NewQuery("SELECT * FROM {{" + table + "}} ORDER BY rowid").Rows()
			if err != nil {
				return fmt.Errorf("%s: %w", table, err)
			}

			if _, err := txApp.DB().NewQuery("DELETE FROM {{" + table + "}}").Execute(); err != nil {
				rows.Close()
				return fmt.Errorf("%s: %w", table, err)
			}

			cols, _ := rows.Columns()
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					rows.Close()
					return err
				}

				params := dbx.Params{}
				for i, col := range cols {
					typ, ok := colTypes[col]
					if !ok {
						continue // a column that the Postgres table doesn't have
					}
					params[col] = adaptFixtureValue(vals[i], typ)
				}

				if _, err := txApp.DB().Insert(table, params).Execute(); err != nil {
					rows.Close()
					return fmt.Errorf("copy %s row: %w", table, err)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}

		return nil
	})
}

// adaptFixtureValue converts a SQLite value for a column of the Postgres type.
func adaptFixtureValue(v any, pgType string) any {
	if v == nil {
		return nil
	}

	if b, ok := v.([]byte); ok {
		v = string(b)
	}

	switch {
	case pgType == "boolean":
		switch x := v.(type) {
		case int64:
			return x != 0
		case float64:
			return x != 0
		case string:
			return x == "1" || strings.EqualFold(x, "true")
		}
	case pgType == "jsonb" || pgType == "json":
		if s, ok := v.(string); ok && s == "" {
			return nil
		}
		return fmt.Sprint(v)
	case strings.Contains(pgType, "double") || pgType == "real":
		switch x := v.(type) {
		case string:
			var f float64
			if _, err := fmt.Sscan(x, &f); err == nil {
				return f
			}
		}
	case pgType == "text" || strings.HasPrefix(pgType, "character"):
		if _, ok := v.(string); !ok {
			return fmt.Sprint(v)
		}
	}

	return v
}

// CompactJSON removes the insignificant whitespace of a JSON text.
//
// A JSON value read back from Postgres (JSONB) is rendered with a space after
// the ":" and "," separators, SQLite returns the text as it was stored. Tests
// that look for a fragment of a stored JSON text compare the compact form so
// that they hold on both. Invalid JSON is returned unchanged.
func CompactJSON(s string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return s
	}
	return buf.String()
}

// PostgresURL returns the connection URL of the Postgres database of the test
// app, or an empty string if the app runs on SQLite (for tests that open the
// same database again, like the CLI commands do from the environment).
func (t *TestApp) PostgresURL() string {
	testPGDBs.mu.Lock()
	name := testPGDBs.byDir[t.DataDir()]
	testPGDBs.mu.Unlock()

	if name == "" {
		return ""
	}

	return withDatabase(testPG.adminURL, name)
}
