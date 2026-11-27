package dialect

// MySQL 8.0+ and MariaDB 10.11+ (issue #106, experimental).
//
// The SQL of the app is written for SQLite and reaches MySQL through the same
// seam as PostgreSQL. What differs, and how this dialect answers it:
//
//   - Identifiers are backtick quoted, like the SQLite flavored SQL the app
//     builds, so nothing needs translating. The connection runs with
//     sql_mode PIPES_AS_CONCAT (the filter SQL concatenates with ||) and
//     NO_BACKSLASH_ESCAPES (the filter SQL writes ESCAPE '\'), see
//     core/db_connect_mysql.go.
//   - Text is LONGTEXT in utf8mb4_bin (byte order, case sensitive, like
//     SQLite's BINARY), ids are VARCHAR(255). A TEXT column cannot have a
//     literal default, so defaults are written as expressions: DEFAULT ('').
//   - JSON is the native JSON type. Extracted values are converted to SQL
//     scalars (see JSONExtract), so comparisons keep SQLite's semantics.
//   - MySQL has no partial indexes, no COLLATE NOCASE and cannot index a TEXT
//     column whole. BuildIndexOn (the index builder that looks at the column
//     types) uses a prefix key for a plain index, and for a unique index on
//     text a unique expression index over SHA2(column, 256). A partial unique
//     index (WHERE cond) becomes an expression that is NULL when cond is false
//     (NULLs do not collide), so "unique where email != ''" keeps its meaning.
//     A partial plain index is created without its WHERE.
//   - DDL commits implicitly: a migration or a collection save that fails
//     half way is not rolled back.
//   - There is no LISTEN/NOTIFY: several replicas are refused.
//   - Statements can fail with a deadlock (1213) or a lock wait timeout
//     (1205): the app retries the transaction (core/db_tx.go).

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dbutils"
)

// MySQL is the MySQL 8.0+ dialect.
var MySQL Dialect = mysqlDialect{}

// MariaDB is the MariaDB 10.11+ dialect (same driver, same code, a few
// differences in collation names and JSON).
var MariaDB Dialect = mysqlDialect{maria: true}

// MySQLErrorNumber extracts the server error number from a driver error, or 0.
// The driver is only linked by package core (core/db_connect_mysql.go), which
// sets this hook, so that this package stays free of it.
var MySQLErrorNumber = func(error) uint16 { return 0 }

type mysqlDialect struct {
	maria bool
}

func (d mysqlDialect) Name() string {
	if d.maria {
		return MariaDBName
	}
	return MySQLName
}

func (mysqlDialect) SeparateWritePool() bool { return false }

func (mysqlDialect) QuoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// textCharset is the character set and the byte order collation of every text column.
const mysqlTextCharset = "CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"

func (mysqlDialect) RandomIDDefault() string {
	return `(CONCAT('r', LOWER(HEX(RANDOM_BYTES(7)))))`
}

// 2006-01-02 15:04:05.000Z in UTC (DATE_FORMAT has microseconds only).
func (mysqlDialect) NowDefault() string {
	return `(CONCAT(SUBSTRING(DATE_FORMAT(UTC_TIMESTAMP(3), '%Y-%m-%d %H:%i:%s.%f'), 1, 23), 'Z'))`
}

