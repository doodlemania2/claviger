package apis

import (
	"database/sql"
	"errors"
	"net/http"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #88, 88-3): retention policies, the retention run and
// legal holds. Superusers only; API keys are refused (a named person acts).
//
//	GET    /api/gdpr/retention/policies            the policies, with the state and result of the last run
//	POST   /api/gdpr/retention/policies            create; always disabled and a dry run
//	GET    /api/gdpr/retention/policies/{id}
//	PATCH  /api/gdpr/retention/policies/{id}       change; a policy that ends up enabled and live needs a fresh passkey
//	DELETE /api/gdpr/retention/policies/{id}
//	POST   /api/gdpr/retention/run                 {preview?, policy?}; the daily job now, or a preview that changes nothing
//	GET    /api/gdpr/holds?active=1                legal holds
//	GET    /api/gdpr/holds/{id}
//	POST   /api/gdpr/holds                         place a hold; fresh passkey
//	POST   /api/gdpr/holds/{id}/lift               lift it; fresh passkey
//
// See docs/retention.md.
func bindRetentionApi(rg *router.RouterGroup[*core.RequestEvent]) {
	g := rg.Group("/gdpr").Bind(RequireSuperuserAuth(), pastoralNoAPIKey())

	p := g.Group("/retention")
	p.GET("/policies", retentionPolicyList)
	p.POST("/policies", retentionPolicyCreate)
	p.GET("/policies/{id}", retentionPolicyView)
	p.PATCH("/policies/{id}", retentionPolicyUpdate)
	p.DELETE("/policies/{id}", retentionPolicyDelete)
	p.POST("/run", retentionRun)

	h := g.Group("/holds")
	h.GET("", legalHoldList)
	h.GET("/{id}", legalHoldView)
	h.POST("", legalHoldPlace).Bind(RequireFreshPasskey(webauthnStepUpMaxAge))
	h.POST("/{id}/lift", legalHoldLift).Bind(RequireFreshPasskey(webauthnStepUpMaxAge))
}

func retentionHTTP(e *core.RequestEvent, err error) error {
	var verrs validation.Errors

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return e.NotFoundError("", nil)
	case errors.Is(err, core.ErrRetentionRunning):
		return e.Error(http.StatusConflict, err.Error(), nil)
	case errors.Is(err, core.ErrLegalHoldLifted):
		return e.Error(http.StatusConflict, err.Error(), nil)
	case errors.As(err, &verrs):
		return e.BadRequestError("An error occurred while validating the submitted data.", verrs)
	}

	return e.BadRequestError(err.Error(), nil)
}

func retentionPolicyJSON(r *core.Record) map[string]any {
	var counts, last map[string]any

	_ = r.UnmarshalJSONField("runCounts", &counts)
	_ = r.UnmarshalJSONField("lastResult", &last)

	return map[string]any{
		"id": r.Id, "name": r.GetString("name"), "collection": r.GetString("collection"),
		"basis": r.GetString("basis"), "after": r.GetInt("after"), "unit": r.GetString("unit"),
		"action": r.GetString("action"), "filter": r.GetString("filter"),
		"enabled": r.GetBool("enabled"), "dryRun": r.GetBool("dryRun"),
		"runStatus": r.GetString("runStatus"), "runCounts": counts,
		"lastRunAt": r.GetDateTime("lastRunAt"), "lastResult": last,
		"created": r.GetDateTime("created"), "updated": r.GetDateTime("updated"),
	}
}

func retentionPolicyList(e *core.RequestEvent) error {
	rows, err := e.App.FindRecordsByFilter(core.CollectionNameRetentionPolicies, "", "created,id", 0, 0)
	if err != nil {
		return e.InternalServerError("Failed to load the policies.", err)
	}

	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, retentionPolicyJSON(r))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func retentionPolicyView(e *core.RequestEvent) error {
	r, err := e.App.FindRecordById(core.CollectionNameRetentionPolicies, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("", nil)
	}

	return e.JSON(http.StatusOK, retentionPolicyJSON(r))
}

func retentionPolicyCreate(e *core.RequestEvent) error {
	var in core.RetentionPolicyInput
	if err := e.BindBody(&in); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	r, err := core.CreateRetentionPolicy(e.App, auditContext(e), in)
	if err != nil {
		return retentionHTTP(e, err)
	}

	return e.JSON(http.StatusCreated, retentionPolicyJSON(r))
}

