package apis

import (
	"embed"
	"html/template"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #79b): the hosted organization pages.
//
//	GET /orgs/invite/{token}      accept an invitation: sign in or sign up, then accept
//	GET /orgs/{slug}              members, invitations and tags of an org, for its members
//	GET /orgs/assets/{file}       the page script and style
//
// Like the OpenID Connect pages, they carry no inline script or style, so the
// Content-Security-Policy is strict: only same-origin files (and, when a
// captcha is required, the provider's domains and nothing else). Everything
// they do goes through the JSON API (/api/orgs, /api/invitations and the
// ordinary auth endpoints of the member collection).
//
// The /orgs/{slug} page is a shell: it does not look the org up, so a stranger
// cannot tell which slugs exist. The org is fetched with the member's token.

//go:embed orgs_pages/*
var orgsPagesFS embed.FS

const orgsPagesMount = "/orgs"

var orgSlugPageRegex = regexp.MustCompile(`^[a-z0-9-]{2,64}$`)

// hostedPagesCSP is the policy of the hosted pages. When any captcha action is
// required it opens the provider's domains, as /passkeys does.
func hostedPagesCSP(cfg core.CaptchaConfig) string {
	csp := "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

	if len(cfg.Required()) == 0 {
		return csp
	}

	src := captchaSourcesFor(cfg.Provider)
	if src.script == "" {
		return csp
	}

	csp = strings.Replace(csp, "script-src 'self'", "script-src 'self'"+src.script, 1)
	csp = strings.Replace(csp, "style-src 'self'", "style-src 'self'"+src.style, 1)
	csp = strings.Replace(csp, "connect-src 'self'", "connect-src 'self'"+src.connect, 1)

	return strings.Replace(csp, "frame-ancestors 'none'", "frame-src"+src.frame+"; frame-ancestors 'none'", 1)
}

const orgsPageHead = `{{define "head"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="referrer" content="no-referrer">
<title>{{.Title}}</title>
<link rel="icon" href="data:,">
<link rel="stylesheet" href="/orgs/assets/hosted.css">
</head>
<body>{{end}}
{{define "signin"}}<section id="signin" hidden>
<h2>Sign in</h2>
<form id="pw" hidden>
<label>Email or username<input name="identity" autocomplete="username webauthn" required></label>
<label>Password<input name="password" type="password" autocomplete="current-password" required></label>
<button type="submit">Sign in</button>
</form>
<form id="totp" hidden>
<label>Authenticator code or backup code<input name="code" inputmode="numeric" autocomplete="one-time-code" required></label>
<button type="submit">Verify</button>
</form>
<div id="otpBox" hidden>
<div class="or"><span>or</span></div>
<button id="otpSend" type="button" class="secondary">Email me a sign-in code</button>
<form id="otpForm" hidden>
<label>Code from the email<input name="code" inputmode="numeric" autocomplete="one-time-code" required></label>
<button type="submit">Sign in</button>
</form>
</div>
<div id="passkeyBox" hidden>
<div class="or"><span>or</span></div>
<button id="passkey" type="button" class="secondary">Sign in with a passkey</button>
</div>
<p id="status" class="msg err" role="alert" hidden></p>
{{if .ShowSignUp}}<p class="muted">No account yet? <button id="toSignUp" type="button" class="linklike">Create one</button></p>{{end}}
</section>{{end}}
`

var orgsPageTemplates = template.Must(template.New("pages").Parse(orgsPageHead + `
{{define "error"}}{{template "head" .}}
<main class="card" id="app">
<h1>{{.Heading}}</h1>
<p class="msg err" role="alert">{{.Message}}</p>
</main>
</body></html>{{end}}

{{define "invite"}}{{template "head" .}}
<main class="card" id="app" data-page="invite" data-token="{{.Token}}" data-collection="{{.Collection}}" data-email="{{.Email}}" data-has-account="{{.HasAccount}}">
<h1>Join {{.OrgName}}</h1>
<p class="sub">You are invited as <strong>{{.Role}}</strong> with <strong>{{.Email}}</strong>.</p>
<p id="istatus" class="msg err" role="alert" hidden></p>
<div id="captchaBox" hidden></div>

<section id="signedIn" hidden>
<p>Signed in as <strong id="who"></strong>.</p>
<button id="accept" type="button">Accept the invitation</button>
<button id="verifyBtn" type="button" class="secondary" hidden>Send me a verification email</button>
<p><button id="signout" type="button" class="linklike">Use another account</button></p>
</section>

{{template "signin" .}}

<section id="signup" hidden>
<form id="signupForm">
<label>Email<input name="email" type="email" value="{{.Email}}" readonly></label>
<label>Name<input name="name" autocomplete="name"></label>
<label>Password<input name="password" type="password" autocomplete="new-password" minlength="8" required></label>
<label>Repeat the password<input name="passwordConfirm" type="password" autocomplete="new-password" minlength="8" required></label>
<button type="submit">Create the account and join</button>
</form>
<p class="muted">Already have an account? <button id="toSignIn" type="button" class="linklike">Sign in</button></p>
</section>

<section id="done" hidden>
<p class="msg ok" id="doneText" role="status"></p>
<p><a id="doneLink" href="/orgs/">Open the organization page</a></p>
</section>
</main>
<script type="module" src="/orgs/assets/invite.js"></script>
</body></html>{{end}}

{{define "org"}}{{template "head" .}}
<main class="card wide" id="app" data-page="org" data-slug="{{.Slug}}" data-collection="{{.Collection}}">
<p id="pstatus" class="msg err" role="alert" hidden></p>
<div id="captchaBox" hidden></div>

<div id="denied" hidden>
<h1>No access</h1>
<p class="muted">This account is not an active member of that organization.</p>
<p><button id="deniedOut" type="button" class="secondary">Sign out</button></p>
</div>

{{template "signin" .}}

<div id="main" hidden>
<header class="bar">
<div><h1 id="orgName"></h1><p class="muted" id="myRole"></p></div>
<button id="signout" type="button" class="secondary">Sign out</button>
</header>

<h2>Members</h2>
<div class="tablewrap"><table id="members">
<thead><tr><th>Member</th><th>Role</th><th>Status</th><th>Tags</th><th></th></tr></thead>
<tbody></tbody>
</table></div>

<div data-manage hidden>
<h2>Invite someone</h2>
<form id="inviteForm" class="inline">
<label>Email<input name="email" type="email" autocomplete="off" required></label>
<label>Role<select id="inviteRole" name="role"><option>member</option></select></label>
<label>Tags (comma separated)<input name="tags" autocomplete="off"></label>
<button type="submit">Send invitation</button>
</form>

<h2>Invitations</h2>
<div class="tablewrap"><table id="invitations">
<thead><tr><th>Email</th><th>Role</th><th>Tags</th><th>Status</th><th>Expires / accepted</th><th>Sent</th><th></th></tr></thead>
<tbody></tbody>
</table></div>
</div>

<h2>Tags</h2>
<p id="taglist"></p>
<form id="tagForm" class="inline" data-manage hidden>
<label>New tag<input name="name" autocomplete="off" maxlength="64" required></label>
<button type="submit">Add tag</button>
</form>
</div>
</main>
<script type="module" src="/orgs/assets/admin.js"></script>
</body></html>{{end}}

`))

type orgsPageData struct {
	Title, Heading, Message  string
	Token, Collection, Email string
	OrgName, Role, Slug      string
	HasAccount, ShowSignUp   bool
}

func orgsPageHeaders(e *core.RequestEvent) {
	h := e.Response.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", hostedPagesCSP(e.App.Settings().Captcha))
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
}

func renderOrgsPage(e *core.RequestEvent, status int, name string, d orgsPageData) error {
	orgsPageHeaders(e)
	e.Response.WriteHeader(status)

	return orgsPageTemplates.ExecuteTemplate(e.Response, name, d)
}

func orgsErrorPage(e *core.RequestEvent, status int, message string) error {
	return renderOrgsPage(e, status, "error", orgsPageData{Title: "Invitation", Heading: "Invitation", Message: message})
}

// invitationLimiter is shared by the pages and the API routes.
func invitationLimiter(app core.App) *FixedWindowLimiter {
	const key = "__clvInvitationLimiter__"

	if l, ok := app.Store().Get(key).(*FixedWindowLimiter); ok && l != nil {
		return l
	}

	l := newInvitationTokenLimiter(app)
	app.Store().Set(key, l)

	return l
}

func bindOrgsPages(app core.App) {
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id:       "__clvOrgsPages__",
		Priority: 9998,
		Func: func(se *core.ServeEvent) error {
			se.Router.GET(orgsPagesMount+"/invite/{token}", orgsInvitePage)
			se.Router.GET(orgsPagesMount+"/assets/{file}", orgsAsset)
			se.Router.GET(orgsPagesMount+"/{slug}", orgsAdminPage)

			return se.Next()
		},
	})
}

