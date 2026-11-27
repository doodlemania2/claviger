package apis

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/sso"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #92): inbound enterprise single sign-on. See
// docs/inbound-sso.md and package sso.
//
// Management (org owners and admins, and superusers; API keys can read only):
//
//	GET    /api/orgs/{id}/sso/presets                          provider presets
//	GET    /api/orgs/{id}/sso/connections                      the org's connections
//	POST   /api/orgs/{id}/sso/connections                      create
//	GET    /api/orgs/{id}/sso/connections/{cid}                one connection
//	PATCH  /api/orgs/{id}/sso/connections/{cid}                change
//	DELETE /api/orgs/{id}/sso/connections/{cid}                delete (its domain claims go with it)
//	POST   /api/orgs/{id}/sso/connections/{cid}/test           check the provider's discovery document
//	GET    /api/orgs/{id}/sso/domains                          the org's domain claims
//	POST   /api/orgs/{id}/sso/domains                          claim a domain {domain, connection}
//	POST   /api/orgs/{id}/sso/domains/{did}/verify             look for the TXT record
//	DELETE /api/orgs/{id}/sso/domains/{did}                    release a claim
//
// Sign-in (public):
//
//	POST /api/sso/discover   {email}: home-realm discovery
//	GET  /api/sso/start      ?connection=&returnTo=&login_hint=: go to the provider
//	GET|POST /api/sso/callback   the provider's redirect URI
//	POST /api/sso/exchange   {code}: the one-time code for the auth response
func bindSSOApi(app core.App, rg *router.RouterGroup[*core.RequestEvent], orgs *router.RouterGroup[*core.RequestEvent]) {
	orgs.GET("/{id}/sso/presets", ssoPresets)
	orgs.GET("/{id}/sso/connections", ssoConnectionsList)
	orgs.POST("/{id}/sso/connections", ssoConnectionsCreate)
	orgs.GET("/{id}/sso/connections/{cid}", ssoConnectionsView)
	orgs.PATCH("/{id}/sso/connections/{cid}", ssoConnectionsUpdate)
	orgs.DELETE("/{id}/sso/connections/{cid}", ssoConnectionsDelete)
	orgs.POST("/{id}/sso/connections/{cid}/test", ssoConnectionsTest)
	orgs.GET("/{id}/sso/domains", ssoDomainsList)
	orgs.POST("/{id}/sso/domains", ssoDomainsCreate)
	orgs.POST("/{id}/sso/domains/{did}/verify", ssoDomainsVerify)
	orgs.DELETE("/{id}/sso/domains/{did}", ssoDomainsDelete)

	pub := rg.Group("/sso")
	pub.POST("/discover", ssoDiscover)
	pub.GET("/start", ssoStart)
	pub.GET("/callback", ssoCallback)
	pub.POST("/callback", ssoCallback)
	pub.POST("/exchange", ssoExchange)

	bindSSORequired(app)
}

func ssoErr(e *core.RequestEvent, err error) error {
	var se *sso.Error
	if errors.As(err, &se) {
		return e.Error(se.Status, se.Message, validation.Errors{"sso": validation.NewError(se.Code, se.Message)})
	}

	if oe, ok := core.AsOrgError(err); ok {
		return e.Error(oe.Status, oe.Message, nil)
	}

	return e.BadRequestError("", err)
}

// ssoManager resolves the org of the route and requires owner, admin or superuser.
func ssoManager(e *core.RequestEvent) (*core.Record, *core.OrgActor, error) {
	org, actor, err := orgSession(e)
	if err != nil {
		return nil, nil, err
	}

	if !actor.CanManage() {
		return nil, nil, e.ForbiddenError("Only owners and admins manage single sign-on.", nil)
	}

	if org.GetBool("archived") {
		return nil, nil, e.BadRequestError("The organization is archived.", nil)
	}

	return org, actor, nil
}

func ssoPresets(e *core.RequestEvent) error {
	if _, _, err := ssoManager(e); err != nil {
		return err
	}

	return e.JSON(http.StatusOK, map[string]any{
		"types":     sso.Types(),
		"presets":   sso.Presets,
		"callback":  ssoCallbackURL(e),
		"verifyTxt": core.SSOVerificationPrefix + "<token>",
	})
}

