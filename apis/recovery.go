package apis

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/mails"
	"github.com/doodlemania2/claviger/tools/router"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #83): staff-vouched recovery codes. See docs/recovery.md
// and core/recovery.go for the rules.
//
//	GET  /api/orgs/{id}/recovery                       open requests of the org (greeter, owner, admin)
//	POST /api/orgs/{id}/recovery                       {user, verification, channel?, note?} issue; fresh passkey
//	POST /api/orgs/{id}/recovery/{rid}/confirm         second staff member, for an owner or admin; fresh passkey
//	POST /api/recovery/redeem                          {code} unauthenticated; returns the passkey reset token
//	POST /api/recovery/report                          {token} unauthenticated; "this was not me"
//
// Issue and confirm answer with the code, once. List never returns a code, a
// reference or a hash. API keys are refused on the staff routes.
func bindRecoveryApi(app core.App, rg *router.RouterGroup[*core.RequestEvent], orgs *router.RouterGroup[*core.RequestEvent]) {
	orgs.GET("/{id}/recovery", recoveryList)
	orgs.POST("/{id}/recovery", recoveryIssue).Bind(RequireFreshPasskey(webauthnStepUpMaxAge))
	orgs.POST("/{id}/recovery/{rid}/confirm", recoveryConfirm).Bind(RequireFreshPasskey(webauthnStepUpMaxAge))

	rec := rg.Group("/recovery")
	rec.POST("/redeem", recoveryRedeem(recoveryRedeemLimiter(app)))
	rec.POST("/report", recoveryReport(recoveryReportLimiter(app)))
}

func recoveryRedeemLimiter(app core.App) *FixedWindowLimiter {
	return storedLimiter(app, "__clvRecoveryRedeemLimiter__", "recovery-redeem", 15, 10*time.Minute)
}

func recoveryReportLimiter(app core.App) *FixedWindowLimiter {
	return storedLimiter(app, "__clvRecoveryReportLimiter__", "recovery-report", 15, 10*time.Minute)
}

func storedLimiter(app core.App, storeKey, name string, max int, window time.Duration) *FixedWindowLimiter {
	if l, ok := app.Store().Get(storeKey).(*FixedWindowLimiter); ok && l != nil {
		return l
	}

	l := NewFixedWindowLimiter(app, name, max, window)
	app.Store().Set(storeKey, l)

	return l
}

// recoveryStaffSession loads the org and checks that the caller is recovery staff.
func recoveryStaffSession(e *core.RequestEvent) (*core.Record, *core.OrgActor, error) {
	if APIKeyFromRequest(e) != nil {
		return nil, nil, apiKeyForbidden(e)
	}

	if !core.RecoveryAvailable(e.App) {
		return nil, nil, e.NotFoundError("Recovery codes are not available.", nil)
	}

	org, err := core.FindOrg(e.App, e.Request.PathValue("id"))
	if err != nil {
		return nil, nil, e.NotFoundError("Organization not found.", nil)
	}

	actor := core.NewOrgActor(e.App, org, e.Auth)

	if !actor.IsMember() {
		return nil, nil, e.NotFoundError("Organization not found.", nil)
	}

	return org, actor, nil
}

type recoveryBody struct {
	User         string `json:"user" form:"user"`
	Channel      string `json:"channel" form:"channel"`
	Verification string `json:"verification" form:"verification"`
	Note         string `json:"note" form:"note"`
}

func recoveryIssue(e *core.RequestEvent) error {
	org, actor, err := recoveryStaffSession(e)
	if err != nil {
		return err
	}

	var body recoveryBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	res, err := core.IssueRecovery(e.App, auditContext(e), actor, org, core.RecoveryInput{
		User: body.User, Channel: body.Channel, Verification: body.Verification, Note: body.Note,
	})
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusCreated, recoveryIssuedJSON(e, org, res))
}

func recoveryConfirm(e *core.RequestEvent) error {
	org, actor, err := recoveryStaffSession(e)
	if err != nil {
		return err
	}

	res, err := core.ConfirmRecovery(e.App, auditContext(e), actor, org, e.Request.PathValue("rid"))
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, recoveryIssuedJSON(e, org, res))
}

// recoveryIssuedJSON builds the one response that carries a code, and sends
// the notices once a code exists.
func recoveryIssuedJSON(e *core.RequestEvent, org *core.Record, res *core.RecoveryIssued) map[string]any {
	rec := res.Record

	out := map[string]any{
		"id":           rec.Id,
		"org":          org.Id,
		"user":         res.Target.Id,
		"channel":      rec.GetString("channel"),
		"verification": rec.GetString("verification"),
		"expires":      rec.GetDateTime("expires"),
		"pending":      res.Pending,
		"state":        core.RecoveryState(rec, time.Now()),
	}

	if name := res.Target.GetString("name"); name != "" {
		out["userName"] = name
	}

	if res.Pending {
		return out
	}

	switch {
	case res.Token != "":
		out["token"] = res.Token
		out["url"] = strings.TrimRight(e.App.Settings().Meta.AppURL, "/") + "/recovery#code=" + res.Token
	default:
		out["code"] = res.DisplayCode()
	}

	out["emailSent"] = notifyRecoveryTarget(e, org, res)
	out["smsSent"] = false

	if rec.GetString("channel") == core.RecoveryChannelSMS {
		out["smsSent"] = sendRecoveryLinkSMS(e.App, org, res.Target)
	}

	return out
}

