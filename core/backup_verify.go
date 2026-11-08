package core

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dialect"
	"github.com/doodlemania2/claviger/tools/types"
)

// Claviger fork-local: automatic restore verification of backups.
//
// A verification extracts ONLY the database files of a backup archive into a
// private temp directory (never pb_data), runs PRAGMA integrity_check and
// PRAGMA foreign_key_check on them and compares the per-table row counts
// with the manifest that was written in the archive at backup time.

const (
	// BackupManifestName is the name of the small JSON file added to every
	// new backup archive with the per-table row counts at backup time.
	//
	// Older binaries restore archives containing it without problems
	// (archive.Extract extracts every regular file and the restore moves the
	// extracted content as is; the file just ends up as an unused file in
	// pb_data, and new backups explicitly skip it).
	BackupManifestName = ".claviger_backup_manifest.json"

	// StoreKeyActiveBackupVerify holds the key of the backup that is
	// currently being verified (if any).
	StoreKeyActiveBackupVerify = "@activeBackupVerify"

	storeKeyBackupVerifyLock = "@backupVerifyLock"

	backupVerifyTempPrefix = "claviger_backup_verify_"

	backupVerificationsTable = "_backupVerifications"

	// the max size of a manifest/db entry we are willing to read
	maxBackupManifestSize = 32 << 20
)

// Verification statuses.
const (
	BackupVerifyStatusVerified = "verified"
	BackupVerifyStatusFailed   = "failed"
	// skipped means that the verification could not be performed (eg. not
	// enough temp disk space) - it says nothing about the backup health.
	BackupVerifyStatusSkipped = "skipped"
)

// ErrBackupVerifyBusy is returned when another verification (or a
// backup/restore operation) is already in progress.
var ErrBackupVerifyBusy = errors.New("try again later - another backup verification or backup/restore operation is in progress")

// backupVerifyDBConnect opens the extracted databases.
// It is set to DefaultDBConnect by backup_verify_driver.go (unless the
// "no_default_driver" build tag is used).
var backupVerifyDBConnect DBConnectFunc

// ---------------------------------------------------------------

// BackupManifest is the content of BackupManifestName.
type BackupManifest struct {
	Version   int                         `json:"version"`
	CreatedAt string                      `json:"createdAt"`
	Tables    map[string]map[string]int64 `json:"tables"` // db filename -> table -> row count

	// DataDatabase is set when the archive has no copy of the app data database
	// (fork, #106): "postgres" means that the data lives in PostgreSQL, which a
	// zip backup does not contain (use pg_dump or the provider's point-in-time
	// recovery). The archive then holds the local files only (storage, the
	// auxiliary logs database).
	DataDatabase string `json:"dataDatabase,omitempty"`
	// Note explains DataDatabase to a human reading the archive.
	Note string `json:"note,omitempty"`
}

// backupVerifyPostgresSkip is the reason of the "skipped" verification of an
// archive without the data database because the app uses PostgreSQL.
const backupVerifyPostgresSkip = "skipped: database is Postgres (the archive holds the storage files only; back the database up with pg_dump or your provider's point-in-time recovery)"

func newBackupManifest() *BackupManifest {
	return &BackupManifest{
		Version:   1,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Tables:    map[string]map[string]int64{},
	}
}

func (m *BackupManifest) encode() []byte {
	raw, _ := json.Marshal(m)
	return raw
}

// addDatabase records the row counts of the (temp) database copy at dbPath.
// Failures are logged and skipped - the manifest is best effort.
func (m *BackupManifest) addDatabase(app App, name string, dbPath string) {
	if backupVerifyDBConnect == nil {
		return
	}

	counts, err := countDBRows(context.Background(), dbPath)
	if err != nil {
		app.Logger().Warn(
			"[Backup] failed to collect the manifest row counts",
			slog.String("db", name),
			slog.String("error", err.Error()),
		)
		return
	}

	m.Tables[name] = counts
}

func quoteSQLIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// countDBRows opens the database at dbPath and returns the row count of each table.
func countDBRows(ctx context.Context, dbPath string) (counts map[string]int64, err error) {
	db, err := backupVerifyDBConnect(dbPath)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, db.Close())
		// WAL leftovers of the temp copy
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	}()

	sdb := db.DB()

	rows, err := sdb.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return nil, err
		}
		tables = append(tables, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}

	counts = make(map[string]int64, len(tables))
	for _, t := range tables {
		var n int64
		if err := sdb.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteSQLIdent(t)).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %q: %w", t, err)
		}
		counts[t] = n
	}

	return counts, nil
}

// ---------------------------------------------------------------

