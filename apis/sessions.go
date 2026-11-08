package apis

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #93): session and ban endpoints.
//
//	GET    /api/sessions                      own active sessions, the current one marked
//	                                          (superuser: ?collection=&record= for any record, ?all=true adds dead ones)
//	DELETE /api/sessions/{id}                 revoke one (own, or any for a superuser)
//	POST   /api/sessions/revoke-others        revoke all own sessions except the current one
//	POST   /api/sessions/revoke-all           superuser: revoke every session of a record
//	GET    /api/bans                          superuser: list bans (?collection=&record=)
//	POST   /api/bans                          superuser: ban (upsert) a record
//	DELETE /api/bans/{id}                     superuser: unban
//
// None of them is reachable with an API key: keys never manage sessions.

func bindSessionsApi(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	sessions := rg.Group("/sessions")
	sessions.GET("", sessionsList).Bind(RequireAuth())
	sessions.DELETE("/{id}", sessionRevoke).Bind(RequireAuth())
	sessions.POST("/revoke-others", sessionsRevokeOthers).Bind(RequireAuth())
	sessions.POST("/revoke-all", sessionsRevokeAll).Bind(RequireSuperuserAuth())

	bans := rg.Group("/bans")
	bans.GET("", bansList).Bind(RequireSuperuserAuth())
	bans.POST("", banCreate).Bind(RequireSuperuserAuth())
	bans.DELETE("/{id}", banDelete).Bind(RequireSuperuserAuth())

	// the same rules for bans created through the records API
	app.OnRecordCreateRequest(core.CollectionNameBans).BindFunc(func(e *core.RecordRequestEvent) error {
		target, err := e.App.FindRecordById(e.Record.GetString("collectionRef"), e.Record.GetString("recordRef"))
		if err != nil {
			return e.Next() // the ref validation reports it
		}
		if err := authorizeBan(e.Auth, target); err != nil {
			return e.ForbiddenError(strings.ToUpper(err.Error()[:1])+err.Error()[1:]+".", nil)
		}
		return e.Next()
	})
}

// authorizeBan applies the ban policy: superusers can be banned only by
// superusers, and nobody can ban themselves.
func authorizeBan(actor, target *core.Record) error {
	if actor == nil || !actor.IsSuperuser() {
		return errors.New("only superusers can ban accounts")
	}
	if actor.Id == target.Id && actor.Collection().Id == target.Collection().Id {
		return errors.New("you cannot ban your own account")
	}
	return nil
}

func sessionView(r *core.Record, currentSid string, withOwner bool) map[string]any {
	now := time.Now()
	expires := r.GetDateTime("expires")
	m := map[string]any{
		"id":            r.Id,
		"created":       r.GetString("created"),
		"lastSeen":      r.GetString("lastSeen"),
		"expires":       r.GetString("expires"),
		"userAgent":     r.GetString("userAgent"),
		"ip":            r.GetString("ip"),
		"authMethod":    r.GetString("authMethod"),
		"revoked":       r.GetBool("revoked"),
		"revokedReason": r.GetString("revokedReason"),
		"active":        !r.GetBool("revoked") && expires.Time().After(now),
		"current":       currentSid != "" && r.Id == currentSid,
	}
	if withOwner {
		m["collectionId"] = r.GetString("collectionRef")
		m["recordId"] = r.GetString("recordRef")
	}
	return m
}

// resolveTarget finds the auth record named by the collection and record
// parameters (collection name or id).
func resolveTarget(app core.App, collection, record string) (*core.Record, error) {
	c, err := app.FindCachedCollectionByNameOrId(collection)
	if err != nil || !c.IsAuth() {
		return nil, errors.New("missing or invalid auth collection")
	}
	r, err := app.FindRecordById(c, record)
	if err != nil {
		return nil, errors.New("record not found")
	}
	return r, nil
}

func refuseAPIKey(e *core.RequestEvent) error {
	if APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}
	return nil
}

