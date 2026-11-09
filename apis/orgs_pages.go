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
<html lang="{{.Text.Lang}}"{{.Text.Attrs}}>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="referrer" content="no-referrer">
<title>{{.Title}}</title>
<link rel="icon" href="data:,">
<link rel="stylesheet" href="/orgs/assets/hosted.css">
{{.Text.DataBlock}}</head>
<body>{{end}}
{{define "signin"}}<section id="signin" hidden>
<h2>{{.Text.T "js.signIn"}}</h2>
<form id="pw" hidden>
<label>{{.Text.T "js.identityLabel"}}<input name="identity" autocomplete="username webauthn" required></label>
<label>{{.Text.T "js.passwordLabel"}}<input name="password" type="password" autocomplete="current-password" required></label>
<button type="submit">{{.Text.T "js.signIn"}}</button>
</form>
<form id="totp" hidden>
<label>{{.Text.T "js.totpLabel"}}<input name="code" inputmode="numeric" autocomplete="one-time-code" required></label>
<button type="submit">{{.Text.T "js.verify"}}</button>
</form>
<div id="otpBox" hidden>
<div class="or"><span>{{.Text.T "js.or"}}</span></div>
<button id="otpSend" type="button" class="secondary">{{.Text.T "js.emailMeCode"}}</button>
<form id="otpForm" hidden>
<label>{{.Text.T "js.codeFromEmail"}}<input name="code" inputmode="numeric" autocomplete="one-time-code" required></label>
<button type="submit">{{.Text.T "js.signIn"}}</button>
</form>
</div>
<div id="passkeyBox" hidden>
<div class="or"><span>{{.Text.T "js.or"}}</span></div>
<button id="passkey" type="button" class="secondary">{{.Text.T "js.passkeySignIn"}}</button>
</div>
<p id="status" class="msg err" role="alert" hidden></p>
{{if .ShowSignUp}}<p class="muted">{{.Text.HTML "orgs.signin.noAccountHtml" (.Text.Args "link" .ToSignUpLink)}}</p>{{end}}
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
<h1>{{.Text.T "orgs.invite.title" (.Text.Args "org" .OrgName)}}</h1>
<p class="sub">{{.Text.HTML "orgs.invite.introHtml" (.Text.Args "role" .RoleLabel "email" .Email)}}</p>
<p id="istatus" class="msg err" role="alert" hidden></p>
<div id="captchaBox" hidden></div>

<section id="signedIn" hidden>
<p>{{.Text.HTML "orgs.invite.signedInAsHtml" (.Text.Args "who" .WhoElement)}}</p>
<button id="accept" type="button">{{.Text.T "orgs.invite.accept"}}</button>
<button id="verifyBtn" type="button" class="secondary" hidden>{{.Text.T "orgs.invite.sendVerification"}}</button>
<p><button id="signout" type="button" class="linklike">{{.Text.T "orgs.invite.useAnother"}}</button></p>
</section>

{{template "signin" .}}

<section id="signup" hidden>
<form id="signupForm">
<label>{{.Text.T "orgs.email"}}<input name="email" type="email" value="{{.Email}}" readonly></label>
<label>{{.Text.T "orgs.invite.name"}}<input name="name" autocomplete="name"></label>
<label>{{.Text.T "js.passwordLabel"}}<input name="password" type="password" autocomplete="new-password" minlength="8" required></label>
<label>{{.Text.T "orgs.invite.repeatPassword"}}<input name="passwordConfirm" type="password" autocomplete="new-password" minlength="8" required></label>
<button type="submit">{{.Text.T "orgs.invite.createAndJoin"}}</button>
</form>
<p class="muted">{{.Text.HTML "orgs.invite.haveAccountHtml" (.Text.Args "link" .ToSignInLink)}}</p>
</section>

<section id="done" hidden>
<p class="msg ok" id="doneText" role="status"></p>
<p><a id="doneLink" href="/orgs/">{{.Text.T "orgs.invite.openOrg"}}</a></p>
</section>
</main>
<script type="module" src="/orgs/assets/invite.js"></script>
</body></html>{{end}}