// BackupVerification is the persisted result of a backup verification.
type BackupVerification struct {
	Key string `json:"key"`

	// Status is one of "verified", "failed" or "skipped".
	Status string `json:"status"`

	// Error is the failure/skip reason (empty when verified).
	Error string `json:"error"`

	// Message is an optional non-fatal note (eg. "no manifest").
	Message string `json:"message"`

	// HasManifest reports whether the archive had a row count manifest.
	HasManifest bool `json:"hasManifest"`

	Tables      int            `json:"tables"`
	Rows        int64          `json:"rows"`
	DurationMs  int64          `json:"durationMs"`
	CheckedAt   types.DateTime `json:"checkedAt"`
	ArchiveSize int64          `json:"archiveSize"`
}

// IsVerifyingBackup returns the key of the backup currently being verified (if any).
func IsVerifyingBackup(app App) string {
	v, _ := app.Store().Get(StoreKeyActiveBackupVerify).(string)
	return v
}

func backupVerifyLock(app App) *sync.Mutex {
	v := app.Store().GetOrSet(storeKeyBackupVerifyLock, func() any {
		return new(sync.Mutex)
	})
	return v.(*sync.Mutex)
}

// VerifyBackup restores the backup with the specified key in a temporary
// directory, checks the integrity of its databases and compares them with the
// archive manifest.
//
// The result is persisted (see LoadBackupVerifications) and logged.
// A non-nil error is returned only when the verification could not even be
// started (busy, missing backup, etc.); an unhealthy backup is reported as a
// result with Status "failed".
//
// It never writes in pb_data (except the small result row in auxiliary.db),
// and it is safe to call concurrently - only one verification runs at a time.
func VerifyBackup(ctx context.Context, app App, key string) (*BackupVerification, error) {
	if key == "" {
		return nil, errors.New("missing backup key")
	}

	if app.Store().Has(StoreKeyActiveBackup) {
		return nil, ErrBackupVerifyBusy
	}

	lock := backupVerifyLock(app)
	if !lock.TryLock() {
		return nil, ErrBackupVerifyBusy
	}
	defer lock.Unlock()

	app.Store().Set(StoreKeyActiveBackupVerify, key)
	defer app.Store().Remove(StoreKeyActiveBackupVerify)

	fsys, err := app.NewBackupsFilesystem()
	if err != nil {
		return nil, err
	}
	defer fsys.Close()
	fsys.SetContext(ctx)

	attrs, err := fsys.Attributes(key)
	if err != nil {
		return nil, fmt.Errorf("missing or invalid backup file %q: %w", key, err)
	}

	start := time.Now()

	result := &BackupVerification{
		Key:         key,
		ArchiveSize: attrs.Size,
	}

	getReader := func(k string) (io.ReadCloser, error) { return fsys.GetReader(k) }

	verifyErr, skipped := runBackupVerification(ctx, app, getReader, key, attrs.Size, result)

	result.DurationMs = time.Since(start).Milliseconds()
	result.CheckedAt, _ = types.ParseDateTime(time.Now())

	switch {
	case verifyErr == nil:
		result.Status = BackupVerifyStatusVerified
	case skipped:
		result.Status = BackupVerifyStatusSkipped
		result.Error = verifyErr.Error()
	default:
		result.Status = BackupVerifyStatusFailed
		result.Error = verifyErr.Error()
	}

	switch result.Status {
	case BackupVerifyStatusVerified:
		// Success heartbeat (mirrors the backup heartbeat): INFO would be
		// dropped by OTEL_MIN_LEVEL=WARN, so tag it to always be exported
		// and alert on the ABSENCE of this log line.
		app.Logger().Info(
			"[Backup verify] Backup verified",
			slog.String("name", key),
			slog.Bool("hasManifest", result.HasManifest),
			slog.Int("tables", result.Tables),
			slog.Int64("rows", result.Rows),
			slog.Int64("durationMs", result.DurationMs),
			slog.Bool(OTelAlwaysAttr, true),
		)
	case BackupVerifyStatusSkipped:
		logSkipped := app.Logger().Warn
		if result.Error == backupVerifyPostgresSkip {
			logSkipped = app.Logger().Info // expected on every Postgres install, not a warning
		}
		logSkipped(
			"[Backup verify] Backup verification skipped",
			slog.String("name", key),
			slog.String("error", result.Error),
		)
	default:
		app.Logger().Error(
			"[Backup verify] Backup verification FAILED",
			slog.String("name", key),
			slog.String("error", result.Error),
			slog.Int64("durationMs", result.DurationMs),
		)
	}

	if err := saveBackupVerification(app, result); err != nil {
		app.Logger().Warn(
			"[Backup verify] Failed to persist the verification result",
			slog.String("name", key),
			slog.String("error", err.Error()),
		)
	}

	return result, nil
}

