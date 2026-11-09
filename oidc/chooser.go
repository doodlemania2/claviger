package oidc

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/doodlemania2/claviger/core"
)

// The account chooser (#94).
//
// A browser can hold several signed-in accounts (sso.go). When a request has to
// pick one (prompt=select_account, or more than one account could carry it and
// there is no login_hint), the sign-in page shows this chooser: one button per
// account (initial, name, email, collection), "use another account", and links
// to sign one or all of them out. Accounts that cannot be used for the client
// (another kind of account, not a member of the client's organization, a
// sign-in older than max_age) are shown greyed out with the reason.
//
// The page is the only place the accounts are listed. They are identified by the
// opaque id of their single sign-on row, never by subject, and the endpoints
// that act on them (select, sign out) require the per-request browser binding
// on top of the HttpOnly single sign-on cookie, so neither another site nor
// another browser can enumerate or pick them, and nothing about them reaches
// the relying party.
//
// Every user-visible string of the page is in chooserText (Go) or STRINGS at
// the top of chooserJS, so the page can be translated without touching the
// logic.

var chooserText = struct {
	Title, Heading, Sub, UseAnother, SignOut, SignOutAll string
	ReasonCollection, ReasonOrg, ReasonReauth            string
}{
	Title:            "Choose an account",
	Heading:          "Choose an account",
	Sub:              "to continue to",
	UseAnother:       "Use another account",
	SignOut:          "Sign out",
	SignOutAll:       "Sign out of all accounts",
	ReasonCollection: "This account cannot be used with this application.",
	ReasonOrg:        "This account is not a member of the organization this application belongs to.",
	ReasonReauth:     "This account has to sign in again.",
}

const (
	flowAuthorize = "authorize"
	flowDevice    = "device"
)

type chooserAccount struct {
	ID, Initial, Name, Email, Collection, Reason string
	Eligible, Preselected                        bool
}

type chooserView struct {
	flow, ref, preselect string
	client               *Client
	entries              []ssoEntry
	other                string // where "use another account" goes
}

func displayName(a *core.Record) string {
	if a.Collection().Fields.GetByName("name") != nil {
		if n := strings.TrimSpace(a.GetString("name")); n != "" {
			return n
		}
	}

	if a.Email() != "" {
		return a.Email()
	}

	if a.Collection().Fields.GetByName("username") != nil && a.GetString("username") != "" {
		return a.GetString("username")
	}

	return a.Id
}

func initialOf(s string) string {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return string(unicode.ToUpper(r))
		}
	}

	return "?"
}

func (m *module) renderChooser(e *core.RequestEvent, v chooserView) error {
	d := extData{
		Page: "chooser", Title: chooserText.Title, Heading: chooserText.Heading, Ref: v.ref, Flow: v.flow,
		ClientName: v.client.Name, Script: mountPath + "/assets/chooser.js", Other: v.other,
		SelectURL: mountPath + "/account/select", SignoutURL: mountPath + "/account/signout", T: chooserText,
	}

	for _, en := range v.entries {
		a := chooserAccount{
			ID: en.row.Id, Name: displayName(en.account), Email: en.account.Email(),
			Collection: en.account.Collection().Name, Eligible: en.eligible(), Preselected: en.row.Id == v.preselect,
		}

		if a.Email == a.Name {
			a.Email = ""
		}

		a.Initial = initialOf(a.Name)

		switch en.reason {
		case reasonCollection:
			a.Reason = chooserText.ReasonCollection
		case reasonOrg:
			a.Reason = chooserText.ReasonOrg
		case reasonReauth:
			a.Reason = chooserText.ReasonReauth
		}

		d.Accounts = append(d.Accounts, a)
	}

	return m.renderExt(e, http.StatusOK, d)
}

// ---- the flows the chooser serves ---------------------------------------

// pendingFlow is an authorize request or device authorization that waits for
// the user to pick an account.
type pendingFlow struct {
	client   *Client
	maxAge   *uint
	reload   string // the page that decides again
	clientID string
	proceed  func(*ssoEntry) (string, error)
}

func (m *module) loadFlow(e *core.RequestEvent, flow, ref string) (*pendingFlow, int, string) {
	switch flow {
	case flowAuthorize:
		req, err := m.store.requestByID(ref)
		if err != nil || req.row.Status != statusPending {
			return nil, http.StatusBadRequest, "This sign-in has expired. Return to the app and try again."
		}

		if !m.checkBinding(e, req) {
			return nil, http.StatusForbidden, "This sign-in was started in another browser."
		}

		client, err := m.store.findClient(req.row.ClientId)
		if err != nil {
			return nil, http.StatusBadRequest, "This application is not available."
		}

		col, err := client.collection()
		if err != nil {
			return nil, http.StatusBadRequest, "This application is not available."
		}

		return &pendingFlow{
			client: client, maxAge: req.d.MaxAge, clientID: client.ClientID,
			reload: mountPath + "/login?authRequestID=" + url.QueryEscape(ref),
			proceed: func(en *ssoEntry) (string, error) {
				return m.continueWith(req, client, col, en)
			},
		}, 0, ""
	case flowDevice:
		row, client, _, status, msg := m.deviceFlow(e, ref)
		if status != 0 {
			return nil, status, msg
		}

		if row.Status != deviceStatusPending {
			return nil, http.StatusBadRequest, "This code has already been used."
		}

		return &pendingFlow{
			client: client, clientID: client.ClientID, reload: deviceVerifyURL(ref),
			proceed: func(en *ssoEntry) (string, error) {
				return m.deviceAuthenticate(row, en.account, grantAuth{
					sid: en.row.Sid, acr: en.row.Acr, amr: parseList(en.row.Amr), authTime: en.row.AuthTime.Time(),
				})
			},
		}, 0, ""
	}

	return nil, http.StatusBadRequest, "Invalid request."
}

