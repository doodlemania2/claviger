package apis

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/pocketbase/ozzo-validation/v4/is"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/mails"
	"github.com/pocketbase/pocketbase/tools/routine"
)

// recordRequestPasskeyReset sends a passkey reset email to the auth record
// matching the submitted email address. It always responds with 204 to
// avoid leaking whether the email is registered.
func recordRequestPasskeyReset(e *core.RequestEvent) error {
	collection, err := findAuthCollection(e)
	if err != nil {
		return err
	}

	if !collection.WebAuthn.Enabled {
		return e.BadRequestError("The collection is not configured to allow WebAuthn authentication.", nil)
	}

	form := new(recordRequestPasskeyResetForm)
	if err = e.BindBody(form); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while loading the submitted data.", err))
	}
	if err = form.validate(); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
	}

	record, err := e.App.FindAuthRecordByEmail(collection, form.Email)
	if err != nil {
		// eagerly write 204 response to avoid email enumeration
		e.NoContent(http.StatusNoContent)
		return fmt.Errorf("failed to fetch %s record with email %s: %w", collection.Name, form.Email, err)
	}

	resendKey := getPasskeyResetResendKey(record)
	if e.App.Store().Has(resendKey) {
		e.NoContent(http.StatusNoContent)
		return errors.New("try again later - you've already requested a passkey reset email")
	}

	app := e.App
	routine.FireAndForget(func() {
		if err := mails.SendRecordPasskeyReset(app, record); err != nil {
			app.Logger().Error("Failed to send passkey reset email", "error", err)
			return
		}

		app.Store().Set(resendKey, struct{}{})
		time.AfterFunc(2*time.Minute, func() {
			app.Store().Remove(resendKey)
		})
	})

	return execAfterSuccessTx(true, e.App, func() error {
		return e.NoContent(http.StatusNoContent)
	})
}

// -------------------------------------------------------------------

type recordRequestPasskeyResetForm struct {
	Email string `form:"email" json:"email"`
}

func (form *recordRequestPasskeyResetForm) validate() error {
	return validation.ValidateStruct(form,
		validation.Field(&form.Email, validation.Required, validation.Length(1, 255), is.EmailFormat),
	)
}

func getPasskeyResetResendKey(record *core.Record) string {
	return "@limitPasskeyResetEmail_" + record.Collection().Id + record.Id
}
