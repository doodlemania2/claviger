//go:build !no_default_driver

package core

// Fork-local addition — not upstream PocketBase (issue #106).
// Opt-in PostgreSQL storage for the app data database through the pure Go pgx driver.

import (
	"context"
	"database/sql"
	"fmt"
	neturl "net/url"
	"os"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dialect"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// DBURLEnv is the name of the environment variable that selects PostgreSQL
// for the app data database (postgres://user:pass@host:5432/dbname).
//
// Unset means SQLite exactly as before.
const DBURLEnv = "CLAVIGER_DB_URL"

// PostgresDBConnect returns a [DBConnectFunc] that opens the provided
// PostgreSQL connection URL. The dbPath argument is ignored.
//
// The URL scheme chooses the engine: postgres:// and postgresql:// are
// PostgreSQL, cockroach:// and cockroachdb:// are CockroachDB (rewritten to
// postgresql:// for the driver). A postgres:// URL that turns out to reach a
// CockroachDB server is detected after connecting and gets the CockroachDB
// dialect too.
func PostgresDBConnect(url string) DBConnectFunc {
	url, forceCRDB := normalizeCockroachURL(url)
	url = postgresExecModeURL(url)

	return func(_ string) (*dbx.DB, error) {
		var sqlDB *sql.DB

		// instrumented driver wrapper while traces/metrics are active
		if db, err := openTelemetryPostgres(url); db != nil || err != nil {
			if err != nil {
				return nil, redactDBURL(err, url)
			}
			sqlDB = db.DB()
		} else {
			var err error
			sqlDB, err = sql.Open("pgx", url)
			if err != nil {
				return nil, redactDBURL(err, url)
			}
		}

		if err := pingPostgres(sqlDB, url); err != nil {
			return nil, err
		}

		// "pgx" selects dbx's PgsqlBuilder (placeholders, quoting, upsert)
		// and the Postgres dialect (see tools/dialect); "cockroach" the same
		// builder and the CockroachDB dialect.
		driverName := "pgx"
		if forceCRDB || isCockroachServer(sqlDB) {
			driverName = dialect.CockroachName
		}

		return dbx.NewFromDB(sqlDB, driverName), nil
	}
}

// normalizeCockroachURL turns the cockroach:// and cockroachdb:// schemes into
// postgresql:// and reports whether it did.
func normalizeCockroachURL(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	lower := strings.ToLower(trimmed)

	for _, scheme := range []string{"cockroachdb://", "cockroach://"} {
		if strings.HasPrefix(lower, scheme) {
			return "postgresql://" + trimmed[len(scheme):], true
		}
	}

	return raw, false
}

// isCockroachServer asks the server for its version string.
func isCockroachServer(sqlDB *sql.DB) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var version string
	if err := sqlDB.QueryRowContext(ctx, "SELECT version()").Scan(&version); err != nil {
		return false
	}

	return strings.Contains(version, "CockroachDB")
}

// pingPostgres fails early, with a clear error, instead of at the first query.
func pingPostgres(sqlDB *sql.DB, url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("failed to connect to the PostgreSQL database (%s): %w", DBURLEnv, redactDBURL(err, url))
	}

	return nil
}

// applyEnvDBConfig selects the data database from the environment:
// with CLAVIGER_DB_URL set, the data db is PostgreSQL and
// the auxiliary db (logs) stays a local SQLite file.
func applyEnvDBConfig(config *BaseAppConfig) {
	url := strings.TrimSpace(os.Getenv(DBURLEnv))
	if url == "" {
		return
	}

	if isMySQLURL(url) {
		config.DBConnect = MySQLDBConnect(url)
	} else if isLibSQLURL(url) {
		config.DBConnect = LibSQLDBConnect(url)
	} else if isMSSQLURL(url) {
		config.DBConnect = MSSQLDBConnect(url)
	} else {
		config.DBConnect = PostgresDBConnect(url)
	}
	config.DBURL = url

	if config.AuxDBConnect == nil {
		config.AuxDBConnect = DefaultDBConnect
	}
}

// redactDBURL keeps the connection URL (it carries the password) out of error messages.
func redactDBURL(err error, url string) error {
	if err == nil || url == "" {
		return err
	}

	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), url, "<redacted>"))
}

// postgresExecModeURL sets pgx's default_query_exec_mode to "describe_exec" unless the
// URL already chooses one. pgx's default caches a server-side prepared
// statement per connection, and PostgreSQL then fails the first reuse after a
// collection's columns change ("cached plan must not change result type").
func postgresExecModeURL(raw string) string {
	const key = "default_query_exec_mode"

	if strings.Contains(raw, key) {
		return raw
	}

	if !strings.Contains(raw, "://") {
		return strings.TrimSpace(raw) + " " + key + "=describe_exec"
	}

	u, err := neturl.Parse(raw)
	if err != nil {
		return raw
	}

	q := u.Query()
	q.Set(key, "describe_exec")
	u.RawQuery = q.Encode()

	return u.String()
}
