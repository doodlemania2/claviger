package apis

import (
	"net/http"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #88, 88-2): consent records.
//
//	GET    /api/me/consents                           my purposes and the minors I am guardian of, with the state
//	POST   /api/me/consents/{purpose}/grant           {member?}: for myself, or for a minor of my household (guardian)
//	POST   /api/me/consents/{purpose}/withdraw        {member?}
//	GET    /api/consent-purposes                      ?org=ID: the active purposes offered to the org's people (superusers: all)
//	POST   /api/consent-purposes                      superuser: {key, name, text, lawfulBasis?, minAge?, requiresGuardian?, marketing?, org?}
//	PATCH  /api/consent-purposes/{id}                 superuser: a changed text raises the version
//	DELETE /api/consent-purposes/{id}                 superuser: a purpose with consent records is archived instead (409)
//	GET    /api/orgs/{id}/consent-purposes            org owners and admins: the purposes of the org and the global ones
//	POST   /api/orgs/{id}/consent-purposes            org owners and admins: as above, the org is always {id}
//	PATCH  /api/orgs/{id}/consent-purposes/{pid}      org owners and admins: own org's purposes only
//	DELETE /api/orgs/{id}/consent-purposes/{pid}      org owners and admins: own org's purposes only (409 with consents)
//	GET    /api/consents/history                      superuser: ?subject=ID (a member id or a user id)
//	GET    /api/orgs/{id}/consents                    org staff: ?member=ID | ?user=ID: the person's consents and history
//	POST   /api/orgs/{id}/consents                    org staff: {purpose, member|user, action, method?, evidenceRef, givenBy?}
//
// Org staff are the owners and admins of the org and the members tagged
// "registrar". API keys are refused on all of them (enforceAPIKeyScopes):
// consents are evidence about people. See docs/consent.md.
func bindConsentsApi(rg *router.RouterGroup[*core.RequestEvent], orgs *router.RouterGroup[*core.RequestEvent]) {
	me := rg.Group("/me/consents").Bind(RequireAuth())
	me.GET("", consentsMine)
	me.POST("/{purpose}/grant", consentsSelfGrant)
	me.POST("/{purpose}/withdraw", consentsSelfWithdraw)

	p := rg.Group("/consent-purposes").Bind(RequireAuth())
	p.GET("", consentPurposesList)
	p.POST("", consentPurposesCreate).Bind(RequireSuperuserAuth())
	p.PATCH("/{id}", consentPurposesUpdate).Bind(RequireSuperuserAuth())
	p.DELETE("/{id}", consentPurposesDelete).Bind(RequireSuperuserAuth())

	rg.GET("/consents/history", consentsAdminHistory).Bind(RequireSuperuserAuth())

	orgs.GET("/{id}/consent-purposes", consentOrgPurposesList)
	orgs.POST("/{id}/consent-purposes", consentOrgPurposesCreate)
	orgs.PATCH("/{id}/consent-purposes/{pid}", consentOrgPurposesUpdate)
	orgs.DELETE("/{id}/consent-purposes/{pid}", consentOrgPurposesDelete)

	orgs.GET("/{id}/consents", consentsOrgView)
	orgs.POST("/{id}/consents", consentsOrgRecord)
}

func consentGate(e *core.RequestEvent) error {
	if APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}

	if !core.ConsentsAvailable(e.App) {
		return e.NotFoundError("Consents are not available.", nil)
	}

	return nil
}

func consentPurposeJSON(p *core.Record) map[string]any {
	return map[string]any{
		"id":               p.Id,
		"key":              p.GetString("key"),
		"name":             p.GetString("name"),
		"text":             p.GetString("text"),
		"version":          p.GetInt("version"),
		"lawfulBasis":      p.GetString("lawfulBasis"),
		"minAge":           p.GetInt("minAge"),
		"requiresGuardian": p.GetBool("requiresGuardian"),
		"marketing":        p.GetBool("marketing"),
		"org":              p.GetString("org"),
		"status":           p.GetString("status"),
		"created":          p.GetDateTime("created"),
		"updated":          p.GetDateTime("updated"),
	}
}

