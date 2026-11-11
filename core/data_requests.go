package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/filesystem"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, 88-1): data subject requests and the export job.
//
// A request is a row of _dataRequests. RequestDataExport creates it (state
// queued) and starts the job. The job builds the ZIP (WriteDataExport), stores
// it as the row's file and sets the state to done (or partial when something
// could not be exported). One export runs at a time per process; further
// requests wait as queued rows. A row left running by a crash is queued again
// when the server starts. Several replicas (Postgres, #106 M7) may each run
// one export at a time: a claim is made with a compare-and-swap on the state.
//
// Audit entries (all on the _dataRequests target, none holding content):
//   - gdpr_export_request   a request was made
//   - gdpr_export           the job ended; meta has the categories and counts
//   - gdpr_export_download  the file was downloaded (and then deleted)
//
// "gdpr_export" is also delivered as the webhook event "gdpr.export".

const (
	CollectionNameDataRequests = "_dataRequests"

	DataRequestTypeExport = "export"
	DataRequestTypeErase  = "erase" // reserved for the erasure milestone

	DataRequestStateAwaitingConfirmation = "awaiting_confirmation" // erasure
	DataRequestStateGrace                = "grace"                 // erasure
	DataRequestStateQueued               = "queued"
	DataRequestStateRunning              = "running"
	DataRequestStateDone                 = "done"
	DataRequestStatePartial              = "partial"
	DataRequestStateFailed               = "failed"
	DataRequestStateCancelled            = "cancelled"
	DataRequestStateExpired              = "expired"

	AuditActionDataExportRequest  = "gdpr_export_request"
	AuditActionDataExport         = "gdpr_export"
	AuditActionDataExportDownload = "gdpr_export_download"

	// DataExportRetentionDaysEnv overrides how long a finished export can be
	// downloaded (default 7 days).
	DataExportRetentionDaysEnv = "CLAVIGER_EXPORT_RETENTION_DAYS"

	dataRequestDueDays    = 30 // GDPR: one month (extendable); CCPA 45 days
	dataExportCleanupCron = "__pbDataExportCleanup__"
)

// DataRequestTypes and DataRequestStates are the allowed select values.
var (
	DataRequestTypes = []string{DataRequestTypeExport, DataRequestTypeErase}

	DataRequestStates = []string{
		DataRequestStateAwaitingConfirmation, DataRequestStateGrace, DataRequestStateQueued,
		DataRequestStateRunning, DataRequestStateDone, DataRequestStatePartial,
		DataRequestStateFailed, DataRequestStateCancelled, DataRequestStateExpired,
	}
)

var (
	// ErrDataExportActive is returned when the subject already has an export
	// that is queued or running.
	ErrDataExportActive = errors.New("an export for this person is already in progress")

	// ErrDataExportUnavailable is returned for a download of an export that
	// has no file (not finished, expired, or already downloaded).
	ErrDataExportUnavailable = errors.New("the export file is not available")

	dataExportRunMu sync.Mutex
)

// RequestDataExport records an export request for the subject and starts the
// job in the background. requester is the person who asked (the subject for
// self service, a superuser otherwise).
func RequestDataExport(app App, subject, requester *Record, selfService bool, ip string) (*Record, error) {
	if subject == nil || !subject.Collection().IsAuth() {
		return nil, errors.New("the export subject must be an auth record")
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameDataRequests)
	if err != nil {
		return nil, err
	}

	var req *Record
	err = app.RunInTransaction(func(txApp App) error {
		active, err := txApp.CountRecords(CollectionNameDataRequests, dbx.And(
			dbx.HashExp{
				"type":          DataRequestTypeExport,
				"collectionRef": subject.Collection().Id,
				"recordRef":     subject.Id,
			},
			dbx.In("state", DataRequestStateQueued, DataRequestStateRunning),
		))
		if err != nil {
			return err
		}
		if active > 0 {
			return ErrDataExportActive
		}

		req = NewRecord(col)
		req.Set("type", DataRequestTypeExport)
		req.Set("state", DataRequestStateQueued)
		req.Set("collectionRef", subject.Collection().Id)
		req.Set("recordRef", subject.Id)
		if requester != nil {
			req.Set("requestedByCollection", requester.Collection().Id)
			req.Set("requestedBy", requester.Id)
		}
		req.Set("selfService", selfService)
		req.Set("dueAt", types.NowDateTime().Add(dataRequestDueDays*24*time.Hour))

		if err := txApp.Save(req); err != nil {
			return err
		}

		return writeDataRequestAudit(txApp, AuditActionDataExportRequest, req, requester, ip, map[string]any{
			"subjectCollection": subject.Collection().Name,
			"subjectId":         subject.Id,
			"selfService":       selfService,
			"dueAt":             req.GetString("dueAt"),
		})
	})
	if err != nil {
		return nil, err
	}

	StartDataExports(app)

	return req, nil
}

