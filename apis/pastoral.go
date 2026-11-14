package apis

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #86): pastoral notes, break-glass access and the held
// notes of the data export. See docs/pastoral-notes.md.
//
//	POST /api/pastoral                              {name?, orgs?, rules?, allowLooseRules?}; superuser; creates the pastoral notes collection with the locked rules
//	POST /api/breakglass                            {collection, recordId, reason, basis, hours?}; a holder of the breakglass tag; fresh passkey; a request, not access
//	GET  /api/breakglass?org=<id>                   owners (with reasons) and holders (without); no org: the caller's own grants
//	POST /api/breakglass/{id}/approve               a DIFFERENT holder; fresh passkey; tells the author and the owners, then opens the grant
//	POST /api/breakglass/{id}/revoke                the org owner, the note's author, or the grantee
//	GET  /api/gdpr/pastoral-holds?org=<id>          an owner of the org: notes held back from a person's export
//	GET  /api/gdpr/pastoral-holds/{id}              the hold with the note (a logged read)
//	POST /api/gdpr/pastoral-holds/{id}/release      {note?}; the next export of the person includes the note
//	POST /api/gdpr/pastoral-holds/{id}/withhold     {note}; the reason is required
//
// API keys are refused on the routes: every act here is by a named person.
func bindPastoralApi(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	bindPastoralRealtimeGuard(app)

	rg.POST("/pastoral", pastoralCreate).Bind(RequireAuth(), RequireSuperuserAuth())

	bg := rg.Group("/breakglass").Bind(RequireAuth(), pastoralNoAPIKey())
	bg.POST("", breakGlassRequest).Bind(RequireFreshPasskey(webauthnStepUpMaxAge))
	bg.GET("", breakGlassList)
	bg.POST("/{id}/approve", breakGlassApprove).Bind(RequireFreshPasskey(webauthnStepUpMaxAge))
	bg.POST("/{id}/revoke", breakGlassRevoke)

	holds := rg.Group("/gdpr/pastoral-holds").Bind(RequireAuth(), pastoralNoAPIKey())
	holds.GET("", pastoralHoldList)
	holds.GET("/{id}", pastoralHoldReview)
	holds.POST("/{id}/release", pastoralHoldRelease)
	holds.POST("/{id}/withhold", pastoralHoldWithhold)
}

func pastoralNoAPIKey() *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id: "claviger_pastoral_no_api_key",
		Func: func(e *core.RequestEvent) error {
			if APIKeyFromRequest(e) != nil {
				return apiKeyForbidden(e)
			}

			return e.Next()
		},
	}
}

// ---- template -----------------------------------------------------------------

type pastoralCreateBody struct {
	Name            string           `json:"name" form:"name"`
	Orgs            []string         `json:"orgs" form:"orgs"`
	Rules           *core.OrgRuleSet `json:"rules" form:"rules"`
	AllowLooseRules bool             `json:"allowLooseRules" form:"allowLooseRules"`
}

func pastoralCreate(e *core.RequestEvent) error {
	var body pastoralCreateBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	col, err := core.CreatePastoralNotes(e.App, core.PastoralNotesOpts{
		Name: body.Name, Rules: body.Rules, AllowLooseRules: body.AllowLooseRules,
	})
	if err != nil {
		return e.BadRequestError("Failed to create the pastoral notes collection: "+err.Error(), nil)
	}

	tags := map[string][]string{}

	for _, ref := range body.Orgs {
		org, err := core.FindOrg(e.App, ref)
		if err != nil {
			return e.BadRequestError("Unknown org "+ref+".", nil)
		}

		made, err := core.EnsurePastoralTags(e.App, auditContext(e), org)
		if err != nil {
			return orgHTTP(e, err)
		}

		tags[org.Id] = made
	}

	return e.JSON(http.StatusCreated, map[string]any{
		"collection": col.Name, "tags": core.PastoralTags, "createdTags": tags,
		"listRule": col.ListRule, "viewRule": col.ViewRule, "createRule": col.CreateRule,
		"updateRule": col.UpdateRule, "deleteRule": col.DeleteRule,
	})
}

// ---- break-glass -----------------------------------------------------------------

type breakGlassBody struct {
	Collection string `json:"collection" form:"collection"`
	RecordId   string `json:"recordId" form:"recordId"`
	Reason     string `json:"reason" form:"reason"`
	Basis      string `json:"basis" form:"basis"`
	Hours      int    `json:"hours" form:"hours"`
}

func breakGlassJSON(v core.BreakGlassView) map[string]any {
	g := v.Record

	out := map[string]any{
		"id":              g.Id,
		"org":             g.GetString("org"),
		"collection":      g.GetString("collection"),
		"recordId":        g.GetString("recordId"),
		"grantee":         g.GetString("grantee"),
		"basis":           g.GetString("basis"),
		"state":           v.State,
		"hours":           g.GetInt("hours"),
		"requestedAt":     g.GetDateTime("requestedAt"),
		"activeFrom":      g.GetDateTime("activeFrom"),
		"expires":         g.GetDateTime("expires"),
		"approvedBy":      g.GetString("approvedBy"),
		"ownerNotifiedAt": g.GetDateTime("ownerNotifiedAt"),
		"revoked":         g.GetBool("revoked"),
		"revokedBy":       g.GetString("revokedBy"),
		"revokedAt":       g.GetDateTime("revokedAt"),
		"readCount":       g.GetInt("readCount"),
	}

	if v.WithReason {
		out["reason"] = g.GetString("reason")
	}

	return out
}

