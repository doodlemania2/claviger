package apis

import (
	"net/http"
	"strconv"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #80): households and guardian-managed minors.
//
//	GET    /api/households                                   ?org=ID: the households of an org (org staff)
//	GET    /api/households/mine                              my households, with their people
//	POST   /api/households                                   {org, name, address?, homePhone?, first?}
//	GET    /api/households/{id}                              members of the household, org staff
//	PATCH  /api/households/{id}                              {name?, address?, homePhone?, status?}
//	POST   /api/households/{id}/members                      {name, role?, guardian?, isMinor?, user?, birthDate?, allergies?, medical?}
//	PATCH  /api/households/{id}/members/{member}             the same keys, and status
//	DELETE /api/households/{id}/members/{member}
//	POST   /api/households/{id}/members/{member}/guardian    make the person a guardian
//	DELETE /api/households/{id}/members/{member}/guardian    step down / remove the guardian flag
//	POST   /api/households/{id}/merge                        {from}: move the people of another household here (staff)
//	POST   /api/households/{id}/split                        {members, name}: move people to a new household (staff or a head)
//	GET    /api/households/duplicates                        ?org=ID: people who may be the same person (staff)
//	POST   /api/households/{id}/members/{member}/photo       multipart, field "photo" (staff, a guardian, the person)
//	DELETE /api/households/{id}/members/{member}/photo
//
// Org staff are the owners and admins of the household's org and the members
// tagged "registrar". They can read and change every household of the org and
// remove a guardian at once. A guardian manages the people of their own
// household; the guards (at least one adult guardian while there is a minor;
// no login and no guardian flag for a minor) are enforced in core and hold for
// every writer. API keys are refused: the data is about families and children.
func bindHouseholdsApi(rg *router.RouterGroup[*core.RequestEvent]) {
	g := rg.Group("/households").Bind(RequireAuth())
	g.GET("", householdsList)
	g.POST("", householdsCreate)
	g.GET("/mine", householdsMine)
	g.GET("/duplicates", householdsDuplicates)
	g.GET("/{id}", householdsView)
	g.PATCH("/{id}", householdsUpdate)
	g.POST("/{id}/members", householdMembersAdd)
	g.PATCH("/{id}/members/{member}", householdMembersUpdate)
	g.DELETE("/{id}/members/{member}", householdMembersRemove)
	g.POST("/{id}/members/{member}/guardian", householdGuardianSet)
	g.DELETE("/{id}/members/{member}/guardian", householdGuardianUnset)
	g.POST("/{id}/merge", householdsMerge)
	g.POST("/{id}/split", householdsSplit)
	g.POST("/{id}/members/{member}/photo", householdPhotoSet)
	g.DELETE("/{id}/members/{member}/photo", householdPhotoRemove)
}

func householdGate(e *core.RequestEvent) error {
	if APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}

	if !core.HouseholdsAvailable(e.App) {
		return e.NotFoundError("Households are not available.", nil)
	}

	return nil
}

// householdSession loads the household and the caller's standing. A caller who
// is neither a member nor org staff gets a 404.
func householdSession(e *core.RequestEvent) (*core.Record, *core.Record, *core.HouseholdActor, error) {
	if err := householdGate(e); err != nil {
		return nil, nil, nil, err
	}

	h, org, err := core.FindHousehold(e.App, e.Request.PathValue("id"))
	if err != nil {
		return nil, nil, nil, e.NotFoundError("Household not found.", nil)
	}

	actor := core.NewHouseholdActor(e.App, org, h, e.Auth)
	if !actor.CanView() {
		return nil, nil, nil, e.NotFoundError("Household not found.", nil)
	}

	return h, org, actor, nil
}

func householdJSON(h *core.Record) map[string]any {
	return map[string]any{
		"id":        h.Id,
		"org":       h.GetString("org"),
		"name":      h.GetString("name"),
		"address":   h.Get("address"),
		"homePhone": h.GetString("homePhone"),
		"status":    h.GetString("status"),
		"created":   h.GetDateTime("created"),
		"updated":   h.GetDateTime("updated"),
	}
}

// householdMemberJSON hides the health and birth data of a person from the
// other members unless the caller manages them (staff, a guardian) or is them.
func householdMemberJSON(m *core.Record, full bool) map[string]any {
	out := map[string]any{
		"id":        m.Id,
		"household": m.GetString("household"),
		"user":      m.GetString("user"),
		"name":      m.GetString("name"),
		"role":      m.GetString("role"),
		"guardian":  m.GetBool("guardian"),
		"isMinor":   m.GetBool("isMinor"),
		"status":    m.GetString("status"),
		"source":    m.GetString("source"),
		"photo":     m.GetString("photo"),
	}

	if full {
		out["birthDate"] = m.GetString("birthDate")
		out["allergies"] = m.Get("allergies")
		out["medical"] = m.Get("medical")
	}

	return out
}

