// Package dialect is the storage seam of Claviger (fork-only, issue #106).
//
// PocketBase was written against SQLite. Instead of rewriting SQL strings on
// their way to the driver, every place that needs to differ between database
// engines asks a Dialect. The Dialect is looked up from the connection that
// the statement is about to run on (see [Of]), so a Postgres data database and
// a SQLite auxiliary database can live in one process without global state.
//
// The SQLite dialect is the previous behaviour moved behind the interface and
// must keep producing byte for byte the SQL the code produced before.
//
// This package has no database driver dependency. Drivers are registered by
// package core (see core/db_connect_postgres.go).
package dialect

import (
	"database/sql"
	"sync"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dbutils"
)

// Dialect names.
const (
	SQLiteName   = "sqlite"
	PostgresName = "postgres"
)

// RowIDColumn is the name under which every record table can be ordered by
// insertion. On SQLite it is the builtin rowid alias. On Postgres it is a real
// hidden identity column with the same name (see [Dialect.HiddenColumns]),
// which is why it is spelled like the SQLite alias, and not "_rowid": the name
// is already a reserved field name in core/field.go, so no user field can
// collide with it.
const RowIDColumn = "_rowid_"

// TableInfoRow describes a single table column (the shape of the SQLite
// "table_info" pragma, kept for API compatibility).
type TableInfoRow struct {
	// the `db:"pk"` tag has special semantic so we cannot rename
	// the original field without specifying a custom mapper
	PK int

	Index        int            `db:"cid"`
	Name         string         `db:"name"`
	Type         string         `db:"type"`
	NotNull      bool           `db:"notnull"`
	DefaultValue sql.NullString `db:"dflt_value"`
}

// ViewDef is a view name with the statement that recreates it.
type ViewDef struct {
	Name      string
	CreateSQL string
}

