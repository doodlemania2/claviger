package cmd_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/cmd"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #75).

func kekB64(t testing.TB) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func envFn(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// dbEnv is envFn plus the Postgres URL of the test app (the way a deployment
// selects the database), if it runs on Postgres.
func dbEnv(app *tests.TestApp, m map[string]string) func(string) string {
	merged := map[string]string{}
	for k, v := range m {
		merged[k] = v
	}
	if u := app.PostgresURL(); u != "" {
		merged[core.DBURLEnv] = u
	}
	return envFn(merged)
}

func dirDigest(t testing.TB, dir string) string {
	t.Helper()
	h := sha256.New()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "data.db") {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		h.Write([]byte(e.Name()))
		h.Write(b)
	}
	return string(h.Sum(nil))
}

func TestKeysCheck(t *testing.T) {
	kek := kekB64(t)
	t.Setenv("CLAVIGER_KEK", kek)

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	usePostgresOfApp(t, app)

	// a database without encrypted fields
	if err := core.DialectOf(app).Checkpoint(app.DB()); err != nil {
		t.Fatal(err)
	}
	msg, err := cmd.CheckFieldKeys(context.Background(), app.DataDir(), "", dbEnv(app, nil))
	if err != nil || !strings.Contains(msg, "no encrypted fields") {
		t.Fatalf("no encrypted fields: %q, %v", msg, err)
	}

	col := core.NewBaseCollection("checked")
	col.Fields.Add(&core.EncryptedTextField{Name: "secret"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	if err := core.DialectOf(app).Checkpoint(app.DB()); err != nil {
		t.Fatal(err)
	}

	before := dirDigest(t, app.DataDir())

	scenarios := []struct {
		name    string
		env     map[string]string
		wantErr string
		wantMsg string
	}{
		{"right key", map[string]string{"CLAVIGER_KEK": kek}, "", "OK (1 encrypted field key(s)"},
		{"no key", nil, "keys check: FAILED", ""},
		{"wrong key", map[string]string{"CLAVIGER_KEK": kekB64(t)}, "different key-encryption key", ""},
		{"malformed key", map[string]string{"CLAVIGER_KEK": "%%%"}, "CLAVIGER_KEK is invalid", ""},
		{"derived key does not match", map[string]string{"PB_ENCRYPTION_KEY": strings.Repeat("a", 32)}, "keys check: FAILED", ""},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			msg, err := cmd.CheckFieldKeys(context.Background(), app.DataDir(), "PB_ENCRYPTION_KEY", dbEnv(app, s.env))
			if s.wantErr == "" {
				if err != nil || !strings.Contains(msg, s.wantMsg) {
					t.Fatalf("got %q, %v", msg, err)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), cmd.KeysCheckFailedPrefix) || !strings.Contains(err.Error(), s.wantErr) {
				t.Fatalf("expected %q, got %v", s.wantErr, err)
			}
			if strings.Contains(err.Error(), kek) {
				t.Fatal("the error must not contain the key")
			}
		})
	}

	// the check wrote nothing
	if after := dirDigest(t, app.DataDir()); after != before {
		t.Fatal("keys check modified the database files")
	}
}

func TestKeysCheckFailsOpenOnInfrastructureProblems(t *testing.T) {
	dir := t.TempDir()

	// no database yet
	msg, err := cmd.CheckFieldKeys(context.Background(), dir, "", envFn(nil))
	if err != nil || !strings.Contains(msg, "skipped") {
		t.Fatalf("missing db: %q, %v", msg, err)
	}

	// not a database
	if err := os.WriteFile(filepath.Join(dir, "data.db"), []byte("this is not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, err = cmd.CheckFieldKeys(context.Background(), dir, "", envFn(nil))
	if err != nil || !strings.Contains(msg, "skipped") {
		t.Fatalf("garbage db: %q, %v", msg, err)
	}
}
