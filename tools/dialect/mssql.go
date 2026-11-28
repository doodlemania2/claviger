package dialect

// SQL Server 2019+ / Azure SQL (issue #106, experimental).
//
// The SQL of the app is written for SQLite and reaches SQL Server through the
// same seam as PostgreSQL and MySQL. What differs, and how this dialect
// answers it:
//
//   - Identifiers are [bracket] quoted: dbx's mssql builder does that for the
//     [[name]] placeholders, see mssql_builder.go for the DDL fixes.
//   - Text is NVARCHAR(MAX) with a binary (BIN2) collation: byte order and case
//     sensitive, like SQLite's BINARY. Ids are NVARCHAR(255). Booleans are BIT,
//     numbers FLOAT. SQL Server has no JSON type before 2025: JSON is stored
//     as NVARCHAR(MAX) text and read with JSON_VALUE / JSON_QUERY / OPENJSON.
//   - JSONExtract returns a sql_variant: a number is a float, a boolean 1 or
//     0, anything else text. The variant compares numbers with numbers and
//     text with text and never fails a conversion, which is what SQLite's
//     dynamic typing gives (an ORDER BY sorts numbers before text, see
//     JSONOrderTerm).
//   - A table valued function over a column (OPENJSON) can only be joined
//     with APPLY: see ApplyJoin and the join helpers of tools/search.
//   - There is no NULL safe equality operator before SQL Server 2022: see
//     NullSafeEqual.
//   - No partial and no functional indexes, a NVARCHAR(MAX) cannot be a key,
//     and a unique index treats NULLs as equal. BuildIndexOn (the builder that
//     looks at the column types) materializes the keys that need it as hidden
//     computed columns named _pbx_<index>_<n> (a SHA2 hash for a unique key on
//     text, LOWER() for COLLATE NOCASE, a CASE WHEN for a partial index, a
//     450 character prefix for a plain index on text) and creates a filtered
//     unique index that skips the NULL keys. The portable definition of the
//     index is kept in an extended property, so that TableIndexes can return
//     exactly what the collection stored.
//   - DDL runs in transactions. A column that has a DEFAULT cannot be dropped
//     before its default constraint is: the builder takes care of it.
//   - Deadlocks (1205) and snapshot update conflicts (3960) are retried.
//   - There is no LISTEN/NOTIFY: several replicas are refused.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dbutils"
)

// MSSQL is the SQL Server dialect.
var MSSQL Dialect = mssqlDialect{}

// MSSQLErrorNumber extracts the server error number from a driver error, or 0.
// The driver is only linked by package core (core/db_connect_mssql.go), which
// sets this hook, so that this package stays free of it.
var MSSQLErrorNumber = func(error) int32 { return 0 }

// mssqlHiddenPrefix is the prefix of the computed columns that hold index keys.
const mssqlHiddenPrefix = "_pbx_"

// MSSQLCollation is the binary collation of every text column (and the one
// that the database should be created with).
const MSSQLCollation = "Latin1_General_100_BIN2"

// mssqlCICollation is the case insensitive, accent sensitive collation used by
// the filter operator "~".
const mssqlCICollation = "Latin1_General_100_CI_AS"

type mssqlDialect struct{}

func (mssqlDialect) Name() string { return MSSQLName }

func (mssqlDialect) SeparateWritePool() bool { return false }

