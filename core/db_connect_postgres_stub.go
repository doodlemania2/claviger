//go:build no_default_driver

package core

import (
	"errors"

	"ganigeorgiev.com/pocketbase/dbx"
)

// DBURLEnv is the name of the environment variable that selects PostgreSQL
// (see db_connect_postgres.go). It has no effect with the "no_default_driver" tag.
const DBURLEnv = "CLAVIGER_DB_URL"

// With the "no_default_driver" build tag the DBConnect option is mandatory
// and the CLAVIGER_DB_URL environment variable is not supported.
func applyEnvDBConfig(config *BaseAppConfig) {}

// PostgresDBConnect is not available without the default drivers.
func PostgresDBConnect(url string) DBConnectFunc {
	return func(_ string) (*dbx.DB, error) {
		return nil, errors.New("PostgreSQL support is not compiled in (no_default_driver build tag)")
	}
}
