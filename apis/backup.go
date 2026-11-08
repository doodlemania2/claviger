package apis

import (
	"context"
	"net/http"
	"path/filepath"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/dialect"
	"github.com/doodlemania2/claviger/tools/router"
	"github.com/doodlemania2/claviger/tools/routine"
	"github.com/doodlemania2/claviger/tools/types"
	"github.com/spf13/cast"
)

// bindBackupApi registers the file api endpoints and the corresponding handlers.
func bindBackupApi(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	sub := rg.Group("/backups")
	sub.GET("", backupsList).Bind(RequireSuperuserAuth())
	sub.POST("", backupCreate).Bind(RequireSuperuserAuth())
	sub.POST("/upload", backupUpload).Bind(BodyLimit(0), RequireSuperuserAuth())
	sub.GET("/{key}", backupDownload) // relies on superuser file token
	sub.DELETE("/{key}", backupDelete).Bind(RequireSuperuserAuth())
	sub.POST("/{key}/restore", backupRestore).Bind(RequireSuperuserAuth())
	sub.POST("/{key}/verify", backupVerify).Bind(RequireSuperuserAuth())
}

type backupFileInfo struct {
	Modified types.DateTime `json:"modified"`
	Key      string         `json:"key"`
	Size     int64          `json:"size"`

	// Claviger: last restore verification result (absent when never verified).
	Verification *core.BackupVerification `json:"verification,omitempty"`
	// Verifying is true while the backup is being verified.
	Verifying bool `json:"verifying,omitempty"`
}

func backupsList(e *core.RequestEvent) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fsys, err := e.App.NewBackupsFilesystem()
	if err != nil {
		return e.BadRequestError("Failed to load backups filesystem.", err)
	}
	defer fsys.Close()

	fsys.SetContext(ctx)

	backups, err := fsys.List("")
	if err != nil {
		return e.BadRequestError("Failed to retrieve backup items. Raw error: \n"+err.Error(), nil)
	}

	result := make([]backupFileInfo, len(backups))

	verifications, err := core.LoadBackupVerifications(e.App)
	if err != nil {
		e.App.Logger().Warn("Failed to load the backup verification results", "error", err.Error())
	}
	verifyingKey := core.IsVerifyingBackup(e.App)

	for i, obj := range backups {
		modified, _ := types.ParseDateTime(obj.ModTime)

		result[i] = backupFileInfo{
			Key:      obj.Key,
			Size:     obj.Size,
			Modified: modified,

			Verification: verifications[obj.Key],
			Verifying:    verifyingKey != "" && verifyingKey == obj.Key,
		}
	}

	return e.JSON(http.StatusOK, result)
}

func backupDownload(e *core.RequestEvent) error {
	fileToken := e.Request.URL.Query().Get("token")

	authRecord, err := e.App.FindAuthRecordByToken(fileToken, core.TokenTypeFile)
	if err != nil || !authRecord.IsSuperuser() {
		return e.ForbiddenError("Insufficient permissions to access the resource.", err)
	}

	allowedIPs := e.App.Settings().SuperuserIPs
	if len(allowedIPs) > 0 && !isIPInList(allowedIPs, e.RealIP()) {
		return e.ForbiddenError("Insufficient permissions to access the resource.", nil)
	}

	fsys, err := e.App.NewBackupsFilesystem()
	if err != nil {
		return e.InternalServerError("Failed to load backups filesystem.", err)
	}
	defer fsys.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fsys.SetContext(ctx)

	key := e.Request.PathValue("key")

	return fsys.Serve(
		e.Response,
		e.Request,
		key,
		filepath.Base(key), // without the path prefix (if any)
	)
}

func backupDelete(e *core.RequestEvent) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fsys, err := e.App.NewBackupsFilesystem()
	if err != nil {
		return e.InternalServerError("Failed to load backups filesystem.", err)
	}
	defer fsys.Close()

	fsys.SetContext(ctx)

	key := e.Request.PathValue("key")

	if key != "" && cast.ToString(e.App.Store().Get(core.StoreKeyActiveBackup)) == key {
		return e.BadRequestError("The backup is currently being used and cannot be deleted.", nil)
	}

	if err := fsys.Delete(key); err != nil {
		return e.BadRequestError("Invalid or already deleted backup file. Raw error: \n"+err.Error(), nil)
	}

	core.DeleteBackupVerification(e.App, key)

	return e.NoContent(http.StatusNoContent)
}

func backupRestore(e *core.RequestEvent) error {
	if e.App.Store().Has(core.StoreKeyActiveBackup) {
		return e.BadRequestError("Try again later - another backup/restore process has already been started.", nil)
	}

	// fork (#106): a zip restore replaces data.db, which a Postgres install does not have
	if name := core.DialectOf(e.App).Name(); name != dialect.SQLiteName {
		return e.BadRequestError("Restoring a zip backup is not available when the database is "+name+
			". Restore the database with pg_restore or your provider's point-in-time recovery.", nil)
	}

	key := e.Request.PathValue("key")

	existsCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fsys, err := e.App.NewBackupsFilesystem()
	if err != nil {
		return e.InternalServerError("Failed to load backups filesystem.", err)
	}
	defer fsys.Close()

	fsys.SetContext(existsCtx)

	if exists, err := fsys.Exists(key); !exists {
		return e.BadRequestError("Missing or invalid backup file.", err)
	}

	routine.FireAndForget(func() {
		// give some optimistic time to write the response before restarting the app
		time.Sleep(1 * time.Second)

		// wait max 10 minutes to fetch the backup
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		if err := e.App.RestoreBackup(ctx, key); err != nil {
			e.App.Logger().Error("Failed to restore backup", "key", key, "error", err.Error())
		}
	})

	return e.NoContent(http.StatusNoContent)
}

// backupVerify starts (in the background) a restore verification of the
// specified backup. The result is available from the backups list.
func backupVerify(e *core.RequestEvent) error {
	if e.App.Store().Has(core.StoreKeyActiveBackup) || core.IsVerifyingBackup(e.App) != "" {
		return e.BadRequestError("Try again later - another backup/restore/verification process has already been started.", nil)
	}

	key := e.Request.PathValue("key")

	existsCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fsys, err := e.App.NewBackupsFilesystem()
	if err != nil {
		return e.InternalServerError("Failed to load backups filesystem.", err)
	}
	defer fsys.Close()

	fsys.SetContext(existsCtx)

	if exists, err := fsys.Exists(key); !exists {
		return e.BadRequestError("Missing or invalid backup file.", err)
	}

	routine.FireAndForget(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		if _, err := core.VerifyBackup(ctx, e.App, key); err != nil {
			e.App.Logger().Error("Failed to verify backup", "key", key, "error", err.Error())
		}
	})

	return e.JSON(http.StatusAccepted, map[string]any{"key": key, "status": "running"})
}
