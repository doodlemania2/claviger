package apis

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #81): safeguarding credentials.
//
//	GET    /api/orgs/{id}/credential-types                 members, and coordinators of an ancestor
//	POST   /api/orgs/{id}/credential-types                 {key, name, validMonths?, requiresDocument?, requiresVerifier?} (owner/admin)
//	PATCH  /api/orgs/{id}/credential-types/{type}          {name?, validMonths?, requiresDocument?, requiresVerifier?, status?} (owner/admin)
//	DELETE /api/orgs/{id}/credential-types/{type}          (owner/admin); a type in use is retired instead
//	GET    /api/orgs/{id}/credentials                      coordinators: ?user=&type=&state=&page=&perPage=
//	POST   /api/orgs/{id}/credentials                      coordinators: {user, type, issuedAt?, expires?, issuer?, document?}
//	PATCH  /api/orgs/{id}/credentials/{cred}               coordinators: {issuedAt?, expires?, issuer?, document?}
//	POST   /api/orgs/{id}/credentials/{cred}/verify        a second coordinator
//	POST   /api/orgs/{id}/credentials/{cred}/revoke        coordinators: {reason?}
//	GET    /api/orgs/{id}/access-status                    coordinators: ?type=&standing=&days=&limit=
//	GET    /api/me/access-status                           the signed-in person's own standing, per org and type
//
// A coordinator is an owner or admin of the org, or a member tagged
// "safeguarding". Coordinators of an ancestor org can read (not write). API
// keys are refused on all of them: the data is about people's clearance.
func bindCredentialsApi(orgs *router.RouterGroup[*core.RequestEvent]) {
	orgs.GET("/{id}/credential-types", credentialTypesList)
	orgs.POST("/{id}/credential-types", credentialTypesCreate)
	orgs.PATCH("/{id}/credential-types/{type}", credentialTypesUpdate)
	orgs.DELETE("/{id}/credential-types/{type}", credentialTypesDelete)
	orgs.GET("/{id}/credentials", credentialsList)
	orgs.POST("/{id}/credentials", credentialsCreate)
	orgs.PATCH("/{id}/credentials/{cred}", credentialsUpdate)
	orgs.POST("/{id}/credentials/{cred}/verify", credentialsVerify)
	orgs.POST("/{id}/credentials/{cred}/revoke", credentialsRevoke)
	orgs.GET("/{id}/access-status", credentialsAccessStatus)
}

// credentialSession loads the org and the caller's standing. A caller who is
// neither a member nor a coordinator of an ancestor gets a 404.
func credentialSession(e *core.RequestEvent) (*core.Record, *core.CredentialActor, error) {
	if APIKeyFromRequest(e) != nil {
		return nil, nil, apiKeyForbidden(e)
	}

	if !core.CredentialsAvailable(e.App) {
		return nil, nil, e.NotFoundError("Credentials are not available.", nil)
	}

	org, err := core.FindOrg(e.App, e.Request.PathValue("id"))
	if err != nil {
		return nil, nil, e.NotFoundError("Organization not found.", nil)
	}

	actor := core.NewCredentialActor(e.App, org, e.Auth)

	if !actor.IsMember() && !actor.Overseer {
		return nil, nil, e.NotFoundError("Organization not found.", nil)
	}

	if org.GetBool("archived") && !actor.Superuser {
		return nil, nil, e.NotFoundError("Organization not found.", nil)
	}

	return org, actor, nil
}

// credentialOverseer is credentialSession plus the read access of coordinators.
func credentialOverseer(e *core.RequestEvent) (*core.Record, *core.CredentialActor, error) {
	org, actor, err := credentialSession(e)
	if err != nil {
		return nil, nil, err
	}

	if !actor.Overseer {
		return nil, nil, e.ForbiddenError("Only safeguarding coordinators can do this.", nil)
	}

	return org, actor, nil
}