func ssoCallbackURL(e *core.RequestEvent) string {
	base := strings.TrimRight(e.App.Settings().Meta.AppURL, "/")
	if base == "" {
		scheme := "http"
		if e.Request.TLS != nil {
			scheme = "https"
		}

		base = scheme + "://" + e.Request.Host
	}

	return base + "/api/sso/callback"
}

func ssoAppOrigin(e *core.RequestEvent) string {
	u, err := url.Parse(ssoCallbackURL(e))
	if err != nil {
		return ""
	}

	return u.Scheme + "://" + u.Host
}

// ---- connections -------------------------------------------------------------

func ssoConnectionJSON(rec *core.Record) map[string]any {
	var mapping map[string]string

	_ = rec.UnmarshalJSONField("roleMapping", &mapping)

	if mapping == nil {
		mapping = map[string]string{}
	}

	var config map[string]any

	_ = rec.UnmarshalJSONField("config", &config)

	if config == nil {
		config = map[string]any{}
	}

	return map[string]any{
		"id":          rec.Id,
		"org":         rec.GetString("org"),
		"name":        rec.GetString("name"),
		"type":        rec.GetString("type"),
		"enabled":     rec.GetBool("enabled"),
		"ssoRequired": rec.GetBool("ssoRequired"),
		"jit":         rec.GetBool("jit"),
		"issuer":      rec.GetString("issuer"),
		"clientId":    rec.GetString("clientId"),
		"hasSecret":   rec.GetString("clientSecret") != "",
		"scopes":      strings.Fields(rec.GetString("scopes")),
		"groupsClaim": rec.GetString("groupsClaim"),
		"roleMapping": mapping,
		"defaultRole": rec.GetString("defaultRole"),
		"config":      config,
		"created":     rec.GetDateTime("created"),
		"updated":     rec.GetDateTime("updated"),
	}
}

type ssoConnectionBody struct {
	Name         *string            `json:"name"`
	Type         *string            `json:"type"`
	Enabled      *bool              `json:"enabled"`
	SSORequired  *bool              `json:"ssoRequired"`
	JIT          *bool              `json:"jit"`
	Issuer       *string            `json:"issuer"`
	ClientID     *string            `json:"clientId"`
	ClientSecret *string            `json:"clientSecret"`
	Scopes       *[]string          `json:"scopes"`
	GroupsClaim  *string            `json:"groupsClaim"`
	DefaultRole  *string            `json:"defaultRole"`
	RoleMapping  *map[string]string `json:"roleMapping"`
	Config       *map[string]any    `json:"config"`
}

// apply writes the given fields onto the record.
func (b *ssoConnectionBody) apply(e *core.RequestEvent, rec *core.Record) error {
	if b.Name != nil {
		rec.Set("name", strings.TrimSpace(*b.Name))
	}

	if b.Type != nil {
		rec.Set("type", *b.Type)
	}

	if b.Enabled != nil {
		rec.Set("enabled", *b.Enabled)
	}

	if b.SSORequired != nil {
		rec.Set("ssoRequired", *b.SSORequired)
	}

	if b.JIT != nil {
		rec.Set("jit", *b.JIT)
	}

	if b.Issuer != nil {
		rec.Set("issuer", sso.NormalizeIssuer(*b.Issuer))
	}

	if b.ClientID != nil {
		rec.Set("clientId", strings.TrimSpace(*b.ClientID))
	}

	if b.ClientSecret != nil && *b.ClientSecret != "" {
		stored, err := core.SetSSOSecret(e.App, *b.ClientSecret)
		if err != nil {
			return err
		}

		rec.Set("clientSecret", stored)
	}

	if b.Scopes != nil {
		rec.Set("scopes", strings.Join(*b.Scopes, " "))
	}

	if b.GroupsClaim != nil {
		rec.Set("groupsClaim", strings.TrimSpace(*b.GroupsClaim))
	}

	if b.DefaultRole != nil {
		rec.Set("defaultRole", strings.TrimSpace(*b.DefaultRole))
	}

	if b.RoleMapping != nil {
		rec.Set("roleMapping", *b.RoleMapping)
	}

	if b.Config != nil {
		rec.Set("config", *b.Config)
	}

	return nil
}

