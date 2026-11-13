package apis

import (
	"net/http"
	"strconv"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #79a): organizations API.
//
//	GET    /api/orgs                                 orgs of the caller (all for superusers)
//	POST   /api/orgs                                 create
//	GET    /api/orgs/rule-templates                  ready-made API rules for org-owned collections (superusers)
//	GET    /api/orgs/config                          effective depth limits and kinds in use (superusers, #84)
//	GET    /api/orgs/ancestor-rule                   oversight rule text for a field (superusers, #84)
//	GET    /api/orgs/transfers                       my transfers, and those of people I guard (#84)
//	GET    /api/orgs/{id}/status                     counts only: levels, descendants by kind, members (owner/admin, #84)
//	GET    /api/orgs/{id}                            one org (id or slug)
//	PATCH  /api/orgs/{id}                            name, slug, parent (superuser), archived
//	DELETE /api/orgs/{id}                            permanent delete; archived orgs only, body {"confirm": "<slug>"}
//	GET    /api/orgs/{id}/members                    members of the org
//	POST   /api/orgs/{id}/members                    add an existing user
//	PATCH  /api/orgs/{id}/members/{member}           role, status, tags
//	DELETE /api/orgs/{id}/members/{member}           remove (or leave)
//
// and the invitations routes of invitations.go (#79b), the credentials routes
// (#81) and the recovery routes of recovery.go (#83).
//
// The authorization lives in core (org_members.go) so that invitations and
// SCIM use the same guards. Every change is saved with the audit context, so
// the entries carry the actor and meta.orgId. API keys can read but never
// change organizations.
func bindOrgsApi(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	sub := rg.Group("/orgs").Bind(RequireAuth())
	sub.GET("", orgsList)
	sub.POST("", orgsCreate)
	sub.GET("/rule-templates", orgRuleTemplates).Bind(RequireSuperuserAuth())
	sub.GET("/config", orgsConfig).Bind(RequireSuperuserAuth())
	sub.GET("/ancestor-rule", orgAncestorRule).Bind(RequireSuperuserAuth())
	sub.GET("/transfers", orgTransfersMine)
	sub.GET("/{id}", orgsView)
	sub.GET("/{id}/status", orgStatus)
	sub.PATCH("/{id}", orgsUpdate)
	sub.DELETE("/{id}", orgsDelete)
	sub.GET("/{id}/members", orgMembersList)
	sub.POST("/{id}/members", orgMembersAdd)
	sub.PATCH("/{id}/members/{member}", orgMembersUpdate)
	sub.DELETE("/{id}/members/{member}", orgMembersRemove)
	bindInvitationsApi(app, rg, sub) // fork-local (#79b)
	bindRecoveryApi(app, rg, sub)    // fork-local (#83)
	bindOrgTagsApi(sub)              // fork-local (#79b)
	bindSCIMTokensApi(sub)           // fork-local (#79c)
	bindCredentialsApi(sub)          // fork-local (#81)
	bindOrgTransfersApi(sub)         // fork-local (#84)
	bindConsentsApi(rg, sub)         // fork-local (#88, 88-2)
}

func orgHTTP(e *core.RequestEvent, err error) error {
	if oe, ok := core.AsOrgError(err); ok {
		var data any

		// #79b: a stable code lets the hosted pages guide the user
		if code := core.InvitationErrorCode(err); code != "" {
			data = validation.Errors{"invitation": validation.NewError(code, oe.Message)}
		}

		return e.Error(oe.Status, oe.Message, data)
	}

	return e.BadRequestError("", err)
}

func orgReadOnlyForKeys(e *core.RequestEvent) error {
	if e.Request.Method != http.MethodGet && APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}

	return nil
}

// orgSession loads the org and the caller's standing. Callers who cannot see
// the org get a 404.
func orgSession(e *core.RequestEvent) (*core.Record, *core.OrgActor, error) {
	if err := orgReadOnlyForKeys(e); err != nil {
		return nil, nil, err
	}

	org, err := core.FindOrg(e.App, e.Request.PathValue("id"))
	if err != nil {
		return nil, nil, e.NotFoundError("Organization not found.", nil)
	}

	actor := core.NewOrgActor(e.App, org, e.Auth)

	// the owner of an archived org keeps access, to restore or delete it
	if actor.Role == "" && !actor.Superuser && org.GetBool("archived") {
		if m, err := core.FindOrgMember(e.App, org.Id, e.Auth.Id); err == nil &&
			m.GetString("status") == core.OrgStatusActive && m.GetString("role") == core.OrgRoleOwner {
			actor.Role = core.OrgRoleOwner
		}
	}

	if !actor.IsMember() {
		return nil, nil, e.NotFoundError("Organization not found.", nil)
	}

	return org, actor, nil
}

