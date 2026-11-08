package dialect

// The Postgres half of the search layer (filters, sorting, JSON): see the
// "Filters, sorting and JSON" section of the Dialect interface.

import (
	"fmt"
	"regexp"
	"strings"
)

// "true" and "false" identifiers are real booleans.
func (postgresDialect) BoolLiteral(v bool) string {
	if v {
		return "TRUE"
	}
	return "FALSE"
}

// "IS" with a non NULL right side is a SQLite extension; the
// standard spelling of a null safe comparison is IS [NOT] DISTINCT FROM.
func (postgresDialect) IsOp(not bool) string {
	if not {
		return "IS DISTINCT FROM"
	}
	return "IS NOT DISTINCT FROM"
}

// Postgres is strictly typed: numbers, booleans and JSON have no
// implicit text conversion in comparisons with text.
func (postgresDialect) AsText(expr string) string { return "CAST(" + expr + " AS TEXT)" }

// SQLite's LIKE is case insensitive for ASCII; Postgres' LIKE is case
// sensitive and ILIKE is the case insensitive one.
func (postgresDialect) LikeOp(not bool) string {
	if not {
		return "NOT ILIKE"
	}
	return "ILIKE"
}

// SQLite sorts NULLs first (ASC) and last (DESC); Postgres is the opposite.
func (postgresDialect) OrderTerm(expr, direction string) string {
	if strings.EqualFold(direction, "DESC") {
		return expr + " DESC NULLS LAST"
	}
	return expr + " " + direction + " NULLS FIRST"
}

// jsonb has its own ordering (strings before numbers, locale collation for
// strings). A row value of (kind, number, text) reproduces the SQLite order
// of json_extract: NULL < numbers (booleans as 0 and 1) < text.
func (d postgresDialect) JSONOrderTerm(expr, direction string) string {
	key := "ROW(" +
		"(CASE WHEN " + expr + " IS NULL THEN 0 WHEN jsonb_typeof(" + expr + ") IN ('number', 'boolean') THEN 1 ELSE 2 END), " +
		"(CASE jsonb_typeof(" + expr + ") WHEN 'number' THEN CAST((" + expr + " #>> '{}') AS DOUBLE PRECISION)" +
		" WHEN 'boolean' THEN (CASE WHEN " + expr + " = 'true'::jsonb THEN 1.0 ELSE 0.0 END) END), " +
		"((" + expr + " #>> '{}') COLLATE \"C\"))"

	return d.OrderTerm(key, direction)
}

func (postgresDialect) NoCaseEqual(column, value string) string {
	return "lower(" + column + ") = lower(" + value + ")"
}

func (postgresDialect) StrictTypes() bool { return true }

func (postgresDialect) ScalarCast(kind ScalarKind, expr string) string {
	switch kind {
	case ScalarNumber:
		return "CAST(" + expr + " AS DOUBLE PRECISION)"
	case ScalarBool:
		return "CAST(" + expr + " AS BOOLEAN)"
	default:
		return "CAST(" + expr + " AS TEXT)"
	}
}

func (postgresDialect) NativeJSON() bool { return true }

func (postgresDialect) JSONEach(column string, multiple bool) string {
	if !multiple {
		return "LATERAL (SELECT [[" + column + "]] AS value)"
	}

	return "LATERAL (SELECT e.v AS value FROM jsonb_array_elements_text(" +
		"CASE WHEN jsonb_typeof([[" + column + "]]) = 'array' THEN [[" + column + "]] ELSE '[]'::jsonb END" +
		") AS e(v))"
}

func (postgresDialect) JSONEachParam(placeholder string) string {
	return "LATERAL (SELECT e.v AS value FROM jsonb_array_elements_text(CAST({:" + placeholder + "} AS JSONB)) AS e(v))"
}

func (postgresDialect) JSONArrayLength(column string, multiple bool) string {
	if !multiple {
		return "(CASE WHEN [[" + column + "]] = '' OR [[" + column + "]] IS NULL THEN 0.0 ELSE 1.0 END)"
	}

	return "CAST(COALESCE(CASE WHEN jsonb_typeof([[" + column + "]]) = 'array' THEN jsonb_array_length([[" + column + "]]) END, 0) AS DOUBLE PRECISION)"
}

var jsonPathPart = regexp.MustCompile(`\[(\d+)\]|([^.\[\]]+)`)

// JSONExtract returns the jsonb value at path. A JSON null is normalized to
// SQL NULL, like SQLite's json_extract does.
func (postgresDialect) JSONExtract(column, path string) string {
	ref := "[[" + column + "]]"

	parts := jsonPathPart.FindAllStringSubmatch(path, -1)
	if len(parts) == 0 {
		return "NULLIF(" + ref + ", 'null'::jsonb)"
	}

	elems := make([]string, 0, len(parts))
	for _, p := range parts {
		elem := p[1]
		if elem == "" {
			elem = p[2]
		}
		elems = append(elems, `"`+strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(elem)+`"`)
	}

	return "NULLIF(" + ref + " #> '{" + strings.ReplaceAll(strings.Join(elems, ","), "'", "''") + "}', 'null'::jsonb)"
}