func credentialTypeJSON(t *core.Record) map[string]any {
	return map[string]any{
		"id":               t.Id,
		"org":              t.GetString("org"),
		"key":              t.GetString("key"),
		"name":             t.GetString("name"),
		"validMonths":      t.GetInt("validMonths"),
		"requiresDocument": t.GetBool("requiresDocument"),
		"requiresVerifier": t.GetBool("requiresVerifier"),
		"status":           t.GetString("status"),
	}
}

func credentialJSON(c *core.Record, now time.Time) map[string]any {
	return map[string]any{
		"id":            c.Id,
		"user":          c.GetString("user"),
		"org":           c.GetString("org"),
		"type":          c.GetString("type"),
		"issuedAt":      c.GetDateTime("issuedAt"),
		"expires":       c.GetDateTime("expires"),
		"issuer":        c.GetString("issuer"),
		"recordedBy":    c.GetString("recordedBy"),
		"verifiedBy":    c.GetString("verifiedBy"),
		"verified":      c.GetBool("verified"),
		"revoked":       c.GetBool("revoked"),
		"revokedReason": c.GetString("revokedReason"),
		"revokedAt":     c.GetDateTime("revokedAt"),
		"document":      c.GetString("document"),
		"state":         core.CredentialState(c, now),
		"created":       c.GetDateTime("created"),
		"updated":       c.GetDateTime("updated"),
	}
}

// ---- types ---------------------------------------------------------------

func credentialTypesList(e *core.RequestEvent) error {
	org, _, err := credentialSession(e)
	if err != nil {
		return err
	}

	typesList, err := core.CredentialTypesFor(e.App, org.Id)
	if err != nil {
		return e.InternalServerError("", err)
	}

	items := make([]map[string]any, 0, len(typesList))
	for _, t := range typesList {
		items = append(items, credentialTypeJSON(t))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

type credentialTypeBody struct {
	Key              string  `json:"key" form:"key"`
	Name             *string `json:"name" form:"name"`
	ValidMonths      *int    `json:"validMonths" form:"validMonths"`
	RequiresDocument *bool   `json:"requiresDocument" form:"requiresDocument"`
	RequiresVerifier *bool   `json:"requiresVerifier" form:"requiresVerifier"`
	Status           *string `json:"status" form:"status"`
}

func credentialTypesCreate(e *core.RequestEvent) error {
	org, actor, err := credentialSession(e)
	if err != nil {
		return err
	}

	var body credentialTypeBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	in := core.CredentialTypeInput{Key: body.Key}
	if body.Name != nil {
		in.Name = *body.Name
	}

	if body.ValidMonths != nil {
		in.ValidMonths = *body.ValidMonths
	}

	if body.RequiresDocument != nil {
		in.RequiresDocument = *body.RequiresDocument
	}

	if body.RequiresVerifier != nil {
		in.RequiresVerifier = *body.RequiresVerifier
	}

	t, err := core.CreateCredentialType(e.App, auditContext(e), actor, org, in)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, credentialTypeJSON(t))
}

func credentialTypeFromPath(e *core.RequestEvent, org *core.Record) (*core.Record, error) {
	t, err := e.App.FindRecordById(core.CollectionNameCredentialTypes, e.Request.PathValue("type"))
	if err != nil || t.GetString("org") != org.Id {
		return nil, e.NotFoundError("Credential type not found.", nil)
	}

	return t, nil
}

func credentialTypesUpdate(e *core.RequestEvent) error {
	org, actor, err := credentialSession(e)
	if err != nil {
		return err
	}

	t, err := credentialTypeFromPath(e, org)
	if err != nil {
		return err
	}

	var body credentialTypeBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	t, err = core.UpdateCredentialType(e.App, auditContext(e), actor, org, t, core.CredentialTypePatch{
		Name: body.Name, ValidMonths: body.ValidMonths, RequiresDocument: body.RequiresDocument,
		RequiresVerifier: body.RequiresVerifier, Status: body.Status,
	})
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, credentialTypeJSON(t))
}

