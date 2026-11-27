package apis

import (
	"net/http"
	"strings"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #82): children's check-in and pickup. See docs/checkin.md.
//
//	POST /api/checkin/install                       {orgs?}; superuser; creates the template collections, retention policies and tags
//	POST /api/checkin/{event}/in                    {household, children}; a guardian, or a kiosk with a kiosk session; the family code and the labels
//	POST /api/checkin/sessions/{id}/release         {code | qr | override+reason, releasedTo?}; staff or a kiosk; with none of those, the guardian's own release by a fresh passkey
//	POST /api/checkin/sessions/{id}/incident        {reason}; a coordinator, fresh passkey; legal hold on the session and its log
//	POST /api/checkin/flags                         {child, people?, reason}; a coordinator; do-not-release
//	POST /api/checkin/flags/{id}/lift               a coordinator
//	POST /api/checkin/handoffs                      {household, kiosk}; a guardian, fresh passkey; one-use handoff for a kiosk
//	POST /api/checkin/kiosk/sessions                {handoff}; a kiosk; a short kiosk session
//	POST /api/checkin/kiosks                        {org, name, allowedEvents?}; an org admin, fresh passkey; the token is shown once
//	GET  /api/checkin/kiosks?org=<id>               an org admin
//	POST /api/checkin/kiosks/{id}/revoke            an org admin; effective at once
//
// A kiosk sends its token in X-Kiosk-Token (and, for a check-in, its session
// in X-Kiosk-Session). It is not an Authorization token: nothing else in the
// API recognises it. API keys are refused on every route.
func bindCheckinApi(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	g := rg.Group("/checkin")

	g.POST("/install", checkinInstall).Bind(RequireAuth(), RequireSuperuserAuth())

	// user or kiosk
	g.POST("/{event}/in", checkinIn)
	g.POST("/sessions/{id}/release", checkinRelease)
	g.POST("/kiosk/sessions", checkinKioskSession)

	// users only
	g.POST("/sessions/{id}/incident", checkinIncident).Bind(RequireAuth(), pastoralNoAPIKey(), RequireFreshPasskey(webauthnStepUpMaxAge))
	g.POST("/flags", checkinFlagSet).Bind(RequireAuth(), pastoralNoAPIKey())
	g.POST("/flags/{id}/lift", checkinFlagLift).Bind(RequireAuth(), pastoralNoAPIKey())
	g.POST("/handoffs", checkinHandoff).Bind(RequireAuth(), pastoralNoAPIKey(), RequireFreshPasskey(webauthnStepUpMaxAge))
	g.POST("/kiosks", kioskRegister).Bind(RequireAuth(), pastoralNoAPIKey(), RequireFreshPasskey(webauthnStepUpMaxAge))
	g.GET("/kiosks", kioskList).Bind(RequireAuth(), pastoralNoAPIKey())
	g.POST("/kiosks/{id}/revoke", kioskRevoke).Bind(RequireAuth(), pastoralNoAPIKey())
}

const kioskFailRateRule = "@pb_kiosk_fail_"

var kioskFailRule = core.RateLimitRule{MaxRequests: 30, Duration: 60}

// checkinCaller identifies the principal: a signed-in user, or a kiosk by its
// token. A bad kiosk token is a 401 with a rate limit on failures.
func checkinCaller(e *core.RequestEvent) (core.CheckinActor, *core.Record, error) {
	if e.Auth != nil {
		if APIKeyFromRequest(e) != nil {
			return core.CheckinActor{}, nil, apiKeyForbidden(e)
		}

		return core.CheckinActor{User: e.Auth}, nil, nil
	}

	token := strings.TrimSpace(e.Request.Header.Get(core.KioskTokenHeader))
	if token == "" {
		return core.CheckinActor{}, nil, e.UnauthorizedError("Sign in, or use a registered kiosk.", nil)
	}

	kiosk, org, err := core.AuthenticateKiosk(e.App, token)
	if err != nil {
		if checkRateLimit(e, kioskFailRateRule, kioskFailRule) != nil {
			return core.CheckinActor{}, nil, e.TooManyRequestsError("Too many requests, please try again later.", nil)
		}

		return core.CheckinActor{}, nil, e.UnauthorizedError("The kiosk token is invalid or revoked.", nil)
	}

	return core.CheckinActor{Kiosk: kiosk}, org, nil
}

func stepUpRequired(e *core.RequestEvent) error {
	return e.ForbiddenError("A recent passkey verification is required for this action.", validation.Errors{
		"passkey": validation.NewError(StepUpRequiredCode, "A recent passkey verification is required."),
	})
}

// ---- install -------------------------------------------------------------------

type checkinInstallBody struct {
	Orgs []string `json:"orgs" form:"orgs"`
}

func checkinInstall(e *core.RequestEvent) error {
	var body checkinInstallBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	ctx := auditContext(e)

	res, err := core.CreateCheckinTemplates(e.App, ctx)
	if err != nil {
		return e.BadRequestError("Failed to install check-in: "+err.Error(), nil)
	}

	tags := map[string][]string{}

	for _, ref := range body.Orgs {
		org, err := core.FindOrg(e.App, ref)
		if err != nil {
			return e.BadRequestError("Unknown org "+ref+".", nil)
		}

		made, err := core.EnsureCheckinTags(e.App, ctx, org)
		if err != nil {
			return orgHTTP(e, err)
		}

		tags[org.Id] = made
	}

	return e.JSON(http.StatusCreated, map[string]any{
		"collections": res.Collections, "retentionPolicies": res.Policies,
		"tags": core.CheckinTags, "createdTags": tags,
	})
}

// ---- check in ------------------------------------------------------------------

type checkinInBody struct {
	Household string   `json:"household" form:"household"`
	Children  []string `json:"children" form:"children"`
}

func checkinIn(e *core.RequestEvent) error {
	actor, _, err := checkinCaller(e)
	if err != nil {
		return err
	}

	var body checkinInBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	event, err := e.App.FindRecordById(core.CheckinEventsCollection, e.Request.PathValue("event"))
	if err != nil {
		return e.NotFoundError("", nil)
	}

	guardian := actor.User

	if actor.Kiosk != nil {
		// the kiosk identifies the guardian by a short session
		claims, err := core.VerifyKioskSession(e.App, actor.Kiosk, strings.TrimSpace(e.Request.Header.Get(core.KioskSessionHeader)))
		if err != nil || claims.Household != body.Household {
			return e.ForbiddenError("The kiosk session is missing, expired or for another household.", nil)
		}

		userCol, cerr := core.OrgUserCollection(e.App)
		if cerr != nil {
			return e.InternalServerError("", cerr)
		}

		if guardian, err = e.App.FindRecordById(userCol, claims.User); err != nil {
			return e.ForbiddenError("The kiosk session is missing, expired or for another household.", nil)
		}
	}

	res, err := core.CheckIn(e.App, auditContext(e), core.CheckinInput{
		Event: event, Household: body.Household, Children: body.Children, Guardian: guardian, Actor: actor,
	})
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusCreated, res)
}

