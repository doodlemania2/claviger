package dialect

// CockroachDB support (issue #106). CockroachDB speaks the PostgreSQL wire
// protocol and most of its SQL, so it is the same code as the Postgres dialect
// (postgresDialect with crdb=true), with the differences below. The tests are
// the Postgres ones, run against a CockroachDB server (docs/datastores.md).
//
//   - Text columns have no COLLATE "C": the default collation is already byte
//     order and the locale "C" is rejected.
//   - The row id identity column caches one value (CACHE 1), so insertion order
//     holds across nodes.
//   - There is no pg_advisory_xact_lock (and the session level advisory lock
//     functions are no-ops), so migrations are not serialized by a lock. Start
//     one instance on an empty database first. The "_migrations" primary key
//     still makes a concurrent second run fail rather than apply twice.
//   - There is no LISTEN/NOTIFY: several replicas are refused (core/multireplica.go).
//   - There is no VACUUM; garbage collection is automatic.
//   - ALTER COLUMN TYPE that rewrites data is not allowed inside a transaction,
//     and a backfill of a column that was added in the same transaction is not
//     reliable, so changing a field between single and multiple is only done
//     for a table without records (see cockroachConvertMultiplicity).
//   - Statements can fail with SQLSTATE 40001 (serialization failure); the app
//     retries the transaction (core/db_retry.go).

import (
	"errors"
	"fmt"

	"ganigeorgiev.com/pocketbase/dbx"
)

// ErrCockroachConvertMultiplicity is returned when a field cannot be turned
// into a multiple (or single) one on CockroachDB because the table has data.
var ErrCockroachConvertMultiplicity = errors.New(
	"changing a field between single and multiple is not supported on CockroachDB for a collection that has records " +
		"(ALTER COLUMN TYPE cannot rewrite data inside a transaction): add a new field, copy the values and drop the old one")

// cockroachLockMigrations is a no-op, see the file comment.
func cockroachLockMigrations(db dbx.Builder) error { return nil }

// cockroachConvertMultiplicity recreates the column when the table is empty
// and refuses when it has records.
func cockroachConvertMultiplicity(db dbx.Builder, table, column string, toMultiple bool) (bool, error) {
	var dataType string
	_ = db.NewQuery(`SELECT data_type::text FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = {:table} AND column_name = {:column}`).
		Bind(dbx.Params{"table": table, "column": column}).Row(&dataType)
	if dataType == "" || (toMultiple && dataType == "jsonb") || (!toMultiple && dataType == "text") {
		return true, nil
	}

	t, c := quoteIdent(table), quoteIdent(column)

	var hasRows int
	if err := db.NewQuery("SELECT 1 FROM " + t + " LIMIT 1").Row(&hasRows); err == nil && hasRows == 1 {
		return true, ErrCockroachConvertMultiplicity
	}

	views, err := pgListViews(db)
	if err != nil {
		return true, err
	}
	for i := len(views) - 1; i >= 0; i-- {
		if _, err := db.NewQuery("DROP VIEW " + quoteIdent(views[i].Name)).Execute(); err != nil {
			return true, err
		}
	}

	var stmt string
	if toMultiple {
		stmt = fmt.Sprintf(`ALTER TABLE %[1]s DROP COLUMN %[2]s, ADD COLUMN %[2]s JSONB DEFAULT '[]'::jsonb`, t, c)
	} else {
		stmt = fmt.Sprintf(`ALTER TABLE %[1]s DROP COLUMN %[2]s, ADD COLUMN %[2]s TEXT DEFAULT '' NOT NULL`, t, c)
	}
	if _, err := db.NewQuery(stmt).Execute(); err != nil {
		return true, err
	}

	for _, v := range views {
		if _, err := db.NewQuery(v.CreateSQL).Execute(); err != nil {
			return true, err
		}
	}

	return true, nil
}