// Dialect isolates everything the storage layer needs to know about a
// specific database engine.
type Dialect interface {
	// Name returns the stable dialect name ("sqlite", "postgres").
	Name() string

	// SeparateWritePool reports whether writes must go through a dedicated
	// single connection pool (SQLite: avoids SQLITE_BUSY). When false the app
	// uses one pool for reads and writes.
	SeparateWritePool() bool

	// QuoteIdent quotes a table, column or index name for raw SQL.
	QuoteIdent(name string) string

	// ---------------------------------------------------------------
	// Column definitions (the part after the column name)
	// ---------------------------------------------------------------

	// RandomIDDefault is the DEFAULT expression of a 15 characters
	// random id ("r" + 14 hex chars), parenthesized.
	RandomIDDefault() string
	// NowDefault is the DEFAULT expression of the "created"/"updated"
	// system columns ("2006-01-02 15:04:05.000Z"), parenthesized.
	//
	// Dates stay TEXT on every dialect: PocketBase stores and compares them
	// as fixed width UTC strings, the filter language relies on it, and it
	// keeps data portable between dialects (a SQLite to Postgres copy is a
	// plain value copy).
	NowDefault() string
	// StringDefault returns a string literal usable in a DEFAULT clause.
	StringDefault(value string) string
	// IDColumn is the definition of a record "id" primary key column.
	IDColumn() string
	// TextColumn is the definition of a TEXT NOT NULL DEFAULT '' column.
	TextColumn() string
	// NumberColumn is the definition of a record "number" column.
	NumberColumn() string
	// BoolColumn is the definition of a record "bool" column.
	BoolColumn() string
	// JSONColumn is the definition of a JSON column. An empty defaultValue
	// means "DEFAULT NULL", otherwise it is the raw JSON text of a
	// NOT NULL default.
	JSONColumn(defaultValue string) string
	// Int64Type is the type name of a 64 bit integer.
	Int64Type() string
	// JSONType is the type name of a JSON column.
	JSONType() string
	// HiddenColumns returns extra columns that every record table gets on
	// creation (name => definition). Nil when the engine has a builtin row id.
	HiddenColumns() map[string]string
	// RowOrder is the ORDER BY term that returns rows in insertion order.
	RowOrder() string

	// ---------------------------------------------------------------
	// DDL
	// ---------------------------------------------------------------

	// BuildIndex renders a (portable, SQLite flavoured) parsed index as a
	// CREATE INDEX statement for this dialect.
	BuildIndex(idx dbutils.Index) string
	// ConvertMultiplicity changes a single value column to a multiple value
	// one (toMultiple=true) or the opposite, keeping the data.
	//
	// handled=false means that the dialect has no special handling and the
	// caller must use its generic (SQLite) strategy.
	ConvertMultiplicity(db dbx.Builder, table, column string, toMultiple bool) (handled bool, err error)

	// ---------------------------------------------------------------
	// Introspection
	// ---------------------------------------------------------------

	// HasTable checks if a table or view exists (case insensitive).
	HasTable(db dbx.Builder, name string) bool
	// TableColumns returns the column names of a table (hidden columns excluded).
	TableColumns(db dbx.Builder, table string) ([]string, error)
	// TableInfo returns the column info of a table or view (hidden columns excluded).
	TableInfo(db dbx.Builder, table string) ([]*TableInfoRow, error)
	// TableIndexes returns the name => CREATE INDEX statement map of the
	// non-implicit indexes of a table.
	TableIndexes(db dbx.Builder, table string) (map[string]string, error)
	// IndexOwner returns the table that owns the named index (case
	// insensitive) skipping the provided tables, or "" if there is none.
	IndexOwner(db dbx.Builder, indexName string, skipTables ...string) string

	// ---------------------------------------------------------------
	// Maintenance and transactions
	// ---------------------------------------------------------------

	// Vacuum reclaims unused disk space.
	Vacuum(db dbx.Builder) error
	// Optimize is the cheap "after schema change" planner maintenance
	// (SQLite: PRAGMA optimize). It is a no-op if there is nothing to do.
	Optimize(db dbx.Builder) error
	// Checkpoint flushes the write ahead log (SQLite only, no-op elsewhere).
	Checkpoint(db dbx.Builder) error
	// LockMigrations serializes concurrent migration runs. It must be called
	// inside the migrations transaction (no-op on SQLite, which has a
	// single writer).
	LockMigrations(db dbx.Builder) error
	// Try runs fn and reports its error. If db is a transaction, a failure
	// of fn is rolled back to a savepoint on engines where a failed
	// statement aborts the whole transaction (Postgres), so the caller can
	// handle the error and continue.
	Try(db dbx.Builder, fn func() error) error
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Dialect{
		"sqlite":   SQLite,
		"sqlite3":  SQLite,
		"pgx":      Postgres,
		"postgres": Postgres,
	}
)

// Register maps a database/sql driver name (the one passed to dbx) to a dialect.
func Register(driverName string, d Dialect) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[driverName] = d
}

// ForDriver returns the dialect of the specified driver name
// (SQLite for unknown names, which is the historic behavior).
func ForDriver(driverName string) Dialect {
	registryMu.RLock()
	defer registryMu.RUnlock()

	if d, ok := registry[driverName]; ok {
		return d
	}

	return SQLite
}

type dbHolder interface {
	DB() *dbx.DB
}

// Of returns the dialect of the connection that the builder (a *dbx.DB,
// a *dbx.Tx or a dbx builder) executes on.
func Of(b dbx.Builder) Dialect {
	if tx, ok := b.(*dbx.Tx); ok {
		b = tx.Builder
	}

	switch v := b.(type) {
	case *dbx.DB:
		return ForDriver(v.DriverName())
	case dbHolder:
		if db := v.DB(); db != nil {
			return ForDriver(db.DriverName())
		}
	}

	return SQLite
}

// IsSQLite is a shorthand for Of(b).Name() == SQLiteName.
func IsSQLite(b dbx.Builder) bool {
	return Of(b).Name() == SQLiteName
}
