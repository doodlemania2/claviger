package apis

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"ganigeorgiev.com/pocketbase/dbx"
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #88, 88-1): the personal data export.
//
//	POST   /api/me/export                       a signed-in user asks for their own export
//	GET    /api/me/exports                      their own requests
//	POST   /api/gdpr/subjects/{id}/export       superuser: export for any auth record
//	GET    /api/gdpr/exports                    superuser: all requests (?subject=<id>)
//	GET    /api/gdpr/exports/{id}               status (the requester, the subject, or a superuser)
//	GET    /api/gdpr/exports/{id}/download      the ZIP, once (session or ?token= file token)
//	DELETE /api/gdpr/exports/{id}               cancel a queued request, or discard the file
//
// API keys are refused on all of them (enforceAPIKeyScopes). Self service needs
// a fresh passkey assertion, or the account password when there is no passkey.
// See docs/data-export.md.

// The limits come from Settings.DataExport. The limit is part of the limiter
// key, so a changed setting takes effect at once (counters start over).
func dataExportRateRule(max int) core.RateLimitRule {
	return core.RateLimitRule{MaxRequests: max, Duration: 3600}
}

func bindDataExportApi(_ core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	me := rg.Group("/me")
	me.POST("/export", dataExportSelf).Bind(RequireAuth())
	me.GET("/exports", dataExportSelfList).Bind(RequireAuth())

	g := rg.Group("/gdpr")
	g.POST("/subjects/{id}/export", dataExportAdmin).Bind(RequireSuperuserAuth())
	g.GET("/exports", dataExportAdminList).Bind(RequireSuperuserAuth())
	g.GET("/exports/{id}", dataExportStatus).Bind(RequireAuth())
	g.DELETE("/exports/{id}", dataExportDelete).Bind(RequireAuth())
	g.GET("/exports/{id}/download", dataExportDownload) // session or file token, checked inside
}

func dataExportView(app core.App, r *core.Record) map[string]any {
	var result map[string]any
	_ = r.UnmarshalJSONField("result", &result)

	collection := r.GetString("collectionRef")
	if c, err := app.FindCachedCollectionByNameOrId(collection); err == nil {
		collection = c.Name
	}

	return map[string]any{
		"id":                r.Id,
		"type":              r.GetString("type"),
		"state":             r.GetString("state"),
		"subjectCollection": collection,
		"subjectId":         r.GetString("recordRef"),
		"requestedBy":       r.GetString("requestedBy"),
		"selfService":       r.GetBool("selfService"),
		"created":           r.GetString("created"),
		"dueAt":             r.GetString("dueAt"),
		"startedAt":         r.GetString("startedAt"),
		"completedAt":       r.GetString("completedAt"),
		"expires":           r.GetString("expires"),
		"downloadedAt":      r.GetString("downloadedAt"),
		"error":             r.GetString("error"),
		"result":            result,
		"downloadable": r.GetString("file") != "" &&
			(r.GetString("state") == core.DataRequestStateDone || r.GetString("state") == core.DataRequestStatePartial),
	}
}

type dataExportSelfForm struct {
	Password string `form:"password" json:"password"`
}

func dataExportSelf(e *core.RequestEvent) error {
	if APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}
	if e.Auth.IsSuperuser() {
		return e.ForbiddenError("Superusers request exports through /api/gdpr/subjects/{id}/export.", nil)
	}

	selfMax := core.DataExportSelfRequestsPerHour(e.App)
	if err := checkRateLimit(e, "@pb_gdpr_export_"+strconv.Itoa(selfMax)+"_"+e.Auth.Id, dataExportRateRule(selfMax)); err != nil {
		return e.TooManyRequestsError("Too many requests, please try again later.", nil)
	}

	// step-up: a stolen or left-open session must not be able to pull the
	// whole record of a person
	if !HasFreshPasskey(e, webauthnStepUpMaxAge) {
		form := &dataExportSelfForm{}
		_ = e.BindBody(form) // an empty body is fine, the check below reports it

		n, err := e.App.CountRecords(core.CollectionNameWebAuthnCredentials, dbx.HashExp{
			"collectionRef": e.Auth.Collection().Id,
			"recordRef":     e.Auth.Id,
		})
		if err != nil {
			return e.InternalServerError("Failed to check the account passkeys.", err)
		}
		if n > 0 {
			return e.ForbiddenError("A recent passkey verification is required for this action.", validation.Errors{
				"passkey": validation.NewError(StepUpRequiredCode, "A recent passkey verification is required."),
			})
		}
		if form.Password == "" || !e.Auth.ValidatePassword(form.Password) {
			return e.BadRequestError("An error occurred while validating the submitted data.", validation.Errors{
				"password": validation.NewError("validation_invalid_password", "Missing or invalid password."),
			})
		}
	}

	return startDataExport(e, e.Auth, e.Auth, true)
}

type dataExportAdminForm struct {
	Collection string `form:"collection" json:"collection"`
}

func dataExportAdmin(e *core.RequestEvent) error {
	if APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}

	adminMax := core.DataExportAdminRequestsPerHour(e.App)
	if err := checkRateLimit(e, "@pb_gdpr_export_admin_"+strconv.Itoa(adminMax)+"_"+e.Auth.Id, dataExportRateRule(adminMax)); err != nil {
		return e.TooManyRequestsError("Too many requests, please try again later.", nil)
	}

	form := &dataExportAdminForm{}
	_ = e.BindBody(form)

	id := e.Request.PathValue("id")

	var subject *core.Record
	if form.Collection != "" {
		col, err := e.App.FindCachedCollectionByNameOrId(form.Collection)
		if err != nil || !col.IsAuth() {
			return e.BadRequestError("The collection must be an auth collection.", nil)
		}
		subject, _ = e.App.FindRecordById(col, id)
	} else {
		collections, err := e.App.FindAllCollections(core.CollectionTypeAuth)
		if err != nil {
			return e.InternalServerError("", err)
		}
		for _, col := range collections {
			if r, err := e.App.FindRecordById(col, id); err == nil {
				if subject != nil {
					return e.BadRequestError("The id exists in several auth collections; pass \"collection\".", nil)
				}
				subject = r
			}
		}
	}
	if subject == nil {
		return e.NotFoundError("", nil)
	}

	return startDataExport(e, subject, e.Auth, false)
}

