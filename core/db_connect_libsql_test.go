//go:build !no_default_driver

package core

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestIsLibSQLURL(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]bool{
		"libsql://db.example.com":          true,
		"LIBSQL://db.example.com?tls=0":    true,
		"http://127.0.0.1:8080":            true,
		"https://db.example.com":           true,
		"postgres://u:p@h/db":              false,
		"mysql://u:p@h/db":                 false,
		"cockroach://u:p@h/db":             false,
		"file:data.db":                     false,
		"":                                 false,
		"  libsql://db.example.com  ":      true,
		"httpx://not.a.libsql.example.com": false,
	} {
		if got := isLibSQLURL(raw); got != want {
			t.Errorf("isLibSQLURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestParseLibSQLURL(t *testing.T) {
	t.Parallel()

	off := false

	scenarios := []struct {
		name     string
		raw      string
		envToken string
		want     libsqlParams
		wantErr  string
	}{
		{"plain", "libsql://db.example.com", "", libsqlParams{url: "libsql://db.example.com"}, ""},
		{"token in the url", "libsql://db.example.com?authToken=abc", "", libsqlParams{url: "libsql://db.example.com", token: "abc"}, ""},
		{"token from the env", "libsql://db.example.com", "envtoken", libsqlParams{url: "libsql://db.example.com", token: "envtoken"}, ""},
		{"the url token wins", "libsql://db.example.com?authToken=abc", "envtoken", libsqlParams{url: "libsql://db.example.com", token: "abc"}, ""},
		{"auth_token alias", "https://db.example.com?auth_token=abc", "", libsqlParams{url: "https://db.example.com", token: "abc"}, ""},
		{"tls off", "libsql://127.0.0.1:8080?tls=0", "", libsqlParams{url: "libsql://127.0.0.1:8080", tls: &off}, ""},
		{"proxy", "http://ns.example.test?proxy=http://127.0.0.1:8080", "", libsqlParams{url: "http://ns.example.test", proxy: "http://127.0.0.1:8080"}, ""},
		{"credentials", "libsql://user:pass@db.example.com", "", libsqlParams{}, "credentials are not part of the URL"},
		{"unknown param", "libsql://db.example.com?foo=1", "", libsqlParams{}, `unknown query parameter "foo"`},
		{"tls off on http", "http://db.example.com?tls=0", "", libsqlParams{}, "tls=0 is only for libsql://"},
		{"bad proxy", "http://db.example.com?proxy=ftp://x", "", libsqlParams{}, "proxy must be an http"},
		{"no host", "libsql://", "", libsqlParams{}, "the host is missing"},
		{"bad scheme", "postgres://db.example.com", "", libsqlParams{}, "the scheme must be"},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			got, err := parseLibSQLURL(s.raw, s.envToken)

			if s.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), s.wantErr) {
					t.Fatalf("expected an error containing %q, got %v", s.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}

			if got.url != s.want.url || got.token != s.want.token || got.proxy != s.want.proxy {
				t.Fatalf("got %+v, want %+v", got, s.want)
			}
			if (got.tls == nil) != (s.want.tls == nil) || (got.tls != nil && *got.tls != *s.want.tls) {
				t.Fatalf("tls: got %v, want %v", got.tls, s.want.tls)
			}
			// the token never stays in the URL that is handed to the driver
			if strings.Contains(got.url, "?") {
				t.Fatalf("the query string must be stripped, got %q", got.url)
			}
		})
	}
}

func TestRedactDBURL(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3t-token-value"

	for _, raw := range []string{
		"libsql://db.example.com?authToken=" + secret,
		"https://db.example.com?auth_token=" + secret + "&tls=1",
		"http://127.0.0.1:8080?jwt=" + secret,
		"postgres://user:" + secret + "@db.example.com:5432/claviger?sslmode=require",
		"mysql://user:" + secret + "@db.example.com:3306/claviger",
	} {
		got := RedactDBURL(raw)
		if strings.Contains(got, secret) {
			t.Errorf("RedactDBURL(%q) leaked the secret: %q", raw, got)
		}
		if !strings.Contains(got, "db.example.com") && !strings.Contains(got, "127.0.0.1") {
			t.Errorf("RedactDBURL(%q) lost the host: %q", raw, got)
		}
	}

	// nothing to hide
	if got := RedactDBURL("libsql://db.example.com"); got != "libsql://db.example.com" {
		t.Errorf("unexpected change: %q", got)
	}
}

func TestLibSQLConnectErrorsDoNotLeakTheToken(t *testing.T) {
	// not parallel: sets an env var
	const urlToken = "tok-in-url-0123456789"
	const envToken = "tok-in-env-0123456789"

	t.Setenv(DBAuthTokenEnv, envToken)

	// nothing listens on the port of a closed listener
	for _, raw := range []string{
		"http://127.0.0.1:1?authToken=" + urlToken,
		"http://127.0.0.1:1",
	} {
		_, err := LibSQLDBConnect(raw)("")
		if err == nil {
			t.Fatalf("expected a connection error for %q", raw)
		}

		msg := err.Error()
		for _, secret := range []string{urlToken, envToken, "authToken=", url.QueryEscape(urlToken)} {
			if strings.Contains(msg, secret) {
				t.Fatalf("the error leaks %q: %s", secret, msg)
			}
		}
	}

	// an invalid URL is reported as is, without a token
	if _, err := LibSQLDBConnect("libsql://u:p@h")(""); err == nil || strings.Contains(err.Error(), ":p@") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRedactLibSQLErr(t *testing.T) {
	t.Parallel()

	err := redactLibSQLErr(errors.New("Get http://h?authToken=abc: Bearer abc rejected"), "http://h?authToken=abc", "abc")
	if strings.Contains(err.Error(), "abc") {
		t.Fatalf("leaked: %v", err)
	}

	if redactLibSQLErr(nil, "x", "y") != nil {
		t.Fatal("expected nil")
	}
}