// notifyRecoveryTarget tells the person that staff issued a code. Email now;
// an SMS notice is added in sendRecoveryNoticeSMS.
func notifyRecoveryTarget(e *core.RequestEvent, org *core.Record, res *core.RecoveryIssued) bool {
	at := time.Now()

	var sent bool

	if token, err := core.NewRecoveryReportToken(res.Target, res.Record.Id); err != nil {
		e.App.Logger().Warn("Failed to sign a recovery report link", "error", err, "org", org.Id)
	} else if res.Target.Email() != "" {
		if err := mails.SendRecoveryNotice(e.App, org, res.Target, at, token); err != nil {
			e.App.Logger().Warn("Failed to send a recovery notice", "error", err, "org", org.Id, "recovery", res.Record.Id)
		} else {
			sent = true
		}
	}

	_ = sendRecoveryNoticeSMS(e.App, org, res.Target, at)

	return sent
}

// sendRecoveryNoticeSMS is the place for the SMS version of the notice ("staff
// issued a recovery code at 10:14, tell us if that was not you") for a person
// with a phone number and no email. There is no SMS sender yet (tools/sms is
// built separately), so it sends nothing and reports false.
func sendRecoveryNoticeSMS(_ core.App, _, _ *core.Record, _ time.Time) bool { return false }

// sendRecoveryLinkSMS is the place for the "sms" channel: a text message with
// the link to the hosted recovery page, sent to the person's own phone while
// they are with staff. The link alone is not enough, the code is still read
// out. It sends nothing yet and reports false.
func sendRecoveryLinkSMS(_ core.App, _, _ *core.Record) bool { return false }

func recoveryList(e *core.RequestEvent) error {
	org, actor, err := recoveryStaffSession(e)
	if err != nil {
		return err
	}

	if !core.RecoveryStaff(e.App, org, actor) {
		return e.ForbiddenError("Only greeters, owners and admins can do this.", nil)
	}

	now := time.Now()

	rows, err := e.App.FindRecordsByFilter(core.CollectionNameRecoveryCodes,
		"org={:org} && usedAt='' && reportedAt='' && expires>{:now}", "-created", 100, 0,
		dbx.Params{"org": org.Id, "now": types.NowDateTime().String()})
	if err != nil {
		return e.InternalServerError("", err)
	}

	items := make([]map[string]any, 0, len(rows))

	for _, r := range rows {
		item := map[string]any{
			"id":           r.Id,
			"user":         r.GetString("user"),
			"issuedBy":     r.GetString("issuedBy"),
			"channel":      r.GetString("channel"),
			"verification": r.GetString("verification"),
			"expires":      r.GetDateTime("expires"),
			"state":        core.RecoveryState(r, now),
			"created":      r.GetDateTime("created"),
		}

		item["canConfirm"] = r.GetBool("elevated") && r.GetString("confirmedBy") == "" &&
			r.GetString("issuedBy") != actor.Record.Id && r.GetString("user") != actor.Record.Id

		if col, err := core.OrgUserCollection(e.App); err == nil {
			if u, err := e.App.FindRecordById(col, r.GetString("user")); err == nil && col.Fields.GetByName("name") != nil {
				item["userName"] = u.GetString("name")
			}
		}

		items = append(items, item)
	}

	return e.JSON(http.StatusOK, map[string]any{
		"items":       items,
		"lifetimeMin": int(core.RecoveryLifetime() / time.Minute),
		"perHour":     core.RecoveryIssueLimit(),
	})
}

type recoveryRedeemBody struct {
	Code  string `json:"code" form:"code"`
	Token string `json:"token" form:"token"`
}

// recoveryRedeem answers one way for every failure.
func recoveryRedeem(limiter *FixedWindowLimiter) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		if ok, wait := limiter.Allow(e.RealIP()); !ok {
			e.Response.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))

			return e.TooManyRequestsError("Too many attempts. Try again later.", nil)
		}

		var body recoveryRedeemBody
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("Invalid or expired code.", nil)
		}

		code := body.Code
		if code == "" {
			code = body.Token
		}

		token, col, err := core.RedeemRecovery(e.App, code, e.RealIP())
		if err != nil {
			return e.BadRequestError("Invalid or expired code.", nil)
		}

		e.Response.Header().Set("Cache-Control", "no-store")

		return e.JSON(http.StatusOK, map[string]any{"token": token, "collection": col.Name})
	}
}

type recoveryReportBody struct {
	Token string `json:"token" form:"token"`
}

func recoveryReport(limiter *FixedWindowLimiter) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		if ok, wait := limiter.Allow(e.RealIP()); !ok {
			e.Response.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))

			return e.TooManyRequestsError("Too many attempts. Try again later.", nil)
		}

		var body recoveryReportBody
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("Invalid or expired link.", nil)
		}

		userId, recordId, err := core.ParseRecoveryReportToken(e.App, body.Token)
		if err != nil {
			return e.BadRequestError("Invalid or expired link.", nil)
		}

		if err := core.ReportRecovery(e.App, auditContext(e), recordId, userId); err != nil {
			return e.BadRequestError("Invalid or expired link.", nil)
		}

		return e.NoContent(http.StatusNoContent)
	}
}