func orgJSON(org *core.Record, role string) map[string]any {
	out := map[string]any{
		"id":       org.Id,
		"name":     org.GetString("name"),
		"slug":     org.GetString("slug"),
		"parent":   org.GetString("parent"),
		"kind":     org.GetString("kind"),
		"archived": org.GetBool("archived"),
		"created":  org.GetDateTime("created"),
		"updated":  org.GetDateTime("updated"),
	}

	if role != "" {
		out["role"] = role
	}

	return out
}

func pageParams(e *core.RequestEvent) (page, perPage int) {
	page, _ = strconv.Atoi(e.Request.URL.Query().Get("page"))
	perPage, _ = strconv.Atoi(e.Request.URL.Query().Get("perPage"))

	if page < 1 {
		page = 1
	}

	if perPage < 1 {
		perPage = 30
	}

	if perPage > 200 {
		perPage = 200
	}

	return page, perPage
}

func orgsList(e *core.RequestEvent) error {
	q := e.Request.URL.Query()
	page, perPage := pageParams(e)
	showArchived := q.Get("archived")

	items := []map[string]any{}
	total := 0

	if e.Auth.IsSuperuser() {
		filter := "id != ''"
		params := dbx.Params{}

		switch showArchived {
		case "true":
			filter = "archived = true"
		case "all":
		default:
			filter = "archived = false"
		}

		if s := strings.TrimSpace(q.Get("q")); s != "" {
			filter += " && (name ~ {:q} || slug ~ {:q})"
			params["q"] = s
		}

		col, err := e.App.FindCachedCollectionByNameOrId(core.CollectionNameOrgs)
		if err != nil {
			return e.InternalServerError("", err)
		}

		all, err := e.App.FindRecordsByFilter(col, filter, "name", 0, 0, params)
		if err != nil {
			return e.InternalServerError("", err)
		}

		total = len(all)

		start := (page - 1) * perPage
		if start < total {
			for _, o := range all[start:min(start+perPage, total)] {
				items = append(items, orgJSON(o, ""))
			}
		}
	} else {
		filter := "user={:u} && status='active' && org.archived=false"
		if showArchived == "true" {
			filter = "user={:u} && status='active' && role='owner' && org.archived=true"
		}

		members, err := e.App.FindRecordsByFilter(core.CollectionNameOrgMembers, filter, "created", 0, 0, dbx.Params{"u": e.Auth.Id})
		if err != nil {
			return e.InternalServerError("", err)
		}

		total = len(members)

		start := (page - 1) * perPage
		if start < total {
			for _, m := range members[start:min(start+perPage, total)] {
				org, err := e.App.FindRecordById(core.CollectionNameOrgs, m.GetString("org"))
				if err != nil {
					continue
				}

				items = append(items, orgJSON(org, m.GetString("role")))
			}
		}
	}

	return e.JSON(http.StatusOK, map[string]any{
		"page": page, "perPage": perPage, "totalItems": total, "items": items,
	})
}

type orgCreateBody struct {
	Name   string `json:"name" form:"name"`
	Slug   string `json:"slug" form:"slug"`
	Parent string `json:"parent" form:"parent"`
	Kind   string `json:"kind" form:"kind"`
	Owner  string `json:"owner" form:"owner"`
}

func orgsCreate(e *core.RequestEvent) error {
	if err := orgReadOnlyForKeys(e); err != nil {
		return err
	}

	var body orgCreateBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	actor := &core.OrgActor{Record: e.Auth, Superuser: e.Auth.IsSuperuser()}

	org, err := core.CreateOrg(e.App, auditContext(e), actor, core.OrgInput{
		Name: body.Name, Slug: body.Slug, Parent: body.Parent, Kind: body.Kind, Owner: body.Owner,
	})
	if err != nil {
		return orgHTTP(e, err)
	}

	role := ""
	if !actor.Superuser {
		role = core.OrgRoleOwner
	}

	return e.JSON(http.StatusOK, orgJSON(org, role))
}

func orgRuleTemplates(e *core.RequestEvent) error {
	field := e.Request.URL.Query().Get("field")
	if field == "" {
		field = "org"
	}

	out := map[string]any{"field": field, "presets": core.OrgRulePresets(field)}

	// #81: ?credentialType=<key>[&credentialScope=org|ancestors|any][&credentialMaxDepth=N]
	// adds the "members, with a current credential" rule set
	if key := e.Request.URL.Query().Get("credentialType"); key != "" {
		depth, _ := strconv.Atoi(e.Request.URL.Query().Get("credentialMaxDepth"))

		set, err := core.CredentialRuleSet(field, core.CredentialRuleOpts{
			Type: key, Scope: e.Request.URL.Query().Get("credentialScope"), MaxDepth: depth,
		})
		if err != nil {
			return e.BadRequestError(err.Error(), nil)
		}

		out["credentialGated"] = set
	}

	return e.JSON(http.StatusOK, out)
}

func orgsView(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	return e.JSON(http.StatusOK, orgJSON(org, actor.Role))
}

type orgUpdateBody struct {
	Name     *string `json:"name" form:"name"`
	Slug     *string `json:"slug" form:"slug"`
	Parent   *string `json:"parent" form:"parent"`
	Kind     *string `json:"kind" form:"kind"`
	Archived *bool   `json:"archived" form:"archived"`
}

