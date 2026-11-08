package core

import (
	"fmt"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dialect"
)

// TableColumns returns all column names of a single table by its name.
func (app *BaseApp) TableColumns(tableName string) ([]string, error) {
	return dialectOf(app).TableColumns(app.ConcurrentDB(), tableName)
}

// TableInfo returns the "table_info" pragma result for the specified table.
func (app *BaseApp) TableInfo(tableName string) ([]*TableInfoRow, error) {
	return dialectOf(app).TableInfo(app.ConcurrentDB(), tableName)
}

// TableIndexes returns a name grouped map with all non empty index of the specified table.
//
// Note: This method doesn't return an error on nonexisting table.
func (app *BaseApp) TableIndexes(tableName string) (map[string]string, error) {
	return dialectOf(app).TableIndexes(app.ConcurrentDB(), tableName)
}

// DeleteTable drops the specified table.
//
// This method is a no-op if a table with the provided name doesn't exist.
//
// NB! Be aware that this method is vulnerable to SQL injection and the
// "dangerousTableName" argument must come only from trusted input!
func (app *BaseApp) DeleteTable(dangerousTableName string) error {
	_, err := app.NonconcurrentDB().NewQuery(fmt.Sprintf(
		"DROP TABLE IF EXISTS {{%s}}",
		dangerousTableName,
	)).Execute()

	return err
}

// HasTable checks if a table (or view) with the provided name exists (case insensitive).
// in the data.db.
func (app *BaseApp) HasTable(tableName string) bool {
	return app.hasTable(app.ConcurrentDB(), tableName)
}

// AuxHasTable checks if a table (or view) with the provided name exists (case insensitive)
// in the auixiliary.db.
func (app *BaseApp) AuxHasTable(tableName string) bool {
	return app.hasTable(app.AuxConcurrentDB(), tableName)
}

func (app *BaseApp) hasTable(db dbx.Builder, tableName string) bool {
	return dialect.Of(db).HasTable(db, tableName)
}

// Vacuum executes VACUUM on the data.db in order to reclaim unused data db disk space.
func (app *BaseApp) Vacuum() error {
	return app.vacuum(app.NonconcurrentDB())
}

// AuxVacuum executes VACUUM on the auxiliary.db in order to reclaim unused auxiliary db disk space.
func (app *BaseApp) AuxVacuum() error {
	return app.vacuum(app.AuxNonconcurrentDB())
}

func (app *BaseApp) vacuum(db dbx.Builder) error {
	return dialect.Of(db).Vacuum(db)
}
