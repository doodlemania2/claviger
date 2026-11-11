package apis

import (
	"context"
	"errors"
	"net/http"
	"time"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/sms"
)

// Fork-local (Claviger #83): superuser-only SMS test send.

// AuditActionSMSTest is the audit action of a test SMS. The entry carries only
// the masked number (meta.to), the provider and the outcome.
const AuditActionSMSTest = "sms.test"

const smsTestBody = "Claviger test message: SMS delivery is configured."

// settingsTestSMS handles POST /api/settings/test/sms {"to": "+14155550123"}.
// It sends a fixed text through the configured provider and answers 204.
func settingsTestSMS(e *core.RequestEvent) error {
	data := struct {
		To string `json:"to" form:"to"`
	}{}
	if err := e.BindBody(&data); err != nil {
		return e.BadRequestError("An error occurred while loading the submitted data.", err)
	}

	if err := validation.Validate(data.To, validation.Required); err != nil {
		return e.BadRequestError("Failed to send the test SMS.", validation.Errors{"to": err})
	}
	if err := sms.ValidateE164(data.To); err != nil {
		return e.BadRequestError("Failed to send the test SMS.", validation.Errors{
			"to": validation.NewError("validation_invalid_phone", "Must be a phone number in E.164 format, for example +14155550123."),
		})
	}

	sender, err := core.NewSMSSender(e.App)
	if err != nil {
		if errors.Is(err, core.ErrSMSNotConfigured) {
			return e.BadRequestError("SMS is not configured. Enable it and save the settings first.", nil)
		}
		return e.BadRequestError("Failed to send the test SMS. Raw error: \n"+err.Error(), nil)
	}

	ctx, cancel := context.WithTimeout(e.Request.Context(), 30*time.Second)
	defer cancel()

	sendErr := sender.Send(ctx, &sms.Message{To: data.To, Body: smsTestBody})

	// audited whether it worked or not; never the full number or the text
	meta := map[string]any{
		"to":       sms.MaskNumber(data.To),
		"provider": e.App.Settings().SMS.Provider,
		"success":  sendErr == nil,
	}
	if sendErr != nil {
		var se *sms.Error
		if errors.As(sendErr, &se) {
			meta["httpStatus"] = se.HTTPStatus
			meta["code"] = se.Code
		}
	}
	if auditErr := writeSMSTestAudit(e, meta); auditErr != nil {
		e.App.Logger().Error("Failed to write the SMS test audit entry", "error", auditErr)
	}

	if sendErr != nil {
		return e.BadRequestError("Failed to send the test SMS. Raw error: \n"+sendErr.Error(), nil)
	}

	return e.NoContent(http.StatusNoContent)
}

func writeSMSTestAudit(e *core.RequestEvent, meta map[string]any) error {
	actor := core.NewAuditActorFromRecord(e.Auth)
	if key := APIKeyFromRequest(e); key != nil {
		actor.APIKeyId = key.Id
		meta["apiKeyId"] = key.Id
	}

	return core.WriteAuditEntry(e.App, auditContext(e), &core.AuditEntry{
		Action:         AuditActionSMSTest,
		Actor:          actor,
		CollectionName: "_params",
		RecordId:       "settings",
		ChangedFields:  []string{},
		IP:             e.RealIP(),
		Meta:           meta,
	}, nil)
}
