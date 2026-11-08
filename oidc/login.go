package oidc

import (
	"context"
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
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>{{.Title}}</title>
<link rel="icon" href="data:,">
<link rel="stylesheet" href="/oidc/assets/oidc.css">
</head>
<body>
<main class="card" id="app" data-page="{{.Page}}" data-request="{{.RequestID}}" data-collection="{{.Collection}}">
<h1>{{.Heading}}</h1>
{{if .Message}}<p class="msg {{.MessageClass}}" role="alert">{{.Message}}</p>{{end}}
{{if eq .Page "login"}}
<p class="sub">to continue to <strong>{{.ClientName}}</strong></p>
<form id="pw" hidden>
<label>Email or username<input name="identity" autocomplete="username webauthn" required></label>
<label>Password<input name="password" type="password" autocomplete="current-password" required></label>
<button type="submit">Sign in</button>
</form>
<form id="totp" hidden>
<label>Authenticator code or backup code<input name="code" inputmode="numeric" autocomplete="one-time-code" required></label>
<button type="submit">Verify</button>
</form>
<div id="captchaBox" hidden></div>
<div id="passkeyBox" hidden>
<div class="or"><span>or</span></div>
<button id="passkey" type="button" class="secondary">Sign in with a passkey</button>
</div>
<p id="status" class="msg err" role="alert" hidden></p>
{{end}}
{{if eq .Page "consent"}}
<p class="sub"><strong>{{.ClientName}}</strong> wants to access your account{{if .Account}} <span class="muted">({{.Account}})</span>{{end}}.</p>
<ul class="scopes">{{range .Scopes}}<li><strong>{{.Label}}</strong><span>{{.Detail}}</span></li>{{end}}</ul>
<div class="row">
<button id="allow" type="button">Allow</button>
<button id="deny" type="button" class="secondary">Cancel</button>
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
	Scopes                                                           []scopeInfo
}

func (m *module) render(e *core.RequestEvent, status int, d pageData) error {
	m.pageHeaders(e, d.Page)
	e.Response.Header().Set("Content-Type", "text/html; charset=utf-8")
	e.Response.WriteHeader(status)

	return pageTemplate.Execute(e.Response, d)
}

func (m *module) errorPage(e *core.RequestEvent, status int, message string) error {
	return m.render(e, status, pageData{Title: "Sign-in problem", Heading: "Sign-in problem", Page: "error", Message: message, MessageClass: "err"})
}