func (mysqlDialect) StringDefault(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func (d mysqlDialect) IDColumn() string {
	return "VARCHAR(255) " + mysqlTextCharset + " PRIMARY KEY DEFAULT " + d.RandomIDDefault() + " NOT NULL"
}

func (mysqlDialect) TextType() string { return "LONGTEXT " + mysqlTextCharset }

func (d mysqlDialect) TextColumn() string { return d.TextType() + " DEFAULT ('') NOT NULL" }

func (mysqlDialect) NumberColumn() string { return "DOUBLE DEFAULT 0 NOT NULL" }

func (mysqlDialect) BoolColumn() string { return "BOOLEAN DEFAULT FALSE NOT NULL" }

func (d mysqlDialect) JSONColumn(defaultValue string) string {
	if defaultValue == "" {
		return d.JSONType() + " DEFAULT NULL"
	}

	return d.JSONType() + " DEFAULT ('" + strings.ReplaceAll(defaultValue, "'", "''") + "') NOT NULL"
}

func (mysqlDialect) Int64Type() string { return "BIGINT" }

func (mysqlDialect) JSONType() string { return "JSON" }

// MySQL has no rowid: a hidden auto increment column keeps the insertion order.
func (mysqlDialect) HiddenColumns() map[string]string {
	return map[string]string{RowIDColumn: "BIGINT NOT NULL AUTO_INCREMENT UNIQUE"}
}

func (mysqlDialect) RowOrder() string { return "`" + RowIDColumn + "` ASC" }

// ---------------------------------------------------------------------
// Filters, sorting and JSON
// ---------------------------------------------------------------------

func (mysqlDialect) BoolLiteral(v bool) string {
	if v {
		return "TRUE"
	}
	return "FALSE"
}

// IsOp is the null safe comparison. MySQL spells it <=> and has no negated
// infix form, so the filter builder asks NullSafeEqual (see tools/search) and
// this is only the fallback for a plain comparison.
func (mysqlDialect) IsOp(not bool) string {
	if not {
		return "<>"
	}
	return "<=>"
}

// NullSafeEqual renders the null safe (in)equality of two expressions.
func (mysqlDialect) NullSafeEqual(left, right string, not bool) string {
	if not {
		return "NOT (" + left + " <=> " + right + ")"
	}
	return left + " <=> " + right
}

// AsText converts to text: MySQL compares a number with ” as numbers (0 = ”),
// SQLite does not. The cast is also a unit for the LIKE operator below, which
// puts a COLLATE after it.
func (mysqlDialect) AsText(expr string) string { return "CAST(" + expr + " AS CHAR)" }

// SQLite's LIKE is case insensitive for ASCII. The text columns are binary, so
// the left operand gets a case insensitive (accent sensitive) collation,
// which the comparison then uses ("a COLLATE x LIKE b").
func (d mysqlDialect) LikeOp(not bool) string {
	coll := "utf8mb4_0900_as_ci"
	if d.maria {
		coll = "utf8mb4_uca1400_as_ci"
	}
	if not {
		return "COLLATE " + coll + " NOT LIKE"
	}
	return "COLLATE " + coll + " LIKE"
}

// MySQL sorts NULLs first for ASC and last for DESC, like SQLite.
func (mysqlDialect) OrderTerm(expr, direction string) string { return expr + " " + direction }

func (d mysqlDialect) JSONOrderTerm(expr, direction string) string {
	return d.OrderTerm(expr, direction)
}

func (mysqlDialect) NoCaseEqual(column, value string) string {
	return "LOWER(" + column + ") = LOWER(" + value + ")"
}

// MySQL converts between text and numbers in comparisons, like SQLite.
func (mysqlDialect) StrictTypes() bool { return false }

func (mysqlDialect) ScalarCast(kind ScalarKind, expr string) string { return expr }

// JSONExtract returns SQL scalars, see below.
func (mysqlDialect) NativeJSON() bool { return false }

const mysqlJSONTableColumns = "'$[*]' COLUMNS (`value` LONGTEXT " + mysqlTextCharset + " PATH '$' NULL ON ERROR)"

func (mysqlDialect) JSONEach(column string, multiple bool) string {
	if !multiple {
		return "LATERAL (SELECT [[" + column + "]] AS `value`)"
	}

	c := "[[" + column + "]]"

	return "JSON_TABLE(CASE WHEN JSON_VALID(" + c + ") THEN (CASE WHEN JSON_TYPE(" + c + ") = 'ARRAY' THEN " + c +
		" ELSE JSON_ARRAY(" + c + ") END) ELSE JSON_ARRAY(" + c + ") END, " + mysqlJSONTableColumns + ")"
}

func (mysqlDialect) JSONEachParam(placeholder string) string {
	return "JSON_TABLE({:" + placeholder + "}, " + mysqlJSONTableColumns + ")"
}

func (mysqlDialect) JSONArrayLength(column string, multiple bool) string {
	c := "[[" + column + "]]"

	if !multiple {
		return "(CASE WHEN " + c + " = '' OR " + c + " IS NULL THEN 0 ELSE 1 END)"
	}

	return "(CASE WHEN JSON_VALID(" + c + ") THEN (CASE WHEN JSON_TYPE(" + c + ") = 'ARRAY' THEN JSON_LENGTH(" + c +
		") ELSE 1 END) WHEN " + c + " = '' OR " + c + " IS NULL THEN 0 ELSE 1 END)"
}

// mysqlScalar converts a JSON value to the SQL value that SQLite's
// json_extract would return: NULL for a JSON null, 1 and 0 for the booleans,
// numbers and text as text (MySQL compares it with numbers as numbers).
func mysqlScalar(x string) string {
	return "(CASE JSON_TYPE(" + x + ") WHEN 'NULL' THEN NULL WHEN 'BOOLEAN' THEN IF(JSON_UNQUOTE(" + x +
		") = 'true', 1, 0) ELSE JSON_UNQUOTE(" + x + ") END)"
}

// JSONExtract extracts the value at path of a JSON column, or, like SQLite's
// helper, of a text column that does not hold JSON (then the path applies to
// the text wrapped in an object).
func (mysqlDialect) JSONExtract(column, path string) string {
	c := "[[" + column + "]]"

	if path != "" && !strings.HasPrefix(path, "[") {
		path = "." + path
	}

	return "(CASE WHEN JSON_VALID(" + c + ") THEN " + mysqlScalar("JSON_EXTRACT("+c+", '$"+path+"')") +
		" ELSE " + mysqlScalar("JSON_EXTRACT(JSON_OBJECT('pb', "+c+"), '$.pb"+path+"')") + " END)"
}

func (mysqlDialect) JSONText(expr string) string { return expr }

func (mysqlDialect) JSONFromScalar(kind ScalarKind, expr string) string { return expr }

func (mysqlDialect) JSONNumber(expr string) string { return expr }

func (mysqlDialect) ColumnCast(expr, columnDef string) string { return expr }

// MySQL requires an ON condition for every LEFT JOIN.
func (mysqlDialect) JoinOn() string { return "TRUE" }

// MySQL requires an alias for every derived table.
func (mysqlDialect) SubqueryAlias() string { return " AS pb_sub" }

func (mysqlDialect) ColumnKind(sqlType string) ColumnKind {
	t := strings.ToLower(strings.TrimSpace(sqlType))
	if t == "tinyint(1)" || t == "bool" || t == "boolean" {
		return ColumnBool
	}
	if i := strings.IndexByte(t, '('); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}

	switch t {
	case "varchar", "char", "text", "tinytext", "mediumtext", "longtext":
		return ColumnText
	case "double", "float", "decimal", "numeric", "real":
		return ColumnFloat
	case "bigint", "int", "integer", "smallint", "mediumint", "tinyint":
		return ColumnInt
	case "json":
		return ColumnJSON
	}

	return ColumnUnknown
}

