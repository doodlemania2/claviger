package oidc

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
	"github.com/zitadel/oidc/v3/pkg/oidc"
)

// The hosted pages follow the discipline of apis/passkeys_page.go with one
// improvement: no inline script or style at all, so the CSP can be strict.

const pagesCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// maxSignInAge is how old the Claviger session may be when it is presented to
// finish an authorize request: it has to be the sign-in that just happened.
const maxSignInAge = 15 * time.Minute

// loginCSP is the policy of the sign-in page. Like /passkeys, it opens the
// domains of the captcha provider (and only those) when a captcha is required
// for password or passkey sign-in; otherwise it is exactly pagesCSP.
func loginCSP(cfg core.CaptchaConfig) string {
	if !cfg.IsRequired(core.CaptchaActionLogin) && !cfg.IsRequired(core.CaptchaActionPasskeyLogin) {
		return pagesCSP
	}

	script, frame, connect, style := apis.CaptchaWidgetSources(cfg.Provider)
	if script == "" {
		return pagesCSP
	}

	csp := pagesCSP
	csp = strings.Replace(csp, "script-src 'self'", "script-src 'self'"+script, 1)
	csp = strings.Replace(csp, "style-src 'self'", "style-src 'self'"+style, 1)
	csp = strings.Replace(csp, "connect-src 'self'", "connect-src 'self'"+connect, 1)

	return strings.Replace(csp, "frame-ancestors 'none'", "frame-src"+frame+"; frame-ancestors 'none'", 1)
}