func startDataExport(e *core.RequestEvent, subject, requester *core.Record, selfService bool) error {
	req, err := core.RequestDataExport(e.App, subject, requester, selfService, e.RealIP())
	if errors.Is(err, core.ErrDataExportActive) {
		return e.JSON(http.StatusConflict, map[string]any{
			"status":  http.StatusConflict,
			"message": "An export for this person is already in progress.",
			"data":    map[string]any{},
		})
	}
	if err != nil {
		return e.InternalServerError("Failed to start the export.", err)
	}

	return e.JSON(http.StatusAccepted, dataExportView(e.App, req))
}

func dataExportSelfList(e *core.RequestEvent) error {
	return listDataExports(e, dbx.HashExp{
		"collectionRef": e.Auth.Collection().Id,
		"recordRef":     e.Auth.Id,
	})
}

func dataExportAdminList(e *core.RequestEvent) error {
	if s := e.Request.URL.Query().Get("subject"); s != "" {
		return listDataExports(e, dbx.HashExp{"recordRef": s})
	}
	return listDataExports(e, dbx.NewExp("1=1"))
}

func listDataExports(e *core.RequestEvent, exp dbx.Expression) error {
	if APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}

	var records []*core.Record
	err := e.App.RecordQuery(core.CollectionNameDataRequests).
		AndWhere(dbx.HashExp{"type": core.DataRequestTypeExport}).
		AndWhere(exp).
		OrderBy("created DESC").
		Limit(200).
		All(&records)
	if err != nil {
		return e.InternalServerError("Failed to load the export requests.", err)
	}

	items := make([]map[string]any, 0, len(records))
	for _, r := range records {
		items = append(items, dataExportView(e.App, r))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

// findDataExport loads the request and checks that auth may see it: a
// superuser, or the person who asked. Anything else is a 404.
func findDataExport(e *core.RequestEvent, auth *core.Record) (*core.Record, error) {
	req, err := e.App.FindRecordById(core.CollectionNameDataRequests, e.Request.PathValue("id"))
	if err != nil || req.GetString("type") != core.DataRequestTypeExport {
		return nil, e.NotFoundError("", nil)
	}

	if auth == nil {
		return nil, e.NotFoundError("", nil)
	}
	if !auth.IsSuperuser() &&
		(req.GetString("requestedBy") != auth.Id || req.GetString("requestedByCollection") != auth.Collection().Id) {
		return nil, e.NotFoundError("", nil)
	}

	return req, nil
}

func dataExportStatus(e *core.RequestEvent) error {
	if APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}

	req, err := findDataExport(e, e.Auth)
	if err != nil {
		return err
	}

	return e.JSON(http.StatusOK, dataExportView(e.App, req))
}

func dataExportDelete(e *core.RequestEvent) error {
	if APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}

	req, err := findDataExport(e, e.Auth)
	if err != nil {
		return err
	}

	switch req.GetString("state") {
	case core.DataRequestStateQueued:
		req.Set("state", core.DataRequestStateCancelled)
	case core.DataRequestStateDone, core.DataRequestStatePartial:
		req.Set("state", core.DataRequestStateExpired)
		req.Set("file", nil)
	default:
		return e.BadRequestError("The request is not in a state that can be cancelled.", nil)
	}

	if err := e.App.Save(req); err != nil {
		return e.InternalServerError("Failed to update the request.", err)
	}

	return e.JSON(http.StatusOK, dataExportView(e.App, req))
}

// dataExportDownload sends the ZIP once. The caller proves who they are with
// the session (Authorization header) or with a file token (pb.files.getToken()),
// so a plain link works in a browser. After the file was sent in full it is
// deleted and the download is audited.
func dataExportDownload(e *core.RequestEvent) error {
	if APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}

	auth := e.Auth
	if auth == nil {
		if token := e.Request.URL.Query().Get("token"); token != "" {
			auth, _ = e.App.FindAuthRecordByToken(token, core.TokenTypeFile)
			if auth != nil && auth.IsSuperuser() {
				if allowed := e.App.Settings().SuperuserIPs; len(allowed) > 0 && !isIPInList(allowed, e.RealIP()) {
					auth = nil
				}
			}
		}
	}

	req, err := findDataExport(e, auth)
	if err != nil {
		return err
	}

	r, size, err := core.OpenDataExportFile(e.App, req)
	if errors.Is(err, core.ErrDataExportUnavailable) {
		return e.NotFoundError("The export is not available (not finished, expired, or already downloaded).", nil)
	}
	if err != nil {
		return e.InternalServerError("Failed to open the export.", err)
	}
	defer r.Close()

	h := e.Response.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", `attachment; filename="export-`+req.GetString("recordRef")+`.zip"`)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if size > 0 {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
	}

	e.Response.WriteHeader(http.StatusOK)

	n, copyErr := io.Copy(e.Response, r)
	if copyErr != nil || (size > 0 && n != size) {
		// not delivered in full: keep the file so the download can be retried
		return nil
	}

	if err := core.FinishDataExportDownload(e.App, req, auth, e.RealIP()); err != nil {
		e.App.Logger().Error("Failed to record the data export download", "request", req.Id, "error", err)
	}

	return nil
}