func credentialTypesDelete(e *core.RequestEvent) error {
	org, actor, err := credentialSession(e)
	if err != nil {
		return err
	}

	t, err := credentialTypeFromPath(e, org)
	if err != nil {
		return err
	}

	if err := core.DeleteCredentialType(e.App, auditContext(e), actor, org, t); err != nil {
		return orgHTTP(e, err)
	}

	return e.NoContent(http.StatusNoContent)
}

// ---- credentials ---------------------------------------------------------

// parseCredentialDate accepts RFC 3339, a date-time with a space, or a plain
// date. A plain date means the END of that day (UTC): "expires 2027-01-31" is
// valid through the 31st.
func parseCredentialDate(s string, endOfDay bool) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, true
	}

	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05.000Z", "2006-01-02 15:04:05Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}

	if t, err := time.Parse("2006-01-02", s); err == nil {
		if endOfDay {
			t = t.Add(24*time.Hour - time.Second)
		}

		return t.UTC(), true
	}

	return time.Time{}, false
}

type credentialBody struct {
	User     string  `json:"user" form:"user"`
	Type     string  `json:"type" form:"type"`
	IssuedAt string  `json:"issuedAt" form:"issuedAt"`
	Expires  string  `json:"expires" form:"expires"`
	Issuer   *string `json:"issuer" form:"issuer"`
	Reason   string  `json:"reason" form:"reason"`
}

func credentialDocument(e *core.RequestEvent) (any, error) {
	files, err := e.FindUploadedFiles("document")
	if err != nil || len(files) == 0 {
		return nil, nil
	}

	return files[0], nil
}

func credentialsList(e *core.RequestEvent) error {
	org, _, err := credentialOverseer(e)
	if err != nil {
		return err
	}

	q := e.Request.URL.Query()
	filter := "org={:org}"
	params := dbx.Params{"org": org.Id}

	if v := q.Get("user"); v != "" {
		filter += " && user={:user}"
		params["user"] = v
	}

	if v := q.Get("type"); v != "" {
		filter += " && type={:type}"
		params["type"] = v
	}

	recs, err := e.App.FindRecordsByFilter(core.CollectionNameCredentials, filter, "-expires", 0, 0, params)
	if err != nil {
		return e.InternalServerError("", err)
	}

	now := time.Now()
	state := q.Get("state")
	items := make([]map[string]any, 0, len(recs))

	for _, c := range recs {
		if state != "" && core.CredentialState(c, now) != state {
			continue
		}

		items = append(items, credentialJSON(c, now))
	}

	page, perPage := pageParams(e)
	total := len(items)
	from := min((page-1)*perPage, total)
	to := min(from+perPage, total)

	return e.JSON(http.StatusOK, map[string]any{
		"page": page, "perPage": perPage, "totalItems": total, "items": items[from:to],
	})
}

func credentialsCreate(e *core.RequestEvent) error {
	org, actor, err := credentialSession(e)
	if err != nil {
		return err
	}

	var body credentialBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	in := core.CredentialInput{User: body.User, Type: body.Type}

	var ok bool
	if in.IssuedAt, ok = parseCredentialDate(body.IssuedAt, false); !ok {
		return e.BadRequestError("Invalid issuedAt date.", nil)
	}

	if in.Expires, ok = parseCredentialDate(body.Expires, true); !ok {
		return e.BadRequestError("Invalid expires date.", nil)
	}

	if body.Issuer != nil {
		in.Issuer = *body.Issuer
	}

	if in.Document, err = credentialDocument(e); err != nil {
		return e.BadRequestError("Invalid document.", err)
	}

	c, err := core.RecordCredential(e.App, auditContext(e), actor, org, in)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, credentialJSON(c, time.Now()))
}

func credentialFromPath(e *core.RequestEvent, org *core.Record) (*core.Record, error) {
	c, err := core.FindOrgCredential(e.App, org, e.Request.PathValue("cred"))
	if err != nil {
		return nil, e.NotFoundError("Credential not found.", nil)
	}

	return c, nil
}