// runBackupVerification does the actual work and fills the stats of result.
// The returned bool is true when the failure is a "skip" (not a backup problem).
func runBackupVerification(
	ctx context.Context,
	app App,
	getReader func(string) (io.ReadCloser, error),
	key string,
	archiveSize int64,
	result *BackupVerification,
) (err error, skipped bool) {
	if backupVerifyDBConnect == nil {
		return errors.New("backup verification is unavailable in builds without the default sqlite driver"), true
	}

	cleanupStaleVerifyTempDirs()

	tempDir, err := os.MkdirTemp("", backupVerifyTempPrefix)
	if err != nil {
		return fmt.Errorf("failed to create the temp dir: %w", err), true
	}
	defer removeAllRetry(app, tempDir)

	// disk space guard (the archive may be downloaded + the dbs extracted)
	if free, ferr := freeDiskSpace(tempDir); ferr == nil {
		if need := uint64(archiveSize) * 2; free < need {
			return fmt.Errorf(
				"not enough free space in the temp dir %q: %d bytes free, at least 2x the archive size (%d bytes) required",
				filepath.Dir(tempDir), free, need,
			), true
		}
	}

	var zipPath string
	if app.Settings().Backups.S3.Enabled {
		// download the archive first
		br, err := getReader(key)
		if err != nil {
			return fmt.Errorf("failed to download the backup: %w", err), false
		}
		defer br.Close()

		zipPath = filepath.Join(tempDir, "archive.zip")
		f, err := os.Create(zipPath)
		if err != nil {
			return err, true
		}
		_, err = io.Copy(f, &ctxReader{ctx: ctx, r: br})
		err = errors.Join(err, f.Close())
		if err != nil {
			return fmt.Errorf("failed to download the backup: %w", err), false
		}
	} else {
		zipPath = filepath.Join(app.DataDir(), LocalBackupsDirName, filepath.Base(key))
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("invalid or corrupted zip archive: %w", err), false
	}
	defer zr.Close()

	// locate the needed entries
	entries := map[string]*zip.File{}
	for _, f := range zr.File {
		switch f.Name {
		case dataDBFilename, auxDBFilename, BackupManifestName:
			entries[f.Name] = f
		}
	}

	if entries[dataDBFilename] == nil {
		// fork (#106): a Postgres install archives the files only
		if f := entries[BackupManifestName]; f != nil {
			if raw, rerr := readZipEntry(f, maxBackupManifestSize); rerr == nil {
				var m BackupManifest
				if json.Unmarshal(raw, &m) == nil && m.DataDatabase == dialect.PostgresName {
					result.HasManifest = true
					result.Message = "no data database in the archive"
					return errors.New(backupVerifyPostgresSkip), true
				}
			}
		}

		return fmt.Errorf("%s is missing from the archive", dataDBFilename), false
	}

	// second disk space guard, now that the real extracted size is known
	var extractSize uint64
	for _, name := range []string{dataDBFilename, auxDBFilename} {
		if f := entries[name]; f != nil {
			extractSize += f.UncompressedSize64
		}
	}
	if free, ferr := freeDiskSpace(tempDir); ferr == nil && free < extractSize {
		return fmt.Errorf(
			"not enough free space in the temp dir %q: %d bytes free, %d bytes required to extract the databases",
			filepath.Dir(tempDir), free, extractSize,
		), true
	}

	// manifest (optional)
	var manifest *BackupManifest
	if f := entries[BackupManifestName]; f != nil {
		raw, err := readZipEntry(f, maxBackupManifestSize)
		if err != nil {
			return fmt.Errorf("invalid manifest: %w", err), false
		}
		manifest = new(BackupManifest)
		if err := json.Unmarshal(raw, manifest); err != nil {
			return fmt.Errorf("invalid manifest: %w", err), false
		}
	}
	result.HasManifest = manifest != nil
	if manifest == nil {
		result.Message = "no manifest"
	}

	extractDir := filepath.Join(tempDir, "restored")
	if err := os.MkdirAll(extractDir, 0o700); err != nil {
		return err, true
	}

	var problems []string

	for _, name := range []string{dataDBFilename, auxDBFilename} {
		f := entries[name]
		if f == nil {
			if manifest != nil && manifest.Tables[name] != nil {
				problems = append(problems, name+": missing from the archive")
			}
			continue
		}

		dbPath := filepath.Join(extractDir, name)
		if err := extractZipEntry(f, dbPath); err != nil {
			problems = append(problems, name+": extraction failed: "+err.Error())
			continue
		}

		dbProblems, tables, rows := checkRestoredDB(ctx, dbPath, name, manifest)
		problems = append(problems, dbProblems...)
		result.Tables += tables
		result.Rows += rows
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; ")), false
	}

	return nil, false
}

