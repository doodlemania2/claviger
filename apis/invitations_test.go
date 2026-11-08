package apis_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #79b): /api/orgs/{id}/invitations and /api/invitations.

const invBase = "/api/orgs/" + orgP1 + "/invitations"

func TestInvitationsCreate(t *testing.T) {
	t.Parallel()

	runInvCases(t, []orgCase{
		{name: "guest", method: http.MethodPost, url: invBase, body: `{"email":"a@x.test"}`, status: 401},
		{name: "members cannot invite", who: "carol", method: http.MethodPost, url: invBase, body: `{"email":"a@x.test"}`, status: 403},
		{name: "viewers cannot invite", who: "dave", method: http.MethodPost, url: invBase, body: `{"email":"a@x.test"}`, status: 403},
		{name: "outsiders get a 404", who: "frank", method: http.MethodPost, url: invBase, body: `{"email":"a@x.test"}`, status: 404},
		{name: "admins of another org get a 404", who: "erin", method: http.MethodPost, url: invBase, body: `{"email":"a@x.test"}`, status: 404},
		{name: "suspended members get a 404", who: "sam", method: http.MethodPost, url: invBase, body: `{"email":"a@x.test"}`, status: 404},
		{
			name: "an admin invites a member and the email goes out", who: "bob", method: http.MethodPost, url: invBase,
			body: `{"email":"  New.Person@Example.ORG ","role":"member","tags":["youth"]}`, status: 200,
			has:    []string{`"email":"new.person@example.org"`, `"status":"pending"`, `"emailSent":true`, `"sendCount":1`, `"tags":["youth"]`},
			hasNot: []string{`tokenHash`, `"token"`},
			after: func(t testing.TB, app *tests.TestApp) {
				if app.TestMailer.TotalSend() != 1 {
					t.Fatalf("expected one email, got %d", app.TestMailer.TotalSend())
				}

				m := app.TestMailer.Messages()[0]
				if m.To[0].Address != "new.person@example.org" || !strings.Contains(m.Subject, "Parish One") {
					t.Fatalf("unexpected message to %v: %q", m.To, m.Subject)
				}

				// the link is the hosted page on the configured app URL, and the token is stored hashed
				tok := sentLinkToken(t, app, 0)
				if !strings.Contains(m.HTML, app.Settings().Meta.AppURL+"/orgs/invite/"+tok) || !strings.Contains(m.HTML, "bob") && !strings.Contains(m.HTML, "Parish One") {
					t.Fatalf("unexpected body %q", m.HTML)
				}

				inv, err := app.FindFirstRecordByData(core.CollectionNameInvitations, "tokenHash", core.HashInvitationToken(tok))
				orgMust(t, err)

				if inv.GetString("invitedBy") != orgUserID("bob") || inv.GetString("org") != orgP1 {
					t.Fatalf("unexpected row %v", inv.PublicExport())
				}
			},
		},
		{name: "default role is member", who: "bob", method: http.MethodPost, url: invBase, body: `{"email":"a@x.test"}`, status: 200, has: []string{`"role":"member"`}},
		{name: "admins invite admins", who: "bob", method: http.MethodPost, url: invBase, body: `{"email":"a@x.test","role":"admin"}`, status: 200},
		{name: "admins cannot invite owners", who: "bob", method: http.MethodPost, url: invBase, body: `{"email":"a@x.test","role":"owner"}`, status: 403,
			after: func(t testing.TB, app *tests.TestApp) {
				if app.TestMailer.TotalSend() != 0 {
					t.Fatal("no email may be sent")
				}
			}},
		{name: "owners invite owners", who: "alice", method: http.MethodPost, url: invBase, body: `{"email":"a@x.test","role":"owner"}`, status: 200},
		{name: "superuser", who: "super", method: http.MethodPost, url: invBase, body: `{"email":"a@x.test","role":"owner"}`, status: 200,
			after: func(t testing.TB, app *tests.TestApp) {
				if got := auditEntries(t, app, "targetCollection='_invitations' && action='create' && actorType='superuser'"); len(got) != 1 {
					t.Fatalf("expected a superuser audit entry, got %d", len(got))
				}
			}},
		{name: "invalid email", who: "bob", method: http.MethodPost, url: invBase, body: `{"email":"nope"}`, status: 400},
		{name: "missing email", who: "bob", method: http.MethodPost, url: invBase, body: `{}`, status: 400},
		{name: "invalid role", who: "bob", method: http.MethodPost, url: invBase, body: `{"email":"a@x.test","role":"god"}`, status: 400},
		{name: "already a member", who: "bob", method: http.MethodPost, url: invBase, body: `{"email":"DAVE@orgs.test"}`, status: 409},
		{name: "a suspended member counts as a member", who: "bob", method: http.MethodPost, url: invBase, body: `{"email":"sam@orgs.test"}`, status: 409},
		{name: "already pending", who: "bob", method: http.MethodPost, url: invBase, body: `{"email":"NewHire@orgs.test"}`, status: 409},
		{name: "an expired pending invitation does not block", who: "bob", method: http.MethodPost, url: invBase, body: `{"email":"late@orgs.test"}`, status: 200,
			after: func(t testing.TB, app *tests.TestApp) {
				if invStatus(t, app, invExpired) != "expired" {
					t.Fatal("the stale invitation must be marked expired")
				}
			}},
		{name: "archived org", who: "hank", method: http.MethodPost, url: "/api/orgs/" + orgA + "/invitations", body: `{"email":"a@x.test"}`, status: 409},
		{
			name: "the hourly limit per org", who: "bob", method: http.MethodPost, url: invBase, body: `{"email":"over@x.test"}`, status: 429,
			before: func(t testing.TB, app *tests.TestApp) {
				for i := 0; i < core.InvitationsPerHour; i++ {
					seedInvitation(t, app, "invfill0000000"+string(rune('a'+i)), orgP1, "f"+string(rune('a'+i))+"@x.test", "member",
						"fill-"+string(rune('a'+i)), "revoked", time.Now().Add(time.Hour), time.Hour)
				}
			},
		},
		{
			name: "the limit is per org", who: "ivan", method: http.MethodPost, url: "/api/orgs/" + orgP2 + "/invitations", body: `{"email":"p2@x.test"}`, status: 200,
			before: func(t testing.TB, app *tests.TestApp) {
				for i := 0; i < core.InvitationsPerHour; i++ {
					seedInvitation(t, app, "invfill0000000"+string(rune('a'+i)), orgP1, "f"+string(rune('a'+i))+"@x.test", "member",
						"fill-"+string(rune('a'+i)), "revoked", time.Now().Add(time.Hour), time.Hour)
				}
			},
		},
		{
			name: "the org can override the email", who: "bob", method: http.MethodPost, url: invBase, body: `{"email":"o@x.test"}`, status: 200,
			before: func(t testing.TB, app *tests.TestApp) {
				org, _ := app.FindRecordById(core.CollectionNameOrgs, orgP1)
				org.Set("settings", map[string]any{"invitationEmail": map[string]any{
					"subject": "Welcome to {ORG_NAME}",
					"body":    "<p>{INVITER} wants you as {ROLE}: <a href=\"{INVITE_URL}\">open</a> {ORG_NAME}</p>",
				}})
				orgMust(t, app.Save(org))
			},
			after: func(t testing.TB, app *tests.TestApp) {
				m := app.TestMailer.Messages()[0]
				if m.Subject != "Welcome to Parish One" || !invContains(m.HTML, "wants you as member", "open", "/orgs/invite/") {
					t.Fatalf("unexpected message %q %q", m.Subject, m.HTML)
				}
			},
		},
		{
			name: "names are escaped in the email", who: "bob", method: http.MethodPost, url: invBase, body: `{"email":"x@x.test"}`, status: 200,
			before: func(t testing.TB, app *tests.TestApp) {
				org, _ := app.FindRecordById(core.CollectionNameOrgs, orgP1)
				org.Set("name", `<script>alert(1)</script>`)
				orgMust(t, app.Save(org))
			},
			after: func(t testing.TB, app *tests.TestApp) {
				if strings.Contains(app.TestMailer.Messages()[0].HTML, "<script>alert(1)") {
					t.Fatal("the org name must be escaped")
				}
			},
		},
		{
			name: "meta.appURL builds the link", who: "bob", method: http.MethodPost, url: invBase, body: `{"email":"x@x.test"}`, status: 200,
			before: func(t testing.TB, app *tests.TestApp) {
				app.Settings().Meta.AppURL = "https://id.example.org/"
				orgMust(t, app.Save(app.Settings()))
			},
			after: func(t testing.TB, app *tests.TestApp) {
				if !strings.Contains(app.TestMailer.Messages()[0].HTML, `href="https://id.example.org/orgs/invite/`) {
					t.Fatalf("unexpected link in %q", app.TestMailer.Messages()[0].HTML)
				}
			},
		},
	})
}

