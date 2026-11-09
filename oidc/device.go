package oidc

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// The OAuth 2.0 device authorization grant (RFC 8628, #96), for CLIs, TVs,
// kiosks and anything else without a usable browser.
//
//	device                          browser
//	  | POST /oidc/device_authorization  (client_id [+ secret], scope)
//	  |<- device_code, user_code XXXX-XXXX, verification_uri[_complete], expires_in, interval
//	  |                                  | GET /oidc/device[?user_code=...]  enter the code
//	  |                                  | sign in (password, passkey, TOTP, captcha: the hosted
//	  |                                  |   sign-in page) or pick a signed-in account
//	  |                                  | see the client and scopes, Approve or Deny
//	  | POST /oidc/oauth/token grant_type=urn:ietf:params:oauth:grant-type:device_code
//	  |<- authorization_pending | slow_down | access_denied | expired_token | tokens
//
// The protocol endpoints come from the library (op.DeviceAuthorization, token
// endpoint dispatch); this file is the storage behind them (StoreDeviceAuthorization,
// GetDeviceAuthorizatonState) and the hosted verification pages.
//
// Storage. A device authorization is a row of _oauthDeviceCodes, not of
// _oauthRequests: it is looked up by two secrets (the device_code, polled by the
// device, and the user_code, typed by a person), it carries poll state
// (lastPoll, pollInterval) and its lifecycle (pending, authenticated, approved,
// denied, consumed) has no authorize-request counterpart. Both codes are stored
// only as SHA-256. The user_code is hashed in its normalised form (upper case,
// no dash), so what the user types matches however they type it.
//
// Safety.
//   - Always a deliberate approval. Even a first-party client, and even when
//     the browser is signed in, shows the client name and scopes and waits for
//     Approve, because a device code can be phished ("enter this code to see
//     your photos"). The page says so.
//   - The browser that looks the code up is bound to the row by a cookie, as
//     for the authorize pages; a second browser cannot take over a row.
//   - Code entry is rate limited per IP, failures much harder (ratelimit.go), and
//     user codes are 8 consonants (20^8, about 2.6e10) that live 10 minutes.
//   - Polling faster than the interval gets slow_down, and the interval grows by
//     5 seconds each time. The approved row is consumed by the first poll that
//     sees it, so a device_code yields tokens once.
//   - Tokens carry the sid and acr of the real sign-in, like every other grant,
//     so revoking the Claviger session ends them.
//
// Every user-visible string of the pages is in deviceText (Go) or STRINGS at the
// top of deviceJS.

const (
	deviceStatusPending       = "pending"       // created by the device, waiting for a person
	deviceStatusAuthenticated = "authenticated" // a person signed in, waiting for Approve / Deny
	deviceStatusApproved      = "approved"
	deviceStatusDenied        = "denied"
	deviceStatusConsumed      = "consumed" // the tokens were issued

	maxPollInterval = 60 * time.Second
)

// Timings. They are variables only so the documentation can name them in one
// place; nothing changes them at runtime.
var (
	deviceLifetime     = 10 * time.Minute
	devicePollInterval = 5 * time.Second
	deviceSlowDownStep = 5 * time.Second
)

var deviceText = struct {
	Title, Heading, Sub, CodeLabel, Continue                    string
	ApproveTitle, ApproveHeading, Wants, Warning, Approve, Deny string
	BadCode, InUse, TooMany                                     string
	Approved, ApprovedDetail, Denied, DeniedDetail              string
	Used, Expired, Unavailable, Browser                         string
}{
	Title:          "Connect a device",
	Heading:        "Connect a device",
	Sub:            "Enter the code shown on your device.",
	CodeLabel:      "Code",
	Continue:       "Continue",
	ApproveTitle:   "Allow access",
	ApproveHeading: "Allow this device?",
	Wants:          "on your device wants to access your account",
	Warning:        "Only continue if you started this on a device you are using right now. If someone else gave you this code, choose Cancel.",
	Approve:        "Allow",
	Deny:           "Cancel",
	BadCode:        "That code was not recognised. Check it and try again.",
	InUse:          "That code is already being used in another browser. Start again on your device.",
	TooMany:        "Too many attempts. Wait a few minutes and try again.",
	Approved:       "Device connected",
	ApprovedDetail: "You can return to your device now.",
	Denied:         "Request cancelled",
	DeniedDetail:   "The device was not given access.",
	Used:           "This code has already been used.",
	Expired:        "This code has expired. Start again on your device.",
	Unavailable:    "This application is not available.",
	Browser:        "This request was started in another browser. Start again on your device.",
}