func (mssqlDialect) QuoteIdent(name string) string {
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

const mssqlCollate = " COLLATE " + MSSQLCollation

func (mssqlDialect) RandomIDDefault() string {
	return `('r' + LOWER(CONVERT(NVARCHAR(14), CRYPT_GEN_RANDOM(7), 2)))`
}

// 2006-01-02 15:04:05.000Z in UTC (style 121 has 7 fraction digits, the first 23 characters keep 3).
func (mssqlDialect) NowDefault() string {
	return `(CONVERT(NVARCHAR(23), SYSUTCDATETIME(), 121) + 'Z')`
}

func (mssqlDialect) StringDefault(value string) string {
	return "N'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func (d mssqlDialect) IDColumn() string {
	return "NVARCHAR(255)" + mssqlCollate + " PRIMARY KEY DEFAULT " + d.RandomIDDefault() + " NOT NULL"
}

func (mssqlDialect) TextType() string { return "NVARCHAR(MAX)" + mssqlCollate }

func (d mssqlDialect) TextColumn() string { return d.TextType() + " DEFAULT '' NOT NULL" }

func (mssqlDialect) NumberColumn() string { return "FLOAT DEFAULT 0 NOT NULL" }

func (mssqlDialect) BoolColumn() string { return "BIT DEFAULT 0 NOT NULL" }

func (d mssqlDialect) JSONColumn(defaultValue string) string {
	if defaultValue == "" {
		return d.JSONType() + " DEFAULT NULL"
	}

	return d.JSONType() + " DEFAULT " + d.StringDefault(defaultValue) + " NOT NULL"
}

func (mssqlDialect) Int64Type() string { return "BIGINT" }

func (d mssqlDialect) JSONType() string { return d.TextType() }

// SQL Server has no rowid: a hidden identity column keeps the insertion order.
func (mssqlDialect) HiddenColumns() map[string]string {
	return map[string]string{RowIDColumn: "BIGINT IDENTITY(1,1) NOT NULL"}
}

func (mssqlDialect) RowOrder() string { return "[" + RowIDColumn + "] ASC" }

func (mssqlDialect) TextCastType() string { return "NVARCHAR(MAX)" }

// ---------------------------------------------------------------------
// Filters, sorting and JSON
// ---------------------------------------------------------------------

// SQL Server has no boolean literal: 1 and 0 compare with a BIT column.
func (mssqlDialect) BoolLiteral(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// IsOp is only the fallback: the filter builder asks NullSafeEqual.
func (mssqlDialect) IsOp(not bool) string {
	if not {
		return "<>"
	}
	return "="
}

// NullSafeEqual renders the null safe (in)equality of two expressions as a
// definite 1 or 0 (a negated three valued comparison would be unknown).
func (mssqlDialect) NullSafeEqual(left, right string, not bool) string {
	cmp := "(CASE WHEN " + left + " = " + right + " OR (" + left + " IS NULL AND " + right + " IS NULL) THEN 1 ELSE 0 END)"
	if not {
		return cmp + " = 0"
	}
	return cmp + " = 1"
}

// AsText converts to text: SQL Server compares ” with a number as numbers (0 = ”).
func (mssqlDialect) AsText(expr string) string { return "CAST(" + expr + " AS NVARCHAR(MAX))" }

// The text columns are binary: the left operand gets a case insensitive
// collation, which the comparison then uses ("a COLLATE x LIKE b").
func (mssqlDialect) LikeOp(not bool) string {
	if not {
		return "COLLATE " + mssqlCICollation + " NOT LIKE"
	}
	return "COLLATE " + mssqlCICollation + " LIKE"
}

// Concat concatenates SQL text expressions (a NULL operand gives NULL, like ||).
func (mssqlDialect) Concat(parts ...string) string { return strings.Join(parts, " + ") }

// SQL Server sorts NULLs first for ASC and last for DESC, like SQLite.
func (mssqlDialect) OrderTerm(expr, direction string) string { return expr + " " + direction }

// JSONOrderTerm orders the sql_variant values of JSONExtract like SQLite
// orders the extracted values: NULL, numbers, then text.
func (mssqlDialect) JSONOrderTerm(expr, direction string) string {
	return "(CASE WHEN " + expr + " IS NULL THEN 0 WHEN SQL_VARIANT_PROPERTY(" + expr + ", 'BaseType') = 'float' THEN 1 ELSE 2 END) " +
		direction + ", " + expr + " " + direction
}

func (mssqlDialect) NoCaseEqual(column, value string) string {
	return "LOWER(" + column + ") = LOWER(" + value + ")"
}

func (mssqlDialect) StrictTypes() bool { return false }

func (mssqlDialect) ScalarCast(kind ScalarKind, expr string) string { return expr }

// JSONExtract returns a typed value (sql_variant): the search layer converts
// the other operand of a comparison to it, or the value to text.
func (mssqlDialect) NativeJSON() bool { return true }

// wrapped single value: the JSON text of a single value column that is not an array.
func mssqlSingleAsArray(c string) string {
	return "'[\"' + STRING_ESCAPE(" + c + ", 'json') + '\"]'"
}

// mssqlIsArray is true for a column that holds a JSON array.
func mssqlIsArray(c string) string {
	return "(ISJSON(" + c + ") = 1 AND LEFT(LTRIM(" + c + "), 1) = '[')"
}

func (mssqlDialect) JSONEach(column string, multiple bool) string {
	c := "[[" + column + "]]"

	if !multiple {
		return "(SELECT " + c + " AS [value])"
	}

	return "OPENJSON(CASE WHEN " + mssqlIsArray(c) + " THEN " + c + " WHEN " + c + " IS NULL OR " + c + " = '' THEN '[]' ELSE " + mssqlSingleAsArray(c) + " END)"
}

func (mssqlDialect) JSONEachParam(placeholder string) string {
	return "OPENJSON({:" + placeholder + "})"
}

func (mssqlDialect) JSONArrayLength(column string, multiple bool) string {
	c := "[[" + column + "]]"

	if !multiple {
		return "(CASE WHEN " + c + " = '' OR " + c + " IS NULL THEN 0 ELSE 1 END)"
	}

	return "(CASE WHEN " + mssqlIsArray(c) + " THEN (SELECT COUNT(*) FROM OPENJSON(" + c + ")) WHEN " + c + " = '' OR " + c + " IS NULL THEN 0 ELSE 1 END)"
}

// mssqlVariantOfText is the sql_variant of a text value (cut to what a variant holds).
func mssqlVariantOfText(v string) string {
	return "CAST(CAST(LEFT(" + v + ", 4000) AS NVARCHAR(4000)) AS sql_variant)"
}

var mssqlPathLeaf = regexp.MustCompile(`^(.*?)(?:\.([^.\[\]]+)|\[(\d+)\])$`)

// mssqlTyped is the typed value (a sql_variant) at path of the JSON document
// doc, like SQLite's json_extract returns it: a number is a float, true and
// false are 1 and 0, a JSON null is NULL, a string is text and an object or
// an array is its JSON text. The document must be valid JSON.
//
// OPENJSON is used because it reports the type of the value: JSON_VALUE can't
// tell the number 5 from the string "5".
func mssqlTyped(doc, path string) string {
	if path == "" {
		return mssqlVariantOfText(doc)
	}

	m := mssqlPathLeaf.FindStringSubmatch(path)
	if m == nil {
		return "NULL"
	}

	parent, leaf := m[1], m[2]
	if leaf == "" {
		leaf = m[3]
	}

	return "(SELECT TOP 1 (CASE j.[type] WHEN 0 THEN NULL WHEN 2 THEN CAST(CAST(j.[value] AS FLOAT) AS sql_variant) " +
		"WHEN 3 THEN CAST(CASE WHEN j.[value] = 'true' THEN 1 ELSE 0 END AS sql_variant) ELSE " + mssqlVariantOfText("j.[value]") + " END) " +
		"FROM OPENJSON(" + doc + ", '$" + strings.ReplaceAll(parent, "'", "''") + "') AS j WHERE j.[key] = N'" + strings.ReplaceAll(leaf, "'", "''") + "')"
}

// JSONExtract extracts the value at path of a JSON column, or, like SQLite's
// helper, of a text column that does not hold JSON (then the path applies to
// the text wrapped in an object).
func (mssqlDialect) JSONExtract(column, path string) string {
	c := "[[" + column + "]]"

	if path != "" && !strings.HasPrefix(path, "[") {
		path = "." + path
	}

	// the text of a non JSON column as a JSON value: a number, true, false,
	// null and a quoted string stay what they are, anything else is a string
	wrapped := "'{\"pb\":' + (CASE WHEN LEFT(LTRIM(" + c + "), 1) = '\"' OR " + c + " IN ('true', 'false', 'null') OR " +
		"(" + c + " <> '' AND " + c + " NOT LIKE '%[^0-9eE.+-]%' AND TRY_CAST(" + c + " AS float) IS NOT NULL) THEN " + c +
		" ELSE '\"' + STRING_ESCAPE(" + c + ", 'json') + '\"' END) + '}'"

	return "(CASE WHEN " + c + " IS NULL THEN NULL WHEN ISJSON(" + c + ") = 1 THEN " + mssqlTyped(c, path) +
		" ELSE " + mssqlTyped(wrapped, ".pb"+path) + " END)"
}

func (mssqlDialect) JSONText(expr string) string { return "CAST(" + expr + " AS NVARCHAR(MAX))" }

func (mssqlDialect) JSONFromScalar(kind ScalarKind, expr string) string {
	return "CAST(" + expr + " AS sql_variant)"
}

// JSONNumber is the number of a JSON number, NULL for any other value.
func (mssqlDialect) JSONNumber(expr string) string {
	return "(CASE WHEN SQL_VARIANT_PROPERTY(" + expr + ", 'BaseType') = 'float' THEN CAST(" + expr + " AS FLOAT) END)"
}

func (mssqlDialect) ColumnCast(expr, columnDef string) string { return expr }

// JoinOn is the condition of a plain join that has none (a lateral one is an APPLY, see ApplyJoin).
func (mssqlDialect) JoinOn() string { return "1 = 1" }

// ApplyJoin tells that a join without a condition must be written
// "OUTER APPLY table alias": SQL Server cannot LEFT JOIN a table valued
// function or a derived table over a column of an earlier table.
func (mssqlDialect) ApplyJoin() string { return "OUTER APPLY" }

// SQL Server requires an alias for every derived table.
func (mssqlDialect) SubqueryAlias() string { return " AS pb_sub" }

func (mssqlDialect) ColumnKind(sqlType string) ColumnKind {
	t := strings.ToLower(strings.TrimSpace(sqlType))
	if i := strings.IndexByte(t, '('); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}

	switch t {
	case "bit":
		return ColumnBool
	case "varchar", "char", "text", "nvarchar", "nchar", "ntext", "uniqueidentifier":
		return ColumnText
	case "float", "real", "decimal", "numeric", "money", "smallmoney":
		return ColumnFloat
	case "bigint", "int", "smallint", "tinyint":
		return ColumnInt
	}

	return ColumnUnknown
}

// RandomOrder is the ORDER BY term of the @random sort.
func (mssqlDialect) RandomOrder() string { return "NEWID()" }

// ---------------------------------------------------------------------
// strftime and geoDistance
// ---------------------------------------------------------------------

var _ FunctionBuilder = mssqlDialect{}

func (mssqlDialect) GeoDistance(lonA, latA, lonB, latB string) string {
	x := `(COS(RADIANS(` + latA + `)) * COS(RADIANS(` + latB + `)) * ` +
		`COS(RADIANS(` + lonB + `) - RADIANS(` + lonA + `)) + ` +
		`SIN(RADIANS(` + latA + `)) * SIN(RADIANS(` + latB + `)))`

	return `(6371 * ACOS(CASE WHEN ` + x + ` > 1 THEN 1 WHEN ` + x + ` < -1 THEN -1 ELSE ` + x + ` END))`
}

var mssqlWeekdayModifier = regexp.MustCompile(`^weekday\s+([0-6])$`)

// mssqlWeekday is the weekday of ts with 0 = Sunday, whatever DATEFIRST is.
func mssqlWeekday(ts string) string {
	return "((DATEPART(weekday, " + ts + ") + @@DATEFIRST - 1) % 7)"
}

var mssqlIntervalModifier = regexp.MustCompile(`(?i)^([+-]?\d+)\s*(second|minute|hour|day|month|year)s?$`)

func mssqlPad(expr string, n int) string {
	return "RIGHT('" + strings.Repeat("0", n) + "' + CAST(" + expr + " AS NVARCHAR(10)), " + strconv.Itoa(n) + ")"
}

// StrFTime translates the strftime() filter function for the formats
// %Y %m %d %e %H %I %k %l %M %S %f %j %p %w %s %F %T %R %%, the modifiers start
// of day/month/year, [+-]N second|minute|hour|day|month|year, utc and subsec.
// Anything else is an error when the filter is built.
func (mssqlDialect) StrFTime(format string, timeValue string, timeValueIsNumber bool, modifiers []string) (string, error) {
	var ts string
	switch {
	case timeValue == "":
		ts = "SYSUTCDATETIME()"
	case timeValueIsNumber:
		// a unix time with the "unixepoch" modifier, a julian day number without
		unix := false
		for _, m := range modifiers {
			if strings.EqualFold(strings.TrimSpace(m), "unixepoch") {
				unix = true
			}
		}
		// milliseconds since the unix epoch
		ms := "CAST(ROUND((CAST(" + timeValue + " AS FLOAT) - 2440587.5) * 86400000, 0) AS BIGINT)"
		if unix {
			ms = "CAST(ROUND(CAST(" + timeValue + " AS FLOAT) * 1000, 0) AS BIGINT)"
		}
		ts = "DATEADD(millisecond, " + ms + " % 86400000, DATEADD(day, " + ms + " / 86400000, CAST('19700101' AS DATETIME2(7))))"
	default:
		t := "CAST(" + timeValue + " AS NVARCHAR(40))"
		ts = "(CASE WHEN LOWER(" + t + ") = 'now' THEN SYSUTCDATETIME() ELSE " +
			"TRY_CONVERT(DATETIME2(7), NULLIF(REPLACE(REPLACE(REPLACE(" + t + ", 'T', ' '), 'Z', ''), 'z', ''), ''), 121) END)"
	}

	for _, m := range modifiers {
		lm := strings.ToLower(strings.TrimSpace(m))

		switch lm {
		case "utc", "auto", "subsec", "subsecond", "unixepoch":
			continue
		case "start of day":
			ts = "CAST(CAST(" + ts + " AS DATE) AS DATETIME2(7))"
			continue
		case "start of month":
			ts = "CAST(DATEFROMPARTS(YEAR(" + ts + "), MONTH(" + ts + "), 1) AS DATETIME2(7))"
			continue
		case "start of year":
			ts = "CAST(DATEFROMPARTS(YEAR(" + ts + "), 1, 1) AS DATETIME2(7))"
			continue
		}

		if match := mssqlWeekdayModifier.FindStringSubmatch(lm); match != nil {
			// advance to the next date that is the given weekday (0 = Sunday)
			ts = "DATEADD(day, ((" + match[1] + " - " + mssqlWeekday(ts) + " + 7) % 7), " + ts + ")"
			continue
		}

		if match := mssqlIntervalModifier.FindStringSubmatch(strings.TrimSpace(m)); match != nil {
			n, _ := strconv.Atoi(match[1])
			ts = "DATEADD(" + strings.ToLower(match[2]) + ", " + strconv.Itoa(n) + ", " + ts + ")"
			continue
		}

		return "", fmt.Errorf("the strftime modifier %q is not supported on SQL Server", m)
	}

	var parts []string
	var lit strings.Builder

	flush := func() {
		if lit.Len() > 0 {
			parts = append(parts, "N'"+strings.ReplaceAll(lit.String(), "'", "''")+"'")
			lit.Reset()
		}
	}

	add := func(sql string) {
		flush()
		parts = append(parts, sql)
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

		switch spec := format[i]; spec {
		case '%':
			lit.WriteByte('%')
		case 'Y':
			add(mssqlPad("YEAR(@@TS@@)", 4))
		case 'm':
			add(mssqlPad("MONTH(@@TS@@)", 2))
		case 'd':
			add(mssqlPad("DAY(@@TS@@)", 2))
		case 'e':
			add("RIGHT(' ' + CAST(DAY(@@TS@@) AS NVARCHAR(10)), 2)")
		case 'H':
			add(mssqlPad("DATEPART(hour, @@TS@@)", 2))
		case 'k':
			add("RIGHT(' ' + CAST(DATEPART(hour, @@TS@@) AS NVARCHAR(10)), 2)")
		case 'I':
			add(mssqlPad("((DATEPART(hour, @@TS@@) + 11) % 12 + 1)", 2))
		case 'l':
			add("RIGHT(' ' + CAST(((DATEPART(hour, @@TS@@) + 11) % 12 + 1) AS NVARCHAR(10)), 2)")
		case 'M':
			add(mssqlPad("DATEPART(minute, @@TS@@)", 2))
		case 'S':
			add(mssqlPad("DATEPART(second, @@TS@@)", 2))
		case 'f':
			add(mssqlPad("DATEPART(second, @@TS@@)", 2) + " + '.' + " + mssqlPad("DATEPART(millisecond, @@TS@@)", 3))
		case 'j':
			add(mssqlPad("DATEPART(dayofyear, @@TS@@)", 3))
		case 'p':
			add("(CASE WHEN DATEPART(hour, @@TS@@) < 12 THEN N'AM' ELSE N'PM' END)")
		case 'w':
			// SQL Server's weekday depends on DATEFIRST: normalize to 0 = Sunday
			add("CAST(" + mssqlWeekday("@@TS@@") + " AS NVARCHAR(1))")
		case 'u':
			add("CAST(((" + mssqlWeekday("@@TS@@") + " + 6) % 7 + 1) AS NVARCHAR(1))")
		case 'V':
			add(mssqlPad("DATEPART(iso_week, @@TS@@)", 2))
		case 'W':
			add(mssqlPad("((DATEPART(dayofyear, @@TS@@) - 1 + 7 - (("+mssqlWeekday("@@TS@@")+" + 6) % 7)) / 7)", 2))
		case 'U':
			add(mssqlPad("((DATEPART(dayofyear, @@TS@@) - 1 + 7 - "+mssqlWeekday("@@TS@@")+") / 7)", 2))
		case 's':
			add("CAST(DATEDIFF_BIG(second, '1970-01-01', @@TS@@) AS NVARCHAR(20))")
		case 'F':
			add(mssqlPad("YEAR(@@TS@@)", 4) + " + '-' + " + mssqlPad("MONTH(@@TS@@)", 2) + " + '-' + " + mssqlPad("DAY(@@TS@@)", 2))
		case 'T':
			add(mssqlPad("DATEPART(hour, @@TS@@)", 2) + " + ':' + " + mssqlPad("DATEPART(minute, @@TS@@)", 2) + " + ':' + " + mssqlPad("DATEPART(second, @@TS@@)", 2))
		case 'R':
			add(mssqlPad("DATEPART(hour, @@TS@@)", 2) + " + ':' + " + mssqlPad("DATEPART(minute, @@TS@@)", 2))
		default:
			return "", fmt.Errorf("the strftime format specifier %%%c is not supported on SQL Server", spec)
		}
	}

	flush()

	var out string
	switch len(parts) {
	case 0:
		out = "(CASE WHEN @@TS@@ IS NULL THEN NULL ELSE N'' END)"
	case 1:
		out = "(" + parts[0] + ")"
	default:
		out = "(" + strings.Join(parts, " + ") + ")"
	}

	// the NULL of an unparsable time value stays NULL
	return strings.ReplaceAll("(CASE WHEN @@TS@@ IS NULL THEN NULL ELSE "+out+" END)", "@@TS@@", ts), nil
}

// ---------------------------------------------------------------------
// DDL
// ---------------------------------------------------------------------

// BuildIndex renders the portable index without knowing the column types.
// See [mssqlDialect.BuildIndexOn] for the version that the app uses.
func (d mssqlDialect) BuildIndex(idx dbutils.Index) string {
	sql, _ := d.BuildIndexOn(nil, idx)
	return sql
}

const mssqlMaxKeyChars = 450

var (
	mssqlIdentRe       = regexp.MustCompile("^[`\\[\"]?([A-Za-z0-9_$]+)[`\\]\"]?$")
	mssqlBacktickIdent = regexp.MustCompile("`([^`]+)`")
)

// the property that holds the portable definition of an index
const mssqlIndexProperty = "claviger_index"

type mssqlColumnType struct {
	dataType string
	maxLen   int
	nullable bool
}

func (c mssqlColumnType) isLongText() bool {
	switch c.dataType {
	case "nvarchar", "varchar", "nchar", "char":
		return c.maxLen < 0 || c.maxLen > mssqlMaxKeyChars
	case "ntext", "text":
		return true
	}
	return false
}

func mssqlColumnTypes(db dbx.Builder, table string) map[string]mssqlColumnType {
	types := map[string]mssqlColumnType{}
	if db == nil {
		return types
	}

	rows := []struct {
		Name     string `db:"name"`
		Type     string `db:"type"`
		MaxLen   *int   `db:"maxlen"`
		Nullable string `db:"nullable"`
	}{}
	_ = db.NewQuery(`SELECT COLUMN_NAME AS name, DATA_TYPE AS type, CHARACTER_MAXIMUM_LENGTH AS maxlen, IS_NULLABLE AS nullable
		FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = SCHEMA_NAME() AND TABLE_NAME = {:table}`).
		Bind(dbx.Params{"table": table}).All(&rows)

	for _, r := range rows {
		c := mssqlColumnType{dataType: strings.ToLower(r.Type), nullable: strings.EqualFold(r.Nullable, "YES")}
		if r.MaxLen != nil {
			c.maxLen = *r.MaxLen
		}
		types[strings.ToLower(r.Name)] = c
	}

	return types
}

func (d mssqlDialect) normalizeWhere(where string, types map[string]mssqlColumnType) string {
	where = mssqlBacktickIdent.ReplaceAllString(where, "[$1]")
	// a JSON column is compared with text: nothing to do (JSON is text here)
	return where
}

func andWhere(where string) string {
	if where == "" {
		return ""
	}
	return " AND (" + where + ")"
}

// BuildIndexOn is the SQL Server batch that creates the portable index, see
// the file comment. db may be nil, then every column is taken for a short one.
func (d mssqlDialect) BuildIndexOn(db dbx.Builder, idx dbutils.Index) (string, error) {
	if !idx.IsValid() {
		return "", nil
	}

	types := mssqlColumnTypes(db, idx.TableName)
	where := d.normalizeWhere(strings.TrimSpace(idx.Where), types)
	table := d.QuoteIdent(idx.TableName)

	var (
		statements []string
		keys       []string
		filters    []string
	)

	// the column that tells the rows apart (the hidden identity of the PB tables)
	rowKey := d.QuoteIdent(RowIDColumn)
	if _, ok := types[RowIDColumn]; !ok && len(types) > 0 {
		rowKey = d.QuoteIdent("id")
	}

	for i, col := range idx.Columns {
		name := strings.TrimSpace(col.Name)
		if name == "" {
			continue
		}

		var plain string // the bare column when it is one
		if m := mssqlIdentRe.FindStringSubmatch(name); m != nil {
			plain = m[1]
		}

		expr := strings.ReplaceAll(mssqlBacktickIdent.ReplaceAllString(name, "[$1]"), "||", "+")
		if plain != "" {
			expr = d.QuoteIdent(plain)
		}

		ct, known := types[strings.ToLower(plain)]
		nocase := strings.EqualFold(strings.TrimSpace(col.Collate), "NOCASE")

		// the key is a plain column when nothing has to be computed
		partial := idx.Unique && where != ""
		needsHash := (plain == "" || (known && ct.isLongText())) && idx.Unique
		needsPrefix := (plain == "" || (known && ct.isLongText())) && !idx.Unique

		if plain != "" && !nocase && !partial && !needsHash && !needsPrefix {
			key := expr
			if col.Sort != "" {
				key += " " + strings.ToUpper(col.Sort)
			}
			keys = append(keys, key)
			if idx.Unique && known && ct.nullable {
				filters = append(filters, expr+" IS NOT NULL")
			}
			continue
		}

		value := expr
		if nocase {
			value = "LOWER(" + value + ")"
		}

		switch {
		case idx.Unique:
			// a unique key is a hash: a filtered index cannot test a
			// computed column, so the rows that SQLite would skip (a NULL
			// key, a partial index whose condition is false) get a key of
			// their own, made of their row number
			value = "CASE WHEN " + value + " IS NOT NULL" + andWhere(where) + " THEN CONVERT(BINARY(32), HASHBYTES('SHA2_256', CAST(" + value + " AS NVARCHAR(MAX)))) " +
				"ELSE CONVERT(BINARY(32), HASHBYTES('SHA2_256', NCHAR(1) + CAST(" + rowKey + " AS NVARCHAR(40)) + N'/" + strconv.Itoa(i) + "')) END"
		case needsPrefix:
			value = "CAST(LEFT(CAST(" + value + " AS NVARCHAR(MAX)), " + strconv.Itoa(mssqlMaxKeyChars) + ") AS NVARCHAR(" + strconv.Itoa(mssqlMaxKeyChars) + "))"
		}

		hidden := mssqlHiddenPrefix + idx.IndexName + "_" + strconv.Itoa(i)

		statements = append(statements, "ALTER TABLE "+table+" ADD "+d.QuoteIdent(hidden)+" AS ("+value+") PERSISTED")

		key := d.QuoteIdent(hidden)
		if col.Sort != "" {
			key += " " + strings.ToUpper(col.Sort)
		}
		keys = append(keys, key)
	}

	var create strings.Builder
	create.WriteString("CREATE ")
	if idx.Unique {
		create.WriteString("UNIQUE ")
	}
	create.WriteString("INDEX " + d.QuoteIdent(idx.IndexName) + " ON " + table + " (" + strings.Join(keys, ", ") + ")")
	if len(filters) > 0 {
		create.WriteString(" WHERE " + strings.Join(filters, " AND "))
	}

	// a statement that uses a column that an earlier statement of the batch
	// added cannot be compiled with it: run it dynamically
	statements = append(statements, "EXEC(N'"+strings.ReplaceAll(create.String(), "'", "''")+"')")

	// the portable definition travels in an extended property, so that
	// TableIndexes can return exactly what the collection stored (the keys
	// above are rewritten and cannot be turned back)
	if portable := idx.Build(); portable != "" && len(portable) <= 3500 {
		statements = append(statements, "DECLARE @pb_schema NVARCHAR(128) = SCHEMA_NAME()", "EXEC sp_addextendedproperty @name = N'"+mssqlIndexProperty+"', @value = N'"+
			strings.ReplaceAll(portable, "'", "''")+"', @level0type = N'SCHEMA', @level0name = @pb_schema, "+
			"@level1type = N'TABLE', @level1name = N'"+strings.ReplaceAll(idx.TableName, "'", "''")+"', "+
			"@level2type = N'INDEX', @level2name = N'"+strings.ReplaceAll(idx.IndexName, "'", "''")+"'")
	}

	return strings.Join(statements, ";\n"), nil
}

// SQL Server drops a view that lost its table? No: it keeps it, like SQLite.
func (mssqlDialect) DropSuffix() string { return "" }

func (mssqlDialect) DetachDependentViews(db dbx.Builder, view string) (func() error, error) {
	return func() error { return nil }, nil
}

// ConvertMultiplicity rebuilds the column (ALTER COLUMN cannot parse the data).
func (d mssqlDialect) ConvertMultiplicity(db dbx.Builder, table, column string, toMultiple bool) (bool, error) {
	t, c, tmpName := d.QuoteIdent(table), d.QuoteIdent(column), "_pb_"+column+"_conv"
	tmp := d.QuoteIdent(tmpName)

	var add, update string
	if toMultiple {
		add = "ALTER TABLE " + t + " ADD " + tmp + " " + d.JSONColumn("[]")
		update = "UPDATE " + t + " SET " + tmp + " = (CASE WHEN COALESCE(" + c + ", '') = '' THEN '[]' " +
			"WHEN " + mssqlIsArray(c) + " THEN " + c + " ELSE " + mssqlSingleAsArray(c) + " END)"
	} else {
		add = "ALTER TABLE " + t + " ADD " + tmp + " " + d.TextColumn()
		update = "UPDATE " + t + " SET " + tmp + " = (CASE WHEN " + c + " IS NULL OR " + c + " = '' THEN '' " +
			"WHEN " + mssqlIsArray(c) + " THEN COALESCE((SELECT TOP 1 j.[value] FROM OPENJSON(" + c + ") AS j ORDER BY CAST(j.[key] AS INT) DESC), '') " +
			"ELSE " + c + " END)"
	}

	for _, q := range []string{
		add,
		"EXEC(N'" + strings.ReplaceAll(update, "'", "''") + "')",
	} {
		if _, err := db.NewQuery(q).Execute(); err != nil {
			return true, err
		}
	}

	if _, err := db.NewQuery(mssqlDropColumnSQL(d, t, c, column)).Execute(); err != nil {
		return true, err
	}

	if _, err := db.NewQuery("EXEC sp_rename N'" + strings.ReplaceAll(t+"."+d.QuoteIdent(tmpName), "'", "''") +
		"', N'" + strings.ReplaceAll(column, "'", "''") + "', N'COLUMN'").Execute(); err != nil {
		return true, err
	}

	return true, nil
}

// mssqlDropColumnSQL drops a column and the default constraint that it has.
func mssqlDropColumnSQL(d mssqlDialect, quotedTable, quotedColumn, rawColumn string) string {
	return "DECLARE @pb_dc NVARCHAR(256) = (SELECT TOP 1 dc.name FROM sys.default_constraints dc " +
		"JOIN sys.columns c ON c.default_object_id = dc.object_id " +
		"WHERE dc.parent_object_id = OBJECT_ID(N'" + strings.ReplaceAll(quotedTable, "'", "''") + "') AND c.name = N'" + strings.ReplaceAll(rawColumn, "'", "''") + "');\n" +
		"IF @pb_dc IS NOT NULL EXEC(N'ALTER TABLE " + strings.ReplaceAll(quotedTable, "'", "''") + " DROP CONSTRAINT ' + @pb_dc);\n" +
		"ALTER TABLE " + quotedTable + " DROP COLUMN " + quotedColumn
}

// ---------------------------------------------------------------------
// Introspection
// ---------------------------------------------------------------------

func (mssqlDialect) HasTable(db dbx.Builder, name string) bool {
	var exists int

	err := db.NewQuery(`
		SELECT TOP 1 1 FROM INFORMATION_SCHEMA.TABLES
		WHERE TABLE_SCHEMA = SCHEMA_NAME() AND LOWER(TABLE_NAME) = LOWER({:name})
	`).Bind(dbx.Params{"name": name}).Row(&exists)

	return err == nil && exists > 0
}

const mssqlColumnsQuery = `
	SELECT
		CAST(c.ORDINAL_POSITION - 1 AS INT) AS cid,
		c.COLUMN_NAME AS name,
		c.DATA_TYPE + CASE WHEN c.CHARACTER_MAXIMUM_LENGTH IS NULL THEN '' WHEN c.CHARACTER_MAXIMUM_LENGTH = -1 THEN '(max)'
			ELSE '(' + CAST(c.CHARACTER_MAXIMUM_LENGTH AS NVARCHAR(10)) + ')' END AS type,
		CAST(CASE WHEN c.IS_NULLABLE = 'NO' THEN 1 ELSE 0 END AS INT) AS [notnull],
		c.COLUMN_DEFAULT AS dflt_value,
		CAST(CASE WHEN EXISTS (
			SELECT 1 FROM INFORMATION_SCHEMA.TABLE_CONSTRAINTS tc
			JOIN INFORMATION_SCHEMA.KEY_COLUMN_USAGE kcu ON kcu.CONSTRAINT_NAME = tc.CONSTRAINT_NAME AND kcu.TABLE_SCHEMA = tc.TABLE_SCHEMA
			WHERE tc.CONSTRAINT_TYPE = 'PRIMARY KEY' AND tc.TABLE_SCHEMA = c.TABLE_SCHEMA AND tc.TABLE_NAME = c.TABLE_NAME AND kcu.COLUMN_NAME = c.COLUMN_NAME
		) THEN 1 ELSE 0 END AS INT) AS pk
	FROM INFORMATION_SCHEMA.COLUMNS c
	WHERE c.TABLE_SCHEMA = SCHEMA_NAME() AND c.TABLE_NAME = {:name} AND c.COLUMN_NAME <> '` + RowIDColumn + `'
		AND c.COLUMN_NAME NOT LIKE '\_pbx\_%' ESCAPE '\'
	ORDER BY c.ORDINAL_POSITION
`

// mssqlStripDefault removes the parentheses that SQL Server wraps around a
// default and the N prefix of a string literal.
func mssqlStripDefault(s string) string {
	s = strings.TrimSpace(s)
	for len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return strings.TrimPrefix(s, "N")
}

func mssqlTableInfo(db dbx.Builder, table string) ([]*TableInfoRow, error) {
	info := []*TableInfoRow{}
	err := db.NewQuery(mssqlColumnsQuery).Bind(dbx.Params{"name": table}).All(&info)
	for _, row := range info {
		if row.DefaultValue.Valid {
			row.DefaultValue.String = mssqlStripDefault(row.DefaultValue.String)
		}
	}
	return info, err
}

func (mssqlDialect) TableColumns(db dbx.Builder, table string) ([]string, error) {
	info, err := mssqlTableInfo(db, table)
	if err != nil {
		return nil, err
	}

	columns := make([]string, 0, len(info))
	for _, row := range info {
		columns = append(columns, row.Name)
	}

	return columns, nil
}

func (mssqlDialect) TableInfo(db dbx.Builder, table string) ([]*TableInfoRow, error) {
	info, err := mssqlTableInfo(db, table)
	if err != nil {
		return nil, err
	}

	if len(info) == 0 {
		return nil, fmt.Errorf("empty table info probably due to invalid or missing table %s", table)
	}

	return info, nil
}

// TableIndexes returns the indexes of the table in the portable form.
func (d mssqlDialect) TableIndexes(db dbx.Builder, table string) (map[string]string, error) {
	rows := []struct {
		Name     string  `db:"name"`
		Unique   int     `db:"is_unique"`
		Seq      int     `db:"seq"`
		Column   string  `db:"col"`
		Desc     int     `db:"is_desc"`
		Filter   *string `db:"filter"`
		Portable *string `db:"portable"`
	}{}

	err := db.NewQuery(`
		SELECT i.name AS name, CAST(i.is_unique AS INT) AS is_unique, CAST(ic.key_ordinal AS INT) AS seq,
			c.name AS col, CAST(ic.is_descending_key AS INT) AS is_desc, i.filter_definition AS filter,
			CAST(ep.value AS NVARCHAR(MAX)) AS portable
		FROM sys.indexes i
		JOIN sys.tables t ON t.object_id = i.object_id
		JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id AND ic.is_included_column = 0
		JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
		LEFT JOIN sys.extended_properties ep ON ep.class = 7 AND ep.major_id = i.object_id AND ep.minor_id = i.index_id AND ep.name = N'` + mssqlIndexProperty + `'
		WHERE t.name = {:table} AND t.schema_id = SCHEMA_ID() AND i.is_primary_key = 0 AND i.is_unique_constraint = 0 AND i.type > 0
		ORDER BY i.name, ic.key_ordinal
	`).Bind(dbx.Params{"table": table}).All(&rows)
	if err != nil {
		return nil, err
	}

	portable := map[string]string{}
	byName := map[string]*dbutils.Index{}
	var order []string
	for _, r := range rows {
		if r.Column == RowIDColumn {
			continue
		}

		if r.Portable != nil && strings.HasPrefix(*r.Portable, "CREATE ") {
			portable[r.Name] = *r.Portable
		}

		idx, ok := byName[r.Name]
		if !ok {
			idx = &dbutils.Index{IndexName: r.Name, TableName: table, Unique: r.Unique == 1}
			byName[r.Name] = idx
			order = append(order, r.Name)
		}

		col := dbutils.IndexColumn{Name: r.Column}
		if r.Desc == 1 {
			col.Sort = "DESC"
		}
		idx.Columns = append(idx.Columns, col)
	}

	sort.Strings(order)

	result := make(map[string]string, len(order))
	for _, name := range order {
		if def, ok := portable[name]; ok {
			result[name] = def
			continue
		}
		result[name] = byName[name].Build()
	}

	return result, nil
}

func (mssqlDialect) IndexOwner(db dbx.Builder, indexName string, skipTables ...string) string {
	q := db.Select("t.name").
		From("sys.indexes i").
		InnerJoin("sys.tables t", dbx.NewExp("t.object_id = i.object_id")).
		AndWhere(dbx.NewExp("t.schema_id = SCHEMA_ID()")).
		AndWhere(dbx.NewExp("i.is_primary_key = 0 AND i.is_unique_constraint = 0"))

	for i, t := range skipTables {
		q.AndWhere(dbx.NewExp(
			fmt.Sprintf("LOWER(t.name) <> LOWER({:skip%d})", i),
			dbx.Params{fmt.Sprintf("skip%d", i): t},
		))
	}

	var owner string
	_ = q.AndWhere(dbx.NewExp("LOWER(i.name) = LOWER({:indexName})", dbx.Params{"indexName": indexName})).
		Limit(1).
		Row(&owner)

	return owner
}

// ---------------------------------------------------------------------
// Maintenance and transactions
// ---------------------------------------------------------------------

func (mssqlDialect) Vacuum(db dbx.Builder) error     { return nil }
func (mssqlDialect) Optimize(db dbx.Builder) error   { return nil }
func (mssqlDialect) Checkpoint(db dbx.Builder) error { return nil }

// LockMigrations takes an application lock that the migrations transaction
// owns (it is released when it ends).
func (mssqlDialect) LockMigrations(db dbx.Builder) error {
	var rc int
	err := db.NewQuery(`DECLARE @rc INT;
		EXEC @rc = sp_getapplock @Resource = N'claviger_migrations', @LockMode = N'Exclusive', @LockOwner = N'Transaction', @LockTimeout = 120000;
		SELECT @rc`).Row(&rc)
	if err != nil {
		return err
	}
	if rc < 0 {
		return fmt.Errorf("failed to take the migrations lock (sp_getapplock returned %d)", rc)
	}
	return nil
}

// Try runs fn and rolls a transaction back to a savepoint if it fails, so the
// caller can handle the error and continue (SQL Server dooms some failed
// batches, for example a DDL batch that adds a column and then fails).
func (mssqlDialect) Try(db dbx.Builder, fn func() error) error {
	if _, ok := db.(*dbx.Tx); !ok {
		return fn()
	}

	name := "claviger_try_" + strconv.FormatUint(savepointSeq.Add(1), 10)

	if _, err := db.NewQuery("SAVE TRANSACTION " + name).Execute(); err != nil {
		return err
	}

	if err := fn(); err != nil {
		if _, rbErr := db.NewQuery("ROLLBACK TRANSACTION " + name).Execute(); rbErr != nil {
			return fmt.Errorf("%w (and savepoint rollback failed: %v)", err, rbErr)
		}
		return err
	}

	return nil
}

func (mssqlDialect) Retries() bool { return true }

// IsRetryable is a deadlock victim (1205) or a snapshot update conflict (3960).
func (mssqlDialect) IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	n := MSSQLErrorNumber(err)
	return n == 1205 || n == 3960
}