func (postgresDialect) JSONText(expr string) string { return "(" + expr + " #>> '{}')" }

func (postgresDialect) JSONFromScalar(kind ScalarKind, expr string) string {
	switch kind {
	case ScalarNumber:
		return "to_jsonb(CAST(" + expr + " AS DOUBLE PRECISION))"
	case ScalarBool:
		return "to_jsonb(CAST(" + expr + " AS BOOLEAN))"
	default:
		return "to_jsonb(CAST(" + expr + " AS TEXT))"
	}
}

func (postgresDialect) JSONNumber(expr string) string {
	return "(CASE WHEN jsonb_typeof(" + expr + ") = 'number' THEN CAST((" + expr + " #>> '{}') AS DOUBLE PRECISION) END)"
}

var pgColumnType = regexp.MustCompile(`(?i)^\s*(.+?)(?:\s+(?:COLLATE|DEFAULT|NOT|NULL|PRIMARY|GENERATED)\b.*)?$`)

// ColumnCast casts to the type at the start of the column definition
// ("DOUBLE PRECISION DEFAULT 0 NOT NULL" => DOUBLE PRECISION).
func (postgresDialect) ColumnCast(expr, columnDef string) string {
	m := pgColumnType.FindStringSubmatch(columnDef)
	if m == nil || strings.TrimSpace(m[1]) == "" {
		return expr
	}

	return "CAST(" + expr + " AS " + strings.TrimSpace(m[1]) + ")"
}

// Postgres requires an ON condition for every LEFT JOIN.
func (postgresDialect) JoinOn() string { return "TRUE" }

// Postgres requires an alias for every derived table.
func (postgresDialect) SubqueryAlias() string { return " AS pb_sub" }

func (postgresDialect) ColumnKind(sqlType string) ColumnKind {
	t := strings.ToLower(strings.TrimSpace(sqlType))
	if i := strings.IndexByte(t, '('); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}

	switch t {
	case "text", "character varying", "character", "varchar", "bpchar", "name", "citext":
		return ColumnText
	case "double precision", "real", "numeric", "decimal", "float8", "float4":
		return ColumnFloat
	case "bigint", "integer", "smallint", "int8", "int4", "int2", "int":
		return ColumnInt
	case "boolean", "bool":
		return ColumnBool
	case "jsonb", "json":
		return ColumnJSON
	}

	return ColumnUnknown
}

// ---------------------------------------------------------------------

var _ FunctionBuilder = postgresDialect{}

func (postgresDialect) GeoDistance(lonA, latA, lonB, latB string) string {
	// the clamping is to prevent floating point rounding errors for values like
	// "1.0002" (acos is only defined for [-1, 1] and Postgres raises an error
	// where SQLite returns NULL)
	return `(6371 * acos(LEAST(1, GREATEST(-1, ` +
		`cos(radians(` + latA + `)) * cos(radians(` + latB + `)) * ` +
		`cos(radians(` + lonB + `) - radians(` + lonA + `)) + ` +
		`sin(radians(` + latA + `)) * sin(radians(` + latB + `))` +
		`))))`
}

var (
	pgIntervalModifier = regexp.MustCompile(`(?i)^([+-]?\d+(?:\.\d+)?)\s*(second|minute|hour|day|month|year)s?$`)
	pgWeekdayModifier  = regexp.MustCompile(`(?i)^weekday\s+([0-6])$`)
	pgZonedTimestamp   = `:[0-9]{2}(\.[0-9]+)?\s*(Z|z|[+-][0-9]{2}(:?[0-9]{2})?)$`
)