// ---------------------------------------------------------------------
// strftime and geoDistance
// ---------------------------------------------------------------------

var _ FunctionBuilder = mysqlDialect{}

func (mysqlDialect) GeoDistance(lonA, latA, lonB, latB string) string {
	return `(6371 * acos(LEAST(1, GREATEST(-1, ` +
		`cos(radians(` + latA + `)) * cos(radians(` + latB + `)) * ` +
		`cos(radians(` + lonB + `) - radians(` + lonA + `)) + ` +
		`sin(radians(` + latA + `)) * sin(radians(` + latB + `))` +
		`))))`
}

// DATE_FORMAT specifiers of the supported strftime ones.
var mysqlStrftimeSpecs = map[byte]string{
	'Y': "%Y", 'm': "%m", 'd': "%d", 'H': "%H", 'M': "%i", 'S': "%S",
	'j': "%j", 'w': "%w", 'e': "%e", 'k': "%k", 'l': "%l", 'I': "%h",
	'p': "%p", 'F': "%Y-%m-%d", 'T': "%H:%i:%S", 'R': "%H:%i",
}

var mysqlIntervalModifier = regexp.MustCompile(`(?i)^([+-]?\d+)\s*(second|minute|hour|day|month|year)s?$`)

// StrFTime translates the strftime() filter function for the formats and
// modifiers of mysqlStrftimeSpecs (plus %f, %s and %%), the start of
// day/month/year and the [+-]N unit modifiers. Anything else is an error when
// the filter is built.
func (mysqlDialect) StrFTime(format string, timeValue string, timeValueIsNumber bool, modifiers []string) (string, error) {
	var ts string
	switch {
	case timeValue == "":
		ts = "UTC_TIMESTAMP(3)"
	case timeValueIsNumber:
		return "", errors.New("strftime() with a number (julian day or unix time) is not supported on MySQL")
	default:
		t := "CAST(" + timeValue + " AS CHAR)"
		ts = "(CASE WHEN LOWER(" + t + ") = 'now' THEN UTC_TIMESTAMP(3) ELSE " +
			"CAST(REPLACE(REPLACE(REPLACE(" + t + ", 'T', ' '), 'Z', ''), 'z', '') AS DATETIME(3)) END)"
	}

	for _, m := range modifiers {
		lm := strings.ToLower(strings.TrimSpace(m))

		switch lm {
		case "utc", "auto", "subsec", "subsecond":
			continue
		case "start of day":
			ts = "CAST(DATE(" + ts + ") AS DATETIME(3))"
			continue
		case "start of month":
			ts = "CAST(DATE_FORMAT(" + ts + ", '%Y-%m-01') AS DATETIME(3))"
			continue
		case "start of year":
			ts = "CAST(DATE_FORMAT(" + ts + ", '%Y-01-01') AS DATETIME(3))"
			continue
		}

		if match := mysqlIntervalModifier.FindStringSubmatch(strings.TrimSpace(m)); match != nil {
			n, _ := strconv.Atoi(match[1])
			ts = "(" + ts + " + INTERVAL " + strconv.Itoa(n) + " " + strings.ToUpper(match[2]) + ")"
			continue
		}

		return "", fmt.Errorf("the strftime modifier %q is not supported on MySQL", m)
	}

	var parts []string
	var lit strings.Builder

	flush := func() {
		if lit.Len() > 0 {
			parts = append(parts, "'"+strings.ReplaceAll(lit.String(), "'", "''")+"'")
			lit.Reset()
		}
	}

	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' {
			lit.WriteByte(c)
			continue
		}

		i++
		if i >= len(format) {
			return "", fmt.Errorf("the strftime format %q ends with a dangling %%", format)
		}

		spec := format[i]

		switch {
		case spec == '%':
			lit.WriteByte('%')
		case spec == 'f':
			flush()
			parts = append(parts, "SUBSTRING(DATE_FORMAT(@@TS@@, '%S.%f'), 1, 6)")
		case spec == 's':
			flush()
			parts = append(parts, "CAST(FLOOR(UNIX_TIMESTAMP(@@TS@@)) AS CHAR)")
		default:
			spec, ok := mysqlStrftimeSpecs[spec]
			if !ok {
				return "", fmt.Errorf("the strftime format specifier %%%c is not supported on MySQL", format[i])
			}
			flush()
			parts = append(parts, "DATE_FORMAT(@@TS@@, '"+spec+"')")
		}
	}

	flush()

	var out string
	switch len(parts) {
	case 0:
		out = "(CASE WHEN @@TS@@ IS NULL THEN NULL ELSE '' END)"
	case 1:
		out = parts[0]
	default:
		out = "CONCAT(" + strings.Join(parts, ", ") + ")"
	}

	return strings.ReplaceAll(out, "@@TS@@", ts), nil
}