func breakGlassOne(g *core.Record, withReason bool) map[string]any {
	return breakGlassJSON(core.BreakGlassView{Record: g, State: core.BreakGlassState(g, time.Now().UTC()), WithReason: withReason})
}

func breakGlassRequest(e *core.RequestEvent) error {
	var body breakGlassBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	g, err := core.RequestBreakGlass(e.App, auditContext(e), e.Auth, core.BreakGlassInput{
		Collection: body.Collection, RecordId: body.RecordId, Reason: body.Reason, Basis: body.Basis, Hours: body.Hours,
	})
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusCreated, breakGlassOne(g, true))
}

func breakGlassList(e *core.RequestEvent) error {
	views, err := core.ListBreakGlass(e.App, e.Auth, e.Request.URL.Query().Get("org"))
	if err != nil {
		return orgHTTP(e, err)
	}

	items := make([]map[string]any, 0, len(views))
	for _, v := range views {
		items = append(items, breakGlassJSON(v))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func breakGlassApprove(e *core.RequestEvent) error {
	g, err := core.ApproveBreakGlass(e.App, auditContext(e), e.Auth, e.Request.PathValue("id"))
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, breakGlassOne(g, false))
}

func breakGlassRevoke(e *core.RequestEvent) error {
	g, err := core.RevokeBreakGlass(e.App, auditContext(e), e.Auth, e.Request.PathValue("id"))
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, breakGlassOne(g, g.GetString("grantee") == e.Auth.Id))
}

// ---- held notes ---------------------------------------------------------------------

func pastoralHoldJSON(h *core.Record) map[string]any {
	return map[string]any{
		"id": h.Id, "org": h.GetString("org"), "collection": h.GetString("collection"),
		"recordId": h.GetString("recordId"), "subject": h.GetString("subject"), "state": h.GetString("state"),
		"heldAt": h.GetDateTime("heldAt"), "decidedBy": h.GetString("decidedBy"),
		"decidedAt": h.GetDateTime("decidedAt"), "note": h.GetString("note"),
	}
}

func pastoralHoldList(e *core.RequestEvent) error {
	rows, err := core.ListPastoralHolds(e.App, e.Auth, e.Request.URL.Query().Get("org"), e.Request.URL.Query().Get("state"))
	if err != nil {
		return orgHTTP(e, err)
	}

	items := make([]map[string]any, 0, len(rows))
	for _, h := range rows {
		items = append(items, pastoralHoldJSON(h))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func pastoralHoldReview(e *core.RequestEvent) error {
	v, err := core.ReviewPastoralHold(e.App, auditContext(e), e.Auth, e.Request.PathValue("id"))
	if err != nil {
		return orgHTTP(e, err)
	}

	out := pastoralHoldJSON(v.Hold)
	out["note"] = v.Note.PublicExport()
	out["decisionNote"] = v.Hold.GetString("note")

	return e.JSON(http.StatusOK, out)
}

type pastoralHoldBody struct {
	Note string `json:"note" form:"note"`
}

func pastoralHoldDecide(e *core.RequestEvent, release bool) error {
	var body pastoralHoldBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	h, err := core.DecidePastoralHold(e.App, auditContext(e), e.Auth, e.Request.PathValue("id"), release, body.Note)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, pastoralHoldJSON(h))
}

func pastoralHoldRelease(e *core.RequestEvent) error  { return pastoralHoldDecide(e, true) }
func pastoralHoldWithhold(e *core.RequestEvent) error { return pastoralHoldDecide(e, false) }

// ---- realtime ---------------------------------------------------------------------

// bindPastoralRealtimeGuard keeps sealed notes out of the realtime stream for
// everyone but their author. A realtime message carries the whole record, and
// reading it that way would bypass the read log of sealed notes (a break-glass
// grantee, or a superuser, fetches the note through the records API instead).
func bindPastoralRealtimeGuard(app core.App) {
	app.OnRealtimeMessageSend().Bind(&hook.Handler[*core.RealtimeMessageEvent]{
		Id:       "__clvPastoralRealtime__",
		Priority: -90000,
		Func: func(e *core.RealtimeMessageEvent) error {
			if e.Message == nil {
				return e.Next()
			}

			name, _, _ := strings.Cut(e.Message.Name, "/")

			col, err := e.App.FindCachedCollectionByNameOrId(name)
			if err != nil || !core.IsPastoralCollection(col) {
				return e.Next()
			}

			var msg struct {
				Record map[string]any `json:"record"`
			}

			if json.Unmarshal(e.Message.Data, &msg) != nil || msg.Record == nil {
				return e.Next()
			}

			if sealed, _ := msg.Record["sealed"].(bool); !sealed {
				return e.Next()
			}

			clientAuth, _ := e.Client.Get(RealtimeClientAuthKey).(*core.Record)
			author, _ := msg.Record["author"].(string)

			if clientAuth != nil && author == clientAuth.Id {
				return e.Next()
			}

			return nil // dropped: not delivered
		},
	})
}