// ---- storage -------------------------------------------------------------

type deviceRow struct {
	Id            string         `db:"id"`
	DeviceHash    string         `db:"deviceHash"`
	UserCodeHash  string         `db:"userCodeHash"`
	ClientId      string         `db:"clientId"`
	Scopes        string         `db:"scopes"`
	Status        string         `db:"status"`
	BindingHash   string         `db:"bindingHash"`
	CollectionRef string         `db:"collectionRef"`
	Subject       string         `db:"subject"`
	Sid           string         `db:"sid"`
	Amr           string         `db:"amr"`
	Acr           string         `db:"acr"`
	AuthTime      types.DateTime `db:"authTime"`
	FamilyId      string         `db:"familyId"`
	LastPoll      types.DateTime `db:"lastPoll"`
	PollInterval  float64        `db:"pollInterval"` // milliseconds
	Expires       types.DateTime `db:"expires"`
}

const deviceColumns = "[[id]], [[deviceHash]], [[userCodeHash]], [[clientId]], [[scopes]], [[status]], [[bindingHash]], [[collectionRef]], " +
	"[[subject]], [[sid]], [[amr]], [[acr]], [[authTime]], [[familyId]], [[lastPoll]], [[pollInterval]], [[expires]]"

var _ op.DeviceAuthorizationStorage = (*Store)(nil)

// normalizeUserCode is the canonical form of what a person typed: upper case
// letters and digits only ("bcdf ghjk" and "BCDF-GHJK" are the same code).
func normalizeUserCode(code string) string {
	var b strings.Builder

	for _, r := range strings.ToUpper(code) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}

	return b.String()
}

// validUserCode reports whether the normalised code can be one we issued
// (8 characters of the library's alphabet).
func validUserCode(norm string) bool {
	if len(norm) != op.UserCodeBase20.CharAmount {
		return false
	}

	for _, r := range norm {
		if !strings.ContainsRune(op.CharSetBase20, r) {
			return false
		}
	}

	return true
}

func formatUserCode(norm string) string {
	if len(norm) == 8 {
		return norm[:4] + "-" + norm[4:]
	}

	return norm
}

// StoreDeviceAuthorization implements op.DeviceAuthorizationStorage. Scopes the
// client may not use are dropped; openid is always part of a device grant.
func (s *Store) StoreDeviceAuthorization(_ context.Context, clientID, deviceCode, userCode string, expires time.Time, scopes []string) error {
	c, err := s.findClient(clientID)
	if err != nil || !c.AllowDeviceFlow {
		return errors.New("the client may not use the device authorization grant")
	}

	if _, err := c.collection(); err != nil {
		return err
	}

	granted := []string{oidc.ScopeOpenID}

	for _, sc := range scopes {
		if sc != oidc.ScopeOpenID && c.allows(sc) && !contains(granted, sc) {
			granted = append(granted, sc)
		}
	}

	userHash := hashToken(normalizeUserCode(userCode))

	s.purgeExpired()

	// a stale row of the same code is no reason to refuse a new one
	if _, err := exec(s.app, "DELETE FROM {{"+core.CollectionNameOAuthDeviceCodes+"}} WHERE [[userCodeHash]] = {:h} AND [[expires]] < {:now}",
		dbx.Params{"h": userHash, "now": nowString()}); err != nil {
		return err
	}

	_, err = s.app.NonconcurrentDB().Insert(core.CollectionNameOAuthDeviceCodes, dbx.Params{
		"id":           core.GenerateDefaultRandomId(),
		"deviceHash":   hashToken(deviceCode),
		"userCodeHash": userHash,
		"clientId":     c.ClientID,
		"scopes":       jsonList(granted),
		"status":       deviceStatusPending,
		"pollInterval": float64(devicePollInterval / time.Millisecond),
		"expires":      dateString(expires),
		"created":      nowString(),
	}).Execute()
	if err != nil {
		var n int
		if qerr := s.app.ConcurrentDB().NewQuery("SELECT COUNT(*) FROM {{" + core.CollectionNameOAuthDeviceCodes + "}} WHERE [[userCodeHash]] = {:h}").
			Bind(dbx.Params{"h": userHash}).Row(&n); qerr == nil && n > 0 {
			return op.ErrDuplicateUserCode
		}

		return err
	}

	return nil
}