// StrFTime translates the strftime() filter function. It supports the
// formats and modifiers that are listed in docs/datastores.md; everything
// else is an error at filter build time (SQLite returns NULL for them).
func (d postgresDialect) StrFTime(format string, timeValue string, timeValueIsNumber bool, modifiers []string) (string, error) {
	var unixEpoch bool
	rest := make([]string, 0, len(modifiers))
	for _, m := range modifiers {
		switch strings.ToLower(strings.TrimSpace(m)) {
		case "unixepoch":
			unixEpoch = true
		case "julianday", "auto", "utc", "subsec", "subsecond":
			// the timestamps are UTC already; "subsec" only affects 'now' precision
		default:
			rest = append(rest, strings.TrimSpace(m))
		}
	}

	var ts string
	switch {
	case timeValue == "":
		ts = "(NOW() AT TIME ZONE 'UTC')"
	case timeValueIsNumber && unixEpoch:
		ts = "(to_timestamp(CAST(" + timeValue + " AS DOUBLE PRECISION)) AT TIME ZONE 'UTC')"
	case timeValueIsNumber:
		// julian day number
		ts = "(to_timestamp((CAST(" + timeValue + " AS DOUBLE PRECISION) - 2440587.5) * 86400.0) AT TIME ZONE 'UTC')"
	default:
		t := "CAST(" + timeValue + " AS TEXT)"
		ts = "(CASE" +
			" WHEN lower(" + t + ") = 'now' THEN NOW() AT TIME ZONE 'UTC'" +
			" WHEN " + t + " ~ '" + pgZonedTimestamp + "' AND pg_input_is_valid(" + t + ", 'timestamptz')" +
			" THEN CAST(" + t + " AS TIMESTAMPTZ) AT TIME ZONE 'UTC'" +
			" WHEN pg_input_is_valid(" + t + ", 'timestamp') THEN CAST(" + t + " AS TIMESTAMP)" +
			" END)"
	}

	for _, m := range rest {
		lm := strings.ToLower(m)

		switch lm {
		case "start of day":
			ts = "date_trunc('day', " + ts + ")"
			continue
		case "start of month":
			ts = "date_trunc('month', " + ts + ")"
			continue
		case "start of year":
			ts = "date_trunc('year', " + ts + ")"
			continue
		}

		if match := pgIntervalModifier.FindStringSubmatch(m); match != nil {
			ts = "(" + ts + " + INTERVAL '" + match[1] + " " + strings.ToLower(match[2]) + "')"
			continue
		}

		if match := pgWeekdayModifier.FindStringSubmatch(m); match != nil {
			ts = "(" + ts + " + (CAST(((" + match[1] + " - CAST(EXTRACT(DOW FROM " + ts + ") AS INTEGER) + 7) % 7) AS INTEGER) * INTERVAL '1 day'))"
			continue
		}

		return "", fmt.Errorf("the strftime modifier %q is not supported on PostgreSQL", m)
	}

	return pgStrftimeFormat(format, ts)
}

// to_char patterns of the supported strftime format specifiers.
var pgStrftimePatterns = map[byte]string{
	'Y': "YYYY", 'm': "MM", 'd': "DD", 'H': "HH24", 'M': "MI", 'S': "SS",
	'f': "SS.MS", 'j': "DDD", 'I': "HH12", 'p': "AM", 'P': "am",
	'G': "IYYY", 'g': "IY", 'V': "IW",
	'F': "YYYY-MM-DD", 'T': "HH24:MI:SS", 'R': "HH24:MI",
}

func pgStrftimeFormat(format, ts string) (string, error) {
	var parts []string // SQL text expressions
	var pattern string // pending to_char pattern
	var literal []byte // pending literal text

	flushPattern := func() {
		if pattern != "" {
			parts = append(parts, "to_char("+ts+", '"+pattern+"')")
			pattern = ""
		}
	}
	flushLiteral := func() {
		if len(literal) > 0 {
			parts = append(parts, "'"+strings.ReplaceAll(string(literal), "'", "''")+"'")
			literal = nil
		}
	}

	for i := 0; i < len(format); i++ {
		c := format[i]

		if c != '%' {
			flushPattern()
			literal = append(literal, c)
			continue
		}

		i++
		if i >= len(format) {
			return "", fmt.Errorf("the strftime format %q ends with a dangling %%", format)
		}

		spec := format[i]

		if p, ok := pgStrftimePatterns[spec]; ok {
			flushLiteral()
			pattern += p
			continue
		}

		flushPattern()

		if spec == '%' {
			literal = append(literal, '%')
			continue
		}

		flushLiteral()

		dow := "CAST(EXTRACT(DOW FROM " + ts + ") AS INTEGER)"
		doy := "CAST(EXTRACT(DOY FROM " + ts + ") AS INTEGER)"

		switch spec {
		case 'w':
			parts = append(parts, "CAST("+dow+" AS TEXT)")
		case 'u':
			parts = append(parts, "CAST(CAST(EXTRACT(ISODOW FROM "+ts+") AS INTEGER) AS TEXT)")
		case 's':
			parts = append(parts, "CAST(CAST(floor(EXTRACT(EPOCH FROM ("+ts+" AT TIME ZONE 'UTC'))) AS BIGINT) AS TEXT)")
		case 'e':
			parts = append(parts, "lpad(to_char("+ts+", 'FMDD'), 2, ' ')")
		case 'k':
			parts = append(parts, "lpad(to_char("+ts+", 'FMHH24'), 2, ' ')")
		case 'l':
			parts = append(parts, "lpad(to_char("+ts+", 'FMHH12'), 2, ' ')")
		case 'W': // week of the year, Monday first (00-53)
			parts = append(parts, "lpad(CAST((("+doy+" - 1 + 7 - (("+dow+" + 6) % 7)) / 7) AS TEXT), 2, '0')")
		case 'U': // week of the year, Sunday first (00-53)
			parts = append(parts, "lpad(CAST((("+doy+" - 1 + 7 - "+dow+") / 7) AS TEXT), 2, '0')")
		default:
			return "", fmt.Errorf("the strftime format specifier %%%c is not supported on PostgreSQL", spec)
		}
	}

	flushPattern()
	flushLiteral()

	switch len(parts) {
	case 0:
		return "(CASE WHEN " + ts + " IS NULL THEN NULL ELSE '' END)", nil
	case 1:
		return "(" + parts[0] + ")", nil
	}

	return "(" + strings.Join(parts, " || ") + ")", nil
}