// ---------------------------------------------------------------------
// DDL
// ---------------------------------------------------------------------

// BuildIndex renders the portable index without knowing the column types: a
// plain CREATE INDEX with backtick quoted identifiers (COLLATE NOCASE becomes
// a LOWER() expression key). See [mysqlDialect.BuildIndexOn] for the version
// that the app uses.
func (d mysqlDialect) BuildIndex(idx dbutils.Index) string {
	sql, _ := d.BuildIndexOn(nil, idx)
	return sql
}

// IndexBuilder is implemented by dialects whose CREATE INDEX depends on the
// types of the indexed columns (they are read from the database).
type IndexBuilder interface {
	BuildIndexOn(db dbx.Builder, idx dbutils.Index) (string, error)
}

// BuildIndexFor renders idx for d, asking the database for the column types
// when the dialect needs them.
func BuildIndexFor(d Dialect, db dbx.Builder, idx dbutils.Index) (string, error) {
	if b, ok := d.(IndexBuilder); ok {
		return b.BuildIndexOn(db, idx)
	}
	return d.BuildIndex(idx), nil
}

const mysqlMaxPrefix = 191

var mysqlIdentRe = regexp.MustCompile("^`?([A-Za-z0-9_$]+)`?$")

// BuildIndexOn is the MySQL CREATE INDEX of a portable index, see the file
// comment. db may be nil, then every column is taken for a non text one.
func (d mysqlDialect) BuildIndexOn(db dbx.Builder, idx dbutils.Index) (string, error) {
	if !idx.IsValid() {
		return "", nil
	}

	types := map[string]string{}
	if db != nil {
		rows := []struct {
			Name string `db:"name"`
			Type string `db:"type"`
		}{}
		_ = db.NewQuery(`SELECT COLUMN_NAME AS name, DATA_TYPE AS type FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = {:table}`).
			Bind(dbx.Params{"table": idx.TableName}).All(&rows)
		for _, r := range rows {
			types[strings.ToLower(r.Name)] = strings.ToLower(r.Type)
		}
	}

	isText := func(col string) bool {
		switch types[strings.ToLower(col)] {
		case "text", "tinytext", "mediumtext", "longtext", "json", "blob", "longblob":
			return true
		}
		return false
	}

	where := strings.TrimSpace(idx.Where)

	var keys []string
	for _, col := range idx.Columns {
		name := strings.TrimSpace(col.Name)
		if name == "" {
			continue
		}

		var plain string // the bare column when it is one
		if m := mysqlIdentRe.FindStringSubmatch(name); m != nil {
			plain = m[1]
		}

		expr := name
		if plain != "" {
			expr = d.QuoteIdent(plain)
		}

		if strings.EqualFold(strings.TrimSpace(col.Collate), "NOCASE") {
			expr = "LOWER(" + expr + ")"
			plain = ""
		}

		var key string
		switch {
		case idx.Unique && where != "":
			// partial unique index: NULL (never equal) when the condition is false
			value := expr
			if plain == "" || isText(plain) {
				value = "SHA2(" + expr + ", 256)"
			}
			key = "((CASE WHEN " + where + " THEN " + value + " END))"
		case idx.Unique && (isText(plain) || plain == ""):
			key = "((SHA2(" + expr + ", 256)))"
		case plain != "" && isText(plain):
			key = expr + "(" + strconv.Itoa(mysqlMaxPrefix) + ")"
		case plain == "":
			// a functional key part cannot be a TEXT: cast it (a prefix key, like the plain columns)
			key = "((CAST(" + expr + " AS CHAR(" + strconv.Itoa(mysqlMaxPrefix) + "))))"
		default:
			key = expr
		}

		if col.Sort != "" && !strings.HasPrefix(key, "(") {
			key += " " + strings.ToUpper(col.Sort)
		}

		keys = append(keys, key)
	}

	var str strings.Builder
	str.WriteString("CREATE ")
	if idx.Unique {
		str.WriteString("UNIQUE ")
	}
	str.WriteString("INDEX ")
	str.WriteString(d.QuoteIdent(idx.IndexName))
	str.WriteString(" ON ")
	str.WriteString(d.QuoteIdent(idx.TableName))
	str.WriteString(" (" + strings.Join(keys, ", ") + ")")

	return str.String(), nil
}

