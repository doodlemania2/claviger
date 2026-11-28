//go:build !no_default_driver

package validators

// Fork-local addition — not upstream PocketBase (issue #106).

import (
	"errors"
	"regexp"
	"strings"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/tools/dbutils"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"
)

// pgUniqueViolation is the SQLSTATE of a unique constraint violation.
const pgUniqueViolation = "23505"

var pgKeyDetail = regexp.MustCompile(`(?s)^Key \((.*)\)=\(`)

// normalizePostgresUniqueError converts a PostgreSQL unique violation into
// validation errors for the fields in the violated index. SQLite names the
// columns in the error message ("UNIQUE constraint failed: posts.title"),
// Postgres reports them in the error detail: Key (title)=(x) already exists.
//
// It returns nil if err is not a unique violation that maps to a field.
func normalizePostgresUniqueError(err error, fieldNames []string) validation.Errors {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgUniqueViolation {
		return nil
	}

	m := pgKeyDetail.FindStringSubmatch(pgErr.Detail)
	if m == nil {
		return nil
	}

	// the indexed columns or expressions, eg. `title, lower(email)`
	key := strings.ToLower(m[1])

	normalizedErrs := validation.Errors{}

	for _, name := range fieldNames {
		if regexp.MustCompile(`(^|[^\w])` + regexp.QuoteMeta(strings.ToLower(name)) + `($|[^\w])`).MatchString(key) {
			normalizedErrs[name] = validation.NewError("validation_not_unique", "Value must be unique")
		}
	}

	if len(normalizedErrs) == 0 {
		return nil
	}

	return normalizedErrs
}

// mysqlDuplicateEntry is the MySQL error number of a unique violation.
const mysqlDuplicateEntry = 1062

var mysqlDupKey = regexp.MustCompile(`for key '(?:[^'.]*\.)?([^']*)'`)

// normalizeMySQLUniqueError converts a MySQL "Duplicate entry 'x' for key
// 'table.index'" into validation errors. The error names the index, not its
// columns: the primary key is the id, any other index is matched to the
// fields whose name it contains (idx_users_email, _pb_users_auth__username_idx).
func normalizeMySQLUniqueError(err error, fieldNames []string, indexes []string) validation.Errors {
	var me *mysql.MySQLError
	if !errors.As(err, &me) || me.Number != mysqlDuplicateEntry {
		return nil
	}

	m := mysqlDupKey.FindStringSubmatch(me.Message)
	if m == nil {
		return nil
	}

	normalizedErrs := validation.Errors{}

	if strings.EqualFold(m[1], "PRIMARY") {
		normalizedErrs["id"] = validation.NewError("validation_not_unique", "Value must be unique")
		return normalizedErrs
	}

	// the columns of the index, from the collection's index definitions
	for _, def := range indexes {
		parsed := dbutils.ParseIndex(def)
		if !strings.EqualFold(parsed.IndexName, m[1]) {
			continue
		}
		for _, col := range parsed.Columns {
			colName := strings.Trim(strings.TrimSpace(col.Name), "`\"")
			for _, name := range fieldNames {
				// a plain column, or an expression that contains it (lower(`email`))
				if strings.EqualFold(colName, name) || regexp.MustCompile(`(?i)(^|[^\w])`+regexp.QuoteMeta(name)+`($|[^\w])`).MatchString(colName) {
					normalizedErrs[name] = validation.NewError("validation_not_unique", "Value must be unique")
				}
			}
		}
		if len(normalizedErrs) > 0 {
			return normalizedErrs
		}
	}

	index := strings.ToLower(m[1])
	for _, name := range fieldNames {
		if regexp.MustCompile(`(^|[^\w])` + regexp.QuoteMeta(strings.ToLower(name)) + `($|[^\w])`).MatchString(index) {
			normalizedErrs[name] = validation.NewError("validation_not_unique", "Value must be unique")
		}
	}

	if len(normalizedErrs) == 0 {
		return nil
	}

	return normalizedErrs
}

var (
	mssqlDupIndex = regexp.MustCompile(`with unique index '([^']*)'`)
	mssqlDupKey   = regexp.MustCompile(`(?i)(?:PRIMARY KEY|UNIQUE KEY|UNIQUE) constraint '([^']*)'`)
)

// normalizeMSSQLUniqueError converts a SQL Server unique violation (2601 for
// a unique index, 2627 for a primary key or unique constraint) into
// validation errors. Like MySQL's, the error names the index, not its
// columns: the primary key is the id, any other index is matched to the
// fields in the collection's index definitions or to the fields whose name it
// contains.
func normalizeMSSQLUniqueError(err error, fieldNames []string, indexes []string) validation.Errors {
	var me mssql.Error
	if !errors.As(err, &me) || (me.Number != 2601 && me.Number != 2627) {
		return nil
	}

	normalizedErrs := validation.Errors{}

	var name string
	if m := mssqlDupIndex.FindStringSubmatch(me.Message); m != nil {
		name = m[1]
	} else if m := mssqlDupKey.FindStringSubmatch(me.Message); m != nil {
		name = m[1]
		if strings.HasPrefix(strings.ToUpper(name), "PK__") || strings.HasPrefix(strings.ToUpper(name), "PK_") {
			normalizedErrs["id"] = validation.NewError("validation_not_unique", "Value must be unique")
			return normalizedErrs
		}
	} else {
		return nil
	}

	for _, def := range indexes {
		parsed := dbutils.ParseIndex(def)
		if !strings.EqualFold(parsed.IndexName, name) {
			continue
		}
		for _, col := range parsed.Columns {
			colName := strings.Trim(strings.TrimSpace(col.Name), "`\"[]")
			for _, field := range fieldNames {
				if strings.EqualFold(colName, field) || regexp.MustCompile(`(?i)(^|[^\w])`+regexp.QuoteMeta(field)+`($|[^\w])`).MatchString(colName) {
					normalizedErrs[field] = validation.NewError("validation_not_unique", "Value must be unique")
				}
			}
		}
		if len(normalizedErrs) > 0 {
			return normalizedErrs
		}
	}

	lower := strings.ToLower(name)
	for _, field := range fieldNames {
		if regexp.MustCompile(`(^|[^\w])` + regexp.QuoteMeta(strings.ToLower(field)) + `($|[^\w])`).MatchString(lower) {
			normalizedErrs[field] = validation.NewError("validation_not_unique", "Value must be unique")
		}
	}

	if len(normalizedErrs) == 0 {
		return nil
	}

	return normalizedErrs
}