type chooserBody struct {
	Flow    string `json:"flow"`
	Ref     string `json:"ref"`
	Account string `json:"account"`
}

// accountSelect continues the flow with the account the user picked.
func (m *module) accountSelect(e *core.RequestEvent) error {
	var body chooserBody
	if err := e.BindBody(&body); err != nil {
		return jsonError(e, http.StatusBadRequest, "Invalid request.")
	}

	pf, status, msg := m.loadFlow(e, body.Flow, body.Ref)
	if pf == nil {
		return jsonError(e, status, msg)
	}

	for _, en := range m.store.ssoEntries(ssoCookieValue(e.Request), pf.client, pf.maxAge) {
		if body.Account == "" || en.row.Id != body.Account {
			continue
		}

		if !en.eligible() {
			return jsonError(e, http.StatusBadRequest, "This account cannot be used here.")
		}

		target, err := pf.proceed(&en)
		if err != nil {
			return jsonError(e, http.StatusBadRequest, "This sign-in has expired. Return to the app and try again.")
		}

		return e.JSON(http.StatusOK, map[string]any{"redirect": target})
	}

	return jsonError(e, http.StatusBadRequest, "That account is no longer signed in.")
}

// accountSignout signs one account (or, with "all", every account) of the
// browser out: their Claviger sessions and every OIDC token issued under them
// are revoked, exactly like end_session.
func (m *module) accountSignout(e *core.RequestEvent) error {
	var body chooserBody
	if err := e.BindBody(&body); err != nil {
		return jsonError(e, http.StatusBadRequest, "Invalid request.")
	}

	pf, status, msg := m.loadFlow(e, body.Flow, body.Ref)
	if pf == nil {
		return jsonError(e, status, msg)
	}

	// every live account, usable for this client or not: any of them can be signed out
	for _, en := range m.store.ssoEntries(ssoCookieValue(e.Request), nil, nil) {
		if body.Account != "all" && en.row.Id != body.Account {
			continue
		}

		if err := m.store.endSession(e.Request.Context(), en.row.Sid, en.row.Subject, "signed out on the sign-in page", pf.clientID); err != nil {
			return jsonError(e, http.StatusInternalServerError, "Failed to sign out.")
		}
	}

	m.pruneSSOCookie(e.Response, e.Request)

	return e.JSON(http.StatusOK, map[string]any{"redirect": pf.reload})
}

// ---- pages shared by the chooser and the device flow ---------------------

// extData is the data of the pages in extTemplate.
type extData struct {
	Page, Title, Heading, Sub, ClientName, Ref, Flow, Script, Message, MessageClass string
	Accounts                                                                        []chooserAccount
	Other, SelectURL, SignoutURL                                                    string
	Scopes                                                                          []scopeInfo
	Account, UserCode, Action                                                       string
	T                                                                               any // the page's strings
}

const defaultLang = "en"

