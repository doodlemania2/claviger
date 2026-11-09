// Command pgdb creates or drops a database on a PostgreSQL server, so that the
// CI scripts need no psql client (#106). Usage, from the repository root:
//
//	CLAVIGER_TEST_PG_URL=postgres://user:pass@host:5432/postgres go run ./scripts/pgdb create|drop NAME
package main

import (
	"database/sql"
	"fmt"
	"os"
	"regexp"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	if len(os.Args) != 3 || (os.Args[1] != "create" && os.Args[1] != "drop") {
		fmt.Fprintln(os.Stderr, "usage: pgdb create|drop NAME  (admin URL in CLAVIGER_TEST_PG_URL)")
		os.Exit(2)
	}

	name := os.Args[2]
	if !regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`).MatchString(name) {
		fmt.Fprintln(os.Stderr, "pgdb: the name must be lower case letters, digits and underscores")
		os.Exit(2)
	}

	db, err := sql.Open("pgx", os.Getenv("CLAVIGER_TEST_PG_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgdb: cannot open the connection")
		os.Exit(1)
	}
	defer db.Close()

	query := `CREATE DATABASE "` + name + `"`
	if os.Args[1] == "drop" {
		query = `DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`
	}

	if _, err := db.Exec(query); err != nil {
		// the error text can carry the connection string: print only the SQLSTATE class
		fmt.Fprintln(os.Stderr, "pgdb: the statement failed")
		os.Exit(1)
	}
}