func TestInvitationsList(t *testing.T) {
	t.Parallel()

	runInvCases(t, []orgCase{
		{name: "guest", url: invBase, status: 401},
		{name: "members cannot list", who: "carol", url: invBase, status: 403},
		{name: "outsiders", who: "frank", url: invBase, status: 404},
		{name: "admins see the invitations of their org only", who: "bob", url: invBase, status: 200,
			has:    []string{`newhire@orgs.test`, `"totalItems":6`, `"status":"revoked"`, `"status":"accepted"`},
			hasNot: []string{`other@orgs.test`, `tokenHash`, invTokenOK}},
		{name: "an invitation past its expiry reads as expired", who: "bob", url: invBase + "?status=expired", status: 200,
			has: []string{`late@orgs.test`, `"totalItems":1`, `"status":"expired"`}},
		{name: "status filter", who: "alice", url: invBase + "?status=pending", status: 200,
			has: []string{`"totalItems":3`, `newhire@orgs.test`}, hasNot: []string{`gone@orgs.test`}},
		{name: "paging", who: "alice", url: invBase + "?perPage=2&page=2", status: 200, has: []string{`"page":2`, `"perPage":2`, `"totalItems":6`}},
		{name: "superuser", who: "super", url: invBase, status: 200, has: []string{`"totalItems":6`}},
	})
}