// checkRestoredDB runs the integrity checks on the extracted db and compares
// the row counts with the manifest (if any).
func checkRestoredDB(ctx context.Context, dbPath, name string, manifest *BackupManifest) (problems []string, tables int, rows int64) {
	db, err := backupVerifyDBConnect(dbPath)
	if err != nil {
		return []string{name + ": failed to open: " + err.Error()}, 0, 0
	}
	defer db.Close()

	sdb := db.DB()

	// integrity_check
	irows, err := sdb.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return []string{name + ": integrity_check failed: " + err.Error()}, 0, 0
	}
	var msgs []string
	for irows.Next() {
		var msg string
		if err := irows.Scan(&msg); err != nil {
			msgs = append(msgs, err.Error())
			break
		}
		if msg != "ok" {
			msgs = append(msgs, msg)
		}
	}
	if err := errors.Join(irows.Err(), irows.Close()); err != nil {
		msgs = append(msgs, err.Error())
	}
	if len(msgs) > 0 {
		problems = append(problems, name+": integrity_check: "+summarize(msgs, 5))
	}

	// foreign_key_check
	frows, err := sdb.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		problems = append(problems, name+": foreign_key_check failed: "+err.Error())
	} else {
		var violations []string
		for frows.Next() {
			var (
				table  string
				rowid  sql.NullInt64
				parent string
				fkid   int64
			)
			if err := frows.Scan(&table, &rowid, &parent, &fkid); err != nil {
				violations = append(violations, err.Error())
				break
			}
			violations = append(violations, fmt.Sprintf("%s(rowid %d)->%s", table, rowid.Int64, parent))
		}
		if err := errors.Join(frows.Err(), frows.Close()); err != nil {
			violations = append(violations, err.Error())
		}
		if len(violations) > 0 {
			problems = append(problems, fmt.Sprintf("%s: foreign_key_check: %d violation(s): %s", name, len(violations), summarize(violations, 5)))
		}
	}

	// the dbs can't be counted reliably if they are malformed
	if len(problems) > 0 {
		return problems, 0, 0
	}

	counts, err := countDBRows(ctx, dbPath)
	if err != nil {
		return append(problems, name+": row count failed: "+err.Error()), 0, 0
	}

	for _, n := range counts {
		rows += n
	}
	tables = len(counts)

	if manifest != nil {
		if expected, ok := manifest.Tables[name]; ok {
			problems = append(problems, compareCounts(name, expected, counts)...)
		}
	}

	return problems, tables, rows
}

func compareCounts(dbName string, expected, actual map[string]int64) []string {
	var diffs []string

	names := make([]string, 0, len(expected)+len(actual))
	for t := range expected {
		names = append(names, t)
	}
	for t := range actual {
		if _, ok := expected[t]; !ok {
			names = append(names, t)
		}
	}
	sort.Strings(names)

	for _, t := range names {
		e, inExpected := expected[t]
		a, inActual := actual[t]
		switch {
		case !inActual:
			diffs = append(diffs, fmt.Sprintf("table %s missing (expected %d rows)", t, e))
		case !inExpected:
			diffs = append(diffs, fmt.Sprintf("unexpected table %s (%d rows)", t, a))
		case e != a:
			diffs = append(diffs, fmt.Sprintf("table %s has %d rows, expected %d", t, a, e))
		}
	}

	if len(diffs) == 0 {
		return nil
	}

	return []string{dbName + ": manifest mismatch: " + summarize(diffs, 5)}
}

func summarize(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s, ... (+%d more)", strings.Join(items[:max], ", "), len(items)-max)
}

// ---------------------------------------------------------------

func readZipEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	raw, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("entry too large")
	}

	return raw, nil
}

// extractZipEntry copies the zip entry into dest, verifying its CRC and size.
func extractZipEntry(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	// +1 to detect archives lying about the uncompressed size
	n, err := io.Copy(out, io.LimitReader(rc, int64(f.UncompressedSize64)+1))
	err = errors.Join(err, out.Close())
	if err != nil {
		return err
	}
	if uint64(n) != f.UncompressedSize64 {
		return fmt.Errorf("size mismatch: got %d bytes, expected %d", n, f.UncompressedSize64)
	}

	return nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// removeAllRetry removes dir with a few retries because on some
// filesystems (eg. NFS) the unlink of just closed files may lag.
func removeAllRetry(app App, dir string) {
	var err error
	for i := 0; i < 5; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(time.Duration(i+1) * 200 * time.Millisecond)
	}

	app.Logger().Warn(
		"[Backup verify] Failed to remove the temp dir (it will be retried on the next verification)",
		slog.String("dir", dir),
		slog.String("error", err.Error()),
	)
}