func credentialsUpdate(e *core.RequestEvent) error {
	org, actor, err := credentialOverseer(e)
	if err != nil {
		return err
	}

	c, err := credentialFromPath(e, org)
	if err != nil {
		return err
	}

	var body credentialBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	patch := core.CredentialPatch{Issuer: body.Issuer}

	var ok bool
	if patch.IssuedAt, ok = parseCredentialDate(body.IssuedAt, false); !ok {
		return e.BadRequestError("Invalid issuedAt date.", nil)
	}

	if patch.Expires, ok = parseCredentialDate(body.Expires, true); !ok {
		return e.BadRequestError("Invalid expires date.", nil)
	}

	if patch.Document, err = credentialDocument(e); err != nil {
		return e.BadRequestError("Invalid document.", err)
	}

	c, err = core.UpdateCredential(e.App, auditContext(e), actor, org, c, patch)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, credentialJSON(c, time.Now()))
}

func credentialsVerify(e *core.RequestEvent) error {
	org, actor, err := credentialOverseer(e)
	if err != nil {
		return err
	}

	c, err := credentialFromPath(e, org)
	if err != nil {
		return err
	}

	c, err = core.VerifyCredential(e.App, auditContext(e), actor, org, c)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, credentialJSON(c, time.Now()))
}

func credentialsRevoke(e *core.RequestEvent) error {
	org, actor, err := credentialOverseer(e)
	if err != nil {
		return err
	}

	c, err := credentialFromPath(e, org)
	if err != nil {
		return err
	}

	var body credentialBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	c, err = core.RevokeCredential(e.App, auditContext(e), actor, org, c, body.Reason)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, credentialJSON(c, time.Now()))
}

// ---- access status -------------------------------------------------------

func credentialsAccessStatus(e *core.RequestEvent) error {
	org, _, err := credentialOverseer(e)
	if err != nil {
		return err
	}

	q := e.Request.URL.Query()

	opts := core.AccessStatusOptions{Type: q.Get("type"), Standing: q.Get("standing")}
	opts.ExpiringDays, _ = strconv.Atoi(q.Get("days"))
	opts.Limit, _ = strconv.Atoi(q.Get("limit"))

	if opts.Standing != "" && !slices.Contains([]string{
		core.CredentialStandingCleared, core.CredentialStandingExpiring, core.CredentialStandingPending,
		core.CredentialStandingLapsed, core.CredentialStandingRevoked, core.CredentialStandingMissing,
	}, opts.Standing) {
		return e.BadRequestError("Invalid standing.", nil)
	}

	opts.Limit = min(opts.Limit, 1000)
	opts.ExpiringDays = min(opts.ExpiringDays, 365)

	res, err := core.CredentialAccessStatus(e.App, org, opts)
	if err != nil {
		return e.InternalServerError("", err)
	}

	days := opts.ExpiringDays
	if days <= 0 {
		days = core.DefaultCredentialExpiringDays
	}

	return e.JSON(http.StatusOK, map[string]any{
		"org": org.Id, "asOf": time.Now().UTC(), "expiringDays": days, "types": res,
	})
}

// ---- my access status ----------------------------------------------------

// bindMyAccessStatusApi registers GET /api/me/access-status for the hosted
// "my credentials" page. It answers only about the caller, so it needs no
// coordinator standing. API keys are refused like the rest.
func bindMyAccessStatusApi(rg *router.RouterGroup[*core.RequestEvent]) {
	rg.GET("/me/access-status", myAccessStatus).Bind(RequireAuth())
}

func myAccessStatus(e *core.RequestEvent) error {
	if APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}

	if !core.CredentialsAvailable(e.App) || e.Auth == nil || e.Auth.IsSuperuser() {
		return e.NotFoundError("Credentials are not available.", nil)
	}

	now := time.Now()

	orgs, err := core.MyCredentialStatus(e.App, e.Auth.Id, now, 0)
	if err != nil {
		return e.InternalServerError("", err)
	}

	return e.JSON(http.StatusOK, map[string]any{
		"asOf": now.UTC(), "expiringDays": core.DefaultCredentialExpiringDays, "orgs": orgs,
	})
}
