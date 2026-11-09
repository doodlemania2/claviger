package core_test

import (
	"archive/zip"
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/backupenc"
	"github.com/doodlemania2/claviger/tools/dialect"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/keys"
)

// not parallel: the key source comes from the process environment

func randomKEK(t testing.TB) string {
	t.Helper()
	b := make([]byte, keys.KEKLength)
	if _, err := crand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// useBackupKEK points the app at the given KEK (and optionally a previous one).
func useBackupKEK(t testing.TB, app core.App, kek, old string) {
	t.Helper()
	t.Setenv("CLAVIGER_KEK", kek)
	t.Setenv("CLAVIGER_KEK_FILE", "")
	t.Setenv("CLAVIGER_KEK_OLD", old)
	t.Setenv("CLAVIGER_KEK_FILE_OLD", "")
	t.Setenv("CLAVIGER_KEK_OLD_FROM_APP_KEY", "")
	// the app caches its key source
	app.Store().Remove("__clavigerFieldKeySource__")
}

func requireSQLite(t testing.TB, app core.App) {
	t.Helper()
	if core.DialectOf(app).Name() != dialect.SQLiteName {
		t.Skip("needs the data.db of a SQLite archive (Postgres archives are covered by tests/pgtests TestBackupsOnPostgres)")
	}
}

func readFile(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// zipEntryNames decrypts (if needed) and lists the archive entries.
func zipEntryNames(t testing.TB, app core.App, raw []byte, ks keys.KeySource) []string {
	t.Helper()
	if backupenc.IsEncrypted(raw) {
		var out bytes.Buffer
		if _, err := backupenc.Decrypt(context.Background(), &out, bytes.NewReader(raw), ks); err != nil {
			t.Fatal(err)
		}
		raw = out.Bytes()
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	return names
}

func loadTestKeySource(t testing.TB, getenv func(string) string) keys.KeySource {
	t.Helper()
	ks, err := keys.LoadWithOld(getenv, "")
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

func TestEncryptedBackupCreate(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	kek := randomKEK(t)
	useBackupKEK(t, app, kek, "")
	ks := loadTestKeySource(t, func(k string) string {
		if k == "CLAVIGER_KEK" {
			return kek
		}
		return ""
	})

	app.Settings().Backups.Encrypt = true
	app.Settings().Meta.AppName = "church"

	if err := app.CreateBackup(context.Background(), "named.zip"); err != nil {
		t.Fatal(err)
	}
	if err := app.CreateBackup(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	// replacing keeps the name
	if err := app.CreateBackup(context.Background(), "named.zip"); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(app.DataDir(), core.LocalBackupsDirName)
	entries, _ := os.ReadDir(dir)

	var archives []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".attrs") {
			archives = append(archives, e.Name())
		}
	}
	sort.Strings(archives)

	if len(archives) != 2 || archives[0] != "named.zip.enc" || !strings.HasPrefix(archives[1], "pb_backup_church_") || !strings.HasSuffix(archives[1], ".zip.enc") {
		t.Fatalf("Unexpected backup names: %v", archives)
	}

	for _, name := range archives {
		raw := readFile(t, filepath.Join(dir, name))
		if !backupenc.IsEncrypted(raw) || bytes.HasPrefix(raw, []byte("PK")) {
			t.Fatalf("%s: expected an encrypted archive", name)
		}
		if bytes.Contains(raw, []byte("auxiliary.db")) {
			t.Fatalf("%s: the zip entry names must not be readable", name)
		}

		names := zipEntryNames(t, app, raw, ks)
		for _, want := range []string{"auxiliary.db", core.BackupManifestName} {
			if !contains(names, want) {
				t.Fatalf("%s: missing %q in %v", name, want, names)
			}
		}
	}

	if core.IsVerifyingBackup(app) != "" || app.Store().Has(core.StoreKeyActiveBackup) {
		t.Fatal("expected the active backup marker to be cleared")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestEncryptedBackupOffIsUnchanged(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	// no key configured: a plain backup must not need one
	useBackupKEK(t, app, "", "")

	if err := app.CreateBackup(context.Background(), "plain.zip"); err != nil {
		t.Fatal(err)
	}
	if err := app.CreateBackup(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(app.DataDir(), core.LocalBackupsDirName)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".attrs") {
			continue
		}
		if strings.HasSuffix(e.Name(), ".enc") {
			t.Fatalf("unexpected encrypted name %q", e.Name())
		}
		raw := readFile(t, filepath.Join(dir, e.Name()))
		if !bytes.HasPrefix(raw, []byte("PK")) {
			t.Fatalf("%s: expected a plain zip", e.Name())
		}
	}

	if got := core.BackupStoredName(app, "x.zip"); got != "x.zip" {
		t.Fatalf("expected an unchanged name, got %q", got)
	}
}

func TestEncryptedBackupNoKey(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	useBackupKEK(t, app, "", "")
	// ensure the PB_ENCRYPTION_KEY fallback is not set either
	t.Setenv(app.EncryptionEnv(), "")

	app.Settings().Backups.Encrypt = true

	err := app.CreateBackup(context.Background(), "nokey.zip")
	if err == nil || !errors.Is(err, keys.ErrNoKeySource) || !strings.Contains(err.Error(), "CLAVIGER_KEK") {
		t.Fatalf("Expected a clear missing key error, got %v", err)
	}

	entries, _ := os.ReadDir(filepath.Join(app.DataDir(), core.LocalBackupsDirName))
	if len(entries) != 0 {
		t.Fatalf("Expected no (partial) backup file, got %v", entries)
	}
	if app.Store().Has(core.StoreKeyActiveBackup) {
		t.Fatal("expected the active backup marker to be cleared")
	}
}

func TestEncryptedBackupVerify(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()
	requireSQLite(t, app)

	kek := randomKEK(t)
	useBackupKEK(t, app, kek, "")

	// a plain one first, then enable the encryption
	if err := app.CreateBackup(context.Background(), "old_plain.zip"); err != nil {
		t.Fatal(err)
	}
	app.Settings().Backups.Encrypt = true
	if err := app.CreateBackup(context.Background(), "new.zip"); err != nil {
		t.Fatal(err)
	}

	for name, wantEnc := range map[string]bool{"old_plain.zip": false, "new.zip.enc": true} {
		res, err := core.VerifyBackup(context.Background(), app, name)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != core.BackupVerifyStatusVerified || !res.HasManifest || res.Tables == 0 || res.Encrypted != wantEnc {
			t.Fatalf("%s: unexpected result %+v", name, res)
		}
	}

	// plain archives verify without any key
	useBackupKEK(t, app, "", "")
	t.Setenv(app.EncryptionEnv(), "")
	if res, err := core.VerifyBackup(context.Background(), app, "old_plain.zip"); err != nil || res.Status != core.BackupVerifyStatusVerified {
		t.Fatalf("expected the plain backup to verify without a key, got %+v %v", res, err)
	}

	// no key at all
	res, err := core.VerifyBackup(context.Background(), app, "new.zip.enc")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != core.BackupVerifyStatusFailed || !res.Encrypted || !strings.Contains(res.Error, "encrypted") || !strings.Contains(res.Error, "CLAVIGER_KEK") {
		t.Fatalf("expected a clear missing key failure, got %+v", res)
	}

	// a wrong key
	useBackupKEK(t, app, randomKEK(t), "")
	res, _ = core.VerifyBackup(context.Background(), app, "new.zip.enc")
	if res.Status != core.BackupVerifyStatusFailed || !strings.Contains(res.Error, "different key-encryption key") || !strings.Contains(res.Error, "CLAVIGER_KEK_OLD") {
		t.Fatalf("expected a clear wrong key failure, got %+v", res)
	}

	// a rotated key with the previous one provided
	newKEK := randomKEK(t)
	useBackupKEK(t, app, newKEK, kek)
	res, _ = core.VerifyBackup(context.Background(), app, "new.zip.enc")
	if res.Status != core.BackupVerifyStatusVerified {
		t.Fatalf("expected the old backup to verify with the old key provided, got %+v", res)
	}

	// new backups use the new key
	if err := app.CreateBackup(context.Background(), "after_rotation.zip"); err != nil {
		t.Fatal(err)
	}
	h, err := backupenc.ReadHeader(bytes.NewReader(readFile(t, backupZipPath(app, "after_rotation.zip.enc"))))
	if err != nil {
		t.Fatal(err)
	}
	newSrc := loadTestKeySource(t, func(k string) string {
		if k == "CLAVIGER_KEK" {
			return newKEK
		}
		return ""
	})
	if h.KeyID != newSrc.KeyID() {
		t.Fatalf("expected the new backup to use the new key id %s, got %s", newSrc.KeyID(), h.KeyID)
	}

	// and the new key alone no longer opens the old backup
	useBackupKEK(t, app, newKEK, "")
	res, _ = core.VerifyBackup(context.Background(), app, "new.zip.enc")
	if res.Status != core.BackupVerifyStatusFailed {
		t.Fatalf("expected a failure without the old key, got %+v", res)
	}
	res, _ = core.VerifyBackup(context.Background(), app, "after_rotation.zip.enc")
	if res.Status != core.BackupVerifyStatusVerified {
		t.Fatalf("expected the new backup to verify, got %+v", res)
	}
}

func TestEncryptedBackupVerifyTamper(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()
	requireSQLite(t, app)

	useBackupKEK(t, app, randomKEK(t), "")
	app.Settings().Backups.Encrypt = true

	// incompressible content, so that the archive has several chunks
	writeRandomFile(t, filepath.Join(app.DataDir(), "storage", "filler.bin"), 300<<10)

	if err := app.CreateBackup(context.Background(), "t.zip"); err != nil {
		t.Fatal(err)
	}
	good := readFile(t, backupZipPath(app, "t.zip.enc"))
	chunk := backupenc.DefaultChunkSize + 16

	br := bytes.NewReader(good)
	if _, err := backupenc.ReadHeader(br); err != nil {
		t.Fatal(err)
	}
	hl := len(good) - br.Len()

	if chunks := (len(good) - hl) / chunk; chunks < 2 {
		t.Skipf("the archive has %d chunks, need at least 2 to reorder", chunks)
	}

	mutations := map[string]func([]byte) []byte{
		"flipped byte": func(b []byte) []byte { b[hl+10] ^= 1; return b },
		"truncated":    func(b []byte) []byte { return b[:len(b)-chunk] },
		"reordered": func(b []byte) []byte {
			c0 := bytes.Clone(b[hl : hl+chunk])
			copy(b[hl:], b[hl+chunk:hl+2*chunk])
			copy(b[hl+chunk:], c0)
			return b
		},
		"swapped header": func(b []byte) []byte {
			other := tamperOtherBackup(t, app)
			copy(b, other[:hl])
			return b
		},
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(backupZipPath(app, name+".zip.enc"), mutate(bytes.Clone(good)), 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := core.VerifyBackup(context.Background(), app, name+".zip.enc")
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != core.BackupVerifyStatusFailed {
				t.Fatalf("expected a failure, got %+v", res)
			}
		})
	}
}

func tamperOtherBackup(t testing.TB, app core.App) []byte {
	t.Helper()
	if err := app.CreateBackup(context.Background(), "other_source.zip"); err != nil {
		t.Fatal(err)
	}
	return readFile(t, backupZipPath(app, "other_source.zip.enc"))
}

// topLevelSnapshot describes pb_data in a way that notices a restore leaking into it.
func topLevelSnapshot(t testing.TB, app core.App) string {
	t.Helper()
	entries, err := os.ReadDir(app.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		switch e.Name() {
		case core.LocalBackupsDirName, core.LocalTempDirName:
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	snap := strings.Join(names, ",")
	if b, err := os.ReadFile(filepath.Join(app.DataDir(), "marker.txt")); err == nil {
		snap += "|marker=" + string(b)
	}

	return snap
}

// movedDirs counts the "old_pb_data_*" scratch dirs, which exist only once a
// restore started to replace pb_data.
func movedDirs(t testing.TB, app core.App) int {
	t.Helper()
	tmp, _ := os.ReadDir(filepath.Join(app.DataDir(), core.LocalTempDirName))
	n := 0
	for _, e := range tmp {
		if strings.HasPrefix(e.Name(), "old_pb_data_") {
			n++
		}
	}
	return n
}

// restoreUntilRestart runs RestoreBackup with the process restart intercepted.
//
// Note: a restore moves pb_data/.notify, which wakes the notify watcher
// goroutine of the app; under -race that goroutine's IsBootstrapped() read is
// reported against app.Cleanup() (a pre-existing race in core/notify_watcher.go,
// like the flaky TestNotifyWatcher_* tests). CI does not run -race.
// It returns what pb_data looked like at the moment the restart would happen.
func restoreUntilRestart(t testing.TB, app *tests.TestApp, name string) (snapshotAtRestart string, restoreErr error) {
	t.Helper()

	errStop := errors.New("restart intercepted")
	reached := false

	const hookId = "__testInterceptRestart__"
	app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id:       hookId,
		Priority: -1000,
		Func: func(e *core.TerminateEvent) error {
			if e.IsRestart {
				reached = true
				snapshotAtRestart = topLevelSnapshot(t, app)
				return errStop // the data dir changes are reverted
			}
			return e.Next()
		},
	})
	defer app.OnTerminate().Unbind(hookId)

	restoreErr = app.RestoreBackup(context.Background(), name)
	if reached && !errors.Is(restoreErr, errStop) {
		t.Fatalf("expected the intercepted restart error, got %v", restoreErr)
	}
	if !reached {
		snapshotAtRestart = ""
	}

	return snapshotAtRestart, restoreErr
}

func TestEncryptedBackupRestore(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()
	requireSQLite(t, app)

	kek := randomKEK(t)
	useBackupKEK(t, app, kek, "")
	app.Settings().Backups.Encrypt = true

	marker := filepath.Join(app.DataDir(), "marker.txt")
	if err := os.WriteFile(marker, []byte("from the backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.CreateBackup(context.Background(), "r.zip"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("current data"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := topLevelSnapshot(t, app)
	var movedBefore int

	t.Run("restores the decrypted content", func(t *testing.T) {
		atRestart, err := restoreUntilRestart(t, app, "r.zip.enc")
		if !strings.Contains(atRestart, "marker=from the backup") {
			t.Fatalf("expected pb_data to hold the backup content at restart, got %q (err %v)", atRestart, err)
		}
		if after := topLevelSnapshot(t, app); after != before {
			t.Fatalf("expected pb_data to be reverted after the intercepted restart:\n%s\n%s", before, after)
		}
	})

	// the intercepted restore above leaves its (empty) scratch dir behind, as a failed restart does
	movedBefore = movedDirs(t, app)

	t.Run("wrong key leaves pb_data untouched", func(t *testing.T) {
		useBackupKEK(t, app, randomKEK(t), "")
		atRestart, err := restoreUntilRestart(t, app, "r.zip.enc")
		if err == nil || atRestart != "" || !strings.Contains(err.Error(), "different key-encryption key") {
			t.Fatalf("expected a clear wrong key error before touching pb_data, got %q %v", atRestart, err)
		}
		if after := topLevelSnapshot(t, app); after != before || movedDirs(t, app) != movedBefore {
			t.Fatalf("pb_data changed:\n%s\n%s", before, after)
		}
	})

	t.Run("missing key leaves pb_data untouched", func(t *testing.T) {
		useBackupKEK(t, app, "", "")
		t.Setenv(app.EncryptionEnv(), "")
		atRestart, err := restoreUntilRestart(t, app, "r.zip.enc")
		if err == nil || atRestart != "" || !strings.Contains(err.Error(), "CLAVIGER_KEK") {
			t.Fatalf("expected a clear missing key error, got %q %v", atRestart, err)
		}
		if after := topLevelSnapshot(t, app); after != before || movedDirs(t, app) != movedBefore {
			t.Fatalf("pb_data changed:\n%s\n%s", before, after)
		}
	})

	t.Run("tampered backup leaves pb_data untouched", func(t *testing.T) {
		useBackupKEK(t, app, kek, "")
		good := readFile(t, backupZipPath(app, "r.zip.enc"))
		for name, mutate := range map[string]func([]byte) []byte{
			"flip":     func(b []byte) []byte { b[len(b)/2] ^= 1; return b },
			"truncate": func(b []byte) []byte { return b[:len(b)-100] },
		} {
			if err := os.WriteFile(backupZipPath(app, name+".zip.enc"), mutate(bytes.Clone(good)), 0o644); err != nil {
				t.Fatal(err)
			}
			atRestart, err := restoreUntilRestart(t, app, name+".zip.enc")
			if err == nil || atRestart != "" || !errors.Is(err, backupenc.ErrCorrupt) {
				t.Fatalf("%s: expected a corruption error before touching pb_data, got %q %v", name, atRestart, err)
			}
			if after := topLevelSnapshot(t, app); after != before || movedDirs(t, app) != movedBefore {
				t.Fatalf("%s: pb_data changed:\n%s\n%s", name, before, after)
			}
		}
	})

	t.Run("rotated key with the old one provided", func(t *testing.T) {
		useBackupKEK(t, app, randomKEK(t), kek)
		atRestart, err := restoreUntilRestart(t, app, "r.zip.enc")
		if !strings.Contains(atRestart, "marker=from the backup") {
			t.Fatalf("expected the restore to work, got %q (err %v)", atRestart, err)
		}
	})

	t.Run("plain backups restore after enabling the encryption", func(t *testing.T) {
		useBackupKEK(t, app, "", "")
		t.Setenv(app.EncryptionEnv(), "")
		app.Settings().Backups.Encrypt = false
		if err := os.WriteFile(marker, []byte("from the plain backup"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := app.CreateBackup(context.Background(), "p.zip"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("current data"), 0o644); err != nil {
			t.Fatal(err)
		}

		app.Settings().Backups.Encrypt = true
		atRestart, err := restoreUntilRestart(t, app, "p.zip")
		if !strings.Contains(atRestart, "marker=from the plain backup") {
			t.Fatalf("expected the plain restore to work, got %q (err %v)", atRestart, err)
		}
	})
}

func TestEncryptedAutoBackup(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	useBackupKEK(t, app, randomKEK(t), "")

	app.Settings().Backups.Cron = "0 0 * * *"
	app.Settings().Backups.CronMaxKeep = 3
	app.Settings().Backups.Encrypt = true
	if err := app.Save(app.Settings()); err != nil {
		t.Fatal(err)
	}
	if err := app.OnSettingsReload().Trigger(&core.SettingsReloadEvent{App: app}); err != nil {
		t.Fatal(err)
	}

	runAutoBackupJob(t, app)

	entries, _ := os.ReadDir(filepath.Join(app.DataDir(), core.LocalBackupsDirName))
	var names []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".attrs") {
			names = append(names, e.Name())
		}
	}
	if len(names) != 1 || !strings.HasPrefix(names[0], "@auto_pb_backup_") || !strings.HasSuffix(names[0], ".zip.enc") {
		t.Fatalf("Expected one encrypted auto backup, got %v", names)
	}

	saved := waitForAutoVerification(t, app, true)
	v := saved[names[0]]
	if v == nil {
		t.Fatalf("expected a verification result for %s, got %+v", names[0], saved)
	}
	if core.DialectOf(app).Name() == dialect.SQLiteName && (v.Status != core.BackupVerifyStatusVerified || !v.Encrypted) {
		t.Fatalf("unexpected verification %+v", v)
	}
}

// -------------------------------------------------------------------

// fakeS3 is a minimal in-memory S3 (path style) for the backups filesystem.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	mod     map[string]time.Time
}

func newFakeS3(t testing.TB) (*fakeS3, *httptest.Server) {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}, mod: map[string]time.Time{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/test_bucket"), "/")

	switch {
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.objects[key] = body
		f.mod[key] = time.Now()
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodHead || (r.Method == http.MethodGet && key != ""):
		body, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Header().Set("Last-Modified", f.mod[key].UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", `"x"`)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	default: // list
		prefix := r.URL.Query().Get("prefix")
		type content struct {
			Key          string    `xml:"Key"`
			LastModified time.Time `xml:"LastModified"`
			Size         int       `xml:"Size"`
		}
		res := struct {
			XMLName  xml.Name  `xml:"ListBucketResult"`
			Contents []content `xml:"Contents"`
		}{}
		for k, v := range f.objects {
			if strings.HasPrefix(k, prefix) {
				res.Contents = append(res.Contents, content{k, f.mod[k], len(v)})
			}
		}
		_ = xml.NewEncoder(w).Encode(res)
	}
}

func TestEncryptedBackupS3(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	store, srv := newFakeS3(t)

	useBackupKEK(t, app, randomKEK(t), "")
	app.Settings().Backups.Encrypt = true
	app.Settings().Backups.S3 = core.S3Config{
		Enabled:        true,
		Bucket:         "test_bucket",
		Region:         "test_region",
		Endpoint:       srv.URL,
		AccessKey:      "key",
		Secret:         "secret",
		ForcePathStyle: true,
	}

	if err := app.CreateBackup(context.Background(), "s3.zip"); err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	raw := store.objects["s3.zip.enc"]
	n := len(store.objects)
	store.mu.Unlock()

	if n != 1 || !backupenc.IsEncrypted(raw) {
		t.Fatalf("expected one encrypted object, got %d (encrypted=%v)", n, backupenc.IsEncrypted(raw))
	}

	fsys, err := app.NewBackupsFilesystem()
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	list, err := fsys.List("")
	if err != nil || len(list) != 1 || list[0].Key != "s3.zip.enc" {
		t.Fatalf("unexpected list %v %v", list, err)
	}

	if core.DialectOf(app).Name() != dialect.SQLiteName {
		return
	}

	res, err := core.VerifyBackup(context.Background(), app, "s3.zip.enc")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != core.BackupVerifyStatusVerified || !res.Encrypted {
		t.Fatalf("expected a verified encrypted S3 backup, got %+v", res)
	}

	// a tampered object fails
	store.mu.Lock()
	bad := bytes.Clone(raw)
	bad[len(bad)/2] ^= 1
	store.objects["bad.zip.enc"] = bad
	store.mod["bad.zip.enc"] = time.Now()
	store.mu.Unlock()

	res, err = core.VerifyBackup(context.Background(), app, "bad.zip.enc")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != core.BackupVerifyStatusFailed {
		t.Fatalf("expected a failure for the tampered S3 object, got %+v", res)
	}

	// restore from S3
	atRestart, err := restoreUntilRestart(t, app, "s3.zip.enc")
	if atRestart == "" {
		t.Fatalf("expected the S3 restore to reach the restart, got %v", err)
	}
}

func writeRandomFile(t testing.TB, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := io.CopyN(f, crand.Reader, int64(size)); err != nil {
		t.Fatal(err)
	}
}

// a large archive is streamed: the heap stays far below its size
func TestEncryptedBackupLargeFileStreams(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	useBackupKEK(t, app, randomKEK(t), "")
	app.Settings().Backups.Encrypt = true

	const size = 96 << 20
	writeRandomFile(t, filepath.Join(app.DataDir(), "storage", "big.bin"), size)

	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	check := func(what string) {
		if grow := int64(peak.Load()) - int64(base.HeapAlloc); grow > 40<<20 {
			t.Fatalf("%s: the heap grew by %d bytes for a %d byte file", what, grow, size)
		}
	}

	if err := app.CreateBackup(context.Background(), "big.zip"); err != nil {
		t.Fatal(err)
	}
	check("create")

	if info, err := os.Stat(backupZipPath(app, "big.zip.enc")); err != nil || info.Size() < size {
		t.Fatalf("expected an archive of at least %d bytes, got %v %v", size, info, err)
	}

	if core.DialectOf(app).Name() == dialect.SQLiteName {
		res, err := core.VerifyBackup(context.Background(), app, "big.zip.enc")
		if err != nil || res.Status != core.BackupVerifyStatusVerified {
			t.Fatalf("unexpected verification %+v %v", res, err)
		}
		check("verify")

		if _, err := restoreUntilRestart(t, app, "big.zip.enc"); err != nil && !strings.Contains(err.Error(), "restart intercepted") {
			t.Fatal(err)
		}
		check("restore")
	}

	close(stop)
	<-done
}