// cleanupStaleVerifyTempDirs removes leftovers of verifications that were
// interrupted (eg. process restart) more than an hour ago.
func cleanupStaleVerifyTempDirs() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), backupVerifyTempPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < time.Hour {
			continue
		}
		_ = os.RemoveAll(filepath.Join(os.TempDir(), e.Name()))
	}
}

// ---------------------------------------------------------------
// persistence
//
// The last result per backup key is stored in a tiny table in auxiliary.db
// (created lazily, so no system migration is needed). It was preferred over:
//   - a sidecar file in the backups filesystem: S3 listings would show it as
//     a backup and the local fs List/prune logic would have to learn about it;
//   - settings: results would be exposed/rewritten through the settings API.
//
// Trade-off: restoring an older backup replaces auxiliary.db, so results of
// backups newer than the restored one are forgotten (they show as "never").

func ensureBackupVerificationsTable(app App) error {
	_, err := app.AuxNonconcurrentDB().NewQuery(
		"CREATE TABLE IF NOT EXISTS {{" + backupVerificationsTable + "}} (" +
			"[[key]] TEXT PRIMARY KEY NOT NULL, " +
			"[[result]] TEXT NOT NULL, " +
			"[[checkedAt]] TEXT NOT NULL DEFAULT '')",
	).Execute()
	return err
}

func saveBackupVerification(app App, v *BackupVerification) error {
	if err := ensureBackupVerificationsTable(app); err != nil {
		return err
	}

	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}

	_, err = app.AuxNonconcurrentDB().NewQuery(
		"INSERT INTO {{" + backupVerificationsTable + "}} ([[key]], [[result]], [[checkedAt]]) " +
			"VALUES ({:key}, {:result}, {:checkedAt}) " +
			"ON CONFLICT([[key]]) DO UPDATE SET [[result]]=excluded.[[result]], [[checkedAt]]=excluded.[[checkedAt]]",
	).Bind(dbx.Params{
		"key":       v.Key,
		"result":    string(raw),
		"checkedAt": v.CheckedAt.String(),
	}).Execute()

	return err
}

// LoadBackupVerifications returns the last persisted verification result per backup key.
func LoadBackupVerifications(app App) (map[string]*BackupVerification, error) {
	if err := ensureBackupVerificationsTable(app); err != nil {
		return nil, err
	}

	var rows []struct {
		Key    string `db:"key"`
		Result string `db:"result"`
	}
	err := app.AuxConcurrentDB().NewQuery("SELECT [[key]], [[result]] FROM {{" + backupVerificationsTable + "}}").All(&rows)
	if err != nil {
		return nil, err
	}

	result := make(map[string]*BackupVerification, len(rows))
	for _, r := range rows {
		v := new(BackupVerification)
		if err := json.Unmarshal([]byte(r.Result), v); err == nil {
			result[r.Key] = v
		}
	}

	return result, nil
}

// DeleteBackupVerification removes the persisted result of a deleted backup (best effort).
func DeleteBackupVerification(app App, key string) {
	_, _ = app.AuxNonconcurrentDB().NewQuery(
		"DELETE FROM {{" + backupVerificationsTable + "}} WHERE [[key]]={:key}",
	).Bind(dbx.Params{"key": key}).Execute()
}

// ---------------------------------------------------------------

// startAutoBackupVerify runs the verification in the background (called
// after the backup lock was released) and alerts the superusers on failure.
func startAutoBackupVerify(app App, name string) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				app.Logger().Error("[Backup verify] panic", slog.Any("error", r))
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		result, err := VerifyBackup(ctx, app, name)
		if err != nil {
			app.Logger().Error(
				"[Backup verify] Failed to start the verification",
				slog.String("name", name),
				slog.String("error", err.Error()),
			)
			return
		}

		if result.Status == BackupVerifyStatusFailed {
			if alertErr := sendSystemAlertToAllSuperusers(
				app,
				"Backup verification failure",
				"The automated backup "+name+" failed its restore verification. Raw error:\n"+result.Error,
			); alertErr != nil {
				app.Logger().Warn(
					"[Backup verify] Failed to send the verification alerts",
					slog.String("name", name),
					slog.String("error", alertErr.Error()),
				)
			}
		}
	}()
}
