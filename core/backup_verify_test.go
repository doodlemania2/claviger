package core_test

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

func rewriteBackupZip(t *testing.T, src, dst string, mutate func(name string, content []byte) ([]byte, bool)) {
	t.Helper()

	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	zw := zip.NewWriter(out)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}

		content, keep := mutate(f.Name, content)
		if !keep {
			continue
		}

		w, err := zw.Create(f.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func backupZipPath(app core.App, name string) string {
	return filepath.Join(app.DataDir(), core.LocalBackupsDirName, name)
}

func countVerifyTempDirs(t *testing.T) int {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "claviger_backup_verify_") {
			n++
		}
	}
	return n
}

func TestVerifyBackup(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	if err := app.CreateBackup(context.Background(), "good.zip"); err != nil {
		t.Fatal(err)
	}

	// the manifest is part of the archive
	zr, err := zip.OpenReader(backupZipPath(app, "good.zip"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest core.BackupManifest
	for _, f := range zr.File {
		if f.Name == core.BackupManifestName {
			rc, _ := f.Open()
			raw, _ := io.ReadAll(rc)
			rc.Close()
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
		}
	}
	zr.Close()
	if len(manifest.Tables["data.db"]) == 0 || len(manifest.Tables["auxiliary.db"]) == 0 {
		t.Fatalf("Expected manifest tables for both dbs, got %v", manifest.Tables)
	}

	before := countVerifyTempDirs(t)

	t.Run("healthy backup", func(t *testing.T) {
		res, err := core.VerifyBackup(context.Background(), app, "good.zip")
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != core.BackupVerifyStatusVerified || !res.HasManifest || res.Tables == 0 || res.Rows == 0 {
			t.Fatalf("Expected verified with manifest, got %+v", res)
		}

		saved, err := core.LoadBackupVerifications(app)
		if err != nil {
			t.Fatal(err)
		}
		if saved["good.zip"] == nil || saved["good.zip"].Status != core.BackupVerifyStatusVerified {
			t.Fatalf("Expected persisted result, got %+v", saved)
		}
	})

	t.Run("no manifest", func(t *testing.T) {
		rewriteBackupZip(t, backupZipPath(app, "good.zip"), backupZipPath(app, "nomanifest.zip"),
			func(name string, c []byte) ([]byte, bool) { return c, name != core.BackupManifestName })

		res, err := core.VerifyBackup(context.Background(), app, "nomanifest.zip")
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != core.BackupVerifyStatusVerified || res.HasManifest || res.Message != "no manifest" {
			t.Fatalf("Expected verified/no manifest, got %+v", res)
		}
	})

	t.Run("truncated db", func(t *testing.T) {
		rewriteBackupZip(t, backupZipPath(app, "good.zip"), backupZipPath(app, "truncated.zip"),
			func(name string, c []byte) ([]byte, bool) {
				if name == "data.db" {
					return c[:len(c)/2], true
				}
				return c, true
			})

		res, err := core.VerifyBackup(context.Background(), app, "truncated.zip")
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != core.BackupVerifyStatusFailed || !strings.Contains(res.Error, "data.db") {
			t.Fatalf("Expected failed with a data.db reason, got %+v", res)
		}
	})

	t.Run("garbage db", func(t *testing.T) {
		rewriteBackupZip(t, backupZipPath(app, "good.zip"), backupZipPath(app, "garbage.zip"),
			func(name string, c []byte) ([]byte, bool) {
				if name == "auxiliary.db" {
					return []byte(strings.Repeat("not a database ", 500)), true
				}
				return c, true
			})

		res, _ := core.VerifyBackup(context.Background(), app, "garbage.zip")
		if res == nil || res.Status != core.BackupVerifyStatusFailed || !strings.Contains(res.Error, "auxiliary.db") {
			t.Fatalf("Expected failed with an auxiliary.db reason, got %+v", res)
		}
	})

	t.Run("manifest mismatch", func(t *testing.T) {
		rewriteBackupZip(t, backupZipPath(app, "good.zip"), backupZipPath(app, "mismatch.zip"),
			func(name string, c []byte) ([]byte, bool) {
				if name == core.BackupManifestName {
					m := manifest
					m.Tables = map[string]map[string]int64{"data.db": {}}
					for k, v := range manifest.Tables["data.db"] {
						m.Tables["data.db"][k] = v
					}
					m.Tables["data.db"]["_collections"] += 5
					m.Tables["data.db"]["ghost_table"] = 3
					raw, _ := json.Marshal(m)
					return raw, true
				}
				return c, true
			})

		res, _ := core.VerifyBackup(context.Background(), app, "mismatch.zip")
		if res == nil || res.Status != core.BackupVerifyStatusFailed ||
			!strings.Contains(res.Error, "manifest mismatch") ||
			!strings.Contains(res.Error, "_collections") ||
			!strings.Contains(res.Error, "ghost_table") {
			t.Fatalf("Expected manifest mismatch, got %+v", res)
		}
	})

	t.Run("not a zip", func(t *testing.T) {
		if err := os.WriteFile(backupZipPath(app, "bad.zip"), []byte("nope"), 0o644); err != nil {
			t.Fatal(err)
		}
		res, _ := core.VerifyBackup(context.Background(), app, "bad.zip")
		if res == nil || res.Status != core.BackupVerifyStatusFailed {
			t.Fatalf("Expected failed, got %+v", res)
		}
	})

	t.Run("missing backup", func(t *testing.T) {
		if _, err := core.VerifyBackup(context.Background(), app, "missing.zip"); err == nil {
			t.Fatal("Expected error")
		}
	})

	t.Run("busy", func(t *testing.T) {
		app.Store().Set(core.StoreKeyActiveBackup, "x")
		_, err := core.VerifyBackup(context.Background(), app, "good.zip")
		app.Store().Remove(core.StoreKeyActiveBackup)
		if err != core.ErrBackupVerifyBusy {
			t.Fatalf("Expected busy, got %v", err)
		}
	})

	// pb_data must not have been used as scratch space
	if _, err := os.Stat(filepath.Join(app.DataDir(), core.LocalTempDirName)); err == nil {
		entries, _ := os.ReadDir(filepath.Join(app.DataDir(), core.LocalTempDirName))
		if len(entries) != 0 {
			t.Fatalf("Expected empty pb_data temp dir, got %v", entries)
		}
	}

	// temp dirs are always cleaned up
	if after := countVerifyTempDirs(t); after != before {
		t.Fatalf("Expected %d leftover verify temp dirs, got %d", before, after)
	}
}

func TestRestoreIgnoresManifest(t *testing.T) {
	// a restored archive must not leave the manifest in pb_data
	// (the manifest is also excluded from new backups)
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	// simulate a stale manifest in pb_data - it must not end up in a new backup
	stale := filepath.Join(app.DataDir(), core.BackupManifestName)
	if err := os.WriteFile(stale, []byte(`{"stale":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.CreateBackup(context.Background(), "a.zip"); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.OpenReader(backupZipPath(app, "a.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	n := 0
	for _, f := range zr.File {
		if f.Name == core.BackupManifestName {
			n++
			rc, _ := f.Open()
			raw, _ := io.ReadAll(rc)
			rc.Close()
			if strings.Contains(string(raw), "stale") {
				t.Fatal("stale manifest copied into the backup")
			}
		}
	}
	if n != 1 {
		t.Fatalf("Expected exactly 1 manifest entry, got %d", n)
	}
}

func TestVerifyBackupConcurrent(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	if err := app.CreateBackup(context.Background(), "c.zip"); err != nil {
		t.Fatal(err)
	}

	const n = 6
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := core.VerifyBackup(context.Background(), app, "c.zip")
			errs <- err
		}()
	}

	ok := 0
	for i := 0; i < n; i++ {
		err := <-errs
		switch err {
		case nil:
			ok++
		case core.ErrBackupVerifyBusy:
		default:
			t.Fatal(err)
		}
	}
	if ok == 0 {
		t.Fatal("Expected at least one verification to run")
	}
	if core.IsVerifyingBackup(app) != "" {
		t.Fatal("Expected the active verification marker to be cleared")
	}
}

func runAutoBackupJob(t *testing.T, app *tests.TestApp) {
	t.Helper()
	for _, j := range app.Cron().Jobs() {
		if j.Id() == "__pbAutoBackup__" {
			j.Run()
			return
		}
	}
	t.Fatal("auto backup job not registered")
}

func waitForAutoVerification(t *testing.T, app *tests.TestApp, wantAny bool) map[string]*core.BackupVerification {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	if !wantAny {
		time.Sleep(2 * time.Second)
	}
	for {
		saved, err := core.LoadBackupVerifications(app)
		if err != nil {
			t.Fatal(err)
		}
		if len(saved) > 0 || !wantAny || time.Now().After(deadline) {
			return saved
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestAutoBackupVerify(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		app, _ := tests.NewTestApp()

		app.Settings().Backups.Cron = "0 0 * * *"
		app.Settings().Backups.CronMaxKeep = 3
		app.Settings().Backups.DisableVerify = disabled
		if err := app.Save(app.Settings()); err != nil {
			t.Fatal(err)
		}
		if err := app.OnSettingsReload().Trigger(&core.SettingsReloadEvent{App: app}); err != nil {
			t.Fatal(err)
		}

		runAutoBackupJob(t, app)

		saved := waitForAutoVerification(t, app, !disabled)
		if disabled && len(saved) != 0 {
			t.Fatalf("Expected no auto verification when disabled, got %+v", saved)
		}
		if !disabled {
			if len(saved) != 1 {
				t.Fatalf("Expected 1 auto verification, got %+v", saved)
			}
			for k, v := range saved {
				if !strings.HasPrefix(k, "@auto_pb_backup_") || v.Status != core.BackupVerifyStatusVerified {
					t.Fatalf("Unexpected auto verification %s: %+v", k, v)
				}
			}
		}

		app.Cleanup()
	}
}