{{define "org"}}{{template "head" .}}
<main class="card wide" id="app" data-page="org" data-slug="{{.Slug}}" data-collection="{{.Collection}}">
<p id="pstatus" class="msg err" role="alert" hidden></p>
<div id="captchaBox" hidden></div>

<div id="denied" hidden>
<h1>{{.Text.T "orgs.org.noAccess"}}</h1>
<p class="muted">{{.Text.T "orgs.org.notMember"}}</p>
<p><button id="deniedOut" type="button" class="secondary">{{.Text.T "orgs.signOut"}}</button></p>
</div>

{{template "signin" .}}

<div id="main" hidden>
<header class="bar">
<div><h1 id="orgName"></h1><p class="muted" id="myRole"></p></div>
<button id="signout" type="button" class="secondary">{{.Text.T "orgs.signOut"}}</button>
</header>

<h2>{{.Text.T "orgs.members"}}</h2>
<div class="tablewrap"><table id="members">
<thead><tr><th>{{.Text.T "orgs.col.member"}}</th><th>{{.Text.T "orgs.col.role"}}</th><th>{{.Text.T "orgs.col.status"}}</th><th>{{.Text.T "orgs.col.tags"}}</th><th></th></tr></thead>
<tbody></tbody>
</table></div>

<div data-manage hidden>
<h2>{{.Text.T "orgs.inviteSomeone"}}</h2>
<form id="inviteForm" class="inline">
<label>{{.Text.T "orgs.email"}}<input name="email" type="email" autocomplete="off" required></label>
<label>{{.Text.T "orgs.col.role"}}<select id="inviteRole" name="role"><option>member</option></select></label>
<label>{{.Text.T "orgs.tagsCommaSeparated"}}<input name="tags" autocomplete="off"></label>
<button type="submit">{{.Text.T "orgs.sendInvitation"}}</button>
</form>

<h2>{{.Text.T "orgs.invitations"}}</h2>
<div class="tablewrap"><table id="invitations">
<thead><tr><th>{{.Text.T "orgs.email"}}</th><th>{{.Text.T "orgs.col.role"}}</th><th>{{.Text.T "orgs.col.tags"}}</th><th>{{.Text.T "orgs.col.status"}}</th><th>{{.Text.T "orgs.col.expiresAccepted"}}</th><th>{{.Text.T "orgs.col.sent"}}</th><th></th></tr></thead>
<tbody></tbody>
</table></div>

<h2>{{.Text.T "orgs.scim.title"}}</h2>
<p class="muted">{{.Text.HTML "orgs.scim.tenantUrlHtml" (.Text.Args "url" .ScimURLElement)}}</p>
<form id="scimTokenForm" class="inline">
<label>{{.Text.T "orgs.scim.tokenName"}}<input name="name" autocomplete="off" maxlength="100" required></label>
<button type="submit">{{.Text.T "orgs.scim.createToken"}}</button>
</form>
<p id="scimNew" class="msg ok" role="status" hidden></p>
<div class="tablewrap"><table id="scimTokens">
<thead><tr><th>{{.Text.T "orgs.col.name"}}</th><th>{{.Text.T "orgs.col.token"}}</th><th>{{.Text.T "orgs.col.created"}}</th><th>{{.Text.T "orgs.col.lastUsed"}}</th><th>{{.Text.T "orgs.col.expires"}}</th><th>{{.Text.T "orgs.col.status"}}</th><th></th></tr></thead>
<tbody></tbody>
</table></div>
<form id="scimSettings">
<label>{{.Text.T "orgs.scim.defaultRole"}}<select id="scimDefaultRole" name="defaultRole"><option>member</option></select></label>
<label>{{.Text.HTML "orgs.scim.groupsLabelHtml"}}<textarea name="groupRoles" rows="3" autocomplete="off"></textarea></label>
<label class="check"><input name="deactivationBans" type="checkbox"> {{.Text.T "orgs.scim.deactivationBans"}}</label>
<button type="submit">{{.Text.T "orgs.scim.save"}}</button>
</form>
</div>