func (s *Store) deviceByDeviceHash(hash string) (*deviceRow, error) {
	var row deviceRow

	err := s.app.ConcurrentDB().NewQuery("SELECT " + deviceColumns + " FROM {{" + core.CollectionNameOAuthDeviceCodes + "}} WHERE [[deviceHash]] = {:h} LIMIT 1").
		Bind(dbx.Params{"h": hash}).One(&row)
	if err != nil {
		return nil, errNotFound
	}

	return &row, nil
}

// deviceByUserCode loads the live authorization a normalised user code names.
func (s *Store) deviceByUserCode(norm string) (*deviceRow, error) {
	var row deviceRow

	err := s.app.ConcurrentDB().NewQuery(
		"SELECT " + deviceColumns + " FROM {{" + core.CollectionNameOAuthDeviceCodes + "}} WHERE [[userCodeHash]] = {:h} AND [[expires]] > {:now} LIMIT 1",
	).Bind(dbx.Params{"h": hashToken(norm), "now": nowString()}).One(&row)
	if err != nil {
		return nil, errNotFound
	}

	return &row, nil
}

// deviceByRef loads the live authorization by its row id (the page reference).
func (s *Store) deviceByRef(ref string) (*deviceRow, error) {
	if ref == "" || len(ref) > 40 {
		return nil, errNotFound
	}

	var row deviceRow

	err := s.app.ConcurrentDB().NewQuery(
		"SELECT " + deviceColumns + " FROM {{" + core.CollectionNameOAuthDeviceCodes + "}} WHERE [[id]] = {:id} AND [[expires]] > {:now} LIMIT 1",
	).Bind(dbx.Params{"id": ref, "now": nowString()}).One(&row)
	if err != nil {
		return nil, errNotFound
	}

	return &row, nil
}

// deviceExtra is what the library's DeviceAuthorizationState cannot carry.
type deviceExtra struct {
	collection, sid, acr, family string
	at                           time.Time
}

// The library hands the state it got from GetDeviceAuthorizatonState (a
// pointer) to the token creation code unchanged, so the pointer is the key that
// reconnects the two. Entries live a minute: long enough for one token response.
var deviceExtras = struct {
	sync.Mutex
	m map[*op.DeviceAuthorizationState]deviceExtra
}{m: map[*op.DeviceAuthorizationState]deviceExtra{}}

func rememberDeviceExtra(st *op.DeviceAuthorizationState, x deviceExtra) {
	deviceExtras.Lock()
	defer deviceExtras.Unlock()

	now := time.Now()
	x.at = now

	for k, v := range deviceExtras.m {
		if now.Sub(v.at) > time.Minute {
			delete(deviceExtras.m, k)
		}
	}

	deviceExtras.m[st] = x
}

func deviceExtraOf(st *op.DeviceAuthorizationState) (deviceExtra, bool) {
	deviceExtras.Lock()
	defer deviceExtras.Unlock()

	x, ok := deviceExtras.m[st]

	return x, ok
}