func ssoFindConnection(e *core.RequestEvent, org *core.Record) (*core.Record, error) {
	rec, err := e.App.FindRecordById(core.CollectionNameSSOConnections, e.Request.PathValue("cid"))
	if err != nil || rec.GetString("org") != org.Id {
		return nil, e.NotFoundError("Connection not found.", nil)
	}

	return rec, nil
}

func ssoConnectionsList(e *core.RequestEvent) error {
	org, _, err := ssoManager(e)
	if err != nil {
		return err
	}

	recs, err := e.App.FindRecordsByFilter(core.CollectionNameSSOConnections, "org={:org}", "created", 0, 0, dbx.Params{"org": org.Id})
	if err != nil {
		return e.InternalServerError("", err)
	}

	items := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		items = append(items, ssoConnectionJSON(r))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func ssoConnectionsView(e *core.RequestEvent) error {
	org, _, err := ssoManager(e)
	if err != nil {
		return err
	}

	rec, err := ssoFindConnection(e, org)
	if err != nil {
		return err
	}

	return e.JSON(http.StatusOK, ssoConnectionJSON(rec))
}

// ssoSave validates the connection as it would be loaded and saves it.
func ssoSave(e *core.RequestEvent, rec *core.Record) error {
	conn, err := sso.LoadConnection(e.App, rec)
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	if err := sso.ValidateConnection(conn); err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	if err := e.App.SaveWithContext(auditContext(e), rec); err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	return nil
}

func ssoConnectionsCreate(e *core.RequestEvent) error {
	org, _, err := ssoManager(e)
	if err != nil {
		return err
	}

	var body ssoConnectionBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	col, err := e.App.FindCachedCollectionByNameOrId(core.CollectionNameSSOConnections)
	if err != nil {
		return e.InternalServerError("", err)
	}

	rec := core.NewRecord(col)
	rec.Set("org", org.Id)
	rec.Set("type", core.SSOTypeOIDC)
	rec.Set("jit", true) // accounts are created on first sign-in unless the admin turns it off

	if err := body.apply(e, rec); err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	if err := ssoSave(e, rec); err != nil {
		return err
	}

	return e.JSON(http.StatusOK, ssoConnectionJSON(rec))
}

func ssoConnectionsUpdate(e *core.RequestEvent) error {
	org, _, err := ssoManager(e)
	if err != nil {
		return err
	}

	rec, err := ssoFindConnection(e, org)
	if err != nil {
		return err
	}

	var body ssoConnectionBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	if body.Type != nil && *body.Type != rec.GetString("type") {
		return e.BadRequestError("The type of a connection cannot change.", nil)
	}

	if err := body.apply(e, rec); err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	if err := ssoSave(e, rec); err != nil {
		return err
	}

	return e.JSON(http.StatusOK, ssoConnectionJSON(rec))
}

func ssoConnectionsDelete(e *core.RequestEvent) error {
	org, _, err := ssoManager(e)
	if err != nil {
		return err
	}

	rec, err := ssoFindConnection(e, org)
	if err != nil {
		return err
	}

	if err := e.App.DeleteWithContext(auditContext(e), rec); err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	return e.NoContent(http.StatusNoContent)
}

func ssoConnectionsTest(e *core.RequestEvent) error {
	org, _, err := ssoManager(e)
	if err != nil {
		return err
	}

	rec, err := ssoFindConnection(e, org)
	if err != nil {
		return err
	}

	if rec.GetString("type") != core.SSOTypeOIDC {
		return e.BadRequestError("This connection type has no test.", nil)
	}

	if err := sso.Discover(e.Request.Context(), rec.GetString("issuer")); err != nil {
		return e.JSON(http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
	}

	return e.JSON(http.StatusOK, map[string]any{"ok": true})
}

// ---- domains -------------------------------------------------------------------

func ssoDomainJSON(rec *core.Record) map[string]any {
	domain := rec.GetString("domain")
	out := map[string]any{
		"id":            rec.Id,
		"org":           rec.GetString("org"),
		"connection":    rec.GetString("connection"),
		"domain":        domain,
		"status":        rec.GetString("status"),
		"verifiedAt":    rec.GetDateTime("verifiedAt"),
		"lastCheckedAt": rec.GetDateTime("lastCheckedAt"),
		"created":       rec.GetDateTime("created"),
	}

	if rec.GetString("status") != core.SSODomainVerified {
		out["verification"] = map[string]any{
			"type":  "TXT",
			"name":  domain,
			"alt":   sso.ChallengeLabel + domain,
			"value": sso.VerificationValue(rec.GetString("token")),
		}
	}

	return out
}

func ssoFindDomain(e *core.RequestEvent, org *core.Record) (*core.Record, error) {
	rec, err := e.App.FindRecordById(core.CollectionNameSSODomains, e.Request.PathValue("did"))
	if err != nil || rec.GetString("org") != org.Id {
		return nil, e.NotFoundError("Domain not found.", nil)
	}

	return rec, nil
}

func ssoDomainsList(e *core.RequestEvent) error {
	org, _, err := ssoManager(e)
	if err != nil {
		return err
	}

	recs, err := e.App.FindRecordsByFilter(core.CollectionNameSSODomains, "org={:org}", "domain", 0, 0, dbx.Params{"org": org.Id})
	if err != nil {
		return e.InternalServerError("", err)
	}

	items := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		items = append(items, ssoDomainJSON(r))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func ssoDomainsCreate(e *core.RequestEvent) error {
	org, _, err := ssoManager(e)
	if err != nil {
		return err
	}

	var body struct {
		Domain     string `json:"domain"`
		Connection string `json:"connection"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	rec, err := sso.CreateDomain(e.App, auditContext(e), org, body.Connection, body.Domain)
	if err != nil {
		return ssoErr(e, err)
	}

	return e.JSON(http.StatusOK, ssoDomainJSON(rec))
}

func ssoDomainsVerify(e *core.RequestEvent) error {
	org, _, err := ssoManager(e)
	if err != nil {
		return err
	}

	rec, err := ssoFindDomain(e, org)
	if err != nil {
		return err
	}

	verified, err := sso.CheckDomain(e.App, auditContext(e), rec)
	if err != nil {
		return ssoErr(e, err)
	}

	out := ssoDomainJSON(rec)
	out["verified"] = verified

	return e.JSON(http.StatusOK, out)
}

func ssoDomainsDelete(e *core.RequestEvent) error {
	org, _, err := ssoManager(e)
	if err != nil {
		return err
	}

	rec, err := ssoFindDomain(e, org)
	if err != nil {
		return err
	}

	if err := e.App.DeleteWithContext(auditContext(e), rec); err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	return e.NoContent(http.StatusNoContent)
}

// ---- sign-in -------------------------------------------------------------------------

const ssoBindCookie = "claviger_sso_bind"

func ssoNoStore(e *core.RequestEvent) {
	e.Response.Header().Set("Cache-Control", "no-store")
}

func ssoSetBind(e *core.RequestEvent, value string, maxAge int) {
	e.SetCookie(&http.Cookie{
		Name: ssoBindCookie, Value: value, Path: "/api/sso", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: strings.HasPrefix(ssoCallbackURL(e), "https://"),
	})
}

func ssoBindValue(e *core.RequestEvent) string {
	c, err := e.Request.Cookie(ssoBindCookie)
	if err != nil {
		return ""
	}

	return c.Value
}

// ssoDiscover is home-realm discovery: does this address belong to a verified
// domain with an enabled connection? It answers the same shape for every
// address and reveals only what the sign-in page needs to send the person on.
func ssoDiscover(e *core.RequestEvent) error {
	ssoNoStore(e)

	var body struct {
		Email string `json:"email" form:"email"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	route := sso.RouteForEmail(e.App, body.Email)
	if route == nil {
		return e.JSON(http.StatusOK, map[string]any{"sso": false})
	}

	return e.JSON(http.StatusOK, map[string]any{
		"sso":        true,
		"collection": core.OrgUserCollectionName(),
		"required":   route.Connection.SSORequired,
		"connection": route.Connection.Id,
		"name":       route.Connection.Name,
		"url":        "/api/sso/start?connection=" + url.QueryEscape(route.Connection.Id),
	})
}

func ssoStart(e *core.RequestEvent) error {
	ssoNoStore(e)

	q := e.Request.URL.Query()

	returnTo := sso.SafeReturnTo(ssoAppOrigin(e), q.Get("returnTo"))
	if returnTo == "" {
		return e.BadRequestError("A returnTo on this server is required.", nil)
	}

	hint := q.Get("login_hint")
	connection := q.Get("connection")

	// a hint must belong to the connection: it is not a way to sign in as
	// someone from a domain the connection does not hold
	if hint != "" {
		if r := sso.RouteForEmail(e.App, hint); r == nil || r.Connection.Id != connection {
			hint = ""
		}
	}

	res, err := sso.Start(e.App, context.WithoutCancel(e.Request.Context()), sso.StartInput{
		ConnectionId: connection, CallbackURL: ssoCallbackURL(e), ReturnTo: returnTo, LoginHint: hint, IP: e.RealIP(),
	})
	if err != nil {
		e.App.Logger().Warn("SSO start failed", "error", err.Error())

		return ssoErr(e, err)
	}

	ssoSetBind(e, res.BindValue, int(sso.FlowLifetime.Seconds()))

	return e.Redirect(http.StatusFound, res.RedirectURL)
}

func ssoCallback(e *core.RequestEvent) error {
	ssoNoStore(e)

	if e.Request.Method == http.MethodPost {
		_ = e.Request.ParseForm()
	}

	res := sso.Callback(e.App, context.WithoutCancel(e.Request.Context()), sso.CallbackInput{
		CallbackURL: ssoCallbackURL(e), BindValue: ssoBindValue(e), Method: e.Request.Method,
		Query: e.Request.URL.Query(), Form: e.Request.PostForm, IP: e.RealIP(),
	})

	// the binding cookie stays until the exchange when there is a code to
	// exchange; a failed sign-in has nothing left to bind
	if res.Err != nil {
		ssoSetBind(e, "", -1)
	}

	if res.ReturnTo == "" {
		if res.Err != nil {
			return e.Error(res.Err.Status, res.Err.Message, validation.Errors{"sso": validation.NewError(res.Err.Code, res.Err.Message)})
		}

		return e.BadRequestError("The sign-in is no longer valid.", nil)
	}

	return e.Redirect(http.StatusSeeOther, res.URL())
}

// ssoExchange trades the one-time code of a finished sign-in for the ordinary
// auth response (token and record) of the account.
func ssoExchange(e *core.RequestEvent) error {
	ssoNoStore(e)

	var body struct {
		Code string `json:"code" form:"code"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	user, err := sso.Exchange(e.App, body.Code, ssoBindValue(e))
	if err != nil {
		return ssoErr(e, err)
	}

	ssoSetBind(e, "", -1)

	return RecordAuthResponse(e, user, core.MFAMethodOAuth2, map[string]any{"sso": true})
}

// ---- "single sign-on required" -------------------------------------------------------

// bindSSORequired refuses password and OTP sign-in for an account whose email
// domain is verified for a connection that requires single sign-on. Passkeys,
// OAuth2 providers and the recovery flows are not touched (docs/inbound-sso.md).
// Superusers are never affected: they are not in the member collection.
func bindSSORequired(app core.App) {
	check := func(e *core.RequestEvent, col *core.Collection, rec *core.Record) error {
		if rec == nil || col.Name != core.OrgUserCollectionName() {
			return nil
		}

		route := sso.RequiredFor(e.App, rec.Email())
		if route == nil {
			return nil
		}

		_ = core.WriteSSOAudit(e.App, core.WithAuditContext(context.Background(), core.NewSelfAuditActor(rec), e.RealIP()),
			core.AuditActionSSOBlocked, col.Name, rec.Id, map[string]any{"orgId": route.Connection.OrgId, "connection": route.Connection.Id})

		return e.Error(http.StatusForbidden, "Your organization requires single sign-on.",
			validation.Errors{"sso": validation.NewError("sso_required", "Your organization requires single sign-on.")})
	}

	app.OnRecordAuthWithPasswordRequest().BindFunc(func(e *core.RecordAuthWithPasswordRequestEvent) error {
		if err := check(e.RequestEvent, e.Collection, e.Record); err != nil {
			return err
		}

		return e.Next()
	})

	app.OnRecordAuthWithOTPRequest().BindFunc(func(e *core.RecordAuthWithOTPRequestEvent) error {
		if err := check(e.RequestEvent, e.Collection, e.Record); err != nil {
			return err
		}

		return e.Next()
	})
}
