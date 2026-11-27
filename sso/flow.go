package sso

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/security"
	"github.com/doodlemania2/claviger/tools/types"
)

// The sign-in sequence, independent of the protocol:
//
//  1. Start: a _ssoFlows row is written (state, nonce and PKCE verifier hashed or
//     held server side) and the browser is sent to the provider. The browser also
//     gets a random binding value (a cookie set by the route); only its hash is
//     stored.
//  2. Callback: the row is found by the state, deleted (a state is used once),
//     and must match the browser's binding value. The Provider verifies the
//     response; ProvisionLogin yields the account. A second row, phase "done",
//     holds the hash of a one-time code.
//  3. The browser goes back to the page that started the sign-in with that code
//     in the URL; the page POSTs it to the exchange route, which (with the
//     binding value again) deletes the row and answers with the ordinary
//     Claviger auth response.
//
// A code in a URL is worthless without the binding cookie, and works once, for
// two minutes.
const (
	FlowLifetime = 10 * time.Minute
	CodeLifetime = 2 * time.Minute

	phasePending = "pending"
	phaseDone    = "done"
)

func hash(v string) string {
	sum := sha256.Sum256([]byte(v))

	return hex.EncodeToString(sum[:])
}

func sameHash(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// StartInput describes a sign-in to begin.
type StartInput struct {
	ConnectionId string
	CallbackURL  string
	// ReturnTo is where the browser goes afterwards, already validated by the
	// caller as a same-origin URL.
	ReturnTo  string
	LoginHint string
	IP        string
}

// StartResult tells the route where to redirect and which binding value to
// put in the cookie.
type StartResult struct {
	RedirectURL string
	BindValue   string
}

// Start begins a sign-in through a connection. The connection must be enabled
// and (when a login hint is given) the hint's domain must route to it: a
// connection cannot be used to sign in people of a domain it does not hold.
func Start(app core.App, ctx context.Context, in StartInput) (*StartResult, error) {
	rec, err := app.FindRecordById(core.CollectionNameSSOConnections, in.ConnectionId)
	if err != nil || !rec.GetBool("enabled") {
		return nil, newErr(http.StatusNotFound, "unknown_connection", "That single sign-on connection is not available.")
	}

	c, err := LoadConnection(app, rec)
	if err != nil {
		return nil, err
	}

	if org, err := app.FindRecordById(core.CollectionNameOrgs, c.OrgId); err != nil || org.GetBool("archived") {
		return nil, newErr(http.StatusNotFound, "unknown_connection", "That single sign-on connection is not available.")
	}

	p, ok := ProviderFor(c.Type)
	if !ok {
		return nil, newErr(http.StatusNotFound, "unknown_connection", "That single sign-on connection is not available.")
	}

	state := security.RandomString(40)
	bind := security.RandomString(40)
	nonce := security.RandomString(32)
	verifier := security.RandomString(64)

	out, err := p.Begin(ctx, c, BeginInput{
		CallbackURL: in.CallbackURL, State: state, Nonce: nonce, CodeVerifier: verifier, LoginHint: in.LoginHint,
	})
	if err != nil {
		return nil, newErr(http.StatusBadGateway, "provider_unreachable", "The identity provider could not be reached: "+err.Error())
	}

	// housekeeping: expired rows go whenever a new one is written
	_, _ = app.DB().NewQuery("DELETE FROM {{" + core.CollectionNameSSOFlows + "}} WHERE [[expires]] < {:now}").
		Bind(dbx.Params{"now": types.NowDateTime().String()}).Execute()

	col, err := app.FindCachedCollectionByNameOrId(core.CollectionNameSSOFlows)
	if err != nil {
		return nil, err
	}

	row := core.NewRecord(col)
	row.Id = core.GenerateDefaultRandomId()
	row.Set("connection", c.Id)
	row.Set("stateHash", hash(state))
	row.Set("bindHash", hash(bind))
	row.Set("nonce", nonce)
	row.Set("verifier", verifier)
	row.Set("returnTo", in.ReturnTo)
	row.Set("phase", phasePending)
	row.Set("expires", types.NowDateTime().Add(FlowLifetime))

	if err := app.UnsafeWithoutHooks().SaveNoValidate(row); err != nil {
		return nil, err
	}

	return &StartResult{RedirectURL: out.RedirectURL, BindValue: bind}, nil
}

// CallbackInput is what the callback route received.
type CallbackInput struct {
	CallbackURL string
	BindValue   string
	Method      string
	Query       url.Values
	Form        url.Values
	IP          string
}

// CallbackResult tells the route where to send the browser: ReturnTo with
// sso_code on success or sso_error on failure. ReturnTo is empty when the
// state matched no sign-in (nowhere safe to go).
type CallbackResult struct {
	ReturnTo string
	Code     string
	Err      *Error
}

// URL builds the redirect target.
func (r *CallbackResult) URL() string {
	u, err := url.Parse(r.ReturnTo)
	if err != nil {
		return r.ReturnTo
	}

	q := u.Query()
	if r.Err != nil {
		q.Set("sso_error", r.Err.Code)
	} else {
		q.Set("sso_code", r.Code)
	}

	u.RawQuery = q.Encode()

	return u.String()
}

// stateOf reads the state from the places a provider may put it.
func stateOf(in CallbackInput) string {
	for _, v := range []url.Values{in.Query, in.Form} {
		if v == nil {
			continue
		}

		if s := v.Get("state"); s != "" {
			return s
		}

		if s := v.Get("RelayState"); s != "" {
			return s
		}
	}

	return ""
}

// Callback completes a sign-in.
func Callback(app core.App, ctx context.Context, in CallbackInput) *CallbackResult {
	state := stateOf(in)
	if state == "" {
		return &CallbackResult{Err: newErr(http.StatusBadRequest, "invalid_state", "The sign-in is no longer valid.")}
	}

	row, err := app.FindFirstRecordByData(core.CollectionNameSSOFlows, "stateHash", hash(state))
	if err != nil || row.GetString("phase") != phasePending {
		return &CallbackResult{Err: newErr(http.StatusBadRequest, "invalid_state", "The sign-in is no longer valid.")}
	}

	// a state is used once, whatever follows
	if err := app.UnsafeWithoutHooks().Delete(row); err != nil {
		return &CallbackResult{Err: newErr(http.StatusInternalServerError, "unavailable", "The sign-in could not continue.")}
	}

	res := &CallbackResult{ReturnTo: row.GetString("returnTo")}
	fail := func(e *Error, meta map[string]any) *CallbackResult {
		res.Err = e

		if meta == nil {
			meta = map[string]any{}
		}

		meta["reason"] = e.Code
		meta["connection"] = row.GetString("connection")
		_ = core.WriteSSOAudit(app, core.WithAuditContext(ctx, core.AuditActor{Type: core.AuditActorTypeSystem}, in.IP),
			core.AuditActionSSOLoginFailed, core.CollectionNameSSOConnections, row.GetString("connection"), meta)

		return res
	}

	if row.GetDateTime("expires").Time().Before(time.Now()) {
		return fail(newErr(http.StatusBadRequest, "expired", "The sign-in took too long. Start again."), nil)
	}

	if !sameHash(row.GetString("bindHash"), hash(in.BindValue)) {
		return fail(newErr(http.StatusForbidden, "other_browser", "The sign-in was started in another browser."), nil)
	}

	c, err := FindConnection(app, row.GetString("connection"))
	if err != nil || !c.Enabled {
		return fail(newErr(http.StatusNotFound, "unknown_connection", "That single sign-on connection is not available."), nil)
	}

	p, ok := ProviderFor(c.Type)
	if !ok {
		return fail(newErr(http.StatusNotFound, "unknown_connection", "That single sign-on connection is not available."), nil)
	}

	id, err := p.Complete(ctx, c, CompleteInput{
		CallbackURL: in.CallbackURL, Nonce: row.GetString("nonce"), CodeVerifier: row.GetString("verifier"),
		Method: in.Method, Query: in.Query, Form: in.Form,
	})
	if err != nil {
		if e, ok := AsError(err); ok {
			return fail(e, nil)
		}

		return fail(newErr(http.StatusBadGateway, "provider_failed", "The identity provider could not complete the sign-in."),
			map[string]any{"error": err.Error()})
	}

	out, err := ProvisionLogin(app, ctx, in.IP, c, id)
	if err != nil {
		if e, ok := AsError(err); ok {
			return fail(e, map[string]any{"subject": id.Subject})
		}

		return fail(newErr(http.StatusInternalServerError, "provision_failed", "The account could not be prepared."),
			map[string]any{"error": err.Error()})
	}

	code := security.RandomString(48)

	col, err := app.FindCachedCollectionByNameOrId(core.CollectionNameSSOFlows)
	if err != nil {
		return fail(newErr(http.StatusInternalServerError, "unavailable", "The sign-in could not continue."), nil)
	}

	done := core.NewRecord(col)
	done.Id = core.GenerateDefaultRandomId()
	done.Set("connection", c.Id)
	done.Set("stateHash", hash("done:"+code))
	done.Set("bindHash", row.GetString("bindHash"))
	done.Set("returnTo", row.GetString("returnTo"))
	done.Set("phase", phaseDone)
	done.Set("codeHash", hash(code))
	done.Set("recordRef", out.User.Id)
	done.Set("collectionRef", out.User.Collection().Id)
	done.Set("expires", types.NowDateTime().Add(CodeLifetime))

	if err := app.UnsafeWithoutHooks().SaveNoValidate(done); err != nil {
		return fail(newErr(http.StatusInternalServerError, "unavailable", "The sign-in could not continue."), nil)
	}

	_ = core.WriteSSOAudit(app, core.WithAuditContext(ctx, core.NewSelfAuditActor(out.User), in.IP), core.AuditActionSSOLogin,
		core.CollectionNameSSOConnections, c.Id, map[string]any{
			"orgId": c.OrgId, "connection": c.Id, "created": out.Created, "linked": out.Linked,
		})

	res.Code = code

	return res
}

// Exchange trades the one-time code (and the browser's binding value) for the
// signed-in account. The caller issues the auth response.
func Exchange(app core.App, code, bindValue string) (*core.Record, error) {
	bad := newErr(http.StatusBadRequest, "invalid_code", "The sign-in code is not valid.")

	if code == "" || len(code) > 200 {
		return nil, bad
	}

	row, err := app.FindFirstRecordByData(core.CollectionNameSSOFlows, "codeHash", hash(code))
	if err != nil || row.GetString("phase") != phaseDone {
		return nil, bad
	}

	// used once, even when the checks below fail
	if err := app.UnsafeWithoutHooks().Delete(row); err != nil {
		return nil, bad
	}

	if row.GetDateTime("expires").Time().Before(time.Now()) || !sameHash(row.GetString("bindHash"), hash(bindValue)) {
		return nil, bad
	}

	col, err := app.FindCachedCollectionByNameOrId(row.GetString("collectionRef"))
	if err != nil {
		return nil, bad
	}

	user, err := app.FindRecordById(col, row.GetString("recordRef"))
	if err != nil {
		return nil, bad
	}

	return user, nil
}

// SafeReturnTo validates a return target: a path of this server ("/..."), or an
// absolute URL on the app's own origin. It returns the normalized value, or ""
// when the target is not acceptable.
func SafeReturnTo(appURL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 1000 {
		return ""
	}

	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}

	if u.Scheme == "" && u.Host == "" {
		if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, "\\") {
			return ""
		}

		return raw
	}

	base, err := url.Parse(appURL)
	if err != nil || base.Host == "" || !strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Host, base.Host) || u.User != nil {
		return ""
	}

	return raw
}