func writeDataRequestAudit(app App, action string, req, actor *Record, ip string, meta map[string]any) error {
	if !auditAvailable(app) {
		return nil
	}

	entry := &AuditEntry{
		Action:         action,
		CollectionId:   req.Collection().Id,
		CollectionName: CollectionNameDataRequests,
		RecordId:       req.Id,
		ChangedFields:  []string{},
		IP:             ip,
		Meta:           meta,
	}
	entry.Actor = NewAuditActorFromRecord(actor)

	return WriteAuditEntry(app, context.Background(), entry, nil)
}

// StartDataExports starts the job in the background unless it is running.
func StartDataExports(app App) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				app.Logger().Error("Data export job panicked", "panic", r)
			}
		}()
		ProcessDataExports(app)
	}()
}

// ProcessDataExports runs the queued export requests, oldest first, one at a
// time, and returns how many it ran. It returns 0 at once when another call
// is already working through the queue (that call will pick the new rows up).
func ProcessDataExports(app App) int {
	if !dataExportRunMu.TryLock() {
		return 0
	}
	defer dataExportRunMu.Unlock()

	ran := 0
	for {
		if !app.IsBootstrapped() || app.NonconcurrentDB() == nil {
			return ran // the app is shutting down
		}

		req, err := claimNextDataExport(app)
		if err != nil {
			app.Logger().Warn("Failed to load the next data export request", "error", err)
			return ran
		}
		if req == nil {
			return ran
		}

		runDataExport(app, req)
		ran++
	}
}

// claimNextDataExport moves the oldest queued export to running.
func claimNextDataExport(app App) (*Record, error) {
	for range 5 {
		var rec *Record
		recs, err := app.FindRecordsByFilter(
			CollectionNameDataRequests,
			"type = 'export' && state = 'queued'",
			"created", 1, 0,
		)
		if err != nil {
			return nil, err
		}
		if len(recs) == 0 {
			return nil, nil
		}
		rec = recs[0]

		// compare-and-swap: another replica may have taken it
		res, err := app.NonconcurrentDB().Update(
			CollectionNameDataRequests,
			dbx.Params{"state": DataRequestStateRunning},
			dbx.HashExp{"id": rec.Id, "state": DataRequestStateQueued},
		).Execute()
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return app.FindRecordById(CollectionNameDataRequests, rec.Id)
		}
	}

	return nil, nil
}

func dataRequestActor(app App, req *Record) *Record {
	id := req.GetString("requestedBy")
	if id == "" {
		return nil
	}
	rec, err := app.FindRecordById(req.GetString("requestedByCollection"), id)
	if err != nil {
		return nil
	}
	return rec
}

func runDataExport(app App, req *Record) {
	started := types.NowDateTime()
	req.Set("startedAt", started)
	actor := dataRequestActor(app, req)

	meta := map[string]any{
		"subjectCollection": req.GetString("collectionRef"),
		"subjectId":         req.GetString("recordRef"),
		"selfService":       req.GetBool("selfService"),
	}

	fail := func(err error) {
		app.Logger().Warn("Data export failed", "request", req.Id, "error", err)

		meta["status"] = "failed"
		meta["error"] = err.Error()

		req.Set("state", DataRequestStateFailed)
		req.Set("error", truncateString(err.Error(), 2000))
		req.Set("completedAt", types.NowDateTime())
		if saveErr := app.Save(req); saveErr != nil {
			app.Logger().Error("Failed to save the data export failure", "request", req.Id, "error", saveErr)
		}
		_ = writeDataRequestAudit(app, AuditActionDataExport, req, actor, "", meta)
	}

	subject, err := app.FindRecordById(req.GetString("collectionRef"), req.GetString("recordRef"))
	if err != nil {
		fail(fmt.Errorf("the subject no longer exists: %w", err))
		return
	}
	meta["subjectCollection"] = subject.Collection().Name

	tmpDir := filepath.Join(app.DataDir(), LocalTempDirName)
	if err := os.MkdirAll(tmpDir, os.ModePerm); err != nil {
		fail(err)
		return
	}
	tmp, err := os.CreateTemp(tmpDir, "export_*.zip")
	if err != nil {
		fail(err)
		return
	}
	defer os.Remove(tmp.Name())

	manifest, err := WriteDataExport(app, subject, tmp)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		fail(err)
		return
	}

	file, err := filesystem.NewFileFromPath(tmp.Name())
	if err != nil {
		fail(err)
		return
	}
	file.OriginalName = "export-" + subject.Id + ".zip"

	state := DataRequestStateDone
	if manifest.Partial() {
		state = DataRequestStatePartial
	}

	now := types.NowDateTime()
	req.Set("file", file)
	req.Set("state", state)
	req.Set("error", "")
	req.Set("completedAt", now)
	req.Set("expires", now.Add(time.Duration(DataExportRetentionDays(app))*24*time.Hour))
	req.Set("result", map[string]any{
		"categories": manifest.Categories(),
		"skipped":    manifest.Skipped,
		"errors":     manifest.Errors,
		"files":      map[string]any{"included": manifest.Files.Included, "bytes": manifest.Files.Bytes, "skipped": len(manifest.Files.Skipped)},
		"dataMap":    manifest.DataMap,
		"bytes":      file.Size,
	})
	if err := app.Save(req); err != nil {
		fail(fmt.Errorf("storing the export file: %w", err))
		return
	}

	meta["status"] = map[bool]string{true: "partial", false: "ok"}[manifest.Partial()]
	meta["categories"] = manifest.Categories()
	meta["files"] = manifest.Files.Included
	meta["bytes"] = file.Size
	meta["expires"] = req.GetString("expires")
	if err := writeDataRequestAudit(app, AuditActionDataExport, req, actor, "", meta); err != nil {
		app.Logger().Error("Failed to write the data export audit entry", "request", req.Id, "error", err)
	}
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// OpenDataExportFile opens the finished export of the request for reading.
// The caller must Close the reader.
func OpenDataExportFile(app App, req *Record) (io.ReadCloser, int64, error) {
	name := req.GetString("file")
	if name == "" || (req.GetString("state") != DataRequestStateDone && req.GetString("state") != DataRequestStatePartial) {
		return nil, 0, ErrDataExportUnavailable
	}
	if exp := req.GetDateTime("expires"); !exp.IsZero() && exp.Time().Before(time.Now()) {
		return nil, 0, ErrDataExportUnavailable
	}

	fsys, err := app.NewFilesystem()
	if err != nil {
		return nil, 0, err
	}

	r, err := fsys.GetReader(req.BaseFilesPath() + "/" + name)
	if err != nil {
		_ = fsys.Close()
		return nil, 0, ErrDataExportUnavailable
	}

	return &fsReadCloser{r: r, fsys: fsys}, r.Size(), nil
}