// GetDeviceAuthorizatonState implements op.DeviceAuthorizationStorage (the
// misspelling is the library's). It is the polling endpoint's brain:
// context.DeadlineExceeded means slow_down, an error access_denied, and the
// state says pending, denied, expired or done.
func (s *Store) GetDeviceAuthorizatonState(_ context.Context, clientID, deviceCode string) (*op.DeviceAuthorizationState, error) {
	row, err := s.deviceByDeviceHash(hashToken(deviceCode))
	if err != nil || row.ClientId != clientID {
		return nil, errNotFound
	}

	c, err := s.findClient(clientID)
	if err != nil || !c.AllowDeviceFlow {
		return nil, errors.New("the client may not use the device authorization grant")
	}

	// polling faster than the interval: slow down, and make the interval longer
	interval := time.Duration(row.PollInterval) * time.Millisecond
	if interval <= 0 {
		interval = devicePollInterval
	}

	n, err := exec(s.app,
		"UPDATE {{"+core.CollectionNameOAuthDeviceCodes+"}} SET [[lastPoll]] = {:now} WHERE [[id]] = {:id} AND ([[lastPoll]] = '' OR [[lastPoll]] <= {:before})",
		dbx.Params{"now": nowString(), "id": row.Id, "before": dateString(time.Now().Add(-interval))})
	if err != nil {
		return nil, err
	}

	if n == 0 {
		next := min(interval+deviceSlowDownStep, maxPollInterval)

		_, _ = exec(s.app, "UPDATE {{"+core.CollectionNameOAuthDeviceCodes+"}} SET [[lastPoll]] = {:now}, [[pollInterval]] = {:iv} WHERE [[id]] = {:id}",
			dbx.Params{"now": nowString(), "iv": float64(next / time.Millisecond), "id": row.Id})

		return nil, context.DeadlineExceeded
	}

	state := &op.DeviceAuthorizationState{
		ClientID: row.ClientId, Scopes: parseList(row.Scopes), Expires: row.Expires.Time(),
	}

	expired := !row.Expires.Time().After(time.Now())

	switch row.Status {
	case deviceStatusDenied:
		state.Denied = true
	case deviceStatusApproved:
		if expired {
			return state, nil
		}

		// the account must still be good: the session alive, not banned, still a
		// member of the client's organization
		if err := s.accountAlive(row.Sid, row.CollectionRef, row.Subject); err != nil || s.clientOrgStillOK(row.ClientId, row.CollectionRef, row.Subject) != nil {
			state.Denied = true

			return state, nil
		}

		// the first poll that sees the approval takes it: one device_code, one set of tokens
		took, err := exec(s.app,
			"UPDATE {{"+core.CollectionNameOAuthDeviceCodes+"}} SET [[status]] = {:consumed} WHERE [[id]] = {:id} AND [[status]] = {:approved}",
			dbx.Params{"consumed": deviceStatusConsumed, "approved": deviceStatusApproved, "id": row.Id})
		if err != nil {
			return nil, err
		}

		if took != 1 {
			state.Expires = time.Now().Add(-time.Second)

			return state, nil
		}

		state.Done = true
		state.Subject = row.Subject
		state.AMR = parseList(row.Amr)
		state.AuthTime = row.AuthTime.Time()
		state.Audience = []string{row.ClientId}
		rememberDeviceExtra(state, deviceExtra{collection: row.CollectionRef, sid: row.Sid, acr: row.Acr, family: row.FamilyId})
	case deviceStatusConsumed:
		state.Expires = time.Now().Add(-time.Second) // the code was used: it reads as expired
	}

	return state, nil
}

// ---- pages ---------------------------------------------------------------

func deviceVerifyURL(ref string) string {
	return mountPath + "/device/verify?ref=" + url.QueryEscape(ref)
}

func deviceBindingCookie(ref string) string { return "claviger_oidc_dev_" + ref }

func (m *module) checkDeviceBinding(e *core.RequestEvent, row *deviceRow) bool {
	if row.BindingHash == "" {
		return false
	}

	c, err := e.Request.Cookie(deviceBindingCookie(row.Id))
	if err != nil || c.Value == "" {
		return false
	}

	return hashEqual(hashToken(c.Value), row.BindingHash)
}