func TestInvitationsResend(t *testing.T) {
	t.Parallel()

	runInvCases(t, []orgCase{
		{name: "guest", method: http.MethodPost, url: invBase + "/" + invOK + "/resend", status: 401},
		{name: "members cannot resend", who: "carol", method: http.MethodPost, url: invBase + "/" + invOK + "/resend", status: 403},
		{name: "an invitation of another org is a 404", who: "bob", method: http.MethodPost, url: invBase + "/" + invOther + "/resend", status: 404},
		{name: "unknown", who: "bob", method: http.MethodPost, url: invBase + "/nope/resend", status: 404},
		{
			name: "rotates the token and emails again", who: "bob", method: http.MethodPost, url: invBase + "/" + invOK + "/resend", status: 200,
			has: []string{`"emailSent":true`, `"sendCount":2`},
			before: func(t testing.TB, app *tests.TestApp) {
				inv, _ := app.FindRecordById(core.CollectionNameInvitations, invOK)
				inv.Set("lastSentAt", time.Now().Add(-2*time.Minute))
				orgMust(t, app.Save(inv))
			},
			after: func(t testing.TB, app *tests.TestApp) {
				tok := sentLinkToken(t, app, 0)
				if tok == invTokenOK {
					t.Fatal("the token must rotate")
				}

				if _, _, err := core.LookupInvitation(app, nil, invTokenOK); err == nil {
					t.Fatal("the old link must stop working")
				}

				if _, _, err := core.LookupInvitation(app, nil, tok); err != nil {
					t.Fatal(err)
				}
			},
		},
		{name: "one resend per minute", who: "bob", method: http.MethodPost, url: invBase + "/" + invOK + "/resend", status: 429,
			before: func(t testing.TB, app *tests.TestApp) {
				inv, _ := app.FindRecordById(core.CollectionNameInvitations, invOK)
				inv.Set("lastSentAt", time.Now().Add(-10*time.Second))
				orgMust(t, app.Save(inv))
			},
			after: func(t testing.TB, app *tests.TestApp) {
				if app.TestMailer.TotalSend() != 0 {
					t.Fatal("no email may be sent")
				}
			}},
		{name: "an admin cannot resend an owner invitation", who: "bob", method: http.MethodPost, url: invBase + "/" + invOK + "/resend", status: 403,
			before: func(t testing.TB, app *tests.TestApp) {
				inv, _ := app.FindRecordById(core.CollectionNameInvitations, invOK)
				inv.Set("role", "owner")
				inv.Set("lastSentAt", time.Now().Add(-time.Hour))
				orgMust(t, app.Save(inv))
			}},
		{name: "a revoked invitation cannot be resent", who: "bob", method: http.MethodPost, url: invBase + "/" + invRevoked + "/resend", status: 409,
			before: func(t testing.TB, app *tests.TestApp) {
				inv, _ := app.FindRecordById(core.CollectionNameInvitations, invRevoked)
				inv.Set("lastSentAt", time.Now().Add(-time.Hour))
				orgMust(t, app.Save(inv))
			}},
		{name: "an accepted invitation cannot be resent", who: "bob", method: http.MethodPost, url: invBase + "/" + invUsed + "/resend", status: 409,
			before: func(t testing.TB, app *tests.TestApp) {
				inv, _ := app.FindRecordById(core.CollectionNameInvitations, invUsed)
				inv.Set("lastSentAt", time.Now().Add(-time.Hour))
				orgMust(t, app.Save(inv))
			}},
		{name: "an expired invitation is renewed", who: "alice", method: http.MethodPost, url: invBase + "/" + invExpired + "/resend", status: 200,
			has: []string{`"status":"pending"`},
			after: func(t testing.TB, app *tests.TestApp) {
				if invStatus(t, app, invExpired) != "pending" {
					t.Fatal("expected pending")
				}
			}},
	})
}

