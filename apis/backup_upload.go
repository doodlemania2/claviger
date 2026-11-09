package apis

import (
	"io"
	"net/http"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/core/validators"
	"github.com/doodlemania2/claviger/tools/backupenc"
	"github.com/doodlemania2/claviger/tools/filesystem"
)

func backupUpload(e *core.RequestEvent) error {
	fsys, err := e.App.NewBackupsFilesystem()
	if err != nil {
		return err
	}
	defer fsys.Close()

	form := new(backupUploadForm)
	form.fsys = fsys
	files, _ := e.FindUploadedFiles("file")
	if len(files) > 0 {
		form.File = files[0]
	}

	err = form.validate()
	if err != nil {
		return e.BadRequestError("An error occurred while validating the submitted data.", err)
	}

	err = fsys.UploadFile(form.File, form.File.OriginalName)
	if err != nil {
		return e.BadRequestError("Failed to upload backup.", err)
	}

	// we don't retrieve the generated backup file because it may not be
	// available yet due to the eventually consistent nature of some S3 providers
	return e.NoContent(http.StatusNoContent)
}

// -------------------------------------------------------------------

type backupUploadForm struct {
	fsys *filesystem.System

	File *filesystem.File `json:"file"`
}

func (form *backupUploadForm) validate() error {
	return validation.ValidateStruct(form,
		validation.Field(
			&form.File,
			validation.Required,
			validation.By(uploadedBackupType),
			validation.By(form.checkUniqueName),
		),
	)
}

// uploadedBackupType accepts a zip archive or (fork, #78) an encrypted backup:
// a file named "*.enc" that starts with the encrypted backup magic bytes.
func uploadedBackupType(value any) error {
	f, _ := value.(*filesystem.File)
	if f == nil || !core.IsEncryptedBackupName(f.OriginalName) {
		return validators.UploadedFileMimeType([]string{"application/zip"})(value)
	}

	invalid := validation.NewError("validation_invalid_mime_type", "The file is not an encrypted Claviger backup.")

	r, err := f.Reader.Open()
	if err != nil {
		return invalid
	}
	defer r.Close()

	head := make([]byte, len(backupenc.Magic))
	n, _ := io.ReadFull(r, head)
	if !backupenc.IsEncrypted(head[:n]) {
		return invalid
	}

	return nil
}

func (form *backupUploadForm) checkUniqueName(value any) error {
	v, _ := value.(*filesystem.File)
	if v == nil {
		return nil // nothing to check
	}

	// note: we use the original name because that is what we upload
	if exists, err := form.fsys.Exists(v.OriginalName); err != nil || exists {
		return validation.NewError("validation_backup_name_exists", "Backup file with the specified name already exists.")
	}

	return nil
}
