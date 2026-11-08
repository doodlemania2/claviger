package apis

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/mails"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #79b): organization invitations.
//
//	POST   /api/orgs/{id}/invitations                   {email, role?, tags?} create and email (owner/admin)
//	GET    /api/orgs/{id}/invitations                   list (owner/admin); ?status=pending|accepted|revoked|expired
//	POST   /api/orgs/{id}/invitations/{inv}/resend      new token, new expiry, email again (owner/admin)
//	DELETE /api/orgs/{id}/invitations/{inv}             revoke (owner/admin)
//	POST   /api/invitations/accept                      {token} as the signed-in invitee
//	POST   /api/invitations/register                    {token, password, passwordConfirm, name?} no account yet
//
// The token travels only in the emailed link (/orgs/invite/{token}); the API
// never returns it. The guards live in core/invitation.go.
func bindInvitationsApi(app core.App, rg *router.RouterGroup[*core.RequestEvent], orgs *router.RouterGroup[*core.RequestEvent]) {
	orgs.GET("/{id}/invitations", invitationsList)
	orgs.POST("/{id}/invitations", invitationsCreate)
	orgs.POST("/{id}/invitations/{inv}/resend", invitationsResend)
	orgs.DELETE("/{id}/invitations/{inv}", invitationsRevoke)

	limiter := invitationLimiter(app)

	inv := rg.Group("/invitations")
	inv.POST("/accept", invitationsAccept(limiter)).Bind(RequireAuth())
	inv.POST("/register", invitationsRegister(limiter)).Bind(requireCaptcha(core.CaptchaActionSignUp))
}

// newInvitationTokenLimiter caps the token guesses per client: a token has 256
// bits, so this is about log noise and cost, not about guessability.
func newInvitationTokenLimiter(app core.App) *FixedWindowLimiter {
	return NewFixedWindowLimiter(app, "invitation-token", 60, 10*time.Minute)
}

func invitationLimited(e *core.RequestEvent, l *FixedWindowLimiter) error {
	if ok, wait := l.Allow(e.RealIP()); !ok {
		e.Response.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))

		return e.TooManyRequestsError("Too many attempts. Try again later.", nil)
	}

	return nil
}

func invitationJSON(app core.App, inv *core.Record) map[string]any {
	return map[string]any{
		"id":         inv.Id,
		"org":        inv.GetString("org"),
		"email":      inv.GetString("email"),
		"role":       inv.GetString("role"),
		"tags":       core.OrgTagNames(app, inv),
		"status":     core.EffectiveInvitationStatus(inv),
		"invitedBy":  inv.GetString("invitedBy"),
		"acceptedBy": inv.GetString("acceptedBy"),
		"acceptedAt": inv.GetDateTime("acceptedAt"),
		"expires":    inv.GetDateTime("expires"),
		"lastSentAt": inv.GetDateTime("lastSentAt"),
		"sendCount":  inv.GetInt("sendCount"),
		"created":    inv.GetDateTime("created"),
		"updated":    inv.GetDateTime("updated"),
	}
}

// inviterName is how the email names the person who invites.
func inviterName(app core.App, actor *core.OrgActor) string {
	if actor.Record == nil || actor.Superuser {
		return "An administrator"
	}

	if actor.Record.Collection().Fields.GetByName("name") != nil {
		if n := strings.TrimSpace(actor.Record.GetString("name")); n != "" {
			return n
		}
	}

	return actor.Record.Email()
}

func sendInvitationEmail(e *core.RequestEvent, org, inv *core.Record, actor *core.OrgActor, token string) bool {
	if err := mails.SendOrgInvitation(e.App, org, inv, inviterName(e.App, actor), token); err != nil {
		e.App.Logger().Warn("Failed to send an organization invitation", "error", err, "org", org.Id, "invitation", inv.Id)

		return false
	}

	return true
}

func invitationFromPath(e *core.RequestEvent, org *core.Record) (*core.Record, error) {
	inv, err := e.App.FindRecordById(core.CollectionNameInvitations, e.Request.PathValue("inv"))
	if err != nil || inv.GetString("org") != org.Id {
		return nil, e.NotFoundError("Invitation not found.", nil)
	}

	return inv, nil
}

func invitationsList(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	if !actor.CanManage() {
		return e.ForbiddenError("Only owners and admins can see invitations.", nil)
	}

	if org.GetBool("archived") && !actor.Superuser {
		return e.NotFoundError("Organization not found.", nil)
	}

	_ = core.ExpireOrgInvitations(e.App, context.Background(), org.Id)

	q := e.Request.URL.Query()
	page, perPage := pageParams(e)

	filter := "org={:org}"
	params := dbx.Params{"org": org.Id}

	if v := q.Get("status"); v != "" && v != "all" {
		filter += " && status={:status}"
		params["status"] = v
	}

	all, err := e.App.FindRecordsByFilter(core.CollectionNameInvitations, filter, "-created", 0, 0, params)
	if err != nil {
		return e.InternalServerError("", err)
	}

	total := len(all)
	items := []map[string]any{}

	start := (page - 1) * perPage
	if start < total {
		for _, inv := range all[start:min(start+perPage, total)] {
			items = append(items, invitationJSON(e.App, inv))
		}
	}

	return e.JSON(http.StatusOK, map[string]any{
		"page": page, "perPage": perPage, "totalItems": total, "items": items,
	})
}