func TestInvitationsRevoke(t *testing.T) {
	t.Parallel()

	runInvCases(t, []orgCase{
		{name: "guest", method: http.MethodDelete, url: invBase + "/" + invOK, status: 401},
		{name: "members cannot revoke", who: "carol", method: http.MethodDelete, url: invBase + "/" + invOK, status: 403},
		{name: "another org", who: "bob", method: http.MethodDelete, url: invBase + "/" + invOther, status: 404},
		{name: "an admin revokes", who: "bob", method: http.MethodDelete, url: invBase + "/" + invOK, status: 204,
			after: func(t testing.TB, app *tests.TestApp) {
				if invStatus(t, app, invOK) != "revoked" {
					t.Fatal("expected revoked")
				}

				if _, _, err := core.LookupInvitation(app, nil, invTokenOK); err == nil {
					t.Fatal("a revoked link must stop working")
				}

				if got := auditEntries(t, app, "targetCollection='_invitations' && action='update' && actorId='"+orgUserID("bob")+"'"); len(got) != 1 {
					t.Fatalf("expected one audit entry, got %d", len(got))
				} else if orgAuditMeta(t, got[0])["orgId"] != orgP1 || !strings.Contains(got[0].GetString("after"), `"status":"revoked"`) {
					t.Fatalf("unexpected audit entry %v", got[0].PublicExport())
				}
			}},
		{name: "an admin cannot revoke an owner invitation", who: "bob", method: http.MethodDelete, url: invBase + "/" + invOK, status: 403,
			before: func(t testing.TB, app *tests.TestApp) {
				inv, _ := app.FindRecordById(core.CollectionNameInvitations, invOK)
				inv.Set("role", "owner")
				orgMust(t, app.Save(inv))
			}},
		{name: "only pending invitations", who: "bob", method: http.MethodDelete, url: invBase + "/" + invUsed, status: 409},
		{name: "twice", who: "bob", method: http.MethodDelete, url: invBase + "/" + invRevoked, status: 409},
	})
}

