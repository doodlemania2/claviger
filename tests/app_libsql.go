package tests

// Fork-local addition (issue #106, experimental): run the regular test apps on
// a libSQL server (sqld).
//
// With CLAVIGER_TEST_DB=libsql, CLAVIGER_TEST_LIBSQL_URL=http://127.0.0.1:8080
// and CLAVIGER_TEST_LIBSQL_ADMIN_URL=http://127.0.0.1:8081 (a sqld started with
// SQLD_ENABLE_NAMESPACES=true and SQLD_ADMIN_LISTEN_ADDR, see docs/datastores.md),
// the default fixture (tests/data) is converted once per test process into a
// template namespace (same conversion as the Postgres mode), and every test app
// is a namespace forked from it (a file copy) that is deleted again in Cleanup.
//
// A namespace is addressed through the first label of the Host header, so the
// connection goes to the server address (the "proxy") with the namespace
// host name in the URL.

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/dialect"
	"github.com/doodlemania2/claviger/tools/security"
	"github.com/tursodatabase/libsql-client-go/libsql"
)

// Environment variables of the libSQL test mode (TestDBEnv selects it with "libsql").
const (
	TestLibSQLURLEnv      = "CLAVIGER_TEST_LIBSQL_URL"
	TestLibSQLAdminURLEnv = "CLAVIGER_TEST_LIBSQL_ADMIN_URL"
)

// IsLibSQL reports whether the test apps run on libSQL (CLAVIGER_TEST_DB=libsql).
func IsLibSQL() bool {
	return strings.EqualFold(os.Getenv(TestDBEnv), "libsql")
}

// SkipOnLibSQL skips the test when the test apps run on libSQL. The reason is mandatory.
func SkipOnLibSQL(t testing.TB, reason string) {
	t.Helper()

	if reason == "" {
		t.Fatal("SkipOnLibSQL needs a reason")
	}

	if IsLibSQL() {
		t.Skip("known_gap(libsql): " + reason)
	}
}

type testLibSQL struct {
	url      string
	adminURL string
	tplName  string
	cloneMu  sync.Mutex
}

var (
	testLibSQLOnce sync.Once
	testLS         *testLibSQL
	testLSErr      error

	testLSDBs = &pgDBs{byDir: map[string]string{}, refs: map[string]int{}}
)

// libsqlNamespaceConnect opens the namespace of the test server.
func libsqlNamespaceConnect(serverURL, namespace string) core.DBConnectFunc {
	return func(_ string) (*dbx.DB, error) {
		connector, err := libsql.NewConnector("http://"+namespace+".libsql.test", libsql.WithProxy(serverURL))
		if err != nil {
			return nil, err
		}

		sqlDB := sql.OpenDB(connector)
		if err := sqlDB.Ping(); err != nil {
			_ = sqlDB.Close()
			return nil, err
		}

		return dbx.NewFromDB(sqlDB, dialect.LibSQLName), nil
	}
}

func libsqlAdmin(method, adminURL, path string) error {
	req, err := http.NewRequest(method, strings.TrimRight(adminURL, "/")+path, strings.NewReader("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode >= 300 {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

func buildLibSQLTemplate() (*testLibSQL, error) {
	p := &testLibSQL{url: os.Getenv(TestLibSQLURLEnv), adminURL: os.Getenv(TestLibSQLAdminURLEnv)}
	if p.url == "" || p.adminURL == "" {
		return nil, fmt.Errorf("%s and %s must be set", TestLibSQLURLEnv, TestLibSQLAdminURLEnv)
	}

	p.tplName = "tpl" + strings.ToLower(security.PseudorandomString(10))
	if err := libsqlAdmin(http.MethodPost, p.adminURL, "/v1/namespaces/"+p.tplName+"/create"); err != nil {
		return nil, err
	}

	if err := fillFixture(libsqlNamespaceConnect(p.url, p.tplName)); err != nil {
		_ = libsqlAdmin(http.MethodDelete, p.adminURL, "/v1/namespaces/"+p.tplName)
		return nil, err
	}

	return p, nil
}

func (p *testLibSQL) clone() (string, error) {
	p.cloneMu.Lock()
	defer p.cloneMu.Unlock()

	name := "t" + strings.ToLower(security.PseudorandomString(12))
	if err := libsqlAdmin(http.MethodPost, p.adminURL, "/v1/namespaces/"+p.tplName+"/fork/"+name); err != nil {
		return "", fmt.Errorf("fork the template: %w", err)
	}

	return name, nil
}

func prepareLibSQL(config *core.BaseAppConfig, srcDataDir, dataDir string) (func(), error) {
	if config.DBConnect != nil {
		return nil, nil
	}

	var name string

	if srcDataDir == "" {
		testLibSQLOnce.Do(func() { testLS, testLSErr = buildLibSQLTemplate() })
		if testLSErr != nil {
			panic(fmt.Sprintf("libsql test template: %v", testLSErr))
		}

		var err error
		if name, err = testLS.clone(); err != nil {
			return nil, err
		}
	} else {
		testLSDBs.mu.Lock()
		name = testLSDBs.byDir[srcDataDir]
		testLSDBs.mu.Unlock()

		if name == "" {
			return nil, nil // a custom data dir of a SQLite app
		}
	}

	testLSDBs.acquire(name)
	testLSDBs.mu.Lock()
	testLSDBs.byDir[dataDir] = name
	testLSDBs.mu.Unlock()

	config.DBConnect = libsqlNamespaceConnect(testLS.url, name)
	config.AuxDBConnect = core.DefaultDBConnect
	limitTestPGConns(config)

	return func() {
		testLSDBs.mu.Lock()
		delete(testLSDBs.byDir, dataDir)
		testLSDBs.refs[name]--
		last := testLSDBs.refs[name] <= 0
		if last {
			delete(testLSDBs.refs, name)
		}
		testLSDBs.mu.Unlock()

		if last {
			_ = libsqlAdmin(http.MethodDelete, testLS.adminURL, "/v1/namespaces/"+name)
		}
	}, nil
}