// bindDevice attaches the browser to the row on the first lookup of its code.
func (m *module) bindDevice(e *core.RequestEvent, row *deviceRow) bool {
	if row.BindingHash != "" {
		return m.checkDeviceBinding(e, row)
	}

	secret := randomToken(24)

	n, err := exec(m.app,
		"UPDATE {{"+core.CollectionNameOAuthDeviceCodes+"}} SET [[bindingHash]] = {:h} WHERE [[id]] = {:id} AND [[bindingHash]] = ''",
		dbx.Params{"h": hashToken(secret), "id": row.Id})
	if err != nil || n != 1 {
		return false
	}

	http.SetCookie(e.Response, &http.Cookie{
		Name:     deviceBindingCookie(row.Id),
		Value:    secret,
		Path:     mountPath,
		MaxAge:   int(deviceLifetime.Seconds()),
		HttpOnly: true,
		Secure:   m.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})

	return true
}

func (m *module) deviceCodePage(e *core.RequestEvent, status int, userCode, message string) error {
	d := extData{
		Page: "device-code", Title: deviceText.Title, Heading: deviceText.Heading, T: deviceText,
		Action: mountPath + "/device", UserCode: userCode, Script: "",
	}

	if message != "" {
		d.Message, d.MessageClass = message, "err"
	}

	return m.renderExt(e, status, d)
}

func (m *module) deviceResultPage(e *core.RequestEvent, status int, heading, message, class string) error {
	return m.renderExt(e, status, extData{
		Page: "device-result", Title: heading, Heading: heading, Message: message, MessageClass: class,
	})
}

// deviceEntryPage is GET /oidc/device: where the person types the code.
func (m *module) deviceEntryPage(e *core.RequestEvent) error {
	code := formatUserCode(normalizeUserCode(e.Request.URL.Query().Get("user_code")))
	if len(code) > 20 {
		code = ""
	}

	return m.deviceCodePage(e, http.StatusOK, code, "")
}

// deviceLookup is POST /oidc/device: the code was typed. A known, live code
// binds this browser to its authorization and moves on to the sign-in.
func (m *module) deviceLookup(e *core.RequestEvent) error {
	e.Request.Body = http.MaxBytesReader(e.Response, e.Request.Body, 4<<10)

	if err := e.Request.ParseForm(); err != nil {
		return m.deviceCodePage(e, http.StatusBadRequest, "", deviceText.BadCode)
	}

	typed := e.Request.PostForm.Get("user_code")
	norm := normalizeUserCode(typed)
	shown := formatUserCode(norm)

	if len(shown) > 20 {
		shown = ""
	}

	// nothing to protect, and no counters written, until a client allows the flow
	if m.clientCount() == 0 {
		return m.deviceCodePage(e, http.StatusBadRequest, shown, deviceText.BadCode)
	}

	set := m.limiters()
	ip := e.RealIP()

	if ip != "" {
		if blocked, retry := set.deviceFail.Blocked(ip); blocked {
			m.app.Logger().Debug("OpenID Connect device code entry refused", "reason", "too many wrong codes", "ip", ip)
			m.retryAfter(e, retry)

			return m.deviceCodePage(e, http.StatusTooManyRequests, "", deviceText.TooMany)
		}

		if ok, retry := set.deviceEntry.Allow(ip); !ok {
			m.app.Logger().Info("OpenID Connect rate limit", "endpoint", "device", "budget", "per IP", "retryAfter", retry.Round(time.Second).String())
			m.retryAfter(e, retry)

			return m.deviceCodePage(e, http.StatusTooManyRequests, "", deviceText.TooMany)
		}
	}

	wrong := func() error {
		if ip != "" {
			set.deviceFail.Allow(ip)

			if blocked, _ := set.deviceFail.Blocked(ip); blocked {
				m.app.Logger().Warn("OpenID Connect device codes keep being guessed, refusing further attempts", "ip", ip)
			}
		}

		return m.deviceCodePage(e, http.StatusBadRequest, shown, deviceText.BadCode)
	}

	if !validUserCode(norm) {
		return wrong()
	}

	row, err := m.store.deviceByUserCode(norm)
	if err != nil || (row.Status != deviceStatusPending && row.Status != deviceStatusAuthenticated) {
		return wrong()
	}

	if !m.bindDevice(e, row) {
		return m.deviceCodePage(e, http.StatusForbidden, "", deviceText.InUse)
	}

	return e.Redirect(http.StatusSeeOther, deviceVerifyURL(row.Id))
}

