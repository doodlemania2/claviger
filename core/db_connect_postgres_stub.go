//go:build no_default_driver

package core

// With the "no_default_driver" build tag the DBConnect option is mandatory
// and the CLAVIGER_DB_URL environment variable is not supported.
func applyEnvDBConfig(config *BaseAppConfig) {}