var extTemplate = template.Must(template.New("ext").Funcs(template.FuncMap{"lang": func() string { return defaultLang }}).Parse(`
{{define "head"}}<!doctype html>
<html lang="{{lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>{{.Title}}</title>
<link rel="icon" href="data:,">
<link rel="stylesheet" href="/oidc/assets/oidc.css">
<link rel="stylesheet" href="/oidc/assets/chooser.css">
</head>
<body>
<main class="card" id="app" data-page="{{.Page}}" data-flow="{{.Flow}}" data-request="{{.Ref}}" data-other="{{.Other}}" data-select="{{.SelectURL}}" data-signout="{{.SignoutURL}}">
<h1>{{.Heading}}</h1>
{{if .Message}}<p class="msg {{.MessageClass}}" role="alert">{{.Message}}</p>{{end}}
{{end}}
{{define "foot"}}</main>
{{if .Script}}<script type="module" src="{{.Script}}"></script>{{end}}
</body>
</html>{{end}}

{{define "chooser"}}{{template "head" .}}
<p class="sub">{{.T.Sub}} <strong>{{.ClientName}}</strong></p>
<ul class="accounts">
{{range .Accounts}}<li>
<button type="button" class="account{{if .Preselected}} pre{{end}}" data-account="{{.ID}}"{{if not .Eligible}} disabled{{end}}>
<span class="avatar" aria-hidden="true">{{.Initial}}</span>
<span class="who"><strong>{{.Name}}</strong>{{if .Email}}<span class="muted">{{.Email}}</span>{{end}}<span class="muted">{{.Collection}}</span>{{if .Reason}}<span class="why">{{.Reason}}</span>{{end}}</span>
</button>
<button type="button" class="link" data-signout="{{.ID}}">{{$.T.SignOut}}</button>
</li>{{end}}
</ul>
<button id="other" type="button" class="secondary">{{.T.UseAnother}}</button>
<button id="signoutAll" type="button" class="link">{{.T.SignOutAll}}</button>
<p id="status" class="msg err" role="alert" hidden></p>
{{template "foot" .}}{{end}}

{{define "device-code"}}{{template "head" .}}
<p class="sub">{{.T.Sub}}</p>
<form method="post" action="{{.Action}}">
<label>{{.T.CodeLabel}}<input name="user_code" value="{{.UserCode}}" autocomplete="off" autocapitalize="characters" spellcheck="false" inputmode="text" maxlength="20" required autofocus class="usercode"></label>
<button type="submit">{{.T.Continue}}</button>
</form>
{{template "foot" .}}{{end}}

{{define "device-approve"}}{{template "head" .}}
<p class="sub"><strong>{{.ClientName}}</strong> {{.T.Wants}}{{if .Account}} <span class="muted">({{.Account}})</span>{{end}}.</p>
<ul class="scopes">{{range .Scopes}}<li><strong>{{.Label}}</strong><span>{{.Detail}}</span></li>{{end}}</ul>
<p class="msg warn">{{.T.Warning}}</p>
<div class="row">
<button id="allow" type="button">{{.T.Approve}}</button>
<button id="deny" type="button" class="secondary">{{.T.Deny}}</button>
</div>
<p id="status" class="msg err" role="alert" hidden></p>
{{template "foot" .}}{{end}}

{{define "device-result"}}{{template "head" .}}
{{template "foot" .}}{{end}}
`))

// renderExt renders a page of extTemplate with the strict page policy.
func (m *module) renderExt(e *core.RequestEvent, status int, d extData) error {
	m.pageHeaders(e, d.Page)
	e.Response.Header().Set("Content-Type", "text/html; charset=utf-8")
	e.Response.WriteHeader(status)

	return extTemplate.ExecuteTemplate(e.Response, d.Page, d)
}

const chooserCSS = `.accounts { list-style: none; margin: 16px 0 0; padding: 0; }
.accounts li { display: flex; gap: 8px; align-items: stretch; margin-top: 8px; }
button.account { display: flex; align-items: center; gap: 12px; flex: 1; min-width: 0; margin: 0; text-align: left;
  font-weight: 500; color: var(--fg); background: transparent; border-color: var(--line); }
button.account.pre { border-color: var(--accent); }
.avatar { flex: none; width: 36px; height: 36px; display: grid; place-items: center; border-radius: 50%;
  color: var(--accent-fg); background: var(--accent); font-weight: 600; }
button.account[disabled] .avatar { background: var(--muted); }
.who { display: flex; flex-direction: column; min-width: 0; }
.who .muted { font-size: .85rem; overflow-wrap: anywhere; font-weight: 400; }
.who .why { font-size: .85rem; color: var(--err-fg); font-weight: 400; }
button.link { width: auto; margin: 0; padding: 6px 8px; font-weight: 500; text-decoration: underline;
  color: var(--accent); background: none; border: none; }
#other { margin-top: 16px; }
#signoutAll { display: block; margin: 8px auto 0; }
.usercode { font-size: 1.5rem; letter-spacing: .15em; text-align: center; text-transform: uppercase; font-family: ui-monospace, monospace; }
.msg.warn { background: var(--err-bg); color: var(--err-fg); }
`

const chooserJS = `const STRINGS = {
  failed: "That did not work. Try again.",
};

const root = document.getElementById("app");
const flow = root.dataset.flow;
const ref = root.dataset.request;
const statusEl = root.querySelector("#status");
const buttons = root.querySelectorAll("button");

function fail(text) {
  statusEl.textContent = text;
  statusEl.hidden = !text;
  buttons.forEach((b) => { if (!b.dataset.keepDisabled) b.disabled = false; });
}

async function act(url, payload) {
  buttons.forEach((b) => { b.dataset.keepDisabled = b.disabled ? "1" : ""; b.disabled = true; });
  const res = await fetch(url, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(Object.assign({ flow, ref }, payload)),
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok || !body.redirect) {
    fail(body.message || STRINGS.failed);
    return;
  }
  window.location.assign(body.redirect);
}

root.addEventListener("click", (ev) => {
  const el = ev.target.closest("button");
  if (!el) return;
  if (el.dataset.account) return act(root.dataset.select, { account: el.dataset.account });
  if (el.dataset.signout) return act(root.dataset.signout, { account: el.dataset.signout });
  if (el.id === "signoutAll") return act(root.dataset.signout, { account: "all" });
  if (el.id === "other") window.location.assign(root.dataset.other);
});
`
