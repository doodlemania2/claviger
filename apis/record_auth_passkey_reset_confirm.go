package apis

import (
	"encoding/base64"
	"net/http"

	"ganigeorgiev.com/pocketbase/dbx"
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
)

// recordConfirmPasskeyResetBegin validates the emailed reset token and starts a
// WebAuthn registration ceremony on behalf of the token's owner. It returns
// PublicKeyCredentialCreationOptions and a sessionToken for the finish step.
//
// The caller does NOT need to be authenticated — the email reset token
// proves identity.
func recordConfirmPasskeyResetBegin(e *core.RequestEvent) error {
	collection, err := findAuthCollection(e)
	if err != nil {
		return err
	}

	if !collection.WebAuthn.Enabled {
		return e.ForbiddenError("The collection is not configured to allow WebAuthn authentication.", nil)
	}

	form := &recordConfirmPasskeyResetBeginForm{app: e.App, collection: collection}
	if err := e.BindBody(form); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while loading the submitted data.", err))
	}
	if err := form.validate(); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
	}

	record, err := e.App.FindAuthRecordByToken(form.Token, core.TokenTypePasskeyReset)
	if err != nil {
		return firstApiError(err, e.BadRequestError("Invalid or expired passkey reset token.", err))
	}

	wa, err := initWebAuthn(e.App)
	if err != nil {
		return e.InternalServerError("Failed to initialize WebAuthn.", err)
	}

	existingCreds, err := loadUserCredentials(e.App, record)
	if err != nil {
		return e.InternalServerError("Failed to load existing credentials.", err)
	}

	user := &webauthnUserAdapter{
		record:      record,
		credentials: existingCreds,
	}

	options, session, err := wa.BeginRegistration(user, webauthnRegistrationOptions(collection)...)
	if err != nil {
		return e.InternalServerError("Failed to begin WebAuthn registration.", err)
	}

	token := storeWebAuthnSession(e.App, session, record.Id, false)

	return e.JSON(http.StatusOK, map[string]any{
		"options":      options,
		"sessionToken": token,
	})
}

// recordConfirmPasskeyResetFinish completes the emailed passkey reset flow.
// On success, all previously registered credentials for the user are removed
// and the new credential becomes the user's only passkey.
func recordConfirmPasskeyResetFinish(e *core.RequestEvent) error {
	collection, err := findAuthCollection(e)
	if err != nil {
		return err
	}

	if !collection.WebAuthn.Enabled {
		return e.ForbiddenError("The collection is not configured to allow WebAuthn authentication.", nil)
	}

	form := &recordConfirmPasskeyResetFinishForm{app: e.App, collection: collection}
	if err := e.BindBody(form); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while loading the submitted data.", err))
	}
	if err := form.validate(); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
	}

	record, err := e.App.FindAuthRecordByToken(form.Token, core.TokenTypePasskeyReset)
	if err != nil {
		return firstApiError(err, e.BadRequestError("Invalid or expired passkey reset token.", err))
	}

	entry, err := retrieveWebAuthnSession(e.App, form.SessionToken)
	if err != nil {
		return e.BadRequestError("Invalid or expired registration session.", err)
	}
	defer deleteWebAuthnSession(e.App, form.SessionToken)

	if entry.Purpose != webauthnPurposeDefault || entry.RecordId != record.Id {
		return e.BadRequestError("Session does not match the token owner.", nil)
	}

	wa, err := initWebAuthn(e.App)
	if err != nil {
		return e.InternalServerError("Failed to initialize WebAuthn.", err)
	}

	existingCreds, err := loadUserCredentials(e.App, record)
	if err != nil {
		return e.InternalServerError("Failed to load existing credentials.", err)
	}

	user := &webauthnUserAdapter{
		record:      record,
		credentials: existingCreds,
	}

	cred, err := wa.FinishRegistration(user, *entry.Session, e.Request)
	if err != nil {
		return e.BadRequestError("Failed to verify WebAuthn registration.", err)
	}

	// Replace the credentials and invalidate the token atomically: either the
	// user ends up with exactly the new passkey and a spent token, or nothing
	// changes and the request fails (the user can retry with the same link).
	var wcRecord *core.WebAuthnCredential
	txErr := e.App.RunInTransaction(func(txApp core.App) error {
		// Remove any previously registered credentials for the user, since
		// the user has just proven recovery via the email-link token.
		existingRecords, err := txApp.FindAllRecords(
			core.CollectionNameWebAuthnCredentials,
			dbx.HashExp{
				"collectionRef": record.Collection().Id,
				"recordRef":     record.Id,
			},
		)
		if err != nil {
			return err
		}
		for _, old := range existingRecords {
			if err := txApp.Delete(old); err != nil {
				return err
			}
		}

		// Store the new credential
		wcRecord = core.NewWebAuthnCredential(txApp)
		wcRecord.SetCollectionRef(collection.Id)
		wcRecord.SetRecordRef(record.Id)
		wcRecord.FromWebAuthnCredential(*cred)
		if form.Name != "" {
			wcRecord.SetName(form.Name)
		}
		if err := txApp.Save(wcRecord); err != nil {
			return err
		}

		// One-time use: invalidate the token by rotating the user's TokenKey
		// (matches the behavior of password reset confirm via SetPassword).
		record.RefreshTokenKey()

		return txApp.Save(record)
	})
	if txErr != nil {
		return e.InternalServerError("Failed to reset the passkey.", txErr)
	}

	e.App.Store().Remove(getPasskeyResetResendKey(record))

	return e.JSON(http.StatusCreated, map[string]any{
		"id":           wcRecord.Id,
		"name":         wcRecord.Name(),
		"credentialId": base64.RawURLEncoding.EncodeToString(cred.ID),
	})
}

