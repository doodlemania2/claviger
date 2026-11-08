package apis

// Fork-local addition — not upstream PocketBase (issue #106).

import (
	"github.com/doodlemania2/claviger/core"
)

// dummyRecordSelect renders the "SELECT {:param} AS col" item of the create
// rule's dummy record table. Postgres can't infer the type of a bound
// parameter that is not compared with a column, so the value is cast to the
// column type of the collection field (a no-op on SQLite).
func dummyRecordSelect(app core.App, collection *core.Collection, param, column string) string {
	expr := "{:" + param + "}"

	if f := collection.Fields.GetByName(column); f != nil {
		expr = core.DialectOf(app).ColumnCast(expr, f.ColumnType(app))
	}

	return expr + " AS [[" + column + "]]"
}