func householdView(e *core.RequestEvent, h *core.Record, actor *core.HouseholdActor) (map[string]any, error) {
	members, err := core.HouseholdMembers(e.App, h)
	if err != nil {
		return nil, err
	}

	out := householdJSON(h)
	items := make([]map[string]any, 0, len(members))

	for _, m := range members {
		full := actor.Staff || actor.Guardian() || (actor.Member != nil && actor.Member.Id == m.Id)
		items = append(items, householdMemberJSON(m, full))
	}

	out["members"] = items

	if actor.Member != nil {
		out["me"] = actor.Member.Id
	}

	out["guardian"] = actor.Guardian()
	out["staff"] = actor.Staff

	return out, nil
}

func householdsList(e *core.RequestEvent) error {
	if err := householdGate(e); err != nil {
		return err
	}

	org, err := core.FindOrg(e.App, e.Request.URL.Query().Get("org"))
	if err != nil {
		return e.NotFoundError("Organization not found.", nil)
	}

	if actor := core.NewHouseholdActor(e.App, org, nil, e.Auth); !actor.Staff {
		return e.NotFoundError("Organization not found.", nil)
	}

	perPage, _ := strconv.Atoi(e.Request.URL.Query().Get("perPage"))
	if perPage < 1 || perPage > 200 {
		perPage = 50
	}

	page, _ := strconv.Atoi(e.Request.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	list, err := core.OrgHouseholds(e.App, org.Id, perPage, (page-1)*perPage)
	if err != nil {
		return e.InternalServerError("", err)
	}

	items := make([]map[string]any, 0, len(list))
	for _, h := range list {
		items = append(items, householdJSON(h))
	}

	return e.JSON(http.StatusOK, map[string]any{"page": page, "perPage": perPage, "items": items})
}

func householdsMine(e *core.RequestEvent) error {
	if err := householdGate(e); err != nil {
		return err
	}

	if e.Auth == nil || e.Auth.IsSuperuser() {
		return e.JSON(http.StatusOK, map[string]any{"items": []any{}})
	}

	list, err := core.MyHouseholds(e.App, e.Auth.Id)
	if err != nil {
		return e.InternalServerError("", err)
	}

	items := make([]map[string]any, 0, len(list))

	for _, h := range list {
		org, err := e.App.FindRecordById(core.CollectionNameOrgs, h.GetString("org"))
		if err != nil {
			continue
		}

		v, err := householdView(e, h, core.NewHouseholdActor(e.App, org, h, e.Auth))
		if err != nil {
			return e.InternalServerError("", err)
		}

		items = append(items, v)
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func householdsCreate(e *core.RequestEvent) error {
	if err := householdGate(e); err != nil {
		return err
	}

	var body map[string]any
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	orgRef, _ := body["org"].(string)

	org, err := core.FindOrg(e.App, orgRef)
	if err != nil {
		return e.NotFoundError("Organization not found.", nil)
	}

	in := core.HouseholdInput{Org: org.Id, Address: body["address"]}
	in.Name, _ = body["name"].(string)
	in.HomePhone, _ = body["homePhone"].(string)
	in.First, _ = body["first"].(map[string]any)

	h, err := core.CreateHousehold(e.App, auditContext(e), e.Auth, org, in)
	if err != nil {
		return orgHTTP(e, err)
	}

	actor := core.NewHouseholdActor(e.App, org, h, e.Auth)

	out, err := householdView(e, h, actor)
	if err != nil {
		return e.InternalServerError("", err)
	}

	return e.JSON(http.StatusOK, out)
}

func householdsView(e *core.RequestEvent) error {
	h, _, actor, err := householdSession(e)
	if err != nil {
		return err
	}

	out, err := householdView(e, h, actor)
	if err != nil {
		return e.InternalServerError("", err)
	}

	return e.JSON(http.StatusOK, out)
}

func householdsUpdate(e *core.RequestEvent) error {
	h, org, actor, err := householdSession(e)
	if err != nil {
		return err
	}

	var body map[string]any
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	if _, err := core.UpdateHousehold(e.App, auditContext(e), actor, org, h, body); err != nil {
		return orgHTTP(e, err)
	}

	out, err := householdView(e, h, actor)
	if err != nil {
		return e.InternalServerError("", err)
	}

	return e.JSON(http.StatusOK, out)
}

func householdMemberFromPath(e *core.RequestEvent, h *core.Record) (*core.Record, error) {
	m, err := core.FindHouseholdMember(e.App, h, e.Request.PathValue("member"))
	if err != nil {
		return nil, e.NotFoundError("Household member not found.", nil)
	}

	return m, nil
}

func householdMembersAdd(e *core.RequestEvent) error {
	h, org, actor, err := householdSession(e)
	if err != nil {
		return err
	}

	var body map[string]any
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	m, err := core.AddHouseholdMember(e.App, auditContext(e), actor, org, h, body)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, householdMemberJSON(m, true))
}

func householdMembersUpdate(e *core.RequestEvent) error {
	h, org, actor, err := householdSession(e)
	if err != nil {
		return err
	}

	m, err := householdMemberFromPath(e, h)
	if err != nil {
		return err
	}

	var body map[string]any
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	m, err = core.UpdateHouseholdMember(e.App, auditContext(e), actor, org, h, m, body)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, householdMemberJSON(m, true))
}

func householdMembersRemove(e *core.RequestEvent) error {
	h, org, actor, err := householdSession(e)
	if err != nil {
		return err
	}

	m, err := householdMemberFromPath(e, h)
	if err != nil {
		return err
	}

	if err := core.RemoveHouseholdMember(e.App, auditContext(e), actor, org, h, m); err != nil {
		return orgHTTP(e, err)
	}

	return e.NoContent(http.StatusNoContent)
}

func householdGuardianSet(e *core.RequestEvent) error { return householdGuardian(e, true) }

func householdGuardianUnset(e *core.RequestEvent) error { return householdGuardian(e, false) }

func householdGuardian(e *core.RequestEvent, guardian bool) error {
	h, org, actor, err := householdSession(e)
	if err != nil {
		return err
	}

	m, err := householdMemberFromPath(e, h)
	if err != nil {
		return err
	}

	m, err = core.SetHouseholdGuardian(e.App, auditContext(e), actor, org, h, m, guardian)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, householdMemberJSON(m, true))
}

