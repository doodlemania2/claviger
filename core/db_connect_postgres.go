//go:build !no_default_driver

package core

// Fork-local addition — not upstream PocketBase (issue #106).
// Opt-in PostgreSQL storage for the app data database through the pure Go pgx driver.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// DBURLEnv is the name of the environment variable that selects PostgreSQL
// for the app data database (postgres://user:pass@host:5432/dbname).
//
// Unset means SQLite exactly as before.
const DBURLEnv = "CLAVIGER_DB_URL"

// PostgresDBConnect returns a [DBConnectFunc] that opens the provided
// PostgreSQL connection URL. The dbPath argument is ignored.
func PostgresDBConnect(url string) DBConnectFunc {
	return func(_ string) (*dbx.DB, error) {
		// instrumented driver wrapper while traces/metrics are active
		if db, err := openTelemetryPostgres(url); db != nil || err != nil {
			if err != nil {
				return nil, redactDBURL(err, url)
			}
			return db, pingPostgres(db.DB(), url)
		}

		sqlDB, err := sql.Open("pgx", url)
		if err != nil {
			return nil, redactDBURL(err, url)
		}

		if err := pingPostgres(sqlDB, url); err != nil {
			return nil, err
		}

		// "pgx" selects dbx's PgsqlBuilder (placeholders, quoting, upsert)
		// and the Postgres dialect (see tools/dialect).
		return dbx.NewFromDB(sqlDB, "pgx"), nil
	}
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

	config.DBConnect = PostgresDBConnect(url)
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
