// Command pgembedded starts a throwaway PostgreSQL 16 (downloaded by
// embedded-postgres on first use), runs the Postgres test package against it,
// and stops it again. Usage, from the repository root:
//
//	cd scripts/pgembedded && go run . [go test args...]
//
// Environment:
//
//	EPG_PORT      server port (default 55432; 54329 belongs to other local tooling)
//	EPG_VERBOSE   print the server log
//	EPG_BINARIES  directory to cache the downloaded binaries in
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pgembedded:", err)
		os.Exit(1)
	}
}

func run() error {
	port := uint32(55432)
	if v := os.Getenv("EPG_PORT"); v != "" {
		var p uint32
		if _, err := fmt.Sscanf(v, "%d", &p); err != nil || p == 0 {
			return fmt.Errorf("invalid EPG_PORT %q", v)
		}
		port = p
	}

	root, err := filepath.Abs("../..")
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "claviger-pgembedded-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	cfg := embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).
		Port(port).
		Username("pb").Password("pb").Database("pbtest").
		RuntimePath(filepath.Join(tmp, "rt")).
		DataPath(filepath.Join(tmp, "data"))
	if dir := os.Getenv("EPG_BINARIES"); dir != "" {
		cfg = cfg.BinariesPath(dir)
	}

	// the server log is very noisy; EPG_VERBOSE=1 prints it
	if os.Getenv("EPG_VERBOSE") == "" {
		cfg = cfg.Logger(io.Discard)
	}

	pg := embeddedpostgres.NewDatabase(cfg)
	if err := pg.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	defer pg.Stop()

	url := fmt.Sprintf("postgres://pb:pb@localhost:%d/pbtest?sslmode=disable", port)

	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"-count=1", "./tests/pgtests/..."}
	}

	cmd := exec.Command("go", append([]string{"test", "-tags", "postgres"}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CLAVIGER_TEST_PG_URL="+url, "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr

	return cmd.Run()
}