func consentStateJSON(s *core.Record) map[string]any {
	return map[string]any{
		"id":            s.Id,
		"subject":       s.GetString("subject"),
		"subjectType":   s.GetString("subjectType"),
		"purpose":       s.GetString("purpose"),
		"status":        core.ConsentStatus(s),
		"policyVersion": s.GetInt("policyVersion"),
		"grantedAt":     s.GetDateTime("grantedAt"),
		"withdrawnAt":   s.GetDateTime("withdrawnAt"),
		"givenBy":       s.GetString("givenBy"),
		"method":        s.GetString("method"),
		"evidenceRef":   s.GetString("evidenceRef"),
		"org":           s.GetString("org"),
	}
}

func consentEventJSON(ev *core.Record) map[string]any {
	return map[string]any{
		"id":              ev.Id,
		"subject":         ev.GetString("subject"),
		"subjectType":     ev.GetString("subjectType"),
		"purpose":         ev.GetString("purpose"),
		"action":          ev.GetString("action"),
		"policyVersion":   ev.GetInt("policyVersion"),
		"text":            ev.GetString("text"),
		"givenBy":         ev.GetString("givenBy"),
		"actor":           ev.GetString("actor"),
		"method":          ev.GetString("method"),
		"evidenceRef":     ev.GetString("evidenceRef"),
		"org":             ev.GetString("org"),
		"pseudonymizedAt": ev.GetDateTime("pseudonymizedAt"),
		"created":         ev.GetDateTime("created"),
	}
}

func consentHistoryJSON(states, events []*core.Record, purposeVersions map[string]int) map[string]any {
	st := make([]map[string]any, 0, len(states))

	for _, s := range states {
		j := consentStateJSON(s)
		if v, ok := purposeVersions[s.GetString("purpose")]; ok {
			j["currentVersion"] = v
			j["outdated"] = core.ConsentStatus(s) == core.ConsentStatusGranted && s.GetInt("policyVersion") < v
		}

		st = append(st, j)
	}

	ev := make([]map[string]any, 0, len(events))
	for _, e := range events {
		ev = append(ev, consentEventJSON(e))
	}

	return map[string]any{"consents": st, "events": ev}
}

func consentPurposeVersions(e *core.RequestEvent) map[string]int {
	out := map[string]int{}

	rows, _ := e.App.FindAllRecords(core.CollectionNameConsentPurposes)
	for _, p := range rows {
		out[p.GetString("key")] = p.GetInt("version")
	}

	return out
}

// ---- self service and guardians --------------------------------------------------

