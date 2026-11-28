package dialect

// libSQL (sqld / Turso) support (issue #106, experimental).
//
// libSQL is a fork of SQLite, and sqld serves it over the network (Hrana over
// HTTP). The SQL is SQLite's SQL, so this dialect is the SQLite one with the
// statements that sqld refuses turned into no-ops:
//
//   - VACUUM, PRAGMA optimize and PRAGMA wal_checkpoint are rejected by the
//     sqld statement parser ("unsupported statement"). The server owns the
//     file, its checkpoints and its page reuse.
//   - The write pool stays separate and single (SQLite semantics, in the app):
//     sqld has one writer too, and queuing writes in the app is cheaper than a
//     round trip that fails with SQLITE_BUSY.
//
// It has its own name, and not "sqlite", so that the code that means "a local
// data.db file" (zip backups, restore, several replicas) refuses instead of
// looking for a file that does not exist (see [IsSQLiteFamily]).

import (
	"ganigeorgiev.com/pocketbase/dbx"
)

// LibSQLName is the name of the libSQL dialect (and of its dbx driver name).
const LibSQLName = "libsql"

// LibSQL is the libSQL (sqld) dialect.
var LibSQL Dialect = libsqlDialect{}

type libsqlDialect struct {
	sqliteDialect
}

func (libsqlDialect) Name() string { return LibSQLName }

// Vacuum is a no-op: sqld rejects VACUUM.
func (libsqlDialect) Vacuum(db dbx.Builder) error { return nil }

// Optimize is a no-op: sqld rejects PRAGMA optimize.
func (libsqlDialect) Optimize(db dbx.Builder) error { return nil }

// Checkpoint is a no-op: the server checkpoints its own WAL.
func (libsqlDialect) Checkpoint(db dbx.Builder) error { return nil }

// IsSQLiteFamily reports whether the dialect speaks SQLite's SQL (SQLite and
// libSQL), which is what the SQL level code (not the file level code) asks.
func IsSQLiteFamily(d Dialect) bool {
	n := d.Name()
	return n == SQLiteName || n == LibSQLName
}

func init() {
	// dbx builds SQLite SQL for it (placeholders, quoting, upsert)
	dbx.BuilderFuncMap[LibSQLName] = dbx.NewSqliteBuilder
	Register(LibSQLName, LibSQL)
}
