//go:build !no_default_driver

package core

// Fork-local addition — not upstream PocketBase (issue #106, experimental).
// Opt-in SQL Server 2019+ / Azure SQL storage for the app data database
// through the pure Go go-mssqldb driver.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	neturl "net/url"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dialect"
	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// mssqlConnector sends a []byte argument as text: the driver sends it as
// VARBINARY, which SQL Server then stores in an NVARCHAR column as its raw
// (UTF-16) bytes. Claviger stores no binary data.
type mssqlConnector struct {
	*mssql.Connector
}

func (c *mssqlConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}

	inner, ok := conn.(*mssql.Conn)
	if !ok {
		return conn, nil
	}

	return &mssqlConn{inner}, nil
}

type mssqlConn struct {
	*mssql.Conn
}

func (c *mssqlConn) CheckNamedValue(nv *driver.NamedValue) error {
	if b, ok := nv.Value.([]byte); ok {
		nv.Value = string(b)
	}

	return c.Conn.CheckNamedValue(nv)
}

func init() {
	dialect.MSSQLErrorNumber = func(err error) int32 {
		var me mssql.Error
		if errors.As(err, &me) {
			return me.Number
		}
		var mp *mssql.Error
		if errors.As(err, &mp) && mp != nil {
			return mp.Number
		}
		return 0
	}
}

// isMSSQLURL reports whether the data database URL selects SQL Server.
func isMSSQLURL(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasPrefix(lower, "sqlserver://") || strings.HasPrefix(lower, "mssql://")
}

// MSSQLDBConnect returns a [DBConnectFunc] that opens the provided
// sqlserver://user:password@host:1433?database=dbname URL (mssql:// is
// accepted too). The dbPath argument is ignored.
func MSSQLDBConnect(rawURL string) DBConnectFunc {
	return func(_ string) (*dbx.DB, error) {
		dsn, err := mssqlDSN(rawURL)
		if err != nil {
			return nil, redactDBURL(err, rawURL)
		}

		cfg, err := msdsn.Parse(dsn)
		if err != nil {
			return nil, redactDBURL(err, rawURL)
		}

		// the connector takes the ? placeholders that dbx's builder generates
		sqlDB := sql.OpenDB(&mssqlConnector{mssql.NewConnectorWithProcessQueryText(cfg)})

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if err := sqlDB.PingContext(ctx); err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("failed to connect to the SQL Server database (%s): %w", DBURLEnv, redactDBURL(err, rawURL))
		}

		var version int
		if err := sqlDB.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('ProductMajorVersion') AS INT)").Scan(&version); err == nil && version != 0 && version < 15 {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("SQL Server %d is not supported (it has no OPENJSON before 2016 and no STRING_ESCAPE): use SQL Server 2019 or newer", version)
		}

		// text is compared as bytes, case and accent sensitive, like SQLite's
		// BINARY: that needs a binary database collation (it also makes the
		// identifiers case sensitive)
		var collation string
		if err := sqlDB.QueryRowContext(ctx, "SELECT CAST(DATABASEPROPERTYEX(DB_NAME(), 'Collation') AS NVARCHAR(128))").Scan(&collation); err == nil &&
			!strings.HasSuffix(strings.ToUpper(collation), "_BIN2") {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("the SQL Server database has the collation %s: create it with COLLATE %s (binary, case sensitive), see docs/datastores.md", collation, dialect.MSSQLCollation)
		}

		return dbx.NewFromDB(sqlDB, dialect.MSSQLName), nil
	}
}

// mssqlDSN converts the URL to the go-mssqldb URL: a mssql:// scheme becomes
// sqlserver:// and a database in the path ("/dbname") becomes the database
// parameter. Everything else is passed to the driver as it is.
func mssqlDSN(rawURL string) (string, error) {
	u, err := neturl.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", errors.New("invalid SQL Server URL")
	}

	if !strings.EqualFold(u.Scheme, "sqlserver") && !strings.EqualFold(u.Scheme, "mssql") {
		return "", errors.New("invalid SQL Server URL (the scheme must be sqlserver://)")
	}
	u.Scheme = "sqlserver"

	if name := strings.TrimPrefix(u.Path, "/"); name != "" && !strings.Contains(name, "/") {
		q := u.Query()
		if q.Get("database") == "" {
			q.Set("database", name)
		}
		u.Path = ""
		u.RawQuery = q.Encode()
	}

	if u.Query().Get("database") == "" {
		return "", errors.New("the SQL Server URL needs a database: sqlserver://user:password@host:1433?database=claviger")
	}

	return u.String(), nil
}
