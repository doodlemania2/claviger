package apis

// Fork-local addition (issue #106, M3): the Postgres side of the superuser SQL
// endpoint (apis/sql.go). SQLite keeps the historic prefix based classification.
//
// On PostgreSQL the classification of a statement is not what protects the
// database from a statement that was mistaken for a read: every query that is
// not clearly a write runs in a READ ONLY transaction, which the server itself
// enforces (a "WITH x AS (DELETE ... RETURNING *) SELECT ..." fails with
// SQLSTATE 25006). Only that failure promotes the statement to the write path,
// after the same guards that a plain DELETE gets.

import (
	"context"
	"database/sql"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/dialect"
)

// postgresWriteKeywords are the first keywords of statements that change data,
// schema, privileges or session state (and so never run in the read only transaction).
var postgresWriteKeywords = map[string]struct{}{
	"INSERT": {}, "UPDATE": {}, "DELETE": {}, "MERGE": {}, "TRUNCATE": {},
	"CREATE": {}, "DROP": {}, "ALTER": {}, "COMMENT": {}, "GRANT": {}, "REVOKE": {},
	"COPY": {}, "VACUUM": {}, "REINDEX": {}, "CLUSTER": {}, "REFRESH": {}, "LOCK": {},
	"DO": {}, "CALL": {}, "SECURITY": {}, "IMPORT": {}, "REASSIGN": {},
}

// isWriteQuery reports if the statement has to take the write path.
func isWriteQuery(d dialect.Dialect, query string, sqliteGuess bool) bool {
	if d.Name() != dialect.PostgresName {
		return sqliteGuess
	}

	_, ok := postgresWriteKeywords[postgresFirstKeyword(query)]

	return ok
}

// sqlTouchesAuditLog reports if the query must be rejected because it
// would modify the (append only) audit log. It is only asked for write queries.
func sqlTouchesAuditLog(d dialect.Dialect, query string) bool {
	if d.Name() != dialect.PostgresName {
		return strings.Contains(strings.ToLower(query), strings.ToLower(core.CollectionNameAuditLog))
	}

	normalized := postgresNormalizeIdentifiers(query)

	if strings.Contains(normalized, strings.ToLower(core.CollectionNameAuditLog)) {
		return true
	}

	// statements that remove the table without naming it
	words := strings.Fields(postgresStripLiterals(normalized))
	for i := 0; i+1 < len(words); i++ {
		if words[i] == "drop" && (words[i+1] == "schema" || words[i+1] == "owned" || words[i+1] == "database") {
			return true
		}
	}

	return false
}

var postgresUnicodeIdent = regexp.MustCompile(`(?i)u&"((?:[^"]|"")*)"(?:\s+uescape\s+'(.)')?`)

// postgresNormalizeIdentifiers lower cases the query and resolves the forms
// in which Postgres lets a name be written differently: quoted identifiers
// ("_auditLog" and _auditlog are the same table, unquoted names are folded to
// lower case) and the Unicode escape form (U&"\005faudit\004cog").
func postgresNormalizeIdentifiers(query string) string {
	query = postgresStripComments(query)

	query = postgresUnicodeIdent.ReplaceAllStringFunc(query, func(m string) string {
		sub := postgresUnicodeIdent.FindStringSubmatch(m)
		escape := byte('\\')
		if len(sub) > 2 && sub[2] != "" {
			escape = sub[2][0]
		}

		return `"` + postgresUnescape(sub[1], escape) + `"`
	})

	query = strings.ToLower(query)

	// "a"."b" / "a" -> a.b / a (a quote doubled inside a name is not a plain name anyway)
	return strings.ReplaceAll(query, `"`, "")
}

// postgresUnescape resolves the \XXXX, \+XXXXXX and doubled escape character
// forms of a U&"..." identifier.
func postgresUnescape(s string, escape byte) string {
	var b strings.Builder

	for i := 0; i < len(s); i++ {
		if s[i] != escape {
			b.WriteByte(s[i])
			continue
		}

		switch {
		case i+1 < len(s) && s[i+1] == escape:
			b.WriteByte(escape)
			i++
		case i+8 <= len(s) && s[i+1] == '+':
			// 6 hex digits fit in 24 bits; anything past the Unicode range is left as is
			if r, err := strconv.ParseUint(s[i+2:i+8], 16, 24); err == nil && r <= utf8.MaxRune {
				b.WriteRune(rune(r))
				i += 7
			}
		case i+5 <= len(s):
			if r, err := strconv.ParseUint(s[i+1:i+5], 16, 16); err == nil && r <= utf8.MaxRune {
				b.WriteRune(rune(r))
				i += 4
			}
		}
	}

	return b.String()
}

// postgresStripComments removes "-- ..." and (nested) "/* ... */" comments,
// leaving quoted text alone.
func postgresStripComments(q string) string {
	var b strings.Builder

	for i := 0; i < len(q); i++ {
		c := q[i]

		switch {
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(q) {
				if q[j] == c {
					if j+1 < len(q) && q[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j >= len(q) {
				j = len(q) - 1
			}
			b.WriteString(q[i : j+1])
			i = j
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			for i < len(q) && q[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			depth := 1
			i += 2
			for i < len(q) && depth > 0 {
				switch {
				case q[i] == '/' && i+1 < len(q) && q[i+1] == '*':
					depth++
					i++
				case q[i] == '*' && i+1 < len(q) && q[i+1] == '/':
					depth--
					i++
				}
				i++
			}
			i--
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}

	return b.String()
}

// postgresStripLiterals blanks the content of '...' strings.
func postgresStripLiterals(q string) string {
	var b strings.Builder

	inString := false
	for i := 0; i < len(q); i++ {
		if q[i] == '\'' {
			inString = !inString
			continue
		}
		if !inString {
			b.WriteByte(q[i])
		}
	}

	return b.String()
}

// postgresFirstKeyword returns the upper cased first word of the statement
// (after whitespace, comments and opening parentheses).
func postgresFirstKeyword(q string) string {
	q = strings.TrimLeft(postgresStripComments(q), " \t\r\n(;")

	end := 0
	for end < len(q) {
		c := q[end]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' {
			end++
			continue
		}
		break
	}

	return strings.ToUpper(q[:end])
}

// runReadQuery runs a statement that is not known to be a write and returns
// its rows. On Postgres it is a READ ONLY transaction (see the file comment);
// release must be called after the rows are consumed.
func runReadQuery(ctx context.Context, app core.App, query string) (rows *dbx.Rows, release func(), err error) {
	db := app.ConcurrentDB()

	if core.DialectOf(app).Name() != dialect.PostgresName {
		rows, err = db.NewQuery(query).WithContext(ctx).Rows()
		return rows, func() {}, err
	}

	dbxDB, ok := db.(*dbx.DB)
	if !ok {
		rows, err = db.NewQuery(query).WithContext(ctx).Rows()
		return rows, func() {}, err
	}

	tx, err := dbxDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, func() {}, err
	}

	rows, err = tx.NewQuery(query).WithContext(ctx).Rows()
	if err != nil {
		_ = tx.Rollback()
		return nil, func() {}, err
	}

	return rows, func() { _ = tx.Rollback() }, nil
}

// isReadOnlyViolation reports if err is Postgres refusing a write in a read only transaction.
func isReadOnlyViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 25006")
}
