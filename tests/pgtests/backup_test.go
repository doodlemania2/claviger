//go:build postgres

package pgtests

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
)

// TestBackupsOnPostgres: a zip backup holds the files but not the database,
// says so in its manifest, its verification is skipped (not failed) and a zip
// restore is refused. The database is backed up with pg_dump (docs/datastores.md).
func TestBackupsOnPostgres(t *testing.T) {
	app := newApp(t)
	seedRecords(t, app)

	// a storage file
	storageFile := filepath.Join(app.DataDir(), "storage", "col", "rec", "doc.txt")
	if err := os.MkdirAll(filepath.Dir(storageFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storageFile, []byte("file body"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := app.CreateBackup(context.Background(), "pg_test.zip"); err != nil {
		t.Fatalf("create backup: %v", err)
	}

	zr, err := zip.OpenReader(filepath.Join(app.DataDir(), "backups", "pg_test.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	names := map[string]*zip.File{}
	for _, f := range zr.File {
		names[f.Name] = f
	}
	if names["data.db"] != nil {
		t.Fatal("a Postgres backup must not contain data.db")
	}
	if names["storage/col/rec/doc.txt"] == nil {
		t.Fatalf("the storage files must be in the archive: %v", keys(names))
	}
	if names["auxiliary.db"] == nil {
		t.Fatalf("the local auxiliary database is still archived: %v", keys(names))
	}
	if names[core.BackupManifestName] == nil {
		t.Fatalf("missing the manifest: %v", keys(names))
	}

	rc, err := names[core.BackupManifestName].Open()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(rc)
	rc.Close()

	var manifest core.BackupManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.DataDatabase != "postgres" || !strings.Contains(manifest.Note, "pg_dump") {
		t.Fatalf("the manifest must say that the database is not in the archive: %s", raw)
	}
	if manifest.Tables["data.db"] != nil {
		t.Fatal("no data.db row counts expected")
	}

	// verification: skipped, with the reason, not failed
	res, err := core.VerifyBackup(context.Background(), app, "pg_test.zip")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != core.BackupVerifyStatusSkipped || !strings.Contains(res.Error, "skipped: database is Postgres") {
		t.Fatalf("expected a skipped verification, got %+v", res)
	}

	// the zip restore is refused and changes nothing
	err = app.RestoreBackup(context.Background(), "pg_test.zip")
	if err == nil || !strings.Contains(err.Error(), "pg_restore") {
		t.Fatalf("expected the restore to be refused, got %v", err)
	}
	if _, err := os.Stat(storageFile); err != nil {
		t.Fatalf("the refused restore touched pb_data: %v", err)
	}

	// the API: the restore is a 400, and the health endpoint names the database for the admin UI
	c := newAPIClient(t, app)
	if r := c.do("POST", "/api/backups/pg_test.zip/restore", "super", nil); r.Status != 400 {
		t.Fatalf("restore: %d %v", r.Status, r.Body)
	}
	r := c.do("GET", "/api/health", "super", nil)
	if got := fmt.Sprint(r.Body.(map[string]any)["data"].(map[string]any)["database"]); got != "postgres" {
		t.Fatalf("health database = %q", got)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestLogsStayInTheLocalAuxiliaryDatabase: request logs are written to, queried
// from and deleted from the local SQLite auxiliary.db, never the Postgres data database.
func TestLogsStayInTheLocalAuxiliaryDatabase(t *testing.T) {
	app := newApp(t)
	c := newAPIClient(t, app)

	if app.HasTable("_logs") {
		t.Fatal("the Postgres data database must not have a _logs table")
	}

	log := &core.Log{}
	log.Id = "pglogtest000001"
	log.Level = 1
	log.Message = "hello from pg test"
	if err := app.AuxSave(log); err != nil {
		t.Fatalf("save log: %v", err)
	}

	r := c.do("GET", "/api/logs?filter=message%7E%27pg+test%27", "super", nil)
	if r.Status != 200 || len(r.Body.(map[string]any)["items"].([]any)) != 1 {
		t.Fatalf("list logs: %d %v", r.Status, r.Body)
	}
	if r := c.do("GET", "/api/logs/stats", "super", nil); r.Status != 200 || len(r.Body.([]any)) == 0 {
		t.Fatalf("logs stats: %d %v", r.Status, r.Body)
	}
	if r := c.do("GET", "/api/logs/"+log.Id, "super", nil); r.Status != 200 {
		t.Fatalf("view log: %d %v", r.Status, r.Body)
	}

	var n int
	if err := app.AuxConcurrentDB().NewQuery("SELECT count(*) FROM _logs").Row(&n); err != nil || n != 1 {
		t.Fatalf("expected the log in auxiliary.db: %d, %v", n, err)
	}
	if err := app.DB().NewQuery("SELECT count(*) FROM _logs").Row(&n); err == nil {
		t.Fatal("the data database must not have _logs")
	}

	if err := app.DeleteOldLogs(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_ = app.AuxConcurrentDB().NewQuery("SELECT count(*) FROM _logs").Row(&n)
	if n != 0 {
		t.Fatalf("old logs were not deleted: %d", n)
	}
}
