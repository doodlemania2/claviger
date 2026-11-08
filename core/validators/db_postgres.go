package validators

// Fork-local addition — not upstream PocketBase (issue #106).

import (
	"errors"
	"regexp"
	"strings"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/jackc/pgx/v5/pgconn"
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