// ---- release -------------------------------------------------------------------

type checkinReleaseBody struct {
	Code       string `json:"code" form:"code"`
	QR         string `json:"qr" form:"qr"`
	ReleasedTo string `json:"releasedTo" form:"releasedTo"`
	Override   bool   `json:"override" form:"override"`
	Reason     string `json:"reason" form:"reason"`
}

func checkinRelease(e *core.RequestEvent) error {
	actor, _, err := checkinCaller(e)
	if err != nil {
		return err
	}

	var body checkinReleaseBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	in := core.ReleaseInput{
		SessionId: e.Request.PathValue("id"), Code: body.Code, QR: body.QR,
		ReleasedTo: body.ReleasedTo, Override: body.Override, Reason: body.Reason, Actor: actor,
	}

	// the guardian's own release: the new token of the step-up ceremony is the assertion
	if body.Code == "" && body.QR == "" && !body.Override {
		if actor.User == nil {
			return orgHTTP(e, core.ErrCheckinDenied)
		}

		if !HasFreshPasskey(e, webauthnStepUpMaxAge) {
			return stepUpRequired(e)
		}

		in.Assertion = true
	}

	s, err := core.ReleaseSession(e.App, auditContext(e), in)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, map[string]any{
		"id": s.Id, "state": s.GetString("state"), "method": s.GetString("method"),
		"releasedAt": s.GetDateTime("releasedAt"),
	})
}

// ---- incident, flags --------------------------------------------------------------

type checkinReasonBody struct {
	Reason string `json:"reason" form:"reason"`
}

func checkinIncident(e *core.RequestEvent) error {
	var body checkinReasonBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	s, err := core.PlaceCheckinIncidentHold(e.App, auditContext(e), e.Auth, e.Request.PathValue("id"), body.Reason)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, map[string]any{"id": s.Id, "incident": true})
}

type checkinFlagBody struct {
	Child  string   `json:"child" form:"child"`
	People []string `json:"people" form:"people"`
	Reason string   `json:"reason" form:"reason"`
}

func checkinFlagSet(e *core.RequestEvent) error {
	var body checkinFlagBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	f, err := core.SetCheckinFlag(e.App, auditContext(e), e.Auth, body.Child, body.People, body.Reason)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusCreated, map[string]any{"id": f.Id, "child": f.GetString("child"), "active": true})
}

func checkinFlagLift(e *core.RequestEvent) error {
	f, err := core.LiftCheckinFlag(e.App, auditContext(e), e.Auth, e.Request.PathValue("id"))
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, map[string]any{"id": f.Id, "active": false})
}