// MySQL does not cascade and leaves a view that lost its table dangling, like SQLite.
func (mysqlDialect) DropSuffix() string { return "" }

func (mysqlDialect) DetachDependentViews(db dbx.Builder, view string) (func() error, error) {
	return func() error { return nil }, nil
}

// ConvertMultiplicity rebuilds the column (ALTER ... MODIFY cannot convert
// between JSON and text). DDL commits implicitly, so this is not atomic.
func (d mysqlDialect) ConvertMultiplicity(db dbx.Builder, table, column string, toMultiple bool) (bool, error) {
	var dataType string
	_ = db.NewQuery(`SELECT DATA_TYPE FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = {:table} AND COLUMN_NAME = {:column}`).
		Bind(dbx.Params{"table": table, "column": column}).Row(&dataType)

	dataType = strings.ToLower(dataType)
	if dataType == "" || (toMultiple && dataType == "json") || (!toMultiple && dataType != "json") {
		return true, nil
	}

	t, c, tmp := d.QuoteIdent(table), d.QuoteIdent(column), d.QuoteIdent("_pb_"+column+"_conv")

	var add, update string
	if toMultiple {
		add = "ALTER TABLE " + t + " ADD COLUMN " + tmp + " " + d.JSONColumn("[]")
		update = "UPDATE " + t + " SET " + tmp + " = (CASE WHEN COALESCE(" + c + ", '') = '' THEN JSON_ARRAY() ELSE JSON_ARRAY(" + c + ") END)"
	} else {
		add = "ALTER TABLE " + t + " ADD COLUMN " + tmp + " " + d.TextColumn()
		update = "UPDATE " + t + " SET " + tmp + " = (CASE WHEN " + c + " IS NULL OR JSON_LENGTH(" + c + ") = 0 THEN '' " +
			"WHEN JSON_TYPE(" + c + ") = 'ARRAY' THEN COALESCE(JSON_UNQUOTE(JSON_EXTRACT(" + c + ", CONCAT('$[', JSON_LENGTH(" + c + ") - 1, ']'))), '') " +
			"ELSE JSON_UNQUOTE(" + c + ") END)"
	}

	for _, q := range []string{
		add,
		update,
		"ALTER TABLE " + t + " DROP COLUMN " + c,
		"ALTER TABLE " + t + " RENAME COLUMN " + tmp + " TO " + c,
	} {
		if _, err := db.NewQuery(q).Execute(); err != nil {
			return true, err
		}
	}

	return true, nil
}