func (m *module) retryAfter(e *core.RequestEvent, retry time.Duration) {
	secs := int(retry.Seconds() + 0.999)
	if secs < 1 {
		secs = 1
	}

	e.Response.Header().Set("Retry-After", strconv.Itoa(secs))
}

// deviceFlow loads the authorization behind a page reference and checks that
// this browser is the one that entered the code. A non-zero status is the
// error to answer with.
func (m *module) deviceFlow(e *core.RequestEvent, ref string) (*deviceRow, *Client, *core.Collection, int, string) {
	row, err := m.store.deviceByRef(ref)
	if err != nil {
		return nil, nil, nil, http.StatusBadRequest, deviceText.Expired
	}

	if !m.checkDeviceBinding(e, row) {
		return nil, nil, nil, http.StatusForbidden, deviceText.Browser
	}

	client, err := m.store.findClient(row.ClientId)
	if err != nil || !client.AllowDeviceFlow {
		return nil, nil, nil, http.StatusBadRequest, deviceText.Unavailable
	}

	col, err := client.collection()
	if err != nil {
		return nil, nil, nil, http.StatusBadRequest, deviceText.Unavailable
	}

	return row, client, col, 0, ""
}

// deviceVerify is GET /oidc/device/verify: sign in or choose an account, then
// approve or deny.
func (m *module) deviceVerify(e *core.RequestEvent) error {
	ref := e.Request.URL.Query().Get("ref")

	row, client, col, status, msg := m.deviceFlow(e, ref)
	if status != 0 {
		return m.deviceResultPage(e, status, deviceText.Title, msg, "err")
	}

	switch row.Status {
	case deviceStatusApproved, deviceStatusConsumed:
		return m.deviceResultPage(e, http.StatusOK, deviceText.Approved, deviceText.ApprovedDetail, "ok")
	case deviceStatusDenied:
		return m.deviceResultPage(e, http.StatusOK, deviceText.Denied, deviceText.DeniedDetail, "ok")
	case deviceStatusAuthenticated:
		return m.deviceApprovePage(e, row, client)
	}

	entries := m.store.ssoEntries(ssoCookieValue(e.Request), client, nil)

	dec := decideSSO(entries, decideParams{alwaysChoose: true, forceLogin: e.Request.URL.Query().Get("account") == "new"})
	if dec.action == actChoose {
		return m.renderChooser(e, chooserView{
			flow: flowDevice, ref: ref, client: client, entries: entries,
			other: deviceVerifyURL(ref) + "&account=new",
		})
	}

	return m.render(e, http.StatusOK, pageData{
		Title: "Sign in", Heading: "Sign in", Page: "login", RequestID: ref, Finish: mountPath + "/device/finish",
		Collection: col.Name, ClientName: client.Name, Script: mountPath + "/assets/login.js",
	})
}

func (m *module) deviceApprovePage(e *core.RequestEvent, row *deviceRow, client *Client) error {
	d := extData{
		Page: "device-approve", Title: deviceText.ApproveTitle, Heading: deviceText.ApproveHeading, Ref: row.Id,
		ClientName: client.Name, Script: mountPath + "/assets/device.js", T: deviceText, Action: mountPath + "/device/decision",
	}

	if account, err := m.app.FindRecordById(row.CollectionRef, row.Subject); err == nil {
		d.Account = account.Email()
		if d.Account == "" {
			d.Account = displayName(account)
		}
	}

	for _, sc := range parseList(row.Scopes) {
		info, ok := scopeDescriptions[sc]
		if !ok {
			info = scopeInfo{Label: sc, Detail: "Access requested by the application."}
		}

		d.Scopes = append(d.Scopes, info)
	}

	return m.renderExt(e, http.StatusOK, d)
}