var (
	mssqlDDLIfNotExistsTable = regexp.MustCompile(`(?is)\bCREATE\s+TABLE\s+IF\s+NOT\s+EXISTS\s+(\{\{[^}]+\}\}|\[\[[^\]]+\]\]|[\w\[\]]+)(\s*\()`)
	mssqlDDLIfNotExistsIndex = regexp.MustCompile(`(?i)\b(CREATE\s+(?:UNIQUE\s+)?INDEX)\s+IF\s+NOT\s+EXISTS\b`)
	mssqlDDLTextKey          = regexp.MustCompile(`(?i)\bTEXT(\s+(?:PRIMARY\s+KEY|UNIQUE)\b)`)
	mssqlDDLText             = regexp.MustCompile(`(?i)\bTEXT\b`)
	mssqlDDLVarcharKey       = regexp.MustCompile(`(?i)\bVARCHAR\((\d+)\)`)
	mssqlDDLBoolean          = regexp.MustCompile(`(?i)\bBOOLEAN\b`)
	mssqlDDLFalse            = regexp.MustCompile(`(?i)\bDEFAULT\s+FALSE\b`)
	mssqlDDLTrue             = regexp.MustCompile(`(?i)\bDEFAULT\s+TRUE\b`)
)

// AdaptDDL: a TEXT key becomes NVARCHAR(255), any other TEXT NVARCHAR(MAX), a
// BOOLEAN a BIT, and the "IF NOT EXISTS" clauses (not in SQL Server) are
// rewritten to an existence check.
func (d mssqlDialect) AdaptDDL(sql string) string {
	sql = mssqlDDLIfNotExistsIndex.ReplaceAllString(sql, "$1")
	sql = mssqlDDLIfNotExistsTable.ReplaceAllStringFunc(sql, func(m string) string {
		sub := mssqlDDLIfNotExistsTable.FindStringSubmatch(m)
		return "IF OBJECT_ID(N'" + strings.ReplaceAll(mssqlRawName(sub[1]), "'", "''") + "', N'U') IS NULL CREATE TABLE " + sub[1] + sub[2]
	})
	sql = mssqlDDLTextKey.ReplaceAllString(sql, "NVARCHAR(255)"+mssqlCollate+"$1")
	sql = mssqlDDLText.ReplaceAllString(sql, "NVARCHAR(MAX)"+mssqlCollate)
	sql = mssqlDDLVarcharKey.ReplaceAllString(sql, "NVARCHAR($1)"+mssqlCollate)
	sql = mssqlDDLBoolean.ReplaceAllString(sql, "BIT")
	sql = mssqlDDLFalse.ReplaceAllString(sql, "DEFAULT 0")
	sql = mssqlDDLTrue.ReplaceAllString(sql, "DEFAULT 1")

	return sql
}