// ---------------------------------------------------------------------
// Introspection
// ---------------------------------------------------------------------

func (mysqlDialect) HasTable(db dbx.Builder, name string) bool {
	var exists int

	err := db.NewQuery(`
		SELECT 1 FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND LOWER(TABLE_NAME) = LOWER({:name})
		LIMIT 1
	`).Bind(dbx.Params{"name": name}).Row(&exists)

	return err == nil && exists > 0
}

const mysqlColumnsQuery = `
	SELECT
		CAST(ORDINAL_POSITION - 1 AS SIGNED) AS cid,
		COLUMN_NAME AS name,
		COLUMN_TYPE AS type,
		CAST(IS_NULLABLE = 'NO' AS SIGNED) AS notnull,
		COLUMN_DEFAULT AS dflt_value,
		CAST(COLUMN_KEY = 'PRI' AS SIGNED) AS pk
	FROM information_schema.COLUMNS
	WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = {:name} AND COLUMN_NAME <> '` + RowIDColumn + `'
	ORDER BY ORDINAL_POSITION
`

func mysqlTableInfo(db dbx.Builder, table string) ([]*TableInfoRow, error) {
	info := []*TableInfoRow{}
	err := db.NewQuery(mysqlColumnsQuery).Bind(dbx.Params{"name": table}).All(&info)
	return info, err
}

