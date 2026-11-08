package dialect

import (
	"fmt"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dbutils"
)

// SQLite is the SQLite dialect: the behavior PocketBase always had.
var SQLite Dialect = sqliteDialect{}

type sqliteDialect struct{}

func (sqliteDialect) Name() string { return SQLiteName }

// QuoteIdent backtick quotes an identifier (the historic PocketBase raw SQL style).
func (sqliteDialect) QuoteIdent(name string) string { return "`" + name + "`" }

func (sqliteDialect) SeparateWritePool() bool { return true }

func (sqliteDialect) RandomIDDefault() string {
	return `('r'||lower(hex(randomblob(7))))`
}

func (sqliteDialect) NowDefault() string {
	return `(strftime('%Y-%m-%d %H:%M:%fZ'))`
}

// note: SQLite accepts (and the historic schema uses) double quoted string literals.
func (sqliteDialect) StringDefault(value string) string { return `"` + value + `"` }

func (d sqliteDialect) IDColumn() string {
	return "TEXT PRIMARY KEY DEFAULT " + d.RandomIDDefault() + " NOT NULL"
}

func (sqliteDialect) TextColumn() string { return "TEXT DEFAULT '' NOT NULL" }

func (sqliteDialect) NumberColumn() string { return "NUMERIC DEFAULT 0 NOT NULL" }

func (sqliteDialect) BoolColumn() string { return "BOOLEAN DEFAULT FALSE NOT NULL" }

func (d sqliteDialect) JSONColumn(defaultValue string) string {
	return jsonColumn(d.JSONType(), defaultValue)
}

func (sqliteDialect) Int64Type() string { return "INTEGER" }

func (sqliteDialect) JSONType() string { return "JSON" }

func (sqliteDialect) HiddenColumns() map[string]string { return nil }

func (sqliteDialect) RowOrder() string { return "rowid ASC" }

func (sqliteDialect) BuildIndex(idx dbutils.Index) string { return idx.Build() }

func (sqliteDialect) ConvertMultiplicity(db dbx.Builder, table, column string, toMultiple bool) (bool, error) {
	return false, nil
}

func (sqliteDialect) HasTable(db dbx.Builder, name string) bool {
	var exists int

	err := db.Select("(1)").
		From("sqlite_schema").
		AndWhere(dbx.HashExp{"type": []any{"table", "view"}}).
		AndWhere(dbx.NewExp("LOWER([[name]])=LOWER({:tableName})", dbx.Params{"tableName": name})).
		Limit(1).
		Row(&exists)

	return err == nil && exists > 0
}

func (sqliteDialect) TableColumns(db dbx.Builder, table string) ([]string, error) {
	columns := []string{}

	err := db.NewQuery("SELECT name FROM PRAGMA_TABLE_INFO({:tableName})").
		Bind(dbx.Params{"tableName": table}).
		Column(&columns)

	return columns, err
}

func (sqliteDialect) TableInfo(db dbx.Builder, table string) ([]*TableInfoRow, error) {
	info := []*TableInfoRow{}

	err := db.NewQuery("SELECT * FROM PRAGMA_TABLE_INFO({:tableName})").
		Bind(dbx.Params{"tableName": table}).
		All(&info)
	if err != nil {
		return nil, err
	}

	// mattn/go-sqlite3 doesn't throw an error on invalid or missing table
	// so we additionally have to check whether the loaded info result is nonempty
	if len(info) == 0 {
		return nil, fmt.Errorf("empty table info probably due to invalid or missing table %s", table)
	}

	return info, nil
}

func (sqliteDialect) TableIndexes(db dbx.Builder, table string) (map[string]string, error) {
	indexes := []struct {
		Name string
		Sql  string
	}{}

	err := db.Select("name", "sql").
		From("sqlite_master").
		AndWhere(dbx.NewExp("sql is not null")).
		AndWhere(dbx.HashExp{
			"type":     "index",
			"tbl_name": table,
		}).
		All(&indexes)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string, len(indexes))

	for _, idx := range indexes {
		result[idx.Name] = idx.Sql
	}

	return result, nil
}

func (sqliteDialect) IndexOwner(db dbx.Builder, indexName string, skipTables ...string) string {
	q := db.Select("tbl_name").
		From("sqlite_master").
		AndWhere(dbx.HashExp{"type": "index"})

	for i, t := range skipTables {
		q.AndWhere(dbx.NewExp(
			fmt.Sprintf("LOWER([[tbl_name]])!=LOWER({:skip%d})", i),
			dbx.Params{fmt.Sprintf("skip%d", i): t},
		))
	}

	var owner string
	_ = q.AndWhere(dbx.NewExp("LOWER([[name]])=LOWER({:indexName})", dbx.Params{"indexName": indexName})).
		Limit(1).
		Row(&owner)

	return owner
}

func (sqliteDialect) Vacuum(db dbx.Builder) error {
	_, err := db.NewQuery("VACUUM").Execute()
	return err
}

func (sqliteDialect) Optimize(db dbx.Builder) error {
	_, err := db.NewQuery("PRAGMA optimize").Execute()
	return err
}

func (sqliteDialect) Checkpoint(db dbx.Builder) error {
	_, err := db.NewQuery("PRAGMA wal_checkpoint(TRUNCATE)").Execute()
	return err
}

func (sqliteDialect) LockMigrations(db dbx.Builder) error { return nil }

func (sqliteDialect) Try(db dbx.Builder, fn func() error) error { return fn() }

// jsonColumn builds a JSON-ish column definition shared by the dialects.
func jsonColumn(typ, defaultValue string) string {
	if defaultValue == "" {
		return typ + " DEFAULT NULL"
	}

	return typ + " DEFAULT '" + strings.ReplaceAll(defaultValue, "'", "''") + "' NOT NULL"
}