type fsReadCloser struct {
	r    io.ReadCloser
	fsys *filesystem.System
}

func (f *fsReadCloser) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *fsReadCloser) Close() error {
	err := f.r.Close()
	_ = f.fsys.Close()
	return err
}

// FinishDataExportDownload is called after the file was sent in full: the
// file is deleted (the link works once), the download is recorded and audited.
func FinishDataExportDownload(app App, req, downloader *Record, ip string) error {
	req.Set("file", nil)
	req.Set("downloadedAt", types.NowDateTime())
	if err := app.Save(req); err != nil {
		return err
	}

	return writeDataRequestAudit(app, AuditActionDataExportDownload, req, downloader, ip, map[string]any{
		"subjectCollection": req.GetString("collectionRef"),
		"subjectId":         req.GetString("recordRef"),
	})
}

// ExpireDataExports deletes the files of exports past their expiry and marks
// the rows expired. It returns how many files were removed.
func ExpireDataExports(app App) (int, error) {
	// the file field is hidden, which the filter language does not accept
	var recs []*Record
	err := app.RecordQuery(CollectionNameDataRequests).
		AndWhere(dbx.HashExp{"type": DataRequestTypeExport}).
		AndWhere(dbx.NewExp("[[file]] != '' AND [[expires]] != '' AND [[expires]] < {:now}", dbx.Params{
			"now": time.Now().UTC().Format(types.DefaultDateLayout),
		})).
		All(&recs)
	if err != nil {
		return 0, err
	}

	n := 0
	var errs []error
	for _, r := range recs {
		r.Set("file", nil)
		r.Set("state", DataRequestStateExpired)
		if err := app.Save(r); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}

	return n, errors.Join(errs...)
}

func (app *BaseApp) registerDataRequestHooks() {
	// the audit entries above describe the requests; a record-change entry
	// for every state change would repeat them (see auditExcludedCollections)

	app.OnServe().Bind(&hook.Handler[*ServeEvent]{
		Id: "__pbDataExportResume__",
		Func: func(e *ServeEvent) error {
			// a crash left rows running: run them again from the start.
			// (Checked with a read first: an UPDATE that matches nothing
			// would reset the connection's changes() counter, which the
			// SQL endpoint reports for DDL on the modernc driver.)
			running, err := e.App.CountRecords(CollectionNameDataRequests, dbx.HashExp{
				"type": DataRequestTypeExport, "state": DataRequestStateRunning,
			})
			if err == nil && running > 0 {
				_, err = e.App.NonconcurrentDB().Update(
					CollectionNameDataRequests,
					dbx.Params{"state": DataRequestStateQueued},
					dbx.HashExp{"type": DataRequestTypeExport, "state": DataRequestStateRunning},
				).Execute()
			}
			if err == nil {
				StartDataExports(e.App)
			}
			return e.Next()
		},
	})

	app.Cron().Add(dataExportCleanupCron, "17 * * * *", func() {
		if _, err := ExpireDataExports(app); err != nil {
			app.Logger().Warn("Failed to expire data exports", "error", err)
		}
		ProcessDataExports(app) // picks up a row a previous run missed
	})
}
