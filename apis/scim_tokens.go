package apis

import (
	"net/http"
	"strings"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #79c): management of the SCIM tokens and settings of an
// org, for its owners and admins (and superusers).
//
//	GET    /api/orgs/{id}/scim-tokens             list (never the secret)
//	POST   /api/orgs/{id}/scim-tokens             create; the only response that carries the token
//	DELETE /api/orgs/{id}/scim-tokens/{token}     revoke
//	GET    /api/orgs/{id}/scim-settings           endpoint URL, default role, group-role mapping, ban option
//	PUT    /api/orgs/{id}/scim-settings           replace them
//
// API keys can read but never change them (orgSession).

var scimTokenCreateRateRule = core.RateLimitRule{MaxRequests: 20, Duration: 3600}

func bindSCIMTokensApi(sub *router.RouterGroup[*core.RequestEvent]) {
	sub.GET("/{id}/scim-tokens", scimTokensList)
	sub.POST("/{id}/scim-tokens", scimTokensCreate)
	sub.DELETE("/{id}/scim-tokens/{token}", scimTokensRevoke)
	sub.GET("/{id}/scim-settings", scimSettingsGet)
	sub.PUT("/{id}/scim-settings", scimSettingsPut)
}

func scimTokenJSON(t *core.Record) map[string]any {
	return map[string]any{
		"id":       t.Id,
		"org":      t.GetString("org"),
		"name":     t.GetString("name"),
		"prefix":   t.GetString("prefix"),
		"created":  t.GetString("created"),
		"lastUsed": t.GetString("lastUsed"),
		"expires":  t.GetString("expires"),
		"revoked":  t.GetBool("revoked"),
		"active":   !core.SCIMTokenInactive(t),
	}
}

func scimEndpoint(e *core.RequestEvent) string {
	scheme := "http"
	if e.Request.TLS != nil || strings.EqualFold(e.Request.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}

	return scheme + "://" + e.Request.Host + "/scim/v2"
}

// scimAdminSession is orgSession for owners and admins.
func scimAdminSession(e *core.RequestEvent) (*core.Record, *core.OrgActor, error) {
	org, actor, err := orgSession(e)
	if err != nil {
		return nil, nil, err
	}

	if !actor.CanManage() {
		return nil, nil, e.ForbiddenError("Only owners and admins can manage SCIM.", nil)
	}

	return org, actor, nil
}

func scimTokensList(e *core.RequestEvent) error {
	org, _, err := scimAdminSession(e)
	if err != nil {
		return err
	}

	tokens, err := core.ListSCIMTokens(e.App, org)
	if err != nil {
		return e.InternalServerError("Failed to load the SCIM tokens.", err)
	}

	items := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		items = append(items, scimTokenJSON(t))
	}

	return e.JSON(http.StatusOK, map[string]any{"endpoint": scimEndpoint(e), "items": items})
}

func scimTokensCreate(e *core.RequestEvent) error {
	org, actor, err := scimAdminSession(e)
	if err != nil {
		return err
	}

	if checkRateLimit(e, "@pb_scimtoken_create_"+e.Auth.Id, scimTokenCreateRateRule) != nil {
		return e.TooManyRequestsError("Too many requests, please try again later.", nil)
	}

	var body struct {
		Name    string `json:"name" form:"name"`
		Expires string `json:"expires" form:"expires"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	var expires time.Time

	if s := strings.TrimSpace(body.Expires); s != "" {
		dt, perr := types.ParseDateTime(s)
		if perr != nil {
			return e.BadRequestError("Invalid expires value.", nil)
		}

		expires = dt.Time()
	}

	rec, token, err := core.CreateSCIMToken(e.App, auditContext(e), actor, org, body.Name, expires)
	if err != nil {
		return orgHTTP(e, err)
	}

	out := scimTokenJSON(rec)
	out["token"] = token
	out["endpoint"] = scimEndpoint(e)

	return e.JSON(http.StatusOK, out)
}

func scimTokensRevoke(e *core.RequestEvent) error {
	org, actor, err := scimAdminSession(e)
	if err != nil {
		return err
	}

	t, err := e.App.FindRecordById(core.CollectionNameSCIMTokens, e.Request.PathValue("token"))
	if err != nil || t.GetString("org") != org.Id {
		return e.NotFoundError("Token not found.", nil)
	}

	if err := core.RevokeSCIMToken(e.App, auditContext(e), actor, org, t); err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, scimTokenJSON(t))
}

func scimSettingsJSON(e *core.RequestEvent, org *core.Record) map[string]any {
	s := core.GetSCIMSettings(org)

	return map[string]any{
		"endpoint": scimEndpoint(e), "defaultRole": s.DefaultRole, "groupRoles": s.GroupRoles,
		"deactivationBans": s.DeactivationBans,
	}
}

func scimSettingsGet(e *core.RequestEvent) error {
	org, _, err := scimAdminSession(e)
	if err != nil {
		return err
	}

	return e.JSON(http.StatusOK, scimSettingsJSON(e, org))
}

func scimSettingsPut(e *core.RequestEvent) error {
	org, actor, err := scimAdminSession(e)
	if err != nil {
		return err
	}

	var body struct {
		DefaultRole      string            `json:"defaultRole" form:"defaultRole"`
		GroupRoles       map[string]string `json:"groupRoles" form:"groupRoles"`
		DeactivationBans bool              `json:"deactivationBans" form:"deactivationBans"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	if body.GroupRoles == nil {
		body.GroupRoles = map[string]string{}
	}

	if err := core.UpdateSCIMSettings(e.App, auditContext(e), actor, org, core.SCIMSettings{
		DefaultRole: body.DefaultRole, GroupRoles: body.GroupRoles, DeactivationBans: body.DeactivationBans,
	}); err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, scimSettingsJSON(e, org))
}