// deviceAuthenticate records who signed in on the row and returns where the
// browser goes next (the approval page).
func (m *module) deviceAuthenticate(row *deviceRow, account *core.Record, a grantAuth) (string, error) {
	n, err := exec(m.app,
		"UPDATE {{"+core.CollectionNameOAuthDeviceCodes+"}} SET [[status]] = {:next}, [[collectionRef]] = {:col}, [[subject]] = {:sub}, "+
			"[[sid]] = {:sid}, [[amr]] = {:amr}, [[acr]] = {:acr}, [[authTime]] = {:at}, [[familyId]] = {:family} "+
			"WHERE [[id]] = {:id} AND [[status]] = {:pending} AND [[expires]] > {:now}",
		dbx.Params{
			"next": deviceStatusAuthenticated, "col": account.Collection().Id, "sub": account.Id, "sid": a.sid,
			"amr": jsonList(a.amr), "acr": a.acr, "at": dateString(a.authTime), "family": randomToken(18),
			"id": row.Id, "pending": deviceStatusPending, "now": nowString(),
		})
	if err != nil {
		return "", err
	}

	if n != 1 {
		return "", errRequestGone
	}

	return deviceVerifyURL(row.Id), nil
}

type deviceBody struct {
	Ref   string `json:"ref"`
	Allow bool   `json:"allow"`
}

// deviceFinish completes the sign-in of the verification page with the token
// the shared sign-in page obtained (password, passkey, TOTP...).
func (m *module) deviceFinish(e *core.RequestEvent) error {
	var body deviceBody
	if err := e.BindBody(&body); err != nil {
		return jsonError(e, http.StatusBadRequest, "Invalid request.")
	}

	row, client, col, status, msg := m.deviceFlow(e, body.Ref)
	if status != 0 {
		return jsonError(e, status, msg)
	}

	if row.Status != deviceStatusPending {
		return jsonError(e, http.StatusBadRequest, deviceText.Used)
	}

	auth, status, msg := m.freshSignIn(e, client, col)
	if status != 0 {
		return jsonError(e, status, msg)
	}

	target, err := m.deviceAuthenticate(row, e.Auth, auth)
	if err != nil {
		return jsonError(e, http.StatusBadRequest, deviceText.Expired)
	}

	// this sign-in is now one of the browser's single sign-on sessions
	m.ssoStart(e, col.Id, e.Auth.Id, auth.sid, auth.acr, auth.amr, auth.authTime)

	return e.JSON(http.StatusOK, map[string]any{"redirect": target})
}

// deviceDecision records Approve or Deny. Both are audited.
func (m *module) deviceDecision(e *core.RequestEvent) error {
	var body deviceBody
	if err := e.BindBody(&body); err != nil {
		return jsonError(e, http.StatusBadRequest, "Invalid request.")
	}

	row, _, _, status, msg := m.deviceFlow(e, body.Ref)
	if status != 0 {
		return jsonError(e, status, msg)
	}

	if row.Status != deviceStatusAuthenticated {
		return jsonError(e, http.StatusBadRequest, deviceText.Used)
	}

	account, err := m.app.FindRecordById(row.CollectionRef, row.Subject)
	if err != nil || m.store.accountAlive(row.Sid, row.CollectionRef, row.Subject) != nil || m.store.clientOrgStillOK(row.ClientId, row.CollectionRef, row.Subject) != nil {
		return jsonError(e, http.StatusUnauthorized, "That sign-in has ended. Start again on your device.")
	}

	next, action := deviceStatusDenied, "oidc_device_deny"
	if body.Allow {
		next, action = deviceStatusApproved, "oidc_device_approve"
	}

	n, err := exec(m.app,
		"UPDATE {{"+core.CollectionNameOAuthDeviceCodes+"}} SET [[status]] = {:next} WHERE [[id]] = {:id} AND [[status]] = {:authn} AND [[expires]] > {:now}",
		dbx.Params{"next": next, "id": row.Id, "authn": deviceStatusAuthenticated, "now": nowString()})
	if err != nil {
		return jsonError(e, http.StatusInternalServerError, "Failed to record the decision.")
	}

	if n != 1 {
		return jsonError(e, http.StatusBadRequest, deviceText.Expired)
	}

	m.store.auditAs(e.Request.Context(), action, account, e.RealIP(), map[string]any{
		"clientId": row.ClientId, "scopes": parseList(row.Scopes), "sid": row.Sid,
	})

	return e.JSON(http.StatusOK, map[string]any{"redirect": deviceVerifyURL(row.Id)})
}

