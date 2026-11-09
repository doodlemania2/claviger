//go:build unix

package cmd_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/cmd"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #75, M3). Not parallel: keys come from the environment.

func runKeys(t testing.TB, app core.App, args ...string) (string, error) {
	t.Helper()
	c := cmd.NewKeysCommand(app)
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

// usePostgresOfApp points the commands, which open the database by themselves
// from the environment, to the Postgres database of the test app (no-op on SQLite).
func usePostgresOfApp(t testing.TB, app *tests.TestApp) {
	t.Helper()
	if u := app.PostgresURL(); u != "" {
		t.Setenv(core.DBURLEnv, u)
	}
}

func keysApp(t *testing.T) (*tests.TestApp, *core.Collection) {
	t.Helper()
	t.Setenv("CLAVIGER_KEK", kekB64(t))
	t.Setenv("CLAVIGER_KEK_FILE", "")
	t.Setenv("CLAVIGER_KEK_OLD", "")

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	usePostgresOfApp(t, app)

	col := core.NewBaseCollection("cliPeople")
	col.Fields.Add(&core.TextField{Name: "name"}, &core.EncryptedTextField{Name: "phone"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		r := core.NewRecord(col)
		r.Set("name", "n")
		r.Set("phone", "555-"+strings.Repeat("1", i+1))
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	return app, col
}

func activeKeyId(t testing.TB, app core.App) string {
	t.Helper()
	var id string
	err := app.DB().Select("keyId").From(core.CollectionNameFieldKeys).Where(dbx.HashExp{"retired": ""}).Row(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestKeysRotateRefusesWhileAServerOwnsTheDirectory(t *testing.T) {
	app, _ := keysApp(t)
	before := activeKeyId(t, app)

	// a running replica: the whole process tree holds a flock on the lock file
	lockPath := filepath.Join(app.DataDir(), ".pb_singlewriter.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"rotate"}, {"rewrap"}, {"convert", "--collection", "cliPeople"}, {"strict", "--collection", "cliPeople", "--field", "phone"}, {"prune"}, {"status"}} {
		out, err := runKeys(t, app, args...)
		if err == nil || !strings.Contains(err.Error(), "refusing to run") || !strings.Contains(err.Error(), "--offline-ok") || !strings.Contains(err.Error(), "single-writer lock") {
			t.Fatalf("%v: expected a refusal, got %v\n%s", args, err, out)
		}
	}
	if activeKeyId(t, app) != before {
		t.Fatal("a refused command must not change anything")
	}

	// the operator's explicit statement overrides it
	out, err := runKeys(t, app, "rotate", "--offline-ok")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if activeKeyId(t, app) == before {
		t.Fatal("--offline-ok must run the rotation")
	}

	// released: no flag needed
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if out, err := runKeys(t, app, "rotate"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestKeysCommandsRefuseDuringAHandover(t *testing.T) {
	app, _ := keysApp(t)
	if err := os.WriteFile(filepath.Join(app.DataDir(), ".pb_handover"), []byte("incoming-replica\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runKeys(t, app, "rotate"); err == nil || !strings.Contains(err.Error(), "handover") {
		t.Fatalf("expected a refusal naming the handover, got %v", err)
	}
	if _, err := runKeys(t, app, "rotate", "--offline-ok"); err != nil {
		t.Fatal(err)
	}
}

func TestKeysCommandsEndToEnd(t *testing.T) {
	app, col := keysApp(t)
	before := activeKeyId(t, app)

	out, err := runKeys(t, app, "status")
	if err != nil || !strings.Contains(out, "cliPeople.phone") || !strings.Contains(out, "12 on the current key") {
		t.Fatalf("status: %v\n%s", err, out)
	}

	out, err = runKeys(t, app, "rotate", "--collection", "cliPeople", "--field", "phone", "--batch-size", "5")
	if err != nil || !strings.Contains(out, "12 re-encrypted") || !strings.Contains(out, "12/12") {
		t.Fatalf("rotate: %v\n%s", err, out)
	}
	after := activeKeyId(t, app)
	if after == before {
		t.Fatal("the key must advance")
	}

	if _, err := runKeys(t, app, "rotate", "--new-kek", "--collection", "cliPeople"); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("expected --new-kek to reject a filter, got %v", err)
	}
	out, err = runKeys(t, app, "rotate", "--new-kek")
	if err != nil || !strings.Contains(out, "0 data key(s) re-wrapped") {
		t.Fatalf("rotate --new-kek: %v\n%s", err, out)
	}
	if out, err = runKeys(t, app, "rewrap"); err != nil || !strings.Contains(out, "2 already used it") {
		t.Fatalf("rewrap: %v\n%s", err, out)
	}

	// legacy value -> convert -> strict
	rows, _ := app.FindAllRecords(col)
	if _, err := app.DB().NewQuery("UPDATE {{cliPeople}} SET phone='legacy' WHERE id={:id}").Bind(dbx.Params{"id": rows[0].Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := runKeys(t, app, "strict", "--collection", "cliPeople", "--field", "phone"); err == nil || !strings.Contains(err.Error(), "keys convert") {
		t.Fatalf("strict must point at convert, got %v", err)
	}
	if out, err = runKeys(t, app, "convert", "--collection", "cliPeople", "--field", "phone"); err != nil || !strings.Contains(out, "1 converted") {
		t.Fatalf("convert: %v\n%s", err, out)
	}
	if out, err = runKeys(t, app, "strict", "--collection", "cliPeople", "--field", "phone"); err != nil || !strings.Contains(out, "strict mode on") {
		t.Fatalf("strict: %v\n%s", err, out)
	}
	if out, err = runKeys(t, app, "strict", "--collection", "cliPeople", "--field", "phone", "--off"); err != nil || !strings.Contains(out, "strict mode off") {
		t.Fatalf("strict off: %v\n%s", err, out)
	}
	if _, err := runKeys(t, app, "convert"); err == nil {
		t.Fatal("convert needs a collection")
	}

	// the old key is unused: prune removes it, a second prune has nothing to do
	if out, err = runKeys(t, app, "prune"); err != nil || !strings.Contains(out, "pruned 1 key(s)") || !strings.Contains(out, before) {
		t.Fatalf("prune: %v\n%s", err, out)
	}
	if out, err = runKeys(t, app, "prune"); err != nil || !strings.Contains(out, "pruned 0 key(s)") {
		t.Fatalf("prune again: %v\n%s", err, out)
	}

	// one audit entry per run, system actor
	entries, _ := app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": core.AuditActionFieldKeyRotate})
	if len(entries) != 1 || entries[0].GetString("actorType") != core.AuditActorTypeSystem {
		t.Fatalf("expected one system rotation entry: %v", entries)
	}
}
