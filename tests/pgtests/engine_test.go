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

// testEngine is the dialect name of the test server: postgres, cockroach, mysql, mariadb or mssql.
func testEngine(t testing.TB) string {
	t.Helper()

	engineOnce.Do(func() {
		switch {
		case isMSSQLServer(t):
			engineName = dialect.MSSQLName
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
	dialect.MSSQLName: {
		"unicode like is ascii only":  "SQL Server's case insensitive collation folds the case of non ASCII letters (accents stay significant), SQLite's LIKE only ASCII",
		"unicode lower is ascii only": "SQL Server's LOWER() and its case insensitive LIKE fold non ASCII letters, SQLite's only ASCII",
		"json type ordering":          "a JSON string sorts before a number (sql_variant orders the type families numbers first), SQLite orders text after numbers",
	},
	dialect.MySQLName: {
		"unicode like is ascii only":  "MySQL's case insensitive collation folds the case of non ASCII letters (accents stay significant), SQLite's LIKE only ASCII",
		"unicode lower is ascii only": "MySQL's LOWER() and its case insensitive LIKE fold non ASCII letters, SQLite's only ASCII",
		"json number":                 "a JSON string that looks like a number (\"5\") equals the number 5: MySQL compares text with numbers as numbers",
		"json number string mismatch": "a JSON string that looks like a number equals the number: MySQL compares text with numbers as numbers",
		"json type ordering":          "a JSON string that looks like a number sorts among the numbers: MySQL compares text with numbers as numbers",
		"strftime weekday modifier":   "the weekday strftime modifier is not supported on MySQL",
		"strftime iso week":           "the %V and %G strftime formats are not supported on MySQL",
		"strftime week of year":       "the %W and %U strftime formats are not supported on MySQL",
		"strftime unixepoch":          "strftime with a unix time number is not supported on MySQL",
		"strftime julian":             "strftime with a julian day number is not supported on MySQL",
		"sort json path":              "sorting by a JSON path orders text values that look like numbers among the numbers",
	},
}

// engineMayAgree reports that the engine behaves like SQLite where the
// PostgreSQL based tables list a difference (MySQL coerces text, numbers and
// booleans in comparisons, like SQLite), so a known gap that does not differ
// is not an error.
func engineMayAgree(t testing.TB) bool {
	t.Helper()
	return testEngine(t) == dialect.MySQLName || testEngine(t) == dialect.MariaDBName || testEngine(t) == dialect.MSSQLName
}