func orgsUpdate(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	var body orgUpdateBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	if err := core.UpdateOrg(e.App, auditContext(e), actor, org, core.OrgUpdate{
		Name: body.Name, Slug: body.Slug, Parent: body.Parent, Kind: body.Kind, Archived: body.Archived,
	}); err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, orgJSON(org, actor.Role))
}

func orgsDelete(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	if !actor.Superuser && actor.Role != core.OrgRoleOwner {
		return e.ForbiddenError("Only owners can delete the organization.", nil)
	}

	var body struct {
		Confirm string `json:"confirm" form:"confirm"`
	}
	_ = e.BindBody(&body)

	if body.Confirm != org.GetString("slug") {
		return e.BadRequestError(`Send {"confirm": "<slug>"} to confirm the deletion.`, nil)
	}

	if err := core.DeleteOrg(e.App, auditContext(e), org); err != nil {
		return orgHTTP(e, err)
	}

	return e.NoContent(http.StatusNoContent)
}

// ---- members ---------------------------------------------------------------

func orgMemberJSON(app core.App, member *core.Record, detailed bool) map[string]any {
	out := map[string]any{
		"id":      member.Id,
		"org":     member.GetString("org"),
		"user":    member.GetString("user"),
		"role":    member.GetString("role"),
		"status":  member.GetString("status"),
		"source":  member.GetString("source"),
		"tags":    core.OrgTagNames(app, member),
		"created": member.GetDateTime("created"),
		"updated": member.GetDateTime("updated"),
	}

	if col, err := core.OrgUserCollection(app); err == nil {
		if u, err := app.FindRecordById(col, member.GetString("user")); err == nil {
			if col.Fields.GetByName("name") != nil {
				out["name"] = u.GetString("name")
			}

			if detailed || u.GetBool("emailVisibility") {
				out["email"] = u.Email()
			}
		}
	}

	return out
}

func orgMembersList(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	if org.GetBool("archived") && !actor.Superuser {
		return e.NotFoundError("Organization not found.", nil)
	}

	q := e.Request.URL.Query()
	page, perPage := pageParams(e)

	filter := "org={:org}"
	params := dbx.Params{"org": org.Id}

	if v := q.Get("role"); v != "" {
		filter += " && role={:role}"
		params["role"] = v
	}

	if v := q.Get("status"); v != "" {
		filter += " && status={:status}"
		params["status"] = v
	}

	// viewers and members see who is active only
	detailed := actor.CanManage()
	if !detailed {
		filter += " && status='active'"
	}

	all, err := e.App.FindRecordsByFilter(core.CollectionNameOrgMembers, filter, "created", 0, 0, params)
	if err != nil {
		return e.InternalServerError("", err)
	}

	total := len(all)
	items := []map[string]any{}

	start := (page - 1) * perPage
	if start < total {
		for _, m := range all[start:min(start+perPage, total)] {
			items = append(items, orgMemberJSON(e.App, m, detailed))
		}
	}

	return e.JSON(http.StatusOK, map[string]any{
		"page": page, "perPage": perPage, "totalItems": total, "items": items,
	})
}

type orgMemberBody struct {
	User   string    `json:"user" form:"user"`
	Role   *string   `json:"role" form:"role"`
	Status *string   `json:"status" form:"status"`
	Tags   *[]string `json:"tags" form:"tags"`
}

func orgMembersAdd(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	var body orgMemberBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	in := core.OrgMemberInput{User: body.User}
	if body.Role != nil {
		in.Role = *body.Role
	}

	if body.Status != nil {
		in.Status = *body.Status
	}

	if body.Tags != nil {
		in.Tags = *body.Tags
	}

	m, err := core.AddOrgMember(e.App, auditContext(e), actor, org, in)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, orgMemberJSON(e.App, m, true))
}

func orgMemberFromPath(e *core.RequestEvent, org *core.Record) (*core.Record, error) {
	m, err := e.App.FindRecordById(core.CollectionNameOrgMembers, e.Request.PathValue("member"))
	if err != nil || m.GetString("org") != org.Id {
		return nil, e.NotFoundError("Member not found.", nil)
	}

	return m, nil
}

func orgMembersUpdate(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	m, err := orgMemberFromPath(e, org)
	if err != nil {
		return err
	}

	var body orgMemberBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	updated, err := core.UpdateOrgMember(e.App, auditContext(e), actor, org, m,
		core.OrgMemberPatch{Role: body.Role, Status: body.Status, Tags: body.Tags})
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, orgMemberJSON(e.App, updated, true))
}

func orgMembersRemove(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	m, err := orgMemberFromPath(e, org)
	if err != nil {
		return err
	}

	if err := core.RemoveOrgMember(e.App, auditContext(e), actor, org, m); err != nil {
		return orgHTTP(e, err)
	}

	return e.NoContent(http.StatusNoContent)
}