type invitationBody struct {
	Email string   `json:"email" form:"email"`
	Role  string   `json:"role" form:"role"`
	Tags  []string `json:"tags" form:"tags"`
}

func invitationsCreate(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	var body invitationBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	inv, token, err := core.CreateInvitation(e.App, auditContext(e), actor, org, core.InvitationInput{
		Email: body.Email, Role: body.Role, Tags: body.Tags,
	})
	if err != nil {
		return orgHTTP(e, err)
	}

	out := invitationJSON(e.App, inv)
	out["emailSent"] = sendInvitationEmail(e, org, inv, actor, token)

	return e.JSON(http.StatusOK, out)
}

func invitationsResend(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	inv, err := invitationFromPath(e, org)
	if err != nil {
		return err
	}

	token, err := core.ResendInvitation(e.App, auditContext(e), actor, org, inv)
	if err != nil {
		return orgHTTP(e, err)
	}

	out := invitationJSON(e.App, inv)
	out["emailSent"] = sendInvitationEmail(e, org, inv, actor, token)

	return e.JSON(http.StatusOK, out)
}

func invitationsRevoke(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	inv, err := invitationFromPath(e, org)
	if err != nil {
		return err
	}

	if err := core.RevokeInvitation(e.App, auditContext(e), actor, org, inv); err != nil {
		return orgHTTP(e, err)
	}

	return e.NoContent(http.StatusNoContent)
}

type invitationAcceptBody struct {
	Token           string `json:"token" form:"token"`
	Password        string `json:"password" form:"password"`
	PasswordConfirm string `json:"passwordConfirm" form:"passwordConfirm"`
	Name            string `json:"name" form:"name"`
}

func invitationAcceptedJSON(org, member *core.Record) map[string]any {
	return map[string]any{
		"org":    orgJSON(org, member.GetString("role")),
		"member": member.Id,
		"role":   member.GetString("role"),
	}
}

func invitationsAccept(limiter *FixedWindowLimiter) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		if err := orgReadOnlyForKeys(e); err != nil {
			return err
		}

		if err := invitationLimited(e, limiter); err != nil {
			return err
		}

		var body invitationAcceptBody
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("Failed to read the request data.", err)
		}

		member, inv, err := core.AcceptInvitation(e.App, auditContext(e), body.Token, e.Auth)
		if err != nil {
			return orgHTTP(e, err)
		}

		org, err := e.App.FindRecordById(core.CollectionNameOrgs, inv.GetString("org"))
		if err != nil {
			return e.InternalServerError("", err)
		}

		return e.JSON(http.StatusOK, invitationAcceptedJSON(org, member))
	}
}

// invitationsRegister creates the account of an invitee who has none and
// accepts the invitation, in one transaction. The emailed link proved the
// mailbox, so the new account is verified. It does not sign the browser in:
// the hosted page signs in with the ordinary auth endpoints right after, so
// every sign-in rule of the collection (second factors included) still applies.
func invitationsRegister(limiter *FixedWindowLimiter) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		if e.Auth != nil && !e.Auth.IsSuperuser() {
			return e.BadRequestError("You are already signed in. Accept the invitation instead.", nil)
		}

		if err := invitationLimited(e, limiter); err != nil {
			return err
		}

		var body invitationAcceptBody
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("Failed to read the request data.", err)
		}

		inv, org, err := core.LookupInvitation(e.App, context.Background(), body.Token)
		if err != nil {
			return orgHTTP(e, err)
		}

		col, err := core.OrgUserCollection(e.App)
		if err != nil {
			return e.InternalServerError("", err)
		}

		if !col.PasswordAuth.Enabled {
			return e.BadRequestError("Creating an account with a password is not available here.", nil)
		}

		email := inv.GetString("email")

		if _, err := e.App.FindAuthRecordByEmail(col, email); err == nil {
			return orgHTTP(e, &core.InvitationError{
				OrgError: core.OrgError{Status: http.StatusConflict, Message: "An account with this email already exists. Sign in to accept the invitation."},
				Code:     core.InvitationCodeAccount,
			})
		}

		if body.Password == "" || body.Password != body.PasswordConfirm {
			return e.BadRequestError("Failed to create the account.", validation.Errors{
				"passwordConfirm": validation.NewError("validation_values_mismatch", "Values don't match."),
			})
		}

		var member *core.Record

		txErr := e.App.RunInTransaction(func(txApp core.App) error {
			user := core.NewRecord(col)
			user.SetEmail(email)
			user.SetPassword(body.Password)
			user.SetVerified(true)

			if col.Fields.GetByName("name") != nil {
				user.Set("name", strings.TrimSpace(body.Name))
			}

			if err := txApp.SaveWithContext(auditContextForCreate(e, col), user); err != nil {
				return err
			}

			self := core.WithAuditContext(context.Background(), core.NewSelfAuditActor(user), e.RealIP())

			member, _, err = core.AcceptInvitation(txApp, self, body.Token, user)

			return err
		})
		if txErr != nil {
			if _, ok := core.AsOrgError(txErr); ok {
				return orgHTTP(e, txErr)
			}

			return e.BadRequestError("Failed to create the account.", txErr)
		}

		return e.JSON(http.StatusOK, invitationAcceptedJSON(org, member))
	}
}
