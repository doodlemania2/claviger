//go:build !no_default_driver

package core

// Fork-local addition — not upstream PocketBase (issue #106, experimental).
// Opt-in MySQL 8.0+ / MariaDB 10.11+ storage for the app data database
// through the pure Go go-sql-driver/mysql driver.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	neturl "net/url"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dialect"
	"github.com/go-sql-driver/mysql"
)

func init() {
	dialect.MySQLErrorNumber = func(err error) uint16 {
		var me *mysql.MySQLError
		if errors.As(err, &me) {
			return me.Number
		}
		return 0
	}
}

// mysqlSQLMode is the session sql_mode of the data connections:
//   - STRICT_TRANS_TABLES: a value that does not fit is an error, not a truncation;
//   - PIPES_AS_CONCAT: the filter SQL concatenates with ||;
//   - NO_BACKSLASH_ESCAPES: the filter SQL writes LIKE ... ESCAPE '\'.
const mysqlSQLMode = "STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION,PIPES_AS_CONCAT,NO_BACKSLASH_ESCAPES"

// isMySQLURL reports whether the data database URL selects MySQL or MariaDB.
func isMySQLURL(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasPrefix(lower, "mysql://") || strings.HasPrefix(lower, "mariadb://")
}

// MySQLDBConnect returns a [DBConnectFunc] that opens the provided
// mysql://user:password@host:3306/dbname URL (or mariadb://). The dbPath
// argument is ignored. A MariaDB server is detected after connecting.
func MySQLDBConnect(rawURL string) DBConnectFunc {
	return func(_ string) (*dbx.DB, error) {
		dsn, err := mysqlDSN(rawURL)
		if err != nil {
			return nil, redactDBURL(err, rawURL)
		}

		sqlDB, err := sql.Open("mysql", dsn)
		if err != nil {
			return nil, redactDBURL(err, rawURL)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if err := sqlDB.PingContext(ctx); err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("failed to connect to the MySQL database (%s): %w", DBURLEnv, redactDBURL(err, rawURL))
		}

		driverName := "mysql"
		var version string
		if err := sqlDB.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err == nil &&
			strings.Contains(strings.ToLower(version), "mariadb") {
			// MariaDB (11.4) has no functional indexes, which the partial and
			// case insensitive unique indexes of the auth collections need
			_ = sqlDB.Close()
			return nil, errors.New("MariaDB is not supported yet (it has no functional indexes, see docs/datastores.md): use MySQL 8.0 or newer")
		}

		return dbx.NewFromDB(sqlDB, driverName), nil
	}
}

// mysqlDSN converts the URL to the go-sql-driver DSN.
func mysqlDSN(rawURL string) (string, error) {
	u, err := neturl.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", errors.New("invalid MySQL URL")
	}

	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	if !strings.Contains(cfg.Addr, ":") {
		cfg.Addr += ":3306"
	}
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
	}
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	cfg.Collation = "utf8mb4_bin"
	cfg.ParseTime = false
	cfg.MultiStatements = true // the system migrations send scripts
	cfg.Params = map[string]string{
		"sql_mode":  "'" + mysqlSQLMode + "'",
		"time_zone": "'+00:00'",
		// MySQL 8.4 turns EXISTS and IN subqueries over JSON_TABLE into a
		// semijoin that matches nothing (a server bug, seen with a utf8mb4_bin
		// database): the back relations and the multi value filters rely on them
		"optimizer_switch": "'semijoin=off'",
	}

	// everything else of the query string is a driver option or a session variable
	for key, values := range u.Query() {
		if len(values) == 0 {
			continue
		}
		switch strings.ToLower(key) {
		case "tls":
			cfg.TLSConfig = values[0]
		case "sql_mode", "time_zone", "collation", "parsetime", "multistatements", "optimizer_switch":
			// owned by Claviger
		default:
			cfg.Params[key] = values[0]
		}
	}

	return cfg.FormatDSN(), nil
}
