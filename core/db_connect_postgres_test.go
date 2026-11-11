package core

import "testing"

func TestPostgresExecModeURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"postgres://u:p@h:5432/db", "postgres://u:p@h:5432/db?default_query_exec_mode=describe_exec"},
		{"postgres://u:p@h/db?sslmode=disable", "postgres://u:p@h/db?default_query_exec_mode=describe_exec&sslmode=disable"},
		{"postgres://h/db?default_query_exec_mode=exec", "postgres://h/db?default_query_exec_mode=exec"},
		{"host=h dbname=db", "host=h dbname=db default_query_exec_mode=describe_exec"},
	}
	for _, c := range cases {
		if got := postgresExecModeURL(c.in); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}
