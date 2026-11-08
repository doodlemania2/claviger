//go:build no_default_driver

package validators

// Fork-local addition — not upstream PocketBase (issue #106).

import (
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
)

// normalizePostgresUniqueError is a no-op in builds without the default
// database drivers (see db_postgres.go).
func normalizePostgresUniqueError(err error, fieldNames []string) validation.Errors {
	return nil
}
