//go:build postgres

package pgtests

import (
	"strings"
	"sync"
	"testing"

	"github.com/doodlemania2/claviger/tools/dialect"
)

var (
	engineOnce sync.Once
	engineName string
)

// testEngine is the dialect name of the test server: postgres, cockroach, mysql or mariadb.
func testEngine(t testing.TB) string {
	t.Helper()

	engineOnce.Do(func() {
		switch {
		case isMySQLServer(t):
			engineName = dialect.MySQLName
			if strings.Contains(strings.ToLower(serverVersion(t)), "mariadb") {
				engineName = dialect.MariaDBName
			}
		case isCockroach(t):
			engineName = dialect.CockroachName
		default:
			engineName = dialect.PostgresName
		}
	})

	return engineName
}

// engineGap returns the reason why the named case is a known difference on
// the engine of the test server (empty when it is not). Like the gaps of the
// case tables, it runs on SQLite only and fails as soon as the two agree, so
// the list can only shrink. The names are the case names of the tables.
func engineGap(t testing.TB, name string) string {
	t.Helper()

	engine := testEngine(t)
	if engine == dialect.MariaDBName {
		engine = dialect.MySQLName
	}

	return engineGaps[engine][name]
}

// engineGaps lists, per engine, the cases that differ from SQLite and why.
// PostgreSQL's own differences are the "gap" fields of the tables.
var engineGaps = map[string]map[string]string{
	dialect.CockroachName: {
		"unicode like is ascii only":  "CockroachDB's ILIKE and lower() fold the case of non ASCII letters, SQLite's only ASCII",
		"unicode lower is ascii only": "CockroachDB's ILIKE and lower() fold the case of non ASCII letters, SQLite's only ASCII",
	},
	dialect.MySQLName: {},
}
