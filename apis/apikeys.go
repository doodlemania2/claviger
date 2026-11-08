package apis

import (
	"net/http"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #95): API key management endpoints.
//
//	GET    /api/apikeys            own keys (superusers: ?all=true for every key)
//	POST   /api/apikeys            create; the only response that carries the key
//	DELETE /api/apikeys/{id}       revoke (own key, or any key for a superuser)
//
// A key-authenticated request is refused before it gets here (see
// enforceAPIKeyScopes) and again by each handler: keys never manage keys.

var apiKeyCreateRateRule = core.RateLimitRule{MaxRequests: 10, Duration: 3600}

func bindAPIKeysApi(_ core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	sub := rg.Group("/apikeys")
	sub.GET("", apiKeysList).Bind(RequireAuth())
	sub.POST("", apiKeyCreate).Bind(RequireAuth())
	sub.DELETE("/{id}", apiKeyRevoke).Bind(RequireAuth())
}

type apiKeyCreateForm struct {
	Name    string   `form:"name" json:"name"`
	Scopes  []string `form:"scopes" json:"scopes"`
	Expires string   `form:"expires" json:"expires"`
}

func apiKeyView(r *core.Record, withOwner bool) map[string]any {
	var scopes []string
	_ = r.UnmarshalJSONField("scopes", &scopes)
	if scopes == nil {
		scopes = []string{}
	}

	m := map[string]any{
		"id":       r.Id,
		"name":     r.GetString("name"),
		"prefix":   r.GetString("prefix"),
		"scopes":   scopes,
		"expires":  r.GetString("expires"),
		"lastUsed": r.GetString("lastUsed"),
		"created":  r.GetString("created"),
		"revoked":  r.GetBool("revoked"),
	}
	if withOwner {
		m["collectionId"] = r.GetString("collectionRef")
		m["ownerId"] = r.GetString("recordRef")
	}

	return m
}

func apiKeysList(e *core.RequestEvent) error {
	if APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}

	all := e.Auth.IsSuperuser() && e.Request.URL.Query().Get("all") == "true"

	var exps []dbx.Expression
	if !all {
		exps = append(exps, dbx.HashExp{"collectionRef": e.Auth.Collection().Id, "recordRef": e.Auth.Id})
	}

	records, err := e.App.FindAllRecords(core.CollectionNameAPIKeys, exps...)
	if err != nil {
		return e.InternalServerError("Failed to load the API keys.", err)
	}

	items := make([]map[string]any, 0, len(records))
	for i := len(records) - 1; i >= 0; i-- { // newest first
		items = append(items, apiKeyView(records[i], all))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func apiKeyCreate(e *core.RequestEvent) error {
	if APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}

	if err := checkRateLimit(e, "@pb_apikey_create_"+e.Auth.Id, apiKeyCreateRateRule); err != nil {
		return e.TooManyRequestsError("Too many requests, please try again later.", nil)
	}

	// accounts with a passkey must prove presence recently, so a stolen or
	// left-open session cannot mint a long-lived credential
	n, err := e.App.CountRecords(core.CollectionNameWebAuthnCredentials, dbx.HashExp{
		"collectionRef": e.Auth.Collection().Id,
		"recordRef":     e.Auth.Id,
	})
	if err != nil {
		return e.InternalServerError("Failed to check the account passkeys.", err)
	}
	if n > 0 && !HasFreshPasskey(e, webauthnStepUpMaxAge) {
		return e.ForbiddenError("A recent passkey verification is required for this action.", validation.Errors{
			"passkey": validation.NewError(StepUpRequiredCode, "A recent passkey verification is required."),
		})
	}

	form := &apiKeyCreateForm{}
	if err := e.BindBody(form); err != nil {
		return e.BadRequestError("An error occurred while loading the submitted data.", err)
	}

	form.Name = strings.TrimSpace(form.Name)
	var expires types.DateTime
	var scopes []string

	err = validation.ValidateStruct(form,
		validation.Field(&form.Name, validation.Required, validation.Length(1, 100)),
		validation.Field(&form.Scopes, validation.By(func(any) error {
			var err error
			scopes, err = core.ValidateAPIKeyScopes(e.App, form.Scopes)
			return err
		})),
		validation.Field(&form.Expires, validation.By(func(any) error {
			if form.Expires == "" {
				return nil
			}
			var err error
			expires, err = types.ParseDateTime(form.Expires)
			if err != nil {
				return validation.NewError("validation_invalid_date", "Must be a valid date.")
			}
			if !expires.Time().After(time.Now()) {
				return validation.NewError("validation_past_date", "Must be in the future.")
			}
			return nil
		})),
	)
	if err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
	}

	active, err := e.App.CountRecords(core.CollectionNameAPIKeys, dbx.HashExp{
		"collectionRef": e.Auth.Collection().Id,
		"recordRef":     e.Auth.Id,
		"revoked":       false,
	})
	if err != nil {
		return e.InternalServerError("Failed to count the API keys.", err)
	}
	if active >= core.APIKeyMaxPerOwner {
		return e.BadRequestError("The maximum number of API keys was reached; revoke one first.", nil)
	}

	collection, err := e.App.FindCachedCollectionByNameOrId(core.CollectionNameAPIKeys)
	if err != nil {
		return e.InternalServerError("", err)
	}

	var key string
	record := core.NewRecord(collection)
	for range 5 { // a prefix collision (2^-40) hits the unique index
		var prefix, hash string
		key, prefix, hash, err = core.GenerateAPIKey()
		if err != nil {
			return e.InternalServerError("Failed to generate the API key.", err)
		}
		if _, err := e.App.FindFirstRecordByData(core.CollectionNameAPIKeys, "prefix", prefix); err == nil {
			continue
		}

		record.Set("collectionRef", e.Auth.Collection().Id)
		record.Set("recordRef", e.Auth.Id)
		record.Set("name", form.Name)
		record.Set("prefix", prefix)
		record.Set("secretHash", hash)
		record.Set("scopes", scopes)
		if !expires.IsZero() {
			record.Set("expires", expires)
		}
		break
	}
	if record.GetString("prefix") == "" {
		return e.InternalServerError("Failed to generate the API key.", nil)
	}

	if err := e.App.SaveWithContext(auditContext(e), record); err != nil {
		return e.InternalServerError("Failed to save the API key.", err)
	}

	view := apiKeyView(record, false)
	view["key"] = key // shown once, never stored

	return e.JSON(http.StatusOK, view)
}

func apiKeyRevoke(e *core.RequestEvent) error {
	if APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}

	record, err := e.App.FindRecordById(core.CollectionNameAPIKeys, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("", nil)
	}

	owned := record.GetString("collectionRef") == e.Auth.Collection().Id && record.GetString("recordRef") == e.Auth.Id
	if !owned && !e.Auth.IsSuperuser() {
		return e.NotFoundError("", nil) // do not reveal other users' key ids
	}

	if !record.GetBool("revoked") {
		record.Set("revoked", true)
		if err := e.App.SaveWithContext(auditContext(e), record); err != nil {
			return e.InternalServerError("Failed to revoke the API key.", err)
		}
	}

	return e.NoContent(http.StatusNoContent)
}
