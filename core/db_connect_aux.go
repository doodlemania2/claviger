package core

// Fork-local addition — not upstream PocketBase (issue #106).

// auxDBConnect returns the connect function of the auxiliary (logs) database,
// which is the data database one unless AuxDBConnect is set. With PostgreSQL
// as data database the auxiliary db stays a local SQLite file per replica.
func (app *BaseApp) auxDBConnect() DBConnectFunc {
	if app.config.AuxDBConnect != nil {
		return app.config.AuxDBConnect
	}

	return app.config.DBConnect
}
