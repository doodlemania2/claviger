package apis

import (
	"errors"
	"html/template"
	"net/http"

	"ganigeorgiev.com/pocketbase/dbx"
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #88, 88-4): erasure of a person.
//
//	POST /api/me/erasure                    {member?, password?}: ask for the erasure of my own account, or of a
//	                                        child of my household (member; a guardian). Needs a fresh passkey (or
//	                                        the password when there is no passkey). A confirmation link is emailed.
//	GET  /api/me/erasure                    my requests (those I made and those about me)
//	POST /api/me/erasure/{id}/cancel        cancel one (a fresh passkey, or the password)
//	POST /api/gdpr/subjects/{id}/erase      superuser, fresh passkey: {collection?, identityCheck?,
//	                                        requireConfirmation?, graceDays?}. Without requireConfirmation the
//	                                        identityCheck (how the person was identified) is required and kept in meta.
//	GET  /api/gdpr/erasures                 superuser: the requests (?subject=<id>&state=...)
//	GET  /api/gdpr/erasures/{id}            superuser
//	POST /api/gdpr/erasures/{id}/cancel     superuser, fresh passkey
//	POST /api/gdpr/erasure/confirm          {token}: the emailed link (no sign-in)
//	POST /api/gdpr/erasure/cancel           {token}: the cancel link of the grace email (no sign-in)
//	GET/POST /erasure/confirm, /erasure/cancel   the hosted pages the emailed links open (a button, no script)
//
// A person can also cancel by signing in with a passkey while the account is
// locked (recordAuthResponse). API keys are refused on all of them. See
// docs/erasure.md.
func bindErasureApi(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	me := rg.Group("/me/erasure").Bind(RequireAuth(), pastoralNoAPIKey())
	me.POST("", erasureSelf)
	me.GET("", erasureSelfList)
	me.POST("/{id}/cancel", erasureSelfCancel)

	g := rg.Group("/gdpr")
	g.POST("/subjects/{id}/erase", erasureAdmin).Bind(RequireSuperuserAuth(), pastoralNoAPIKey(), RequireFreshPasskey(webauthnStepUpMaxAge))
	g.GET("/erasures", erasureAdminList).Bind(RequireSuperuserAuth(), pastoralNoAPIKey())
	g.GET("/erasures/{id}", erasureAdminView).Bind(RequireSuperuserAuth(), pastoralNoAPIKey())
	g.POST("/erasures/{id}/cancel", erasureAdminCancel).Bind(RequireSuperuserAuth(), pastoralNoAPIKey(), RequireFreshPasskey(webauthnStepUpMaxAge))
	g.POST("/erasure/confirm", erasureConfirmAPI)
	g.POST("/erasure/cancel", erasureCancelAPI)

	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id:       "__clvErasurePages__",
		Priority: 9998,
		Func: func(se *core.ServeEvent) error {
			se.Router.GET("/erasure/confirm", erasurePageConfirm)
			se.Router.POST("/erasure/confirm", erasurePageConfirmPost)
			se.Router.GET("/erasure/cancel", erasurePageCancel)
			se.Router.POST("/erasure/cancel", erasurePageCancelPost)

			return se.Next()
		},
	})
}

func erasureHTTP(e *core.RequestEvent, err error) error {
	var ee *core.ErasureError
	if errors.As(err, &ee) {
		data := map[string]any{"code": ee.Code}
		if len(ee.Ids) > 0 {
			data["ids"] = ee.Ids
		}

		return e.JSON(ee.Status, map[string]any{"status": ee.Status, "message": ee.Message, "data": data})
	}

	return e.InternalServerError("", err)
}

// erasureView is the request as the API shows it. The ids of kept rows and the
// hold ids are for superusers; the identity check is kept in meta for staff.
func erasureView(app core.App, r *core.Record, staff bool) map[string]any {
	var result map[string]any
	_ = r.UnmarshalJSONField("result", &result)

	var meta map[string]any
	_ = r.UnmarshalJSONField("meta", &meta)

	out := map[string]any{
		"id":          r.Id,
		"type":        r.GetString("type"),
		"state":       r.GetString("state"),
		"mode":        meta["mode"],
		"selfService": r.GetBool("selfService"),
		"created":     r.GetString("created"),
		"dueAt":       r.GetString("dueAt"),
		"confirmedAt": r.GetString("confirmedAt"),
		"graceUntil":  r.GetString("graceUntil"),
		"cancelledAt": r.GetString("cancelledAt"),
		"completedAt": r.GetString("completedAt"),
		"error":       r.GetString("error"),
	}

	collection := r.GetString("collectionRef")
	if c, err := app.FindCachedCollectionByNameOrId(collection); err == nil {
		collection = c.Name
	}

	out["subjectCollection"] = collection
	out["subjectId"] = r.GetString("recordRef")

	if !staff && result != nil {
		delete(result, "holds")

		if kept, ok := result["retained"].([]any); ok {
			for _, k := range kept {
				if m, ok := k.(map[string]any); ok {
					delete(m, "ids")
				}
			}
		}
	}

	out["result"] = result

	if staff {
		out["requestedBy"] = r.GetString("requestedBy")
		out["identityCheck"] = meta["identityCheck"]
	}

	return out
}