func TestInvitationsAccept(t *testing.T) {
	t.Parallel()

	const url = "/api/invitations/accept"

	body := func(tok string) string { return `{"token":"` + tok + `"}` }

	runInvCases(t, []orgCase{
		{name: "guest", method: http.MethodPost, url: url, body: body(invTokenFrank), status: 401},
		{
			name: "the invitee accepts", who: "frank", method: http.MethodPost, url: url, body: body(invTokenFrank), status: 200,
			has: []string{`"role":"admin"`, `"slug":"parish-1"`},
			after: func(t testing.TB, app *tests.TestApp) {
				org, _ := core.FindOrg(app, orgP1)
				m, err := core.FindOrgMember(app, org.Id, orgUserID("frank"))
				orgMust(t, err)

				if m.GetString("role") != "admin" || m.GetString("source") != "invitation" || m.GetString("status") != "active" {
					t.Fatalf("unexpected membership %v", m.PublicExport())
				}

				inv, _ := app.FindRecordById(core.CollectionNameInvitations, invFrank)
				if inv.GetString("status") != "accepted" || inv.GetString("acceptedBy") != orgUserID("frank") {
					t.Fatalf("unexpected invitation %v", inv.PublicExport())
				}

				// both writes are audited with the invitee as actor and the org
				for _, f := range []string{"targetCollection='_orgMembers' && action='create'", "targetCollection='_invitations' && action='update'"} {
					got := auditEntries(t, app, f+" && actorId='"+orgUserID("frank")+"'")
					if len(got) != 1 || orgAuditMeta(t, got[0])["orgId"] != orgP1 {
						t.Fatalf("%s: expected one audit entry with the org, got %d", f, len(got))
					}
				}
			},
		},
		{name: "the email must match", who: "erin", method: http.MethodPost, url: url, body: body(invTokenFrank), status: 403,
			has: []string{`"code":"email_mismatch"`},
			after: func(t testing.TB, app *tests.TestApp) {
				if invStatus(t, app, invFrank) != "pending" {
					t.Fatal("a refusal must not consume the invitation")
				}
			}},
		{name: "a superuser cannot accept", who: "super", method: http.MethodPost, url: url, body: body(invTokenFrank), status: 403},
		{name: "an unverified account is never linked", who: "frank", method: http.MethodPost, url: url, body: body(invTokenFrank), status: 403,
			has: []string{`"code":"email_not_verified"`},
			before: func(t testing.TB, app *tests.TestApp) {
				u, _ := app.FindRecordById("users", orgUserID("frank"))
				u.SetVerified(false)
				orgMust(t, app.Save(u))
			},
			after: func(t testing.TB, app *tests.TestApp) {
				if _, err := core.FindOrgMember(app, orgP1, orgUserID("frank")); err == nil {
					t.Fatal("no membership may be created")
				}
			}},
		{name: "expired", who: "frank", method: http.MethodPost, url: url, body: body(invTokenExpired), status: 400, has: []string{core.InvitationInvalidMessage}},
		{name: "revoked", who: "frank", method: http.MethodPost, url: url, body: body(invTokenRevoked), status: 400, has: []string{core.InvitationInvalidMessage}},
		{name: "reused", who: "erin", method: http.MethodPost, url: url, body: body(invTokenUsed), status: 400, has: []string{core.InvitationInvalidMessage}},
		{name: "unknown token", who: "frank", method: http.MethodPost, url: url, body: body("nope"), status: 400, has: []string{core.InvitationInvalidMessage}},
		{name: "missing token", who: "frank", method: http.MethodPost, url: url, body: `{}`, status: 400},
		{name: "archived orgs", who: "frank", method: http.MethodPost, url: url, body: body(invTokenFrank), status: 400,
			before: func(t testing.TB, app *tests.TestApp) {
				org, _ := app.FindRecordById(core.CollectionNameOrgs, orgP1)
				org.Set("archived", true)
				orgMust(t, app.Save(org))
			}},
		{name: "an already-member invitee gets a conflict", who: "erin", method: http.MethodPost, url: url, body: body("tok-self-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), status: 409,
			before: func(t testing.TB, app *tests.TestApp) {
				seedInvitation(t, app, "invself00000001", orgP2, "erin@orgs.test", "viewer", "tok-self-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "pending", time.Now().Add(time.Hour), time.Hour)
			}},
	})
}

func TestInvitationsRegister(t *testing.T) {
	t.Parallel()

	const url = "/api/invitations/register"

	body := func(tok, pw string) string {
		return `{"token":"` + tok + `","password":"` + pw + `","passwordConfirm":"` + pw + `","name":"Nina Newbie"}`
	}

	fake := &fakeCaptcha{}

	runInvCases(t, []orgCase{
		{
			name: "creates a verified account, the membership and accepts", method: http.MethodPost, url: url, body: body(invTokenNewbie, "supersecret1"), status: 200,
			has: []string{`"role":"member"`, `"slug":"parish-1"`},
			after: func(t testing.TB, app *tests.TestApp) {
				u, err := app.FindAuthRecordByEmail("users", "newbie@orgs.test")
				orgMust(t, err)

				if !u.Verified() || u.GetString("name") != "Nina Newbie" || !u.ValidatePassword("supersecret1") {
					t.Fatalf("unexpected account %v", u.PublicExport())
				}

				m, err := core.FindOrgMember(app, orgP1, u.Id)
				orgMust(t, err)

				if m.GetString("source") != "invitation" || invStatus(t, app, invNewbie) != "accepted" {
					t.Fatal("expected an invitation membership and an accepted invitation")
				}

				// the new account is the actor of its own creation and of the acceptance
				if got := auditEntries(t, app, "targetCollection='_orgMembers' && action='create' && actorId='"+u.Id+"'"); len(got) != 1 {
					t.Fatalf("expected the new user as actor of the membership, got %d", len(got))
				}
			},
		},
		{name: "an existing account must sign in", method: http.MethodPost, url: url, body: body(invTokenFrank, "supersecret1"), status: 409,
			has: []string{`"code":"account_exists"`}},
		{name: "passwords must match", method: http.MethodPost, url: url, status: 400,
			body: `{"token":"` + invTokenNewbie + `","password":"supersecret1","passwordConfirm":"other-secret1"}`,
			after: func(t testing.TB, app *tests.TestApp) {
				if invStatus(t, app, invNewbie) != "pending" {
					t.Fatal("expected pending")
				}
			}},
		{name: "a weak password rolls everything back", method: http.MethodPost, url: url, body: body(invTokenNewbie, "short"), status: 400,
			after: func(t testing.TB, app *tests.TestApp) {
				if _, err := app.FindAuthRecordByEmail("users", "newbie@orgs.test"); err == nil {
					t.Fatal("no account may be created")
				}

				if invStatus(t, app, invNewbie) != "pending" {
					t.Fatal("expected pending")
				}
			}},
		{name: "expired", method: http.MethodPost, url: url, body: body(invTokenExpired, "supersecret1"), status: 400},
		{name: "revoked", method: http.MethodPost, url: url, body: body(invTokenRevoked, "supersecret1"), status: 400},
		{name: "reused", method: http.MethodPost, url: url, body: body(invTokenUsed, "supersecret1"), status: 400},
		{name: "unknown", method: http.MethodPost, url: url, body: body("nope", "supersecret1"), status: 400},
		{name: "signed-in users accept instead", who: "alice", method: http.MethodPost, url: url, body: body(invTokenNewbie, "supersecret1"), status: 400},
		{
			name: "the sign-up captcha applies when it is on", method: http.MethodPost, url: url, body: body(invTokenNewbie, "supersecret1"), status: 400,
			has: []string{`"captchaToken":{"code":"captcha_failed"`},
			before: func(t testing.TB, app *tests.TestApp) {
				app.Settings().Captcha = core.CaptchaConfig{Provider: core.CaptchaProviderTurnstile, SiteKey: "s", SecretKey: "k"}
				app.Settings().Captcha.Actions.SignUp = true
				apis.SetCaptchaVerifierForTest(app, fake)
			},
			after: func(t testing.TB, app *tests.TestApp) {
				if invStatus(t, app, invNewbie) != "pending" {
					t.Fatal("expected pending")
				}
			},
		},
	})

	runInvCases(t, []orgCase{{
		name: "a valid captcha token lets the sign-up through", method: http.MethodPost, url: url, status: 200,
		body: `{"captchaToken":"ok","token":"` + invTokenNewbie + `","password":"supersecret1","passwordConfirm":"supersecret1"}`,
		before: func(t testing.TB, app *tests.TestApp) {
			ok := &fakeCaptcha{}
			ok.res.Success = true
			app.Settings().Captcha = core.CaptchaConfig{Provider: core.CaptchaProviderTurnstile, SiteKey: "s", SecretKey: "k"}
			app.Settings().Captcha.Actions.SignUp = true
			apis.SetCaptchaVerifierForTest(app, ok)
		},
		after: func(t testing.TB, app *tests.TestApp) {
			if invStatus(t, app, invNewbie) != "accepted" {
				t.Fatal("expected accepted")
			}
		},
	}})
}