<h2>{{.Text.T "orgs.col.tags"}}</h2>
<p id="taglist"></p>
<form id="tagForm" class="inline" data-manage hidden>
<label>{{.Text.T "orgs.newTag"}}<input name="name" autocomplete="off" maxlength="64" required></label>
<button type="submit">{{.Text.T "orgs.addTag"}}</button>
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

	// fork-local (#97): the language of the page and the pieces of markup
	// that sentences wrap around (the catalog decides where they go)
	Text                                                   *PageText
	RoleLabel                                              string
	WhoElement, ScimURLElement, ToSignUpLink, ToSignInLink template.HTML
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

	if d.Text == nil {
		d.Text = NewPageText(e, "")
	}

	d.RoleLabel = d.Text.RoleLabel(d.Role)
	d.WhoElement = `<strong id="who"></strong>`
	d.ScimURLElement = `<code id="scimUrl"></code>`
	d.ToSignUpLink = template.HTML(`<button id="toSignUp" type="button" class="linklike">` + //nolint:gosec // the label is escaped
		template.HTMLEscapeString(d.Text.T("orgs.signin.createOne")) + `</button>`)
	d.ToSignInLink = template.HTML(`<button id="toSignIn" type="button" class="linklike">` + //nolint:gosec // the label is escaped
		template.HTMLEscapeString(d.Text.T("js.signIn")) + `</button>`)

	e.Response.WriteHeader(status)

	return orgsPageTemplates.ExecuteTemplate(e.Response, name, d)
}

// orgsErrorPage renders a refusal; messageKey is a catalog key.
func orgsErrorPage(e *core.RequestEvent, status int, messageKey string) error {
	text := NewPageText(e, "")
	title := text.T("orgs.invite.errorTitle")

	return renderOrgsPage(e, status, "error", orgsPageData{Title: title, Heading: title, Message: text.T(messageKey), Text: text})
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
		return orgsErrorPage(e, http.StatusTooManyRequests, "js.tooMany")
	}

	inv, org, err := core.LookupInvitation(e.App, e.Request.Context(), e.Request.PathValue("token"))
	if err != nil {
		return orgsErrorPage(e, http.StatusNotFound, "orgs.err.invitationInvalid")
	}

	col, err := core.OrgUserCollection(e.App)
	if err != nil {
		return orgsErrorPage(e, http.StatusNotFound, "orgs.err.invitationsUnavailable")
	}

	account, accountErr := e.App.FindAuthRecordByEmail(col, inv.GetString("email"))

	// the invitee's own preference (when the account has one) comes before Accept-Language
	var recordLang string
	if accountErr == nil {
		recordLang = core.RecordLanguage(e.App, account)
	}

	text := NewPageText(e, recordLang)

	return renderOrgsPage(e, http.StatusOK, "invite", orgsPageData{
		Text:  text,
		Title: text.T("orgs.invite.title", text.Args("org", org.GetString("name"))), OrgName: org.GetString("name"), Role: inv.GetString("role"),
		Email: inv.GetString("email"), Token: e.Request.PathValue("token"), Collection: col.Name,
		HasAccount: accountErr == nil, ShowSignUp: accountErr != nil,
	})
}

func orgsAdminPage(e *core.RequestEvent) error {
	slug := e.Request.PathValue("slug")
	if !orgSlugPageRegex.MatchString(slug) {
		return orgsErrorPage(e, http.StatusNotFound, "orgs.err.notFound")
	}

	col, err := core.OrgUserCollection(e.App)
	if err != nil {
		return orgsErrorPage(e, http.StatusNotFound, "orgs.err.orgsUnavailable")
	}

	text := NewPageText(e, "")

	return renderOrgsPage(e, http.StatusOK, "org", orgsPageData{Text: text, Title: text.T("orgs.org.title"), Slug: slug, Collection: col.Name})
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