// -------------------------------------------------------------------

type recordConfirmPasskeyResetBeginForm struct {
	app        core.App
	collection *core.Collection

	Token string `form:"token" json:"token"`
}

func (form *recordConfirmPasskeyResetBeginForm) validate() error {
	return validation.ValidateStruct(form,
		validation.Field(&form.Token, validation.Required, validation.By(form.checkToken)),
	)
}

func (form *recordConfirmPasskeyResetBeginForm) checkToken(value any) error {
	v, _ := value.(string)
	if v == "" {
		return nil
	}

	record, err := form.app.FindAuthRecordByToken(v, core.TokenTypePasskeyReset)
	if err != nil || record == nil {
		return validation.NewError("validation_invalid_token", "Invalid or expired token.")
	}

	if record.Collection().Id != form.collection.Id {
		return validation.NewError("validation_token_collection_mismatch", "The provided token is for different auth collection.")
	}

	return nil
}

type recordConfirmPasskeyResetFinishForm struct {
	app        core.App
	collection *core.Collection

	Token        string `form:"token" json:"token"`
	SessionToken string `form:"sessionToken" json:"sessionToken"`
	Name         string `form:"name" json:"name"`
}

func (form *recordConfirmPasskeyResetFinishForm) validate() error {
	return validation.ValidateStruct(form,
		validation.Field(&form.Token, validation.Required, validation.By(form.checkToken)),
		validation.Field(&form.SessionToken, validation.Required, validation.Length(1, 255)),
		validation.Field(&form.Name, validation.Length(0, 255)),
	)
}

func (form *recordConfirmPasskeyResetFinishForm) checkToken(value any) error {
	v, _ := value.(string)
	if v == "" {
		return nil
	}

	record, err := form.app.FindAuthRecordByToken(v, core.TokenTypePasskeyReset)
	if err != nil || record == nil {
		return validation.NewError("validation_invalid_token", "Invalid or expired token.")
	}

	if record.Collection().Id != form.collection.Id {
		return validation.NewError("validation_token_collection_mismatch", "The provided token is for different auth collection.")
	}

	return nil
}