func (mysqlDialect) TableColumns(db dbx.Builder, table string) ([]string, error) {
	info, err := mysqlTableInfo(db, table)
	if err != nil {
		return nil, err
	}

	columns := make([]string, 0, len(info))
	for _, row := range info {
		columns = append(columns, row.Name)
	}

	return columns, nil
}

func (mysqlDialect) TableInfo(db dbx.Builder, table string) ([]*TableInfoRow, error) {
	info, err := mysqlTableInfo(db, table)
	if err != nil {
		return nil, err
	}

	if len(info) == 0 {
		return nil, fmt.Errorf("empty table info probably due to invalid or missing table %s", table)
	}

	return info, nil
}

// TableIndexes returns the indexes of the table in the portable form.
// Expression keys (the unique and partial ones, see the file comment) are
// returned as their plain column list.
func (d mysqlDialect) TableIndexes(db dbx.Builder, table string) (map[string]string, error) {
	rows := []struct {
		Name      string  `db:"name"`
		NonUnique int     `db:"non_unique"`
		Seq       int     `db:"seq"`
		Column    *string `db:"col"`
		Expr      *string `db:"expr"`
		Sort      *string `db:"sort"`
	}{}

	err := db.NewQuery(`
		SELECT INDEX_NAME AS name, CAST(NON_UNIQUE AS SIGNED) AS non_unique, CAST(SEQ_IN_INDEX AS SIGNED) AS seq,
			COLUMN_NAME AS col, EXPRESSION AS expr, COLLATION AS sort
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = {:table} AND INDEX_NAME <> 'PRIMARY'
		ORDER BY INDEX_NAME, SEQ_IN_INDEX
	`).Bind(dbx.Params{"table": table}).All(&rows)
	if err != nil {
		return nil, err
	}

	byName := map[string]*dbutils.Index{}
	var order []string
	for _, r := range rows {
		// the unique key of the hidden row id column, and the ones that a
		// UNIQUE column of a CREATE TABLE made (named like their column)
		if r.Column != nil && (*r.Column == RowIDColumn || (r.NonUnique == 0 && *r.Column == r.Name)) {
			continue
		}

		idx, ok := byName[r.Name]
		if !ok {
			idx = &dbutils.Index{IndexName: r.Name, TableName: table, Unique: r.NonUnique == 0}
			byName[r.Name] = idx
			order = append(order, r.Name)
		}

		col := dbutils.IndexColumn{}
		switch {
		case r.Column != nil:
			col.Name = *r.Column
		case r.Expr != nil:
			col.Name = *r.Expr
		}
		if r.Sort != nil && *r.Sort == "D" {
			col.Sort = "DESC"
		}
		idx.Columns = append(idx.Columns, col)
	}

	sort.Strings(order)

	result := make(map[string]string, len(order))
	for _, name := range order {
		result[name] = byName[name].Build()
	}

	return result, nil
}

func (mysqlDialect) IndexOwner(db dbx.Builder, indexName string, skipTables ...string) string {
	q := db.Select("TABLE_NAME").
		From("information_schema.STATISTICS").
		AndWhere(dbx.NewExp("TABLE_SCHEMA = DATABASE()"))

	for i, t := range skipTables {
		q.AndWhere(dbx.NewExp(
			fmt.Sprintf("LOWER(TABLE_NAME) != LOWER({:skip%d})", i),
			dbx.Params{fmt.Sprintf("skip%d", i): t},
		))
	}

	var owner string
	_ = q.AndWhere(dbx.NewExp("LOWER(INDEX_NAME) = LOWER({:indexName})", dbx.Params{"indexName": indexName})).
		Limit(1).
		Row(&owner)

	return owner
}