func (m *module) bindPages(se *core.ServeEvent) {
	se.Router.GET(mountPath+"/login", m.loginPage)
	se.Router.POST(mountPath+"/login/finish", m.loginFinish)
	se.Router.GET(mountPath+"/consent", m.consentPage)
	se.Router.POST(mountPath+"/consent", m.consentDecision)

	se.Router.GET(mountPath+"/logged-out", func(e *core.RequestEvent) error {
		return m.render(e, http.StatusOK, pageData{Title: "Signed out", Heading: "You are signed out", Page: "info", Message: "You can close this window.", MessageClass: "ok"})
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
		return m.errorPage(e, http.StatusBadRequest, "This sign-in link has expired. Return to the app and try again.")
	}

	if !m.bind(e, req) {
		return m.errorPage(e, http.StatusForbidden, "This sign-in was started in another browser. Return to the app and try again.")
	}

	client, err := m.store.findClient(req.row.ClientId)
	if err != nil {
		return m.errorPage(e, http.StatusBadRequest, "This application is not available.")
	}

	col, err := client.collection()
	if err != nil {
		return m.errorPage(e, http.StatusBadRequest, "This application is not available.")
	}

	return m.render(e, http.StatusOK, pageData{
		Title: "Sign in", Heading: "Sign in", Page: "login", RequestID: id,
		Collection: col.Name, ClientName: client.Name, Script: mountPath + "/assets/login.js",
	})
}

type finishBody struct {
	AuthRequestID string `json:"authRequestID"`
}

func jsonError(e *core.RequestEvent, status int, message string) error {
	return e.JSON(status, map[string]any{"status": status, "message": message})
}

// loginFinish completes an authorize request with the Claviger token the
// sign-in page obtained from the ordinary auth endpoints (password, passkey,
// TOTP: whatever the collection requires ends in the same token).
func (m *module) loginFinish(e *core.RequestEvent) error {
	var body finishBody
	if err := e.BindBody(&body); err != nil {
		return jsonError(e, http.StatusBadRequest, "Invalid request.")
	}

	req, err := m.store.requestByID(body.AuthRequestID)
	if err != nil || (req.row.Status != statusPending && req.row.Status != statusAuthenticated) {
		return jsonError(e, http.StatusBadRequest, "This sign-in has expired. Return to the app and try again.")
	}

	if !m.checkBinding(e, req) {
		return jsonError(e, http.StatusForbidden, "This sign-in was started in another browser.")
	}

	client, err := m.store.findClient(req.row.ClientId)
	if err != nil {
		return jsonError(e, http.StatusBadRequest, "This application is not available.")
	}

	col, err := client.collection()
	if err != nil {
		return jsonError(e, http.StatusBadRequest, "This application is not available.")
	}

	if e.Auth == nil || e.Auth.Collection().Id != col.Id {
		return jsonError(e, http.StatusUnauthorized, "Sign in with an account of the right type.")
	}

	// only a real, fresh sign-in counts: not an API key, not a token that
	// predates sessions, not one that has been lying around
	sid := apis.SessionIdFromRequest(e)
	if sid == "" {
		return jsonError(e, http.StatusUnauthorized, "A fresh sign-in is required.")
	}

	if core.IsBanned(m.app, e.Auth) {
		return jsonError(e, http.StatusForbidden, "This account cannot sign in.")
	}

	var sess struct {
		Created    types.DateTime `db:"created"`
		AuthMethod string         `db:"authMethod"`
	}
	if err := m.app.ConcurrentDB().NewQuery(
		"SELECT [[created]], [[authMethod]] FROM {{" + core.CollectionNameSessions + "}} WHERE [[id]] = {:sid}",
	).Bind(dbx.Params{"sid": sid}).One(&sess); err != nil {
		return jsonError(e, http.StatusUnauthorized, "A fresh sign-in is required.")
	}

	authTime := sess.Created.Time()
	if time.Since(authTime) > maxSignInAge {
		return jsonError(e, http.StatusUnauthorized, "That sign-in is too old. Sign in again.")
	}

	if req.d.MaxAge != nil && time.Since(authTime) > time.Duration(*req.d.MaxAge)*time.Second {
		return jsonError(e, http.StatusUnauthorized, "Sign in again.")
	}

	amr, acr := authnContext(sess.AuthMethod, apis.HasFreshPasskey(e, maxSignInAge))

	next := statusDone
	if !client.FirstParty {
		consented, cerr := m.hasConsent(e.Auth, client.ClientID, req.d.Scopes)
		if cerr != nil {
			return jsonError(e, http.StatusInternalServerError, "Failed to check the consent.")
		}

		if !consented {
			next = statusAuthenticated
		}
	}

	n, err := exec(m.app,
		"UPDATE {{"+core.CollectionNameOAuthRequests+"}} SET [[status]] = {:next}, [[collectionRef]] = {:col}, [[subject]] = {:sub}, "+
			"[[sid]] = {:sid}, [[amr]] = {:amr}, [[acr]] = {:acr}, [[authTime]] = {:at} "+
			"WHERE [[requestId]] = {:id} AND [[status]] IN ({:pending}, {:authn}) AND [[expires]] > {:now}",
		dbx.Params{
			"next": next, "col": col.Id, "sub": e.Auth.Id, "sid": sid, "amr": jsonList(amr), "acr": acr,
			"at": dateString(authTime), "id": req.row.RequestId, "pending": statusPending, "authn": statusAuthenticated,
			"now": nowString(),
		})
	if err != nil || n != 1 {
		return jsonError(e, http.StatusBadRequest, "This sign-in has expired. Return to the app and try again.")
	}

	target := mountPath + "/authorize/callback?id=" + url.QueryEscape(req.row.RequestId)
	if next == statusAuthenticated {
		target = mountPath + "/consent?authRequestID=" + url.QueryEscape(req.row.RequestId)
	}

	return e.JSON(http.StatusOK, map[string]any{"redirect": target})
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

func (m *module) hasConsent(account *core.Record, clientID string, scopes []string) (bool, error) {
	rec, err := m.app.FindFirstRecordByFilter(core.CollectionNameOAuthConsents,
		"collectionRef = {:c} && recordRef = {:r} && clientId = {:k}",
		dbx.Params{"c": account.Collection().Id, "r": account.Id, "k": clientID})
	if err != nil {
		return false, nil //nolint:nilerr // no row: not consented
	}

	var granted []string
	_ = rec.UnmarshalJSONField("scopes", &granted)

	for _, sc := range scopes {
		if !slices.Contains(granted, sc) {
			return false, nil
		}
	}

	return true, nil
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

var scopeDescriptions = map[string]scopeInfo{
	oidc.ScopeOpenID:        {"Sign you in", "Confirm who you are to this application."},
	oidc.ScopeProfile:       {"Your profile", "Your name and username."},
	oidc.ScopeEmail:         {"Your email address", "Your email address and whether it is verified."},
	oidc.ScopeOfflineAccess: {"Stay signed in", "Keep access when you are not using the application."},
}

func (m *module) consentPage(e *core.RequestEvent) error {
	id := e.Request.URL.Query().Get("authRequestID")

	req, err := m.store.requestByID(id)
	if err != nil || req.row.Status != statusAuthenticated || !m.checkBinding(e, req) {
		return m.errorPage(e, http.StatusBadRequest, "This request has expired. Return to the app and try again.")
	}

	client, err := m.store.findClient(req.row.ClientId)
	if err != nil {
		return m.errorPage(e, http.StatusBadRequest, "This application is not available.")
	}

	account, _ := m.app.FindRecordById(req.row.CollectionRef, req.row.Subject)

	d := pageData{
		Title: "Allow access", Heading: "Allow access?", Page: "consent", RequestID: id,
		ClientName: client.Name, Script: mountPath + "/assets/consent.js",
	}

	if account != nil {
		d.Account = account.Email()
	}

	for _, sc := range req.d.Scopes {
		info, ok := scopeDescriptions[sc]
		if !ok {
			info = scopeInfo{Label: sc, Detail: "Access requested by the application."}
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
		return jsonError(e, http.StatusBadRequest, "Invalid request.")
	}

	req, err := m.store.requestByID(body.AuthRequestID)
	if err != nil || req.row.Status != statusAuthenticated {
		return jsonError(e, http.StatusBadRequest, "This request has expired. Return to the app and try again.")
	}

	if !m.checkBinding(e, req) {
		return jsonError(e, http.StatusForbidden, "This request was started in another browser.")
	}

	if !body.Allow {
		if _, err := exec(m.app,
			"UPDATE {{"+core.CollectionNameOAuthRequests+"}} SET [[status]] = {:denied} WHERE [[requestId]] = {:id} AND [[status]] = {:authn}",
			dbx.Params{"denied": statusDenied, "id": req.row.RequestId, "authn": statusAuthenticated}); err != nil {
			return jsonError(e, http.StatusInternalServerError, "Failed to record the decision.")
		}

		return e.JSON(http.StatusOK, map[string]any{"redirect": denyRedirect(req)})
	}

	account, err := m.app.FindRecordById(req.row.CollectionRef, req.row.Subject)
	if err != nil {
		return jsonError(e, http.StatusBadRequest, "This request has expired.")
	}

	if err := m.saveConsent(account, req.row.ClientId, req.d.Scopes, e.RealIP()); err != nil {
		return jsonError(e, http.StatusInternalServerError, "Failed to save the consent.")
	}

	n, err := exec(m.app,
		"UPDATE {{"+core.CollectionNameOAuthRequests+"}} SET [[status]] = {:done} WHERE [[requestId]] = {:id} AND [[status]] = {:authn}",
		dbx.Params{"done": statusDone, "id": req.row.RequestId, "authn": statusAuthenticated})
	if err != nil || n != 1 {
		return jsonError(e, http.StatusBadRequest, "This request has expired.")
	}

	return e.JSON(http.StatusOK, map[string]any{"redirect": mountPath + "/authorize/callback?id=" + url.QueryEscape(req.row.RequestId)})
}

// denyRedirect is the error response sent to the (already validated)
// redirect URI when the user declines.
func denyRedirect(req *authRequest) string {
	u, err := url.Parse(req.d.RedirectURI)
	if err != nil {
		return mountPath + "/logged-out"
	}

	q := u.Query()
	q.Set("error", "access_denied")
	q.Set("error_description", "The user denied the request.")

	if req.d.State != "" {
		q.Set("state", req.d.State)
	}

	u.RawQuery = q.Encode()

	return u.String()
}
