package core

// Fork-local addition — not upstream PocketBase (issue #106). See tools/dialect
// for the storage seam and docs/datastores.md for the user facing description.

import "github.com/doodlemania2/claviger/tools/dialect"

// TableInfoRow is the column info row returned by [App.TableInfo].
type TableInfoRow = dialect.TableInfoRow

// DialectOf returns the dialect of the app data database
// (SQLite unless a Postgres database was configured).
//
// Inside a transaction it is the dialect of the transaction connection.
func DialectOf(app App) dialect.Dialect {
	return dialectOf(app)
}

// AuxDialectOf returns the dialect of the app auxiliary database.
func AuxDialectOf(app App) dialect.Dialect {
	return dialect.Of(app.AuxConcurrentDB())
}

func dialectOf(app App) dialect.Dialect {
	return dialect.Of(app.ConcurrentDB())
}