func householdsMerge(e *core.RequestEvent) error {
	h, org, actor, err := householdSession(e)
	if err != nil {
		return err
	}

	var body struct {
		From string `json:"from" form:"from"`
	}

	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	if err := core.MergeHouseholds(e.App, auditContext(e), actor, org, h, body.From); err != nil {
		return orgHTTP(e, err)
	}

	// reload: the move bypassed the record in hand
	h, err = e.App.FindRecordById(core.CollectionNameHouseholds, h.Id)
	if err != nil {
		return e.InternalServerError("", err)
	}

	out, err := householdView(e, h, actor)
	if err != nil {
		return e.InternalServerError("", err)
	}

	return e.JSON(http.StatusOK, out)
}

func householdsSplit(e *core.RequestEvent) error {
	h, org, actor, err := householdSession(e)
	if err != nil {
		return err
	}

	var body struct {
		Members []string `json:"members" form:"members"`
		Name    string   `json:"name" form:"name"`
	}

	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	from, to, err := core.SplitHousehold(e.App, auditContext(e), actor, org, h, body.Members, body.Name)
	if err != nil {
		return orgHTTP(e, err)
	}

	views := map[string]any{}

	for key, rec := range map[string]*core.Record{"from": from, "to": to} {
		rec, err = e.App.FindRecordById(core.CollectionNameHouseholds, rec.Id)
		if err != nil {
			return e.InternalServerError("", err)
		}

		v, err := householdView(e, rec, core.NewHouseholdActor(e.App, org, rec, e.Auth))
		if err != nil {
			return e.InternalServerError("", err)
		}

		views[key] = v
	}

	return e.JSON(http.StatusOK, views)
}

func householdsDuplicates(e *core.RequestEvent) error {
	if err := householdGate(e); err != nil {
		return err
	}

	org, err := core.FindOrg(e.App, e.Request.URL.Query().Get("org"))
	if err != nil {
		return e.NotFoundError("Organization not found.", nil)
	}

	if actor := core.NewHouseholdActor(e.App, org, nil, e.Auth); !actor.Staff {
		return e.NotFoundError("Organization not found.", nil)
	}

	groups, truncated, err := core.HouseholdDuplicates(e.App, org.Id)
	if err != nil {
		return e.InternalServerError("", err)
	}

	return e.JSON(http.StatusOK, map[string]any{"items": groups, "truncated": truncated})
}

func householdPhotoSet(e *core.RequestEvent) error {
	h, org, actor, err := householdSession(e)
	if err != nil {
		return err
	}

	m, err := householdMemberFromPath(e, h)
	if err != nil {
		return err
	}

	files, err := e.FindUploadedFiles("photo")
	if err != nil || len(files) != 1 {
		return e.BadRequestError("Send exactly one image in the multipart field \"photo\".", err)
	}

	m, err = core.SetHouseholdMemberPhoto(e.App, auditContext(e), actor, org, h, m, files[0])
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, householdMemberJSON(m, true))
}

func householdPhotoRemove(e *core.RequestEvent) error {
	h, org, actor, err := householdSession(e)
	if err != nil {
		return err
	}

	m, err := householdMemberFromPath(e, h)
	if err != nil {
		return err
	}

	m, err = core.RemoveHouseholdMemberPhoto(e.App, auditContext(e), actor, org, h, m)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, householdMemberJSON(m, true))
}