func consentsMine(e *core.RequestEvent) error {
	if err := consentGate(e); err != nil {
		return err
	}

	subjects, err := core.ConsentOverview(e.App, e.Auth)
	if err != nil {
		return e.InternalServerError("", err)
	}

	items := make([]map[string]any, 0, len(subjects))

	for _, s := range subjects {
		purposes := make([]map[string]any, 0, len(s.Purposes))

		for _, ps := range s.Purposes {
			item := map[string]any{
				"purpose":  consentPurposeJSON(ps.Purpose),
				"status":   ps.Status,
				"outdated": ps.Outdated,
				"canGrant": ps.CanGrant,
				"reason":   ps.Reason,
			}

			if ps.State != nil {
				item["consent"] = consentStateJSON(ps.State)
			}

			purposes = append(purposes, item)
		}

		out := map[string]any{"type": s.Type, "id": s.Id, "name": s.Name, "minor": s.Minor(), "purposes": purposes}
		if s.Household != nil {
			out["household"] = s.Household.Id
		}

		items = append(items, out)
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func consentsSelf(e *core.RequestEvent, action string) error {
	if err := consentGate(e); err != nil {
		return err
	}

	var body struct {
		Member string `json:"member" form:"member"`
	}
	// an empty body is fine: the consent is for the caller
	_ = e.BindBody(&body)

	res, err := core.ConsentSelf(e.App, auditContext(e), e.Auth, e.Request.PathValue("purpose"), body.Member, action)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, map[string]any{
		"changed": res.Changed,
		"consent": consentStateJSON(res.State),
	})
}

func consentsSelfGrant(e *core.RequestEvent) error { return consentsSelf(e, core.ConsentActionGrant) }

func consentsSelfWithdraw(e *core.RequestEvent) error {
	return consentsSelf(e, core.ConsentActionWithdraw)
}

// ---- purposes ----------------------------------------------------------------------

func consentPurposesList(e *core.RequestEvent) error {
	if err := consentGate(e); err != nil {
		return err
	}

	var rows []*core.Record

	var err error

	switch orgRef := e.Request.URL.Query().Get("org"); {
	case e.Auth != nil && e.Auth.IsSuperuser() && orgRef == "":
		rows, err = e.App.FindRecordsByFilter(core.CollectionNameConsentPurposes, "", "name", 0, 0)
	default:
		org, ferr := core.FindOrg(e.App, orgRef)
		if ferr != nil && orgRef != "" {
			return e.NotFoundError("Organization not found.", nil)
		}

		if ferr != nil {
			// without an org: the global purposes
			rows, err = e.App.FindRecordsByFilter(core.CollectionNameConsentPurposes,
				"org='' && status='active'", "name", 0, 0)
			break
		}

		actor := core.NewOrgActor(e.App, org, e.Auth)
		if !actor.IsMember() && !actor.Superuser {
			return e.NotFoundError("Organization not found.", nil)
		}

		filter := "(org='' || org={:o})"
		if !actor.Superuser {
			filter += " && status='active'"
		}

		rows, err = e.App.FindRecordsByFilter(core.CollectionNameConsentPurposes, filter, "name", 0, 0, map[string]any{"o": org.Id})
	}

	if err != nil {
		return e.InternalServerError("", err)
	}

	items := make([]map[string]any, 0, len(rows))
	for _, p := range rows {
		items = append(items, consentPurposeJSON(p))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func consentPurposesCreate(e *core.RequestEvent) error {
	if err := consentGate(e); err != nil {
		return err
	}

	var body map[string]any
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	p, err := core.CreateConsentPurpose(e.App, auditContext(e), body)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, consentPurposeJSON(p))
}

func consentPurposesUpdate(e *core.RequestEvent) error {
	if err := consentGate(e); err != nil {
		return err
	}

	p, err := core.FindConsentPurpose(e.App, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("Consent purpose not found.", nil)
	}

	var body map[string]any
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	p, err = core.UpdateConsentPurpose(e.App, auditContext(e), p, body)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, consentPurposeJSON(p))
}

func consentPurposesDelete(e *core.RequestEvent) error {
	if err := consentGate(e); err != nil {
		return err
	}

	p, err := core.FindConsentPurpose(e.App, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("Consent purpose not found.", nil)
	}

	if err := core.DeleteConsentPurpose(e.App, auditContext(e), p); err != nil {
		return orgHTTP(e, err)
	}

	return e.NoContent(http.StatusNoContent)
}

// ---- purposes of an org ---------------------------------------------------------------

func consentOrgFind(e *core.RequestEvent) (*core.Record, error) {
	if err := consentGate(e); err != nil {
		return nil, err
	}

	org, err := core.FindOrg(e.App, e.Request.PathValue("id"))
	if err != nil {
		return nil, e.NotFoundError("Organization not found.", nil)
	}

	return org, nil
}

func consentOrgPurposesList(e *core.RequestEvent) error {
	org, err := consentOrgFind(e)
	if err != nil {
		return err
	}

	actor := core.NewOrgActor(e.App, org, e.Auth)
	if !actor.IsMember() {
		return e.NotFoundError("Organization not found.", nil)
	}

	if !actor.CanManage() {
		return e.ForbiddenError("Only the owners and admins of the organization can manage its consent purposes.", nil)
	}

	rows, err := e.App.FindRecordsByFilter(core.CollectionNameConsentPurposes,
		"org='' || org={:o}", "name", 0, 0, map[string]any{"o": org.Id})
	if err != nil {
		return e.InternalServerError("", err)
	}

	items := make([]map[string]any, 0, len(rows))
	for _, p := range rows {
		items = append(items, consentPurposeJSON(p))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func consentOrgPurposesCreate(e *core.RequestEvent) error {
	org, err := consentOrgFind(e)
	if err != nil {
		return err
	}

	var body map[string]any
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	p, err := core.CreateOrgConsentPurpose(e.App, auditContext(e), org, e.Auth, body)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, consentPurposeJSON(p))
}

func consentOrgPurposesUpdate(e *core.RequestEvent) error {
	org, err := consentOrgFind(e)
	if err != nil {
		return err
	}

	p, err := core.FindOrgConsentPurpose(e.App, org, e.Auth, e.Request.PathValue("pid"))
	if err != nil {
		return orgHTTP(e, err)
	}

	var body map[string]any
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	p, err = core.UpdateOrgConsentPurpose(e.App, auditContext(e), org, e.Auth, p, body)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, consentPurposeJSON(p))
}

func consentOrgPurposesDelete(e *core.RequestEvent) error {
	org, err := consentOrgFind(e)
	if err != nil {
		return err
	}

	p, err := core.FindOrgConsentPurpose(e.App, org, e.Auth, e.Request.PathValue("pid"))
	if err != nil {
		return orgHTTP(e, err)
	}

	if err := core.DeleteConsentPurpose(e.App, auditContext(e), p); err != nil {
		return orgHTTP(e, err)
	}

	return e.NoContent(http.StatusNoContent)
}

// ---- history and staff ---------------------------------------------------------------

func consentsAdminHistory(e *core.RequestEvent) error {
	if err := consentGate(e); err != nil {
		return err
	}

	subject := e.Request.URL.Query().Get("subject")
	if subject == "" {
		return e.BadRequestError("subject is required.", nil)
	}

	states, events, err := core.ConsentHistory(e.App, subject)
	if err != nil {
		return e.InternalServerError("", err)
	}

	return e.JSON(http.StatusOK, consentHistoryJSON(states, events, consentPurposeVersions(e)))
}

func consentOrgSession(e *core.RequestEvent) (*core.Record, error) {
	if err := consentGate(e); err != nil {
		return nil, err
	}

	org, err := core.FindOrg(e.App, e.Request.PathValue("id"))
	if err != nil {
		return nil, e.NotFoundError("Organization not found.", nil)
	}

	actor := core.NewHouseholdActor(e.App, org, nil, e.Auth)
	if !actor.IsMember() && !actor.Superuser {
		return nil, e.NotFoundError("Organization not found.", nil)
	}

	if !actor.Staff {
		return nil, e.ForbiddenError("Only the staff of the organization can do this.", nil)
	}

	return org, nil
}

func consentsOrgView(e *core.RequestEvent) error {
	org, err := consentOrgSession(e)
	if err != nil {
		return err
	}

	q := e.Request.URL.Query()

	var subject *core.ConsentSubject

	switch {
	case q.Get("member") != "":
		subject, err = core.FindConsentMember(e.App, q.Get("member"))
		if err == nil && subject.Org.Id != org.Id {
			err = e.NotFoundError("Household member not found.", nil)
		}
	case q.Get("user") != "":
		subject, err = core.FindConsentUser(e.App, q.Get("user"))
	default:
		return e.BadRequestError("Give either member or user.", nil)
	}

	if err != nil {
		return orgHTTP(e, err)
	}

	states, events, err := core.ConsentHistory(e.App, subject.Id)
	if err != nil {
		return e.InternalServerError("", err)
	}

	// the org sees the consents of its own purposes and the global ones
	states = filterConsentRows(states, org.Id)
	events = filterConsentRows(events, org.Id)

	out := consentHistoryJSON(states, events, consentPurposeVersions(e))
	out["subject"] = map[string]any{"type": subject.Type, "id": subject.Id, "name": subject.Name, "minor": subject.Minor()}

	return e.JSON(http.StatusOK, out)
}

// filterConsentRows keeps the rows recorded for this org or for no org.
func filterConsentRows(rows []*core.Record, orgId string) []*core.Record {
	out := make([]*core.Record, 0, len(rows))

	for _, r := range rows {
		if o := r.GetString("org"); o == "" || o == orgId {
			out = append(out, r)
		}
	}

	return out
}

func consentsOrgRecord(e *core.RequestEvent) error {
	org, err := consentOrgSession(e)
	if err != nil {
		return err
	}

	var body struct {
		Purpose     string `json:"purpose" form:"purpose"`
		Member      string `json:"member" form:"member"`
		User        string `json:"user" form:"user"`
		Action      string `json:"action" form:"action"`
		Method      string `json:"method" form:"method"`
		EvidenceRef string `json:"evidenceRef" form:"evidenceRef"`
		GivenBy     string `json:"givenBy" form:"givenBy"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	res, err := core.ConsentStaff(e.App, auditContext(e), e.Auth, org, core.ConsentStaffInput{
		Purpose: body.Purpose, Member: body.Member, User: body.User, Action: body.Action,
		Method: body.Method, EvidenceRef: body.EvidenceRef, GivenBy: body.GivenBy,
	})
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, map[string]any{"changed": res.Changed, "consent": consentStateJSON(res.State)})
}