// auditAs writes an audit entry whose actor is the end user.
func (s *Store) auditAs(ctx context.Context, action string, account *core.Record, ip string, meta map[string]any) {
	ctx = core.WithAuditContext(detach(ctx), core.NewAuditActorFromRecord(account), ip)

	err := core.WriteAuditEntry(s.app, ctx, &core.AuditEntry{
		Action:         action,
		Actor:          core.NewAuditActorFromRecord(account),
		IP:             ip,
		CollectionId:   account.Collection().Id,
		CollectionName: account.Collection().Name,
		RecordId:       account.Id,
		Meta:           meta,
	}, nil)
	if err != nil {
		s.app.Logger().Warn("Failed to write the OIDC audit entry", "action", action, "error", err)
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}

	return false
}

const deviceJS = `const STRINGS = {
  failed: "The decision could not be saved.",
};

const root = document.getElementById("app");
const ref = root.dataset.request;
const statusEl = root.querySelector("#status");
const buttons = root.querySelectorAll("button");

async function decide(allow) {
  buttons.forEach((b) => { b.disabled = true; });
  const res = await fetch("/oidc/device/decision", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ ref, allow }),
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok || !body.redirect) {
    statusEl.textContent = body.message || STRINGS.failed;
    statusEl.hidden = false;
    buttons.forEach((b) => { b.disabled = false; });
    return;
  }
  window.location.assign(body.redirect);
}

root.querySelector("#allow").addEventListener("click", () => decide(true));
root.querySelector("#deny").addEventListener("click", () => decide(false));
`

func (m *module) bindDeviceRoutes(se *core.ServeEvent) {
	se.Router.GET(mountPath+"/device", m.deviceEntryPage)
	se.Router.POST(mountPath+"/device", m.deviceLookup)
	se.Router.GET(mountPath+"/device/verify", m.deviceVerify)
	se.Router.POST(mountPath+"/device/finish", m.deviceFinish)
	se.Router.POST(mountPath+"/device/decision", m.deviceDecision)
	se.Router.POST(mountPath+"/account/select", m.accountSelect)
	se.Router.POST(mountPath+"/account/signout", m.accountSignout)
}

// deviceNeedsClientAuth reports whether the request is a device_code poll of a
// confidential client that sent no client credentials. The library looks at the
// state of the authorization before it checks the credentials, and the storage
// hands out (consumes) an approval when it sees one, so an anonymous caller
// holding a device_code could burn the real client's approval. Such a request
// is refused before the library runs.
func (m *module) deviceNeedsClientAuth(r *http.Request) bool {
	if r.Method != http.MethodPost || r.URL.Path != mountPath+"/oauth/token" || r.Body == nil {
		return false
	}

	body, err := readAndRestore(r)
	if err != nil {
		return false
	}

	form, err := url.ParseQuery(string(body))
	if err != nil || form.Get("grant_type") != string(oidc.GrantTypeDeviceCode) {
		return false
	}

	if _, _, ok := r.BasicAuth(); ok {
		return false // the library verifies them
	}

	c, err := m.store.findClient(form.Get("client_id"))

	return err == nil && !c.Public
}

func writeInvalidClient(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("WWW-Authenticate", `Basic realm="oidc", charset="UTF-8"`)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"confidential client requires authentication"}`))
}
