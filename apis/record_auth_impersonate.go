package apis

import (
	"time"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
)

// note: for now allow superusers but it may change in the future to allow access
// also to users with "Manage API" rule access depending on the use cases that will arise
func recordAuthImpersonate(e *core.RequestEvent) error {
	if !e.HasSuperuserAuth() {
		return e.ForbiddenError("", nil)
	}

	collection, err := findAuthCollection(e)
	if err != nil {
		return err
	}

	record, err := e.App.FindRecordById(collection, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("", err)
	}

	form := &impersonateForm{}
	if err = e.BindBody(form); err != nil {
		return e.BadRequestError("An error occurred while loading the submitted data.", err)
	}
	if err = form.validate(); err != nil {
		return e.BadRequestError("An error occurred while validating the submitted data.", err)
	}

	// fork-local (#93): superusers cannot be impersonated, every impersonation
	// needs a reason and is recorded in the audit log
	if record.IsSuperuser() {
		return e.ForbiddenError("Superuser accounts cannot be impersonated.", nil)
	}

	token, err := record.NewStaticAuthToken(time.Duration(form.Duration) * time.Second)
	if err != nil {
		return e.InternalServerError("Failed to generate static auth token", err)
	}

	e.Set(requestEventKeySessionMethod, core.SessionAuthMethodImpersonate)
	e.Set(requestEventKeySessionClaims, map[string]any{core.TokenClaimImpersonatedBy: e.Auth.Id})

	if err = auditImpersonation(e, record, form.Reason, form.Duration); err != nil {
		return e.InternalServerError("Failed to record the impersonation.", err)
	}

	return recordAuthResponse(e, record, token, "", nil)
}

// -------------------------------------------------------------------

type impersonateForm struct {
	// Duration is the optional custom token duration in seconds.
	Duration int64 `form:"duration" json:"duration"`

	// Reason is required (fork, #93) and stored in the audit log.
	Reason string `form:"reason" json:"reason"`
}

func (form *impersonateForm) validate() error {
	return validation.ValidateStruct(form,
		validation.Field(&form.Duration, validation.Min(0)),
		validation.Field(&form.Reason, validation.Required, validation.Length(1, 500)),
	)
}