func sessionsList(e *core.RequestEvent) error {
	if err := refuseAPIKey(e); err != nil {
		return err
	}

	q := e.Request.URL.Query()
	collectionId, recordId := e.Auth.Collection().Id, e.Auth.Id
	foreign := false

	if q.Get("collection") != "" || q.Get("record") != "" {
		if !e.Auth.IsSuperuser() {
			return e.ForbiddenError("Only superusers can list the sessions of other accounts.", nil)
		}
		target, err := resolveTarget(e.App, q.Get("collection"), q.Get("record"))
		if err != nil {
			return e.NotFoundError(err.Error(), nil)
		}
		collectionId, recordId = target.Collection().Id, target.Id
		foreign = true
	}

	exps := []dbx.Expression{dbx.HashExp{"collectionRef": collectionId, "recordRef": recordId}}
	if q.Get("all") != "true" || !e.Auth.IsSuperuser() {
		exps = append(exps,
			dbx.HashExp{"revoked": false},
			dbx.NewExp("[[expires]] > {:now}", dbx.Params{"now": time.Now().UTC().Format(types.DefaultDateLayout)}),
		)
	}

	records, err := e.App.FindAllRecords(core.CollectionNameSessions, dbx.And(exps...))
	if err != nil {
		return e.InternalServerError("Failed to load the sessions.", err)
	}

	current := SessionIdFromRequest(e)
	items := make([]map[string]any, 0, len(records))
	for i := len(records) - 1; i >= 0; i-- { // newest first
		items = append(items, sessionView(records[i], current, foreign))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func sessionRevoke(e *core.RequestEvent) error {
	if err := refuseAPIKey(e); err != nil {
		return err
	}

	record, err := e.App.FindRecordById(core.CollectionNameSessions, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("", nil)
	}

	owned := record.GetString("collectionRef") == e.Auth.Collection().Id && record.GetString("recordRef") == e.Auth.Id
	if !owned && !e.Auth.IsSuperuser() {
		return e.NotFoundError("", nil) // do not reveal other users' session ids
	}

	if !record.GetBool("revoked") {
		reason := core.SessionRevokedByUser
		if !owned {
			reason = core.SessionRevokedByAdmin
		}

		record.Set("revoked", true)
		record.Set("revokedReason", reason)
		if err := e.App.SaveWithContext(auditContext(e), record); err != nil {
			return e.InternalServerError("Failed to revoke the session.", err)
		}
	}

	return e.NoContent(http.StatusNoContent)
}

func sessionsRevokeOthers(e *core.RequestEvent) error {
	if err := refuseAPIKey(e); err != nil {
		return err
	}

	n, err := core.RevokeSessions(e.App, e.Auth.Collection().Id, e.Auth.Id, core.SessionRevokedByUser, SessionIdFromRequest(e))
	if err != nil {
		return e.InternalServerError("Failed to revoke the sessions.", err)
	}

	if err := auditAction(e, "revoke_sessions", e.Auth, map[string]any{
		"scope":   "others",
		"revoked": n,
	}); err != nil {
		return e.InternalServerError("Failed to record the action.", err)
	}

	return e.JSON(http.StatusOK, map[string]any{"revoked": n})
}

type sessionsRevokeAllForm struct {
	Collection string `form:"collection" json:"collection"`
	Record     string `form:"record" json:"record"`
	Reason     string `form:"reason" json:"reason"`

	// InvalidateAllTokens also rotates the record's token key, which kills
	// every token it ever signed, including those issued before sessions
	// existed (they have no session to revoke).
	InvalidateAllTokens bool `form:"invalidateAllTokens" json:"invalidateAllTokens"`
}

func sessionsRevokeAll(e *core.RequestEvent) error {
	if err := refuseAPIKey(e); err != nil {
		return err
	}

	form := &sessionsRevokeAllForm{}
	if err := e.BindBody(form); err != nil {
		return e.BadRequestError("An error occurred while loading the submitted data.", err)
	}
	if err := validation.ValidateStruct(form,
		validation.Field(&form.Collection, validation.Required),
		validation.Field(&form.Record, validation.Required),
		validation.Field(&form.Reason, validation.Length(0, 255)),
	); err != nil {
		return e.BadRequestError("An error occurred while validating the submitted data.", err)
	}

	target, err := resolveTarget(e.App, form.Collection, form.Record)
	if err != nil {
		return e.NotFoundError(err.Error(), nil)
	}

	reason := core.SessionRevokedByAdmin
	if r := strings.TrimSpace(form.Reason); r != "" {
		reason = core.SessionRevokedByAdmin + ": " + r
	}

	n, err := core.RevokeSessions(e.App, target.Collection().Id, target.Id, reason, "")
	if err != nil {
		return e.InternalServerError("Failed to revoke the sessions.", err)
	}

	if form.InvalidateAllTokens {
		target.RefreshTokenKey()
		if err := e.App.SaveNoValidateWithContext(auditContext(e), target); err != nil {
			return e.InternalServerError("Failed to rotate the token key.", err)
		}
	}

	if err := auditAction(e, "revoke_sessions", target, map[string]any{
		"scope":               "all",
		"revoked":             n,
		"reason":              form.Reason,
		"invalidateAllTokens": form.InvalidateAllTokens,
	}); err != nil {
		return e.InternalServerError("Failed to record the action.", err)
	}

	return e.JSON(http.StatusOK, map[string]any{"revoked": n})
}

// -------------------------------------------------------------------

func banView(r *core.Record) map[string]any {
	return map[string]any{
		"id":           r.Id,
		"collectionId": r.GetString("collectionRef"),
		"recordId":     r.GetString("recordRef"),
		"reason":       r.GetString("reason"),
		"until":        r.GetString("until"),
		"createdBy":    r.GetString("createdBy"),
		"created":      r.GetString("created"),
	}
}

func bansList(e *core.RequestEvent) error {
	q := e.Request.URL.Query()

	var exps []dbx.Expression
	if q.Get("collection") != "" || q.Get("record") != "" {
		target, err := resolveTarget(e.App, q.Get("collection"), q.Get("record"))
		if err != nil {
			return e.NotFoundError(err.Error(), nil)
		}
		exps = append(exps, dbx.HashExp{"collectionRef": target.Collection().Id, "recordRef": target.Id})
	}

	records, err := e.App.FindAllRecords(core.CollectionNameBans, exps...)
	if err != nil {
		return e.InternalServerError("Failed to load the bans.", err)
	}

	items := make([]map[string]any, 0, len(records))
	for i := len(records) - 1; i >= 0; i-- {
		items = append(items, banView(records[i]))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

type banCreateForm struct {
	Collection string `form:"collection" json:"collection"`
	Record     string `form:"record" json:"record"`
	Reason     string `form:"reason" json:"reason"`
	Until      string `form:"until" json:"until"`
}

func banCreate(e *core.RequestEvent) error {
	if err := refuseAPIKey(e); err != nil {
		return err
	}

	form := &banCreateForm{}
	if err := e.BindBody(form); err != nil {
		return e.BadRequestError("An error occurred while loading the submitted data.", err)
	}

	var until types.DateTime
	if err := validation.ValidateStruct(form,
		validation.Field(&form.Collection, validation.Required),
		validation.Field(&form.Record, validation.Required),
		validation.Field(&form.Reason, validation.Length(0, 500)),
		validation.Field(&form.Until, validation.By(func(any) error {
			if form.Until == "" {
				return nil
			}
			var err error
			until, err = types.ParseDateTime(form.Until)
			if err != nil {
				return validation.NewError("validation_invalid_date", "Must be a valid date.")
			}
			if !until.Time().After(time.Now()) {
				return validation.NewError("validation_past_date", "Must be in the future.")
			}
			return nil
		})),
	); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
	}

	target, err := resolveTarget(e.App, form.Collection, form.Record)
	if err != nil {
		return e.NotFoundError(err.Error(), nil)
	}

	if err := authorizeBan(e.Auth, target); err != nil {
		return e.ForbiddenError(strings.ToUpper(err.Error()[:1])+err.Error()[1:]+".", nil)
	}

	ban, err := e.App.FindFirstRecordByFilter(core.CollectionNameBans,
		"collectionRef={:c} && recordRef={:r}",
		dbx.Params{"c": target.Collection().Id, "r": target.Id},
	)
	if err != nil {
		collection, cerr := e.App.FindCachedCollectionByNameOrId(core.CollectionNameBans)
		if cerr != nil {
			return e.InternalServerError("", cerr)
		}
		ban = core.NewRecord(collection)
		ban.Set("collectionRef", target.Collection().Id)
		ban.Set("recordRef", target.Id)
	}

	ban.Set("reason", strings.TrimSpace(form.Reason))
	ban.Set("until", until)
	ban.Set("createdBy", e.Auth.Collection().Name+":"+e.Auth.Id)

	if err := e.App.SaveWithContext(auditContext(e), ban); err != nil {
		return firstApiError(err, e.InternalServerError("Failed to save the ban.", err))
	}

	return e.JSON(http.StatusOK, banView(ban))
}

func banDelete(e *core.RequestEvent) error {
	if err := refuseAPIKey(e); err != nil {
		return err
	}

	ban, err := e.App.FindRecordById(core.CollectionNameBans, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("", nil)
	}

	if err := e.App.DeleteWithContext(auditContext(e), ban); err != nil {
		return e.InternalServerError("Failed to remove the ban.", err)
	}

	return e.NoContent(http.StatusNoContent)
}

// -------------------------------------------------------------------

// auditAction writes an audit entry for an action that is not a single record
// change (bulk revocations, impersonation).
func auditAction(e *core.RequestEvent, action string, target *core.Record, meta map[string]any) error {
	actor := core.NewAuditActorFromRecord(e.Auth)
	if key := APIKeyFromRequest(e); key != nil {
		actor.APIKeyId = key.Id
		meta["apiKeyId"] = key.Id
	}

	entry := &core.AuditEntry{
		Action:         action,
		Actor:          actor,
		CollectionId:   target.Collection().Id,
		CollectionName: target.Collection().Name,
		RecordId:       target.Id,
		ChangedFields:  []string{},
		IP:             e.RealIP(),
		Meta:           meta,
	}

	return core.WriteAuditEntry(e.App, auditContext(e), entry, nil)
}

func auditImpersonation(e *core.RequestEvent, target *core.Record, reason string, durationSeconds int64) error {
	return auditAction(e, "impersonate", target, map[string]any{
		"reason":          reason,
		"impersonatorId":  e.Auth.Id,
		"durationSeconds": durationSeconds,
	})
}