func mssqlRawName(s string) string {
	return strings.Trim(s, "{}[]")
}

func (d mssqlDialect) DropIndexIfExists(db dbx.Builder, table, name string) error {
	var exists int
	err := db.NewQuery(`SELECT TOP 1 1 FROM sys.indexes i JOIN sys.tables t ON t.object_id = i.object_id
		WHERE t.schema_id = SCHEMA_ID() AND LOWER(t.name) = LOWER({:table}) AND LOWER(i.name) = LOWER({:name})`).
		Bind(dbx.Params{"table": table, "name": name}).Row(&exists)
	if err != nil || exists != 1 {
		return nil
	}

	if _, err = db.NewQuery("DROP INDEX " + d.QuoteIdent(name) + " ON " + d.QuoteIdent(table)).Execute(); err != nil {
		return err
	}

	// the computed key columns of the index
	var hidden []string
	_ = db.NewQuery(`SELECT c.name FROM sys.columns c JOIN sys.tables t ON t.object_id = c.object_id
		WHERE t.schema_id = SCHEMA_ID() AND LOWER(t.name) = LOWER({:table}) AND c.name LIKE {:prefix} ESCAPE '\'`).
		Bind(dbx.Params{
			"table":  table,
			"prefix": strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`, `[`, `\[`).Replace(mssqlHiddenPrefix+name+"_") + "%",
		}).Column(&hidden)

	hiddenRe := regexp.MustCompile("^" + regexp.QuoteMeta(mssqlHiddenPrefix+name) + `_\d+$`)
	for _, col := range hidden {
		if !hiddenRe.MatchString(col) {
			continue
		}
		if _, err := db.NewQuery("ALTER TABLE " + d.QuoteIdent(table) + " DROP COLUMN " + d.QuoteIdent(col)).Execute(); err != nil {
			return err
		}
	}

	return nil
}

func (mssqlDialect) FinishIndexDrops(db dbx.Builder) error { return nil }

// MSSQLAllColumns returns every column of the table, the hidden ones (row id,
// computed index keys) included: the columns that "SELECT table.*" selects.
func MSSQLAllColumns(db dbx.Builder, table string) []string {
	var cols []string
	_ = db.NewQuery(`SELECT c.name FROM sys.columns c JOIN sys.tables t ON t.object_id = c.object_id
		WHERE t.schema_id = SCHEMA_ID() AND t.name = {:table} ORDER BY c.column_id`).
		Bind(dbx.Params{"table": table}).Column(&cols)
	return cols
}