// ---- self service ------------------------------------------------------------

type erasureSelfForm struct {
	Member   string `form:"member" json:"member"`
	Password string `form:"password" json:"password"`
}

// erasureStepUp requires a fresh passkey, or the account password when the
// account has no passkey (the rule of the personal data export).
func erasureStepUp(e *core.RequestEvent, password string) error {
	if HasFreshPasskey(e, webauthnStepUpMaxAge) {
		return nil
	}

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

	if password == "" || !e.Auth.ValidatePassword(password) {
		return e.BadRequestError("An error occurred while validating the submitted data.", validation.Errors{
			"password": validation.NewError("validation_invalid_password", "Missing or invalid password."),
		})
	}

	return nil
}

func erasureSelf(e *core.RequestEvent) error {
	if e.Auth.IsSuperuser() {
		return e.ForbiddenError("Superusers start erasures through /api/gdpr/subjects/{id}/erase.", nil)
	}

	if err := checkRateLimit(e, "@pb_gdpr_erase_"+e.Auth.Id, dataExportRateRule(10)); err != nil {
		return e.TooManyRequestsError("Too many requests, please try again later.", nil)
	}

	form := &erasureSelfForm{}
	_ = e.BindBody(form)

	if err := erasureStepUp(e, form.Password); err != nil {
		return err
	}

	subject, mode := e.Auth, core.ErasureModeSelf

	if form.Member != "" {
		m, err := e.App.FindRecordById(core.CollectionNameHouseholdMembers, form.Member)
		if err != nil {
			return e.NotFoundError("", nil)
		}

		subject, mode = m, core.ErasureModeGuardian
	}

	req, err := core.RequestErasure(e.App, core.ErasureRequestInput{
		Subject: subject, Requester: e.Auth, Mode: mode, IP: e.RealIP(), Ctx: auditContext(e),
	})
	if err != nil {
		return erasureHTTP(e, err)
	}

	return e.JSON(http.StatusAccepted, erasureView(e.App, req, false))
}

func erasureSelfList(e *core.RequestEvent) error {
	var rows []*core.Record

	err := e.App.RecordQuery(core.CollectionNameDataRequests).
		AndWhere(dbx.HashExp{"type": core.DataRequestTypeErase}).
		AndWhere(dbx.Or(
			dbx.HashExp{"collectionRef": e.Auth.Collection().Id, "recordRef": e.Auth.Id},
			dbx.HashExp{"requestedByCollection": e.Auth.Collection().Id, "requestedBy": e.Auth.Id},
		)).
		OrderBy("created DESC").Limit(50).All(&rows)
	if err != nil {
		return e.InternalServerError("Failed to load the requests.", err)
	}

	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, erasureView(e.App, r, false))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func erasureSelfCancel(e *core.RequestEvent) error {
	req, err := e.App.FindRecordById(core.CollectionNameDataRequests, e.Request.PathValue("id"))
	if err != nil || req.GetString("type") != core.DataRequestTypeErase {
		return e.NotFoundError("", nil)
	}

	mine := (req.GetString("collectionRef") == e.Auth.Collection().Id && req.GetString("recordRef") == e.Auth.Id) ||
		(req.GetString("requestedByCollection") == e.Auth.Collection().Id && req.GetString("requestedBy") == e.Auth.Id)
	if !mine {
		return e.NotFoundError("", nil)
	}

	form := &erasureSelfForm{}
	_ = e.BindBody(form)

	if err := erasureStepUp(e, form.Password); err != nil {
		return err
	}

	if err := core.CancelErasure(e.App, auditContext(e), req, e.Auth, "session", e.RealIP()); err != nil {
		return erasureHTTP(e, err)
	}

	fresh, _ := e.App.FindRecordById(core.CollectionNameDataRequests, req.Id)

	return e.JSON(http.StatusOK, erasureView(e.App, fresh, false))
}

// ---- superusers ---------------------------------------------------------------