func retentionPolicyUpdate(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")

	existing, err := e.App.FindRecordById(core.CollectionNameRetentionPolicies, id)
	if err != nil {
		return e.NotFoundError("", nil)
	}

	var in core.RetentionPolicyInput
	if err := e.BindBody(&in); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	enabled, dryRun := existing.GetBool("enabled"), existing.GetBool("dryRun")
	if in.Enabled != nil {
		enabled = *in.Enabled
	}

	if in.DryRun != nil {
		dryRun = *in.DryRun
	}

	// a policy that will really change data is armed with a fresh passkey
	if enabled && !dryRun && !HasFreshPasskey(e, webauthnStepUpMaxAge) {
		return e.ForbiddenError("A recent passkey verification is required for this action.", validation.Errors{
			"passkey": validation.NewError(StepUpRequiredCode, "A recent passkey verification is required."),
		})
	}

	r, err := core.UpdateRetentionPolicy(e.App, auditContext(e), id, in)
	if err != nil {
		return retentionHTTP(e, err)
	}

	return e.JSON(http.StatusOK, retentionPolicyJSON(r))
}

func retentionPolicyDelete(e *core.RequestEvent) error {
	if err := core.DeleteRetentionPolicy(e.App, auditContext(e), e.Request.PathValue("id")); err != nil {
		return retentionHTTP(e, err)
	}

	return e.NoContent(http.StatusNoContent)
}

type retentionRunBody struct {
	Preview bool   `json:"preview" form:"preview"`
	Policy  string `json:"policy" form:"policy"`
}

func retentionRun(e *core.RequestEvent) error {
	var body retentionRunBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	res, err := core.RunRetention(auditContext(e), e.App, core.RetentionRunOptions{Preview: body.Preview, PolicyId: body.Policy})
	if err != nil {
		return retentionHTTP(e, err)
	}

	return e.JSON(http.StatusOK, res)
}

// ---- legal holds -----------------------------------------------------------------

func legalHoldJSON(h *core.Record) map[string]any {
	return map[string]any{
		"id": h.Id, "scope": h.GetString("scope"),
		"subject": h.GetString("subject"), "subjectCollection": h.GetString("subjectCollection"),
		"collection": h.GetString("collection"), "recordId": h.GetString("recordId"),
		"reason": h.GetString("reason"), "reference": h.GetString("reference"),
		"placedBy": h.GetString("placedBy"), "placedAt": h.GetDateTime("placedAt"),
		"liftedAt": h.GetDateTime("liftedAt"), "liftedBy": h.GetString("liftedBy"),
		"active": h.GetDateTime("liftedAt").IsZero(),
	}
}

func legalHoldList(e *core.RequestEvent) error {
	filter := ""
	if q := e.Request.URL.Query().Get("active"); q == "1" || q == "true" {
		filter = `liftedAt = ""`
	}

	rows, err := e.App.FindRecordsByFilter(core.CollectionNameLegalHolds, filter, "-placedAt", 0, 0)
	if err != nil {
		return e.InternalServerError("Failed to load the holds.", err)
	}

	items := make([]map[string]any, 0, len(rows))
	for _, h := range rows {
		items = append(items, legalHoldJSON(h))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func legalHoldView(e *core.RequestEvent) error {
	h, err := e.App.FindRecordById(core.CollectionNameLegalHolds, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("", nil)
	}

	return e.JSON(http.StatusOK, legalHoldJSON(h))
}

func legalHoldPlace(e *core.RequestEvent) error {
	var in core.LegalHoldInput
	if err := e.BindBody(&in); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	h, err := core.PlaceLegalHold(e.App, auditContext(e), e.Auth, in)
	if err != nil {
		return retentionHTTP(e, err)
	}

	return e.JSON(http.StatusCreated, legalHoldJSON(h))
}

func legalHoldLift(e *core.RequestEvent) error {
	h, err := core.LiftLegalHold(e.App, auditContext(e), e.Request.PathValue("id"), e.Auth)
	if err != nil {
		return retentionHTTP(e, err)
	}

	return e.JSON(http.StatusOK, legalHoldJSON(h))
}