func (m *module) pageHeaders(e *core.RequestEvent, page string) {
	h := e.Response.Header()

	csp := pagesCSP
	if page == "login" {
		csp = loginCSP(m.app.Settings().Captcha)
	}

	h.Set("Content-Security-Policy", csp)
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="{{.Text.Lang}}"{{.Text.Attrs}}>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>{{.Title}}</title>
<link rel="icon" href="data:,">
<link rel="stylesheet" href="/oidc/assets/oidc.css">
{{if .Script}}{{.Text.DataBlock}}{{end}}</head>
<body>
<main class="card" id="app" data-page="{{.Page}}" data-request="{{.RequestID}}" data-collection="{{.Collection}}"{{if .Finish}} data-finish="{{.Finish}}"{{end}}{{if .LoginHint}} data-login-hint="{{.LoginHint}}"{{end}}>
<h1>{{.Heading}}</h1>
{{if .Message}}<p class="msg {{.MessageClass}}" role="alert">{{.Message}}</p>{{end}}
{{if eq .Page "login"}}
<p class="sub">{{.Text.HTML "oidc.login.continueToHtml" (.Text.Args "client" .ClientName)}}</p>
<form id="pw" hidden>
<label>{{.Text.T "js.identityLabel"}}<input name="identity" autocomplete="username webauthn" required></label>
<label>{{.Text.T "js.passwordLabel"}}<input name="password" type="password" autocomplete="current-password" required></label>
<button type="submit">{{.Text.T "js.signIn"}}</button>
</form>
<form id="totp" hidden>
<label>{{.Text.T "js.totpLabel"}}<input name="code" inputmode="numeric" autocomplete="one-time-code" required></label>
<button type="submit">{{.Text.T "js.verify"}}</button>
</form>
<div id="captchaBox" hidden></div>
<div id="passkeyBox" hidden>
<div class="or"><span>{{.Text.T "js.or"}}</span></div>
<button id="passkey" type="button" class="secondary">{{.Text.T "js.passkeySignIn"}}</button>
</div>
<p id="status" class="msg err" role="alert" hidden></p>
{{end}}
{{if eq .Page "consent"}}
<p class="sub">{{.Text.HTML "oidc.consent.wantsHtml" (.Text.Args "client" .ClientName "account" .AccountHTML)}}</p>
<ul class="scopes">{{range .Scopes}}<li><strong>{{.Label}}</strong><span>{{.Detail}}</span></li>{{end}}</ul>
<div class="row">
<button id="allow" type="button">{{.Text.T "oidc.consent.allow"}}</button>
<button id="deny" type="button" class="secondary">{{.Text.T "oidc.consent.cancel"}}</button>
</div>
<p id="status" class="msg err" role="alert" hidden></p>
{{end}}
</main>
{{if .Script}}<script type="module" src="{{.Script}}"></script>{{end}}
</body>
</html>`))

type scopeInfo struct{ Label, Detail string }

type pageData struct {
	Title, Heading, Page, RequestID, Collection, ClientName, Account string
	Message, MessageClass, Script                                    string
	Finish, LoginHint                                                string // sign-in page: where the token goes (default /oidc/login/finish), prefilled identity
	Scopes                                                           []scopeInfo

	// fork-local (#97): the language of the page; render fills it in when the caller did not
	Text        *apis.PageText
	AccountHTML template.HTML
}

func (m *module) render(e *core.RequestEvent, status int, d pageData) error {
	m.pageHeaders(e, d.Page)

	if d.Text == nil {
		d.Text = apis.NewPageText(e, "")
	}

	if d.Account != "" {
		d.AccountHTML = template.HTML(` <span class="muted">(` + template.HTMLEscapeString(d.Account) + `)</span>`) //nolint:gosec // escaped
	}

	e.Response.Header().Set("Content-Type", "text/html; charset=utf-8")
	e.Response.WriteHeader(status)

	return pageTemplate.Execute(e.Response, d)
}

// errorPage renders a refusal; message is a catalog key (text that is not a
// key is shown as it is).
func (m *module) errorPage(e *core.RequestEvent, status int, message string) error {
	text := apis.NewPageText(e, "")
	title := text.T("oidc.problem")

	return m.render(e, status, pageData{Title: title, Heading: title, Page: "error", Message: text.T(message), MessageClass: "err", Text: text})
}

func (m *module) bindPages(se *core.ServeEvent) {
	se.Router.GET(mountPath+"/login", m.loginPage)
	se.Router.POST(mountPath+"/login/finish", m.loginFinish)
	se.Router.GET(mountPath+"/consent", m.consentPage)
	se.Router.POST(mountPath+"/consent", m.consentDecision)

	se.Router.GET(mountPath+"/logged-out", func(e *core.RequestEvent) error {
		text := apis.NewPageText(e, "")

		return m.render(e, http.StatusOK, pageData{
			Title: text.T("oidc.signedOut.title"), Heading: text.T("oidc.signedOut.heading"), Page: "info",
			Message: text.T("oidc.signedOut.message"), MessageClass: "ok", Text: text,
		})
	})

	asset := func(contentType, body string) func(e *core.RequestEvent) error {
		return func(e *core.RequestEvent) error {
			h := e.Response.Header()
			h.Set("Content-Type", contentType)
			h.Set("Cache-Control", "public, max-age=300")
			h.Set("X-Content-Type-Options", "nosniff")
			_, err := e.Response.Write([]byte(body))

			return err
		}
	}

	se.Router.GET(mountPath+"/assets/oidc.css", asset("text/css; charset=utf-8", oidcCSS))
	se.Router.GET(mountPath+"/assets/login.js", asset("application/javascript; charset=utf-8", loginJS))
	se.Router.GET(mountPath+"/assets/consent.js", asset("application/javascript; charset=utf-8", consentJS))
	se.Router.GET(mountPath+"/assets/chooser.css", asset("text/css; charset=utf-8", chooserCSS))
	se.Router.GET(mountPath+"/assets/chooser.js", asset("application/javascript; charset=utf-8", chooserJS))
	se.Router.GET(mountPath+"/assets/device.js", asset("application/javascript; charset=utf-8", deviceJS))

	m.bindDeviceRoutes(se)
}

// ---- browser binding -----------------------------------------------------
//
// The authorize request id travels in a URL, so it is a bearer secret. A
// per-request cookie, set by the first load of the sign-in page and required
// by every later step, makes sure the browser that finishes the sign-in is
// the one that started it (login CSRF / request fixation).

func bindingCookieName(requestID string) string {
	id := requestID
	if len(id) > 16 {
		id = id[:16]
	}

	return "claviger_oidc_" + id
}

// checkBinding reports whether the request carries the binding cookie of the
// authorize request.
func (m *module) checkBinding(e *core.RequestEvent, req *authRequest) bool {
	if req == nil || req.row.BindingHash == "" {
		return false
	}

	c, err := e.Request.Cookie(bindingCookieName(req.row.RequestId))
	if err != nil || c.Value == "" {
		return false
	}

	return hashEqual(hashToken(c.Value), req.row.BindingHash)
}

// bind attaches the browser to the request on its first visit.
func (m *module) bind(e *core.RequestEvent, req *authRequest) bool {
	if req.row.BindingHash != "" {
		return m.checkBinding(e, req)
	}

	secret := randomToken(24)

	n, err := exec(m.app,
		"UPDATE {{"+core.CollectionNameOAuthRequests+"}} SET [[bindingHash]] = {:hash} WHERE [[requestId]] = {:id} AND [[bindingHash]] = ''",
		dbx.Params{"hash": hashToken(secret), "id": req.row.RequestId})
	if err != nil || n != 1 {
		return false
	}

	http.SetCookie(e.Response, &http.Cookie{
		Name:     bindingCookieName(req.row.RequestId),
		Value:    secret,
		Path:     mountPath,
		MaxAge:   int(authRequestLifetime.Seconds()),
		HttpOnly: true,
		Secure:   m.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})

	return true
}

// ---- sign-in -------------------------------------------------------------

func (m *module) loginPage(e *core.RequestEvent) error {
	id := e.Request.URL.Query().Get("authRequestID")

	req, err := m.store.requestByID(id)
	if err != nil || req.row.Status == statusDone || req.row.Status == statusDenied {
		return m.errorPage(e, http.StatusBadRequest, "oidc.err.linkExpired")
	}

	if !m.bind(e, req) {
		return m.errorPage(e, http.StatusForbidden, "oidc.err.otherBrowser")
	}

	client, err := m.store.findClient(req.row.ClientId)
	if err != nil {
		return m.errorPage(e, http.StatusBadRequest, "oidc.err.appUnavailable")
	}

	col, err := client.collection()
	if err != nil {
		return m.errorPage(e, http.StatusBadRequest, "oidc.err.appUnavailable")
	}

	if req.row.Status == statusPending {
		entries := m.store.ssoEntries(ssoCookieValue(e.Request), client, req.d.MaxAge)

		if slices.Contains(req.d.Prompt, oidc.PromptNone) {
			// no page may be shown: one account carries the request or the client gets an error
			entry, code := decideNone(entries, req.d.LoginHint)
			if entry == nil {
				return m.denyRequest(e, req, code, "The user is not signed in, or more than one account is.")
			}

			target, aerr := m.continueWith(req, client, col, entry)
			if aerr == nil {
				return e.Redirect(http.StatusFound, target)
			}

			if errors.Is(aerr, errNeedsConsent) {
				return m.denyRequest(e, req, "consent_required", "The user has not consented to this client yet.")
			}

			return m.denyRequest(e, req, "login_required", "The user is not signed in.")
		}

		// the browser's accounts carry the request without credentials, or the
		// user picks one
		dec := decideSSO(entries, decideParams{
			prompt: req.d.Prompt, hint: req.d.LoginHint, forceLogin: e.Request.URL.Query().Get("account") == "new",
		})

		switch dec.action {
		case actContinue:
			if target, aerr := m.continueWith(req, client, col, dec.entry); aerr == nil {
				return e.Redirect(http.StatusFound, target)
			}
		case actChoose:
			return m.renderChooser(e, chooserView{
				flow: flowAuthorize, ref: id, client: client, entries: entries, preselect: dec.preselect,
				other: mountPath + "/login?authRequestID=" + url.QueryEscape(id) + "&account=new",
			})
		}
	}

	text := apis.NewPageText(e, "")

	return m.render(e, http.StatusOK, pageData{
		Title: text.T("js.signIn"), Heading: text.T("js.signIn"), Page: "login", RequestID: id,
		Collection: col.Name, ClientName: client.Name, Script: mountPath + "/assets/login.js", Text: text,
		LoginHint: req.d.LoginHint,
	})
}

// continueWith completes the authorize request with an account of the browser.
func (m *module) continueWith(req *authRequest, client *Client, col *core.Collection, entry *ssoEntry) (string, error) {
	return m.advance(req, client, col, entry.account, grantAuth{
		sid: entry.row.Sid, acr: entry.row.Acr, amr: parseList(entry.row.Amr), authTime: entry.row.AuthTime.Time(),
	})
}

type finishBody struct {
	AuthRequestID string `json:"authRequestID"`
}

// jsonError answers a page script; message is a catalog key (text that is not
// a key is sent as it is).
func jsonError(e *core.RequestEvent, status int, message string) error {
	return e.JSON(status, map[string]any{"status": status, "message": apis.NewPageText(e, "").T(message)})
}

// loginFinish completes an authorize request with the Claviger token the
// sign-in page obtained from the ordinary auth endpoints (password, passkey,
// TOTP: whatever the collection requires ends in the same token).
func (m *module) loginFinish(e *core.RequestEvent) error {
	var body finishBody
	if err := e.BindBody(&body); err != nil {
		return jsonError(e, http.StatusBadRequest, "oidc.err.invalidRequest")
	}

	req, err := m.store.requestByID(body.AuthRequestID)
	if err != nil || (req.row.Status != statusPending && req.row.Status != statusAuthenticated) {
		return jsonError(e, http.StatusBadRequest, "oidc.err.signInExpired")
	}

	if !m.checkBinding(e, req) {
		return jsonError(e, http.StatusForbidden, "oidc.err.signInOtherBrowser")
	}

	client, err := m.store.findClient(req.row.ClientId)
	if err != nil {
		return jsonError(e, http.StatusBadRequest, "oidc.err.appUnavailable")
	}

	col, err := client.collection()
	if err != nil {
		return jsonError(e, http.StatusBadRequest, "oidc.err.appUnavailable")
	}

	auth, status, msg := m.freshSignIn(e, client, col)
	if status != 0 {
		return jsonError(e, status, msg)
	}

	authTime := auth.authTime

	// max_age (and prompt=login, which the library turns into max_age=0) asks
	// for a sign-in that is recent enough. One that happened while this very
	// request waited is always recent enough.
	startedAt := req.row.Expires.Time().Add(-authRequestLifetime)
	if req.d.MaxAge != nil && authTime.Before(startedAt.Add(-time.Second)) &&
		time.Since(authTime) > time.Duration(*req.d.MaxAge)*time.Second {
		return jsonError(e, http.StatusUnauthorized, "oidc.err.signInAgain")
	}

	target, err := m.advance(req, client, col, e.Auth, auth)
	if err != nil {
		if errors.Is(err, errRequestGone) {
			return jsonError(e, http.StatusBadRequest, "oidc.err.signInExpired")
		}

		return jsonError(e, http.StatusInternalServerError, "oidc.err.consentCheckFailed")
	}

	// this sign-in is now one of the browser's single sign-on sessions
	m.ssoStart(e, col.Id, e.Auth.Id, auth.sid, auth.acr, auth.amr, authTime)

	return e.JSON(http.StatusOK, map[string]any{"redirect": target})
}

// freshSignIn checks that the request carries the token of a real, fresh
// sign-in of an account that may use the client, and returns how and when it
// happened. A non-zero status is the HTTP error to answer with. The authorize
// flow and the device flow share it.
func (m *module) freshSignIn(e *core.RequestEvent, client *Client, col *core.Collection) (grantAuth, int, string) {
	if e.Auth == nil || e.Auth.Collection().Id != col.Id {
		return grantAuth{}, http.StatusUnauthorized, "oidc.err.wrongAccountType"
	}

	// only a real, fresh sign-in counts: not an API key, not a token that
	// predates sessions, not one that has been lying around
	sid := apis.SessionIdFromRequest(e)
	if sid == "" {
		return grantAuth{}, http.StatusUnauthorized, "oidc.err.freshRequired"
	}

	if core.IsBanned(m.app, e.Auth) {
		return grantAuth{}, http.StatusForbidden, "oidc.err.cannotSignIn"
	}

	if err := m.store.orgBoundOK(client, e.Auth); err != nil {
		return grantAuth{}, http.StatusForbidden, "oidc.err.notOrgMember"
	}

	var sess struct {
		Created    types.DateTime `db:"created"`
		AuthMethod string         `db:"authMethod"`
	}
	if err := m.app.ConcurrentDB().NewQuery(
		"SELECT [[created]], [[authMethod]] FROM {{" + core.CollectionNameSessions + "}} WHERE [[id]] = {:sid}",
	).Bind(dbx.Params{"sid": sid}).One(&sess); err != nil {
		return grantAuth{}, http.StatusUnauthorized, "oidc.err.freshRequired"
	}

	authTime := sess.Created.Time()
	if time.Since(authTime) > maxSignInAge {
		return grantAuth{}, http.StatusUnauthorized, "oidc.err.tooOld"
	}

	amr, acr := authnContext(sess.AuthMethod, apis.HasFreshPasskey(e, maxSignInAge))

	return grantAuth{sid: sid, acr: acr, amr: amr, authTime: authTime}, 0, ""
}

// grantAuth is how and when the user authenticated.
type grantAuth struct {
	sid, acr string
	amr      []string
	authTime time.Time
}

var (
	errRequestGone  = errors.New("the authorize request is gone")
	errNeedsConsent = errors.New("consent is required but no page may be shown")
)

// advance records the authentication on the authorize request and returns where
// the browser goes next: the consent page, or the callback that issues the code.
func (m *module) advance(req *authRequest, client *Client, col *core.Collection, account *core.Record, a grantAuth) (string, error) {
	next := statusDone

	if !client.FirstParty {
		consented := m.store.hasConsent(account, client.ClientID, req.d.Scopes)
		if slices.Contains(req.d.Prompt, oidc.PromptConsent) {
			consented = false
		}

		if !consented {
			next = statusAuthenticated
		}
	}

	// prompt=none may not show the consent page
	if next == statusAuthenticated && slices.Contains(req.d.Prompt, oidc.PromptNone) {
		return "", errNeedsConsent
	}

	n, err := exec(m.app,
		"UPDATE {{"+core.CollectionNameOAuthRequests+"}} SET [[status]] = {:next}, [[collectionRef]] = {:col}, [[subject]] = {:sub}, "+
			"[[sid]] = {:sid}, [[amr]] = {:amr}, [[acr]] = {:acr}, [[authTime]] = {:at} "+
			"WHERE [[requestId]] = {:id} AND [[status]] IN ({:pending}, {:authn}) AND [[expires]] > {:now}",
		dbx.Params{
			"next": next, "col": col.Id, "sub": account.Id, "sid": a.sid, "amr": jsonList(a.amr), "acr": a.acr,
			"at": dateString(a.authTime), "id": req.row.RequestId, "pending": statusPending, "authn": statusAuthenticated,
			"now": nowString(),
		})
	if err != nil {
		return "", err
	}

	if n != 1 {
		return "", errRequestGone
	}

	if next == statusAuthenticated {
		return mountPath + "/consent?authRequestID=" + url.QueryEscape(req.row.RequestId), nil
	}

	return mountPath + "/authorize/callback?id=" + url.QueryEscape(req.row.RequestId), nil
}

// denyRequest ends the authorize request with an error redirected to the
// (already validated) redirect URI.
func (m *module) denyRequest(e *core.RequestEvent, req *authRequest, code, description string) error {
	_, _ = exec(m.app,
		"UPDATE {{"+core.CollectionNameOAuthRequests+"}} SET [[status]] = {:denied} WHERE [[requestId]] = {:id} AND [[status]] IN ({:pending}, {:authn})",
		dbx.Params{"denied": statusDenied, "id": req.row.RequestId, "pending": statusPending, "authn": statusAuthenticated})

	return e.Redirect(http.StatusFound, errorRedirect(req, code, description))
}

func ssoCookieValue(r *http.Request) string {
	if c, err := r.Cookie(ssoCookieName); err == nil {
		return c.Value
	}

	return ""
}

// authnContext maps how the session was authenticated to the amr values (RFC
// 8176) and the acr the provider asserts.
func authnContext(sessionMethod string, freshPasskey bool) ([]string, string) {
	var amr []string

	switch sessionMethod {
	case core.MFAMethodWebAuthn, core.SessionAuthMethodDiscover:
		amr = []string{"hwk", "user"}
	case core.MFAMethodTOTP:
		amr = []string{"pwd", "otp", "mfa"}
	case core.SessionAuthMethodMFA:
		amr = []string{"pwd", "mfa"}
	case core.MFAMethodOTP:
		amr = []string{"otp"}
	default:
		amr = []string{"pwd"}
	}

	if freshPasskey && !slices.Contains(amr, "hwk") {
		amr = append(amr, "hwk", "user")
	}

	switch {
	case slices.Contains(amr, "hwk"):
		return amr, ACRPasskey
	case slices.Contains(amr, "mfa"):
		return amr, ACRMFA
	}

	return amr, ACRPassword
}

// ---- consent -------------------------------------------------------------

// hasConsent reports whether the account already granted all the scopes to the client.
func (s *Store) hasConsent(account *core.Record, clientID string, scopes []string) bool {
	rec, err := s.app.FindFirstRecordByFilter(core.CollectionNameOAuthConsents,
		"collectionRef = {:c} && recordRef = {:r} && clientId = {:k}",
		dbx.Params{"c": account.Collection().Id, "r": account.Id, "k": clientID})
	if err != nil {
		return false // no row: not consented
	}

	var granted []string
	_ = rec.UnmarshalJSONField("scopes", &granted)

	for _, sc := range scopes {
		if !slices.Contains(granted, sc) {
			return false
		}
	}

	return true
}

func (m *module) saveConsent(account *core.Record, clientID string, scopes []string, ip string) error {
	ctx := core.WithAuditContext(context.Background(), core.NewAuditActorFromRecord(account), ip)

	rec, err := m.app.FindFirstRecordByFilter(core.CollectionNameOAuthConsents,
		"collectionRef = {:c} && recordRef = {:r} && clientId = {:k}",
		dbx.Params{"c": account.Collection().Id, "r": account.Id, "k": clientID})
	if err != nil {
		col, cerr := m.app.FindCachedCollectionByNameOrId(core.CollectionNameOAuthConsents)
		if cerr != nil {
			return cerr
		}

		rec = core.NewRecord(col)
		rec.Set("collectionRef", account.Collection().Id)
		rec.Set("recordRef", account.Id)
		rec.Set("clientId", clientID)
	}

	var granted []string
	_ = rec.UnmarshalJSONField("scopes", &granted)

	for _, sc := range scopes {
		if !slices.Contains(granted, sc) {
			granted = append(granted, sc)
		}
	}

	rec.Set("scopes", granted)

	return m.app.SaveWithContext(ctx, rec)
}

// scopeDescriptions lists the scopes the consent page can describe; the texts
// are the catalog keys "oidc.scope.<scope>.label" and ".detail".
var scopeDescriptions = map[string]struct{}{
	oidc.ScopeOpenID: {}, oidc.ScopeProfile: {}, oidc.ScopeEmail: {}, oidc.ScopeOfflineAccess: {}, ScopeOrgs: {},
}

func (m *module) consentPage(e *core.RequestEvent) error {
	id := e.Request.URL.Query().Get("authRequestID")

	req, err := m.store.requestByID(id)
	if err != nil || req.row.Status != statusAuthenticated || !m.checkBinding(e, req) {
		return m.errorPage(e, http.StatusBadRequest, "oidc.err.requestExpired")
	}

	client, err := m.store.findClient(req.row.ClientId)
	if err != nil {
		return m.errorPage(e, http.StatusBadRequest, "oidc.err.appUnavailable")
	}

	account, _ := m.app.FindRecordById(req.row.CollectionRef, req.row.Subject)

	// the account's own language preference comes before Accept-Language
	text := apis.NewPageText(e, core.RecordLanguage(m.app, account))

	d := pageData{
		Title: text.T("oidc.consent.title"), Heading: text.T("oidc.consent.heading"), Page: "consent", RequestID: id,
		ClientName: client.Name, Script: mountPath + "/assets/consent.js", Text: text,
	}

	if account != nil {
		d.Account = account.Email()
	}

	for _, sc := range req.d.Scopes {
		info := scopeInfo{Label: sc, Detail: text.T("oidc.scope.unknown")}
		if _, known := scopeDescriptions[sc]; known {
			info = scopeInfo{Label: text.T("oidc.scope." + sc + ".label"), Detail: text.T("oidc.scope." + sc + ".detail")}
		}

		d.Scopes = append(d.Scopes, info)
	}

	return m.render(e, http.StatusOK, d)
}

type consentBody struct {
	AuthRequestID string `json:"authRequestID"`
	Allow         bool   `json:"allow"`
}

func (m *module) consentDecision(e *core.RequestEvent) error {
	var body consentBody
	if err := e.BindBody(&body); err != nil {
		return jsonError(e, http.StatusBadRequest, "oidc.err.invalidRequest")
	}

	req, err := m.store.requestByID(body.AuthRequestID)
	if err != nil || req.row.Status != statusAuthenticated {
		return jsonError(e, http.StatusBadRequest, "oidc.err.requestExpired")
	}

	if !m.checkBinding(e, req) {
		return jsonError(e, http.StatusForbidden, "oidc.err.requestOtherBrowser")
	}

	if !body.Allow {
		if _, err := exec(m.app,
			"UPDATE {{"+core.CollectionNameOAuthRequests+"}} SET [[status]] = {:denied} WHERE [[requestId]] = {:id} AND [[status]] = {:authn}",
			dbx.Params{"denied": statusDenied, "id": req.row.RequestId, "authn": statusAuthenticated}); err != nil {
			return jsonError(e, http.StatusInternalServerError, "oidc.err.decisionFailed")
		}

		return e.JSON(http.StatusOK, map[string]any{"redirect": denyRedirect(req)})
	}

	account, err := m.app.FindRecordById(req.row.CollectionRef, req.row.Subject)
	if err != nil {
		return jsonError(e, http.StatusBadRequest, "oidc.err.requestExpiredShort")
	}

	if err := m.saveConsent(account, req.row.ClientId, req.d.Scopes, e.RealIP()); err != nil {
		return jsonError(e, http.StatusInternalServerError, "oidc.err.consentSaveFailed")
	}

	n, err := exec(m.app,
		"UPDATE {{"+core.CollectionNameOAuthRequests+"}} SET [[status]] = {:done} WHERE [[requestId]] = {:id} AND [[status]] = {:authn}",
		dbx.Params{"done": statusDone, "id": req.row.RequestId, "authn": statusAuthenticated})
	if err != nil || n != 1 {
		return jsonError(e, http.StatusBadRequest, "oidc.err.requestExpiredShort")
	}

	return e.JSON(http.StatusOK, map[string]any{"redirect": mountPath + "/authorize/callback?id=" + url.QueryEscape(req.row.RequestId)})
}

// denyRedirect is the error response sent to the (already validated)
// redirect URI when the user declines.
func denyRedirect(req *authRequest) string {
	return errorRedirect(req, "access_denied", "The user denied the request.")
}

// errorRedirect is the error response (RFC 6749 4.1.2.1) for the request.
func errorRedirect(req *authRequest, code, description string) string {
	u, err := url.Parse(req.d.RedirectURI)
	if err != nil {
		return mountPath + "/logged-out"
	}

	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", description)
	if req.d.State != "" {
		q.Set("state", req.d.State)
	}

	u.RawQuery = q.Encode()

	return u.String()
}