type erasureAdminForm struct {
	Collection          string `form:"collection" json:"collection"`
	IdentityCheck       string `form:"identityCheck" json:"identityCheck"`
	RequireConfirmation bool   `form:"requireConfirmation" json:"requireConfirmation"`
	GraceDays           *int   `form:"graceDays" json:"graceDays"`
}

func erasureAdmin(e *core.RequestEvent) error {
	form := &erasureAdminForm{}
	_ = e.BindBody(form)

	id := e.Request.PathValue("id")

	var subject *core.Record

	find := func(col *core.Collection) error {
		if r, err := e.App.FindRecordById(col, id); err == nil {
			if subject != nil {
				return e.BadRequestError("The id exists in several collections; pass \"collection\".", nil)
			}

			subject = r
		}

		return nil
	}

	if form.Collection != "" {
		col, err := e.App.FindCachedCollectionByNameOrId(form.Collection)
		if err != nil || (!col.IsAuth() && col.Name != core.CollectionNameHouseholdMembers) {
			return e.BadRequestError("The collection must be an auth collection or the household members.", nil)
		}

		subject, _ = e.App.FindRecordById(col, id)
	} else {
		cols, err := e.App.FindAllCollections(core.CollectionTypeAuth)
		if err != nil {
			return e.InternalServerError("", err)
		}

		if hm, err := e.App.FindCachedCollectionByNameOrId(core.CollectionNameHouseholdMembers); err == nil {
			cols = append(cols, hm)
		}

		for _, col := range cols {
			if err := find(col); err != nil {
				return err
			}
		}
	}

	if subject == nil {
		return e.NotFoundError("", nil)
	}

	req, err := core.RequestErasure(e.App, core.ErasureRequestInput{
		Subject: subject, Requester: e.Auth, Mode: core.ErasureModeAdmin, IP: e.RealIP(), Ctx: auditContext(e),
		IdentityCheck: form.IdentityCheck, RequireConfirmation: form.RequireConfirmation, GraceDays: form.GraceDays,
	})
	if err != nil {
		return erasureHTTP(e, err)
	}

	return e.JSON(http.StatusAccepted, erasureView(e.App, req, true))
}

func erasureAdminList(e *core.RequestEvent) error {
	q := e.App.RecordQuery(core.CollectionNameDataRequests).AndWhere(dbx.HashExp{"type": core.DataRequestTypeErase})

	if s := e.Request.URL.Query().Get("subject"); s != "" {
		q = q.AndWhere(dbx.HashExp{"recordRef": s})
	}

	if s := e.Request.URL.Query().Get("state"); s != "" {
		q = q.AndWhere(dbx.HashExp{"state": s})
	}

	var rows []*core.Record
	if err := q.OrderBy("created DESC").Limit(200).All(&rows); err != nil {
		return e.InternalServerError("Failed to load the requests.", err)
	}

	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, erasureView(e.App, r, true))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func erasureAdminView(e *core.RequestEvent) error {
	r, err := e.App.FindRecordById(core.CollectionNameDataRequests, e.Request.PathValue("id"))
	if err != nil || r.GetString("type") != core.DataRequestTypeErase {
		return e.NotFoundError("", nil)
	}

	return e.JSON(http.StatusOK, erasureView(e.App, r, true))
}

func erasureAdminCancel(e *core.RequestEvent) error {
	r, err := e.App.FindRecordById(core.CollectionNameDataRequests, e.Request.PathValue("id"))
	if err != nil || r.GetString("type") != core.DataRequestTypeErase {
		return e.NotFoundError("", nil)
	}

	if err := core.CancelErasure(e.App, auditContext(e), r, e.Auth, "admin", e.RealIP()); err != nil {
		return erasureHTTP(e, err)
	}

	fresh, _ := e.App.FindRecordById(core.CollectionNameDataRequests, r.Id)

	return e.JSON(http.StatusOK, erasureView(e.App, fresh, true))
}

// ---- the emailed links ------------------------------------------------------------

type erasureTokenForm struct {
	Token string `form:"token" json:"token"`
}

func erasureLinkLimit(e *core.RequestEvent) error {
	if err := checkRateLimit(e, "@pb_gdpr_erase_link_"+e.RealIP(), core.RateLimitRule{MaxRequests: 20, Duration: 3600}); err != nil {
		return e.TooManyRequestsError("Too many requests, please try again later.", nil)
	}

	return nil
}

func erasureConfirmAPI(e *core.RequestEvent) error {
	if err := erasureLinkLimit(e); err != nil {
		return err
	}

	form := &erasureTokenForm{}
	_ = e.BindBody(form)

	req, err := core.ConfirmErasure(e.App, core.WithAuditContext(e.Request.Context(), core.AuditActor{Type: core.AuditActorTypeSystem}, e.RealIP()), form.Token, e.RealIP())
	if err != nil {
		return erasureHTTP(e, err)
	}

	return e.JSON(http.StatusOK, erasureView(e.App, req, false))
}

