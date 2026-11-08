package dialect

import (
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dbutils"
)

// Postgres is the PostgreSQL (16+) dialect.
var Postgres Dialect = postgresDialect{}

// migrationsLockKey is the pg_advisory_xact_lock key that serializes
// migration runs of concurrently starting instances ("Claviger" in ASCII).
const migrationsLockKey int64 = 0x436c617669676572

var savepointSeq atomic.Uint64

type postgresDialect struct{}

func (postgresDialect) Name() string { return PostgresName }

// A server database has no single writer limit, so reads and
// writes share one pool.
func (postgresDialect) SeparateWritePool() bool { return false }

// Same shape as the SQLite one: "r" followed by 14 hex chars.
// (gen_random_uuid is builtin since Postgres 13).
func (postgresDialect) RandomIDDefault() string {
	return `('r'||substr(replace(gen_random_uuid()::text,'-',''),1,14))`
}

// Same text format as the SQLite default: 2006-01-02 15:04:05.000Z (UTC).
func (postgresDialect) NowDefault() string {
	return `(to_char(now() at time zone 'utc','YYYY-MM-DD HH24:MI:SS.MS"Z"'))`
}

func (postgresDialect) StringDefault(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// Text columns use the "C" collation: strings compare and sort byte by byte,
// like SQLite's default BINARY collation, whatever the locale of the database.
// Without it the filter operators >, <, ORDER BY and the case folding of ILIKE
// would depend on the cluster's locale (and give other results than SQLite).
const textCollation = ` COLLATE "C"`

func (d postgresDialect) IDColumn() string {
	return "TEXT" + textCollation + " PRIMARY KEY DEFAULT " + d.RandomIDDefault() + " NOT NULL"
}

func (postgresDialect) TextColumn() string { return "TEXT" + textCollation + " DEFAULT '' NOT NULL" }

// PocketBase numbers are float64 end to end. DOUBLE PRECISION keeps those
// semantics and is returned by the driver as a float64 (NUMERIC is
// returned as a string and has exact decimal arithmetic SQLite never had).
func (postgresDialect) NumberColumn() string { return "DOUBLE PRECISION DEFAULT 0 NOT NULL" }

func (postgresDialect) BoolColumn() string { return "BOOLEAN DEFAULT FALSE NOT NULL" }

func (d postgresDialect) JSONColumn(defaultValue string) string {
	return jsonColumn(d.JSONType(), defaultValue)
}

func (postgresDialect) Int64Type() string { return "BIGINT" }

func (postgresDialect) JSONType() string { return "JSONB" }

// Postgres has no rowid, so every record table gets a hidden identity
// column to keep the insertion order (list default order, @rowid sort).
func (postgresDialect) HiddenColumns() map[string]string {
	return map[string]string{RowIDColumn: "BIGINT GENERATED ALWAYS AS IDENTITY"}
}

func (postgresDialect) RowOrder() string { return `"` + RowIDColumn + `" ASC` }

// BuildIndex renders the portable (SQLite flavored) index definition:
//   - identifiers are double quoted;
//   - "COLLATE NOCASE" becomes a lower(...) expression index (no citext extension needed);
//   - other collations are quoted;
//   - expressions are wrapped in parentheses, as Postgres requires, and their
//     backtick identifier quotes (also in the WHERE clause) become double quotes.
func (d postgresDialect) BuildIndex(idx dbutils.Index) string {
	if !idx.IsValid() {
		return ""
	}

	var str strings.Builder

	str.WriteString("CREATE ")
	if idx.Unique {
		str.WriteString("UNIQUE ")
	}
	str.WriteString("INDEX ")
	if idx.Optional {
		str.WriteString("IF NOT EXISTS ")
	}
	if idx.SchemaName != "" {
		str.WriteString(quoteIdent(idx.SchemaName))
		str.WriteString(".")
	}
	str.WriteString(quoteIdent(idx.IndexName))
	str.WriteString(" ON ")
	str.WriteString(quoteIdent(idx.TableName))
	str.WriteString(" (")

	var hasCol bool
	for _, col := range idx.Columns {
		name := strings.TrimSpace(col.Name)
		if name == "" {
			continue
		}

		if hasCol {
			str.WriteString(", ")
		}
		hasCol = true

		isExpr := strings.Contains(name, "(") || strings.Contains(name, " ")
		if isExpr {
			name = pgPortableQuotes(name)
		}

		var term string
		if isExpr {
			term = "(" + name + ")"
		} else {
			term = quoteIdent(name)
		}

		collate := strings.TrimSpace(col.Collate)

		switch {
		case strings.EqualFold(collate, "NOCASE"):
			if isExpr {
				term = "(lower(" + name + "))"
			} else {
				term = "lower(" + term + ")"
			}
		case strings.EqualFold(collate, "BINARY"):
			term += ` COLLATE "C"`
		case collate != "":
			term += " COLLATE " + quoteIdent(collate)
		}

		str.WriteString(term)

		if col.Sort != "" {
			str.WriteString(" ")
			str.WriteString(strings.ToUpper(col.Sort))
		}
	}

	str.WriteString(")")

	if idx.Where != "" {
		str.WriteString(" WHERE ")
		str.WriteString(pgPortableQuotes(idx.Where))
	}

	return str.String()
}

// pgPortableQuotes converts the backtick identifier quotes of the portable
// index form (the one stored in the collection "indexes" field) in an
// expression or WHERE clause to Postgres double quotes.
func pgPortableQuotes(expr string) string {
	return strings.ReplaceAll(expr, "`", `"`)
}

// ConvertMultiplicity changes the column type in place with
// "ALTER COLUMN ... TYPE ... USING". Postgres refuses to retype a
// column that is used by a view so the views are dropped and recreated.
func (postgresDialect) ConvertMultiplicity(db dbx.Builder, table, column string, toMultiple bool) (bool, error) {
	// a field that is added together with its data has the target type already
	// (the caller cannot tell it from a real single <-> multiple change)
	var dataType string
	_ = db.NewQuery(`SELECT data_type::text FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = {:table} AND column_name = {:column}`).
		Bind(dbx.Params{"table": table, "column": column}).Row(&dataType)
	if dataType == "" || (toMultiple && dataType == "jsonb") || (!toMultiple && dataType == "text") {
		return true, nil
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

	t, c := quoteIdent(table), quoteIdent(column)

	var alter string
	if toMultiple {
		// single (text) -> multiple (array)
		alter = fmt.Sprintf(
			`ALTER TABLE %[1]s
				ALTER COLUMN %[2]s DROP DEFAULT,
				ALTER COLUMN %[2]s TYPE JSONB USING (
					CASE WHEN COALESCE(%[2]s, '') = '' THEN '[]'::jsonb ELSE jsonb_build_array(%[2]s) END
				),
				ALTER COLUMN %[2]s SET DEFAULT '[]'::jsonb`,
			t, c,
		)
	} else {
		// multiple (array) -> single (keep only the last element)
		alter = fmt.Sprintf(
			`ALTER TABLE %[1]s
				ALTER COLUMN %[2]s DROP DEFAULT,
				ALTER COLUMN %[2]s TYPE TEXT COLLATE "C" USING (
					CASE
						WHEN %[2]s IS NULL THEN ''
						WHEN jsonb_typeof(%[2]s) = 'array' THEN COALESCE(%[2]s ->> -1, '')
						WHEN jsonb_typeof(%[2]s) = 'string' THEN %[2]s #>> '{}'
						ELSE %[2]s::text
					END
				),
				ALTER COLUMN %[2]s SET DEFAULT ''`,
			t, c,
		)
	}

	if _, err := db.NewQuery(alter).Execute(); err != nil {
		return true, err
	}

	for _, v := range views {
		if _, err := db.NewQuery(v.CreateSQL).Execute(); err != nil {
			return true, err
		}
	}

	return true, nil
}

func pgListViews(db dbx.Builder) ([]ViewDef, error) {
	rows := []struct {
		Name string `db:"name"`
		Def  string `db:"def"`
	}{}

	// ordered by oid to keep the creation (and therefore dependency) order
	err := db.NewQuery(`
		SELECT c.relname::text AS name, pg_get_viewdef(c.oid, true) AS def
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'v' AND n.nspname = current_schema()
		ORDER BY c.oid
	`).All(&rows)
	if err != nil {
		return nil, err
	}

	result := make([]ViewDef, 0, len(rows))
	for _, r := range rows {
		result = append(result, ViewDef{
			Name:      r.Name,
			CreateSQL: "CREATE VIEW " + quoteIdent(r.Name) + " AS " + strings.TrimRight(strings.TrimSpace(r.Def), "; \n"),
		})
	}

	return result, nil
}

func (postgresDialect) HasTable(db dbx.Builder, name string) bool {
	var exists int

	err := db.NewQuery(`
		SELECT 1 FROM information_schema.tables
		WHERE table_schema = current_schema() AND LOWER(table_name) = LOWER({:name})
		LIMIT 1
	`).Bind(dbx.Params{"name": name}).Row(&exists)

	return err == nil && exists > 0
}

// pgColumnsQuery lists the visible columns of a table or view
// (the hidden row id column is excluded).
const pgColumnsQuery = `
	SELECT
		(a.attnum - 1)::int AS cid,
		a.attname::text AS name,
		format_type(a.atttypid, a.atttypmod) AS type,
		a.attnotnull AS notnull,
		pg_get_expr(d.adbin, d.adrelid) AS dflt_value,
		(CASE WHEN EXISTS (
			SELECT 1 FROM pg_index i
			WHERE i.indrelid = a.attrelid AND i.indisprimary AND a.attnum = ANY (i.indkey)
		) THEN 1 ELSE 0 END)::int AS pk
	FROM pg_class c
	JOIN pg_namespace n ON n.oid = c.relnamespace
	JOIN pg_attribute a ON a.attrelid = c.oid
	LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
	WHERE n.nspname = current_schema()
		AND c.relname = {:name}
		AND c.relkind IN ('r', 'v', 'p', 'm')
		AND a.attnum > 0
		AND NOT a.attisdropped
		AND a.attname <> '` + RowIDColumn + `'
	ORDER BY a.attnum
`

func (postgresDialect) TableColumns(db dbx.Builder, table string) ([]string, error) {
	info, err := pgTableInfo(db, table)
	if err != nil {
		return nil, err
	}

	columns := make([]string, 0, len(info))
	for _, row := range info {
		columns = append(columns, row.Name)
	}

	return columns, nil
}

func (postgresDialect) TableInfo(db dbx.Builder, table string) ([]*TableInfoRow, error) {
	info, err := pgTableInfo(db, table)
	if err != nil {
		return nil, err
	}

	if len(info) == 0 {
		return nil, fmt.Errorf("empty table info probably due to invalid or missing table %s", table)
	}

	return info, nil
}

func pgTableInfo(db dbx.Builder, table string) ([]*TableInfoRow, error) {
	info := []*TableInfoRow{}

	err := db.NewQuery(pgColumnsQuery).Bind(dbx.Params{"name": table}).All(&info)

	return info, err
}

func (postgresDialect) TableIndexes(db dbx.Builder, table string) (map[string]string, error) {
	rows := []struct {
		Name   string `db:"name"`
		SQL    string `db:"sql"`
		Schema string `db:"schema"`
	}{}

	// constraint indexes (primary key, unique constraints) are
	// excluded, like SQLite excludes its "sqlite_autoindex_*" ones
	err := db.NewQuery(`
		SELECT i.indexname::text AS name, i.indexdef AS sql, i.schemaname::text AS schema
		FROM pg_indexes i
		WHERE i.schemaname = current_schema()
			AND i.tablename = {:table}
			AND NOT EXISTS (
				SELECT 1 FROM pg_constraint c
				JOIN pg_class ic ON ic.oid = c.conindid
				WHERE ic.relname = i.indexname AND c.connamespace = (SELECT oid FROM pg_namespace WHERE nspname = i.schemaname)
			)
	`).Bind(dbx.Params{"table": table}).All(&rows)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string, len(rows))
	for _, r := range rows {
		// normalize to the portable form (no schema qualification, no access method)
		def := strings.Replace(r.SQL, " ON "+r.Schema+".", " ON ", 1)
		def = strings.Replace(def, " USING btree", "", 1)
		result[r.Name] = def
	}

	return result, nil
}

func (postgresDialect) IndexOwner(db dbx.Builder, indexName string, skipTables ...string) string {
	q := db.Select("tablename").
		From("pg_indexes").
		AndWhere(dbx.NewExp("schemaname = current_schema()"))

	for i, t := range skipTables {
		q.AndWhere(dbx.NewExp(
			fmt.Sprintf("LOWER([[tablename]]) != LOWER({:skip%d})", i),
			dbx.Params{fmt.Sprintf("skip%d", i): t},
		))
	}

	var owner string
	_ = q.AndWhere(dbx.NewExp("LOWER([[indexname]]) = LOWER({:indexName})", dbx.Params{"indexName": indexName})).
		Limit(1).
		Row(&owner)

	return owner
}

func (postgresDialect) Vacuum(db dbx.Builder) error {
	_, err := db.NewQuery("VACUUM").Execute()
	return err
}

// autovacuum/analyze take care of it
func (postgresDialect) Optimize(db dbx.Builder) error { return nil }

// there is no wal file to truncate
func (postgresDialect) Checkpoint(db dbx.Builder) error { return nil }

func (postgresDialect) LockMigrations(db dbx.Builder) error {
	_, err := db.NewQuery("SELECT pg_advisory_xact_lock(" + strconv.FormatInt(migrationsLockKey, 10) + ")").Execute()
	return err
}

// Try wraps fn in a savepoint when db is a transaction because a failed
// statement aborts the whole Postgres transaction ("current transaction
// is aborted, commands ignored until end of transaction block").
func (postgresDialect) Try(db dbx.Builder, fn func() error) error {
	if _, ok := db.(*dbx.Tx); !ok {
		return fn()
	}

	name := "claviger_try_" + strconv.FormatUint(savepointSeq.Add(1), 10)

	if _, err := db.NewQuery("SAVEPOINT " + name).Execute(); err != nil {
		return err
	}

	if err := fn(); err != nil {
		if _, rbErr := db.NewQuery("ROLLBACK TO SAVEPOINT " + name).Execute(); rbErr != nil {
			return fmt.Errorf("%w (and savepoint rollback failed: %v)", err, rbErr)
		}
		_, _ = db.NewQuery("RELEASE SAVEPOINT " + name).Execute()
		return err
	}

	_, err := db.NewQuery("RELEASE SAVEPOINT " + name).Execute()

	return err
}

func (postgresDialect) QuoteIdent(name string) string { return quoteIdent(name) }

// quoteIdent double quotes a Postgres identifier.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