// ---------------------------------------------------------------------
// Maintenance and transactions
// ---------------------------------------------------------------------

func (mysqlDialect) Vacuum(db dbx.Builder) error     { return nil }
func (mysqlDialect) Optimize(db dbx.Builder) error   { return nil }
func (mysqlDialect) Checkpoint(db dbx.Builder) error { return nil }

// LockMigrations is a no-op: GET_LOCK belongs to a session and is not released
// at the end of the migrations transaction. Start one instance on an empty
// database first.
func (mysqlDialect) LockMigrations(db dbx.Builder) error { return nil }

// Try just runs fn: a failed statement does not abort a MySQL transaction.
func (mysqlDialect) Try(db dbx.Builder, fn func() error) error { return fn() }

func (mysqlDialect) Retries() bool { return true }

// IsRetryable is a deadlock (1213) or a lock wait timeout (1205).
func (mysqlDialect) IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	n := MySQLErrorNumber(err)
	return n == 1213 || n == 1205
}

var (
	mysqlDDLDefaultLiteral = regexp.MustCompile(`(?i)\bDEFAULT\s+('(?:[^']|'')*')`)
	mysqlDDLTextKey        = regexp.MustCompile(`(?i)\bTEXT(\s+(?:PRIMARY\s+KEY|UNIQUE)\b)`)
	mysqlDDLText           = regexp.MustCompile(`(?i)\bTEXT\b`)
	mysqlDDLIfNotExists    = regexp.MustCompile(`(?i)\b(CREATE\s+(?:UNIQUE\s+)?INDEX)\s+IF\s+NOT\s+EXISTS\b`)
)

// AdaptDDL: a TEXT key becomes VARCHAR(255), any other TEXT LONGTEXT, a
// literal default becomes an expression default (TEXT and JSON columns cannot
// have a literal one) and CREATE INDEX IF NOT EXISTS (not in MySQL) loses its
// clause. Index keys on text columns are the caller's business (use VARCHAR).
func (d mysqlDialect) AdaptDDL(sql string) string {
	if !d.maria {
		sql = mysqlDDLIfNotExists.ReplaceAllString(sql, "$1")
	}
	sql = mysqlDDLDefaultLiteral.ReplaceAllString(sql, "DEFAULT ($1)")
	sql = mysqlDDLTextKey.ReplaceAllString(sql, "VARCHAR(255) "+mysqlTextCharset+"$1")
	sql = mysqlDDLText.ReplaceAllString(sql, "LONGTEXT "+mysqlTextCharset)

	return sql
}

func (d mysqlDialect) DropIndexIfExists(db dbx.Builder, table, name string) error {
	var exists int
	err := db.NewQuery(`SELECT 1 FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND LOWER(TABLE_NAME) = LOWER({:table}) AND LOWER(INDEX_NAME) = LOWER({:name}) LIMIT 1`).
		Bind(dbx.Params{"table": table, "name": name}).Row(&exists)
	if err != nil || exists != 1 {
		return nil
	}

	_, err = db.NewQuery("DROP INDEX " + d.QuoteIdent(name) + " ON " + d.QuoteIdent(table)).Execute()
	return err
}

func (mysqlDialect) FinishIndexDrops(db dbx.Builder) error { return nil }

// RandomOrder is the ORDER BY term of the @random sort.
func (mysqlDialect) RandomOrder() string { return "RAND()" }

// NoRows is "no row in the FROM ... WHERE ... tail", see tools/search.
func (mysqlDialect) NoRows(fromWhere string) string {
	return "0 = (SELECT COUNT(*) " + fromWhere + ")"
}
