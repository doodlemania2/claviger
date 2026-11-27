package apis

import (
	"net/http"
	"strings"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #82, 82-4): kiosk registration, handoff and sessions.

type kioskRegisterBody struct {
	Org           string   `json:"org" form:"org"`
	Name          string   `json:"name" form:"name"`
	AllowedEvents []string `json:"allowedEvents" form:"allowedEvents"`
}

// kioskAdminOrg finds the org and checks that the caller is an owner or admin.
func kioskAdminOrg(e *core.RequestEvent, ref string) (*core.Record, error) {
	org, err := core.FindOrg(e.App, ref)
	if err != nil {
		return nil, e.NotFoundError("", nil)
	}

	m := core.ActiveOrgMember(e.App, org, e.Auth.Id)
	if m == nil || (m.GetString("role") != core.OrgRoleOwner && m.GetString("role") != core.OrgRoleAdmin) {
		return nil, e.NotFoundError("", nil)
	}

	return org, nil
}

func kioskJSON(k *core.Record) map[string]any {
	var allowed []string

	_ = k.UnmarshalJSONField("allowedEvents", &allowed)

	return map[string]any{
		"id": k.Id, "org": k.GetString("org"), "name": k.GetString("name"), "status": k.GetString("status"),
		"allowedEvents": allowed, "registeredBy": k.GetString("registeredBy"), "lastSeen": k.GetDateTime("lastSeen"),
	}
}

func kioskRegister(e *core.RequestEvent) error {
	var body kioskRegisterBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	org, err := kioskAdminOrg(e, body.Org)
	if err != nil {
		return err
	}

	k, token, err := core.RegisterKiosk(e.App, auditContext(e), org, e.Auth, body.Name, body.AllowedEvents)
	if err != nil {
		return orgHTTP(e, err)
	}

	out := kioskJSON(k)
	out["token"] = token // shown once; only its hash is stored

	e.Response.Header().Set("Cache-Control", "no-store")

	return e.JSON(http.StatusCreated, out)
}

func kioskList(e *core.RequestEvent) error {
	org, err := kioskAdminOrg(e, e.Request.URL.Query().Get("org"))
	if err != nil {
		return err
	}

	rows, err := e.App.FindRecordsByFilter(core.CollectionNameKiosks, "org={:o}", "created", 0, 0, map[string]any{"o": org.Id})
	if err != nil {
		return e.InternalServerError("", err)
	}

	items := make([]map[string]any, 0, len(rows))
	for _, k := range rows {
		items = append(items, kioskJSON(k))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func kioskRevoke(e *core.RequestEvent) error {
	k, err := e.App.FindRecordById(core.CollectionNameKiosks, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("", nil)
	}

	org, err := kioskAdminOrg(e, k.GetString("org"))
	if err != nil {
		return err
	}

	k, err = core.RevokeKiosk(e.App, auditContext(e), org, e.Auth, k.Id)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, kioskJSON(k))
}

type checkinHandoffBody struct {
	Household string `json:"household" form:"household"`
	Kiosk     string `json:"kiosk" form:"kiosk"`
}

// checkinHandoff is the guardian's side: with a fresh passkey on their phone
// they ask for a handoff for one kiosk and give the code to the kiosk (a QR).
func checkinHandoff(e *core.RequestEvent) error {
	var body checkinHandoffBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	tok, exp, err := core.IssueKioskHandoff(e.App, e.Auth, body.Household, body.Kiosk)
	if err != nil {
		return orgHTTP(e, err)
	}

	e.Response.Header().Set("Cache-Control", "no-store")

	return e.JSON(http.StatusOK, map[string]any{"handoff": tok, "expires": exp.UTC()})
}

type checkinKioskSessionBody struct {
	Handoff string `json:"handoff" form:"handoff"`
}

// checkinKioskSession is the kiosk's side: it exchanges the handoff for a
// short session (5 minutes by default, the org's setting).
func checkinKioskSession(e *core.RequestEvent) error {
	actor, org, err := checkinCaller(e)
	if err != nil {
		return err
	}

	if actor.Kiosk == nil {
		return e.ForbiddenError("Only a kiosk can open a kiosk session.", nil)
	}

	var body checkinKioskSessionBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	tok, exp, err := core.ExchangeKioskHandoff(e.App, actor.Kiosk, org, strings.TrimSpace(body.Handoff))
	if err != nil {
		return orgHTTP(e, err)
	}

	e.Response.Header().Set("Cache-Control", "no-store")

	return e.JSON(http.StatusOK, map[string]any{"session": tok, "expires": exp.UTC()})
}