func erasureCancelAPI(e *core.RequestEvent) error {
	if err := erasureLinkLimit(e); err != nil {
		return err
	}

	form := &erasureTokenForm{}
	_ = e.BindBody(form)

	req, err := core.CancelErasureByToken(e.App, core.WithAuditContext(e.Request.Context(), core.AuditActor{Type: core.AuditActorTypeSystem}, e.RealIP()), form.Token, e.RealIP())
	if err != nil {
		return erasureHTTP(e, err)
	}

	return e.JSON(http.StatusOK, erasureView(e.App, req, false))
}

type erasurePageData struct {
	Text                         *PageText
	Title, Heading, Message      string
	Intro, Button, Action, Token string
	OK                           bool
}

var erasurePageTemplates = template.Must(template.Must(orgsPageTemplates.Clone()).Parse(`
{{define "erasurelink"}}{{template "head" .}}
<main class="card" id="app" data-page="erasure">
<h1>{{.Heading}}</h1>
{{if .Message}}<p class="msg {{if .OK}}ok{{else}}err{{end}}" role="{{if .OK}}status{{else}}alert{{end}}">{{.Message}}</p>{{end}}
{{if .Token}}<p class="sub">{{.Intro}}</p>
<form method="post" action="{{.Action}}">
<input type="hidden" name="token" value="{{.Token}}">
<button type="submit">{{.Button}}</button>
</form>{{end}}
</main>
</body></html>{{end}}
`))

// erasurePage renders the link page. kind is "confirm" or "cancel". With a
// token the page offers the button; "done" and "invalid" show the outcome. A GET
// never changes anything: a mail scanner that opens the link does nothing.
func erasurePage(e *core.RequestEvent, kind, token, outcome string, status int) error {
	orgsPageHeaders(e)

	text := NewPageText(e, "")
	title := text.T("erasure." + kind + ".title")

	d := erasurePageData{Text: text, Title: title, Heading: title}

	switch outcome {
	case "done":
		d.Message, d.OK = text.T("erasure."+kind+".done"), true
	case "invalid":
		d.Message = text.T("erasure.invalid")
	default:
		if token == "" {
			d.Message = text.T("erasure.invalid")
		} else {
			d.Token = token
			d.Intro = text.T("erasure." + kind + ".intro")
			d.Button = text.T("erasure." + kind + ".button")
			d.Action = "/erasure/" + kind
		}
	}

	e.Response.WriteHeader(status)

	return erasurePageTemplates.ExecuteTemplate(e.Response, "erasurelink", d)
}

func erasurePageConfirm(e *core.RequestEvent) error {
	return erasurePage(e, "confirm", e.Request.URL.Query().Get("token"), "", http.StatusOK)
}

func erasurePageCancel(e *core.RequestEvent) error {
	return erasurePage(e, "cancel", e.Request.URL.Query().Get("token"), "", http.StatusOK)
}

func erasurePageConfirmPost(e *core.RequestEvent) error {
	if err := erasureLinkLimit(e); err != nil {
		return err
	}

	if err := e.Request.ParseForm(); err != nil {
		return erasurePage(e, "confirm", "", "invalid", http.StatusBadRequest)
	}

	ctx := core.WithAuditContext(e.Request.Context(), core.AuditActor{Type: core.AuditActorTypeSystem}, e.RealIP())

	if _, err := core.ConfirmErasure(e.App, ctx, e.Request.PostForm.Get("token"), e.RealIP()); err != nil {
		return erasurePage(e, "confirm", "", "invalid", http.StatusBadRequest)
	}

	return erasurePage(e, "confirm", "", "done", http.StatusOK)
}

func erasurePageCancelPost(e *core.RequestEvent) error {
	if err := erasureLinkLimit(e); err != nil {
		return err
	}

	if err := e.Request.ParseForm(); err != nil {
		return erasurePage(e, "cancel", "", "invalid", http.StatusBadRequest)
	}

	ctx := core.WithAuditContext(e.Request.Context(), core.AuditActor{Type: core.AuditActorTypeSystem}, e.RealIP())

	if _, err := core.CancelErasureByToken(e.App, ctx, e.Request.PostForm.Get("token"), e.RealIP()); err != nil {
		return erasurePage(e, "cancel", "", "invalid", http.StatusBadRequest)
	}

	return erasurePage(e, "cancel", "", "done", http.StatusOK)
}