func orgsInvitePage(e *core.RequestEvent) error {
	if err := invitationLimited(e, invitationLimiter(e.App)); err != nil {
		return orgsErrorPage(e, http.StatusTooManyRequests, "Too many attempts. Try again later.")
	}

	inv, org, err := core.LookupInvitation(e.App, e.Request.Context(), e.Request.PathValue("token"))
	if err != nil {
		return orgsErrorPage(e, http.StatusNotFound, core.InvitationInvalidMessage+" Ask for a new invitation.")
	}

	col, err := core.OrgUserCollection(e.App)
	if err != nil {
		return orgsErrorPage(e, http.StatusNotFound, "Invitations are not available.")
	}

	_, accountErr := e.App.FindAuthRecordByEmail(col, inv.GetString("email"))

	return renderOrgsPage(e, http.StatusOK, "invite", orgsPageData{
		Title: "Join " + org.GetString("name"), OrgName: org.GetString("name"), Role: inv.GetString("role"),
		Email: inv.GetString("email"), Token: e.Request.PathValue("token"), Collection: col.Name,
		HasAccount: accountErr == nil, ShowSignUp: accountErr != nil,
	})
}

func orgsAdminPage(e *core.RequestEvent) error {
	slug := e.Request.PathValue("slug")
	if !orgSlugPageRegex.MatchString(slug) {
		return orgsErrorPage(e, http.StatusNotFound, "Page not found.")
	}

	col, err := core.OrgUserCollection(e.App)
	if err != nil {
		return orgsErrorPage(e, http.StatusNotFound, "Organizations are not available.")
	}

	return renderOrgsPage(e, http.StatusOK, "org", orgsPageData{Title: "Organization", Slug: slug, Collection: col.Name})
}

func orgsAsset(e *core.RequestEvent) error {
	name := path.Base(e.Request.PathValue("file"))

	var contentType string

	switch {
	case strings.HasSuffix(name, ".js"):
		contentType = "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		contentType = "text/css; charset=utf-8"
	default:
		return e.NotFoundError("", nil)
	}

	raw, err := orgsPagesFS.ReadFile("orgs_pages/" + name)
	if err != nil {
		return e.NotFoundError("", nil)
	}

	h := e.Response.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("X-Content-Type-Options", "nosniff")

	_, err = e.Response.Write(raw)

	return err
}
