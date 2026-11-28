//go:build !no_default_driver

package core

// Fork-local addition — not upstream PocketBase (issue #106, experimental).
// Opt-in libSQL (sqld / Turso) storage for the app data database through
// the pure Go libsql-client-go driver (Hrana over HTTP, no CGO).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dialect"
	"github.com/tursodatabase/libsql-client-go/libsql"
)

// DBAuthTokenEnv is the name of the environment variable that holds the
// authentication token (JWT) of a libSQL server, so that it never has to
// sit in the connection URL (which ends up in process lists and manifests).
const DBAuthTokenEnv = "CLAVIGER_DB_AUTH_TOKEN"

// isLibSQLURL reports whether the data database URL selects libSQL:
// libsql:// (TLS, unless ?tls=0), http:// or https://.
func isLibSQLURL(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasPrefix(lower, "libsql://") ||
		strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://")
}

// libsqlParams is a parsed libSQL data database URL.
type libsqlParams struct {
	// url is the server URL without query string and credentials.
	url string
	// token is the auth token ("" for a server without authentication).
	token string
	// tls is false only for libsql://host:port?tls=0
	tls *bool
	// proxy is the address that is actually connected to, while the host of
	// the URL stays the HTTP Host header (a sqld behind an ingress, or one
	// namespace of a multi tenant sqld, which picks it from the host name).
	proxy string
}

// parseLibSQLURL splits the URL into the server address, the auth token and
// the TLS option. The token comes from the "authToken" query parameter or,
// when that is absent, from envToken (CLAVIGER_DB_AUTH_TOKEN).
func parseLibSQLURL(raw, envToken string) (*libsqlParams, error) {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, errors.New("invalid libSQL URL")
	}

	switch strings.ToLower(u.Scheme) {
	case "libsql", "http", "https":
	default:
		return nil, errors.New("invalid libSQL URL: the scheme must be libsql://, http:// or https://")
	}
	if u.Host == "" {
		return nil, errors.New("invalid libSQL URL: the host is missing")
	}
	if u.User != nil {
		return nil, errors.New("invalid libSQL URL: credentials are not part of the URL, use ?authToken=... or " + DBAuthTokenEnv)
	}

	p := &libsqlParams{token: strings.TrimSpace(envToken)}

	for key, values := range u.Query() {
		if len(values) == 0 {
			continue
		}
		switch strings.ToLower(key) {
		case "authtoken", "auth_token", "jwt":
			if v := strings.TrimSpace(values[0]); v != "" {
				p.token = v
			}
		case "proxy":
			pu, err := neturl.Parse(values[0])
			if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
				return nil, errors.New("invalid libSQL URL: proxy must be an http:// or https:// address")
			}
			p.proxy = values[0]
		case "tls":
			on := !(values[0] == "0" || strings.EqualFold(values[0], "false"))
			p.tls = &on
		default:
			return nil, fmt.Errorf("invalid libSQL URL: unknown query parameter %q (supported: authToken, tls, proxy)", key)
		}
	}

	if p.tls != nil && !*p.tls && !strings.EqualFold(u.Scheme, "libsql") {
		return nil, errors.New("invalid libSQL URL: tls=0 is only for libsql:// URLs (use http:// for a plain text server)")
	}

	u.RawQuery = ""
	u.Fragment = ""
	p.url = u.String()

	return p, nil
}

// LibSQLDBConnect returns a [DBConnectFunc] that opens the provided
// libsql://host[:port]?authToken=... (or http(s)://) server URL.
// The dbPath argument is ignored.
//
// No SQLite pragma is sent: the connections are remote, the server owns the
// file (WAL, synchronous, foreign keys and busy timeout are its settings).
func LibSQLDBConnect(rawURL string) DBConnectFunc {
	return func(_ string) (*dbx.DB, error) {
		p, err := parseLibSQLURL(rawURL, os.Getenv(DBAuthTokenEnv))
		if err != nil {
			return nil, err
		}

		opts := []libsql.Option{}
		if p.token != "" {
			opts = append(opts, libsql.WithAuthToken(p.token))
		}
		if p.tls != nil {
			opts = append(opts, libsql.WithTls(*p.tls))
		}
		if p.proxy != "" {
			opts = append(opts, libsql.WithProxy(p.proxy))
		}

		connector, err := libsql.NewConnector(p.url, opts...)
		if err != nil {
			return nil, redactLibSQLErr(err, rawURL, p.token)
		}

		sqlDB := sql.OpenDB(connector)

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if err := sqlDB.PingContext(ctx); err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("failed to connect to the libSQL database (%s): %w", DBURLEnv, redactLibSQLErr(err, rawURL, p.token))
		}

		// a quick sanity check that the server speaks SQLite SQL and that the
		// token is accepted (the ping of the HTTP driver sends no statement)
		var one int
		if err := sqlDB.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("failed to query the libSQL database (%s): %w", DBURLEnv, redactLibSQLErr(err, rawURL, p.token))
		}

		return dbx.NewFromDB(sqlDB, dialect.LibSQLName), nil
	}
}

// redactLibSQLErr keeps the connection URL and the token out of error messages.
func redactLibSQLErr(err error, rawURL, token string) error {
	if err == nil {
		return nil
	}

	msg := strings.ReplaceAll(err.Error(), rawURL, "<redacted>")
	if token != "" {
		msg = strings.ReplaceAll(msg, token, "<redacted>")
	}

	return errors.New(msg)
}

// RedactDBURL masks the secrets of a data database URL (the password of a
// postgres:// or mysql:// URL, the authToken of a libsql:// one) so that it is
// safe to put in a log line.
func RedactDBURL(raw string) string {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "<redacted>"
	}

	if u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = neturl.UserPassword(u.User.Username(), "xxxxx")
		}
	}

	q := u.Query()
	changed := false
	for key := range q {
		switch strings.ToLower(key) {
		case "authtoken", "auth_token", "jwt", "password", "sslpassword":
			q.Set(key, "xxxxx")
			changed = true
		}
	}
	if changed {
		u.RawQuery = q.Encode()
	}

	return u.String()
}
