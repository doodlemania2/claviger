package core

// Fork-local addition — not upstream PocketBase (issue #106).

import (
	"github.com/doodlemania2/claviger/tools/dialect"
)

// defaultViewFieldForColumn returns the field for a view column whose type
// couldn't be taken from a collection field or a cast in the view query.
//
// SQLite is dynamically typed: the declared type of a view column is not
// reliable, so the column is a JSON field (see [defaultViewField]). Postgres
// views have real column types, which the dialect introspection reports.
func defaultViewFieldForColumn(app App, row *TableInfoRow) Field {
	if row.Name == FieldNameId {
		return defaultViewIdField()
	}

	switch dialectOf(app).ColumnKind(row.Type) {
	case dialect.ColumnText:
		return &TextField{Name: row.Name}
	case dialect.ColumnFloat:
		return &NumberField{Name: row.Name}
	case dialect.ColumnInt:
		return &NumberField{Name: row.Name, OnlyInt: true}
	case dialect.ColumnBool:
		return &BoolField{Name: row.Name}
	}

	return defaultViewField(row.Name)
}
