package oidc

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
	"github.com/zitadel/oidc/v3/pkg/oidc"
)

// Single sign-on sessions of the hosted sign-in page (#114, #94).
//
// After a sign-in on /oidc/login the browser gets an opaque cookie. Each
// signed-in account of the browser has one server-side session: 32 random
// bytes that carry nothing (no token, no id, no claim), whose SHA-256 is a row
// of _oauthSso naming the Claviger session (sid) the sign-in created. The
// cookie holds up to maxSSOAccounts of these values, joined with "~" (not a
// base64url character), so a browser can be signed in to several accounts at
// once (a person's work and personal account, a shared family device). A later
// /oidc/authorize from the same browser can then skip the credentials: the
// sign-in page finds the live accounts, checks that the Claviger session behind
// each is still alive, and either completes the request as if the user had
// just signed in, with the authentication time, acr and amr of the real
// sign-in, or, when there is a choice to make, shows the account chooser
// (chooser.go). Consent is still asked for any client that is not first party.
//
// An account leaves the browser's set when
//   - the Claviger session it is bound to is revoked or expires (the user
//     revoked it, a password change or a ban revoked it, an administrator did),
//     because it is re-checked on every use;
//   - the account is banned or deleted;
//   - end_session names it (id_token_hint), or a bare end_session / the
//     "sign out of all accounts" link of the chooser ends all of them, or the
//     chooser signs that one account out (the row and the Claviger session go,
//     the cookie is rewritten);
//   - it signs in again (the new session replaces the old row);
//   - a sixth account signs in (the oldest is dropped from the browser; its
//     Claviger session stays alive for the relying parties that hold it);
//   - its own lifetime of 12 hours since sign-in runs out. That is deliberately
//     shorter than a Claviger session: the cookie sits in a browser that may be
//     shared, while the long-lived token belongs to the relying party.
//
// A request with prompt=login, or with max_age older than a sign-in (max_age=0
// always), ignores that account's session and asks for credentials; that
// sign-in then replaces the session. prompt=select_account shows the chooser.
// prompt=none uses the session or fails with login_required,
// account_selection_required or consent_required, without showing a page.
//
// Nothing about the other accounts of the browser is ever sent to a relying
// party: the chooser page is same-origin, behind the HttpOnly cookie and the
// per-request browser binding, and the code, token and error redirects only
// carry the account that was chosen.
const (
	ssoCookieName = "claviger_oidc_sso"
	ssoLifetime   = 12 * time.Hour

	// maxSSOAccounts is how many accounts one browser keeps signed in.
	maxSSOAccounts = 5

	ssoSeparator = "~"
)

type ssoRow struct {
	Id            string         `db:"id"`
	CollectionRef string         `db:"collectionRef"`
	Subject       string         `db:"subject"`
	Sid           string         `db:"sid"`
	Amr           string         `db:"amr"`
	Acr           string         `db:"acr"`
	AuthTime      types.DateTime `db:"authTime"`
	Expires       types.DateTime `db:"expires"`
}

func withSSO(ctx context.Context, cookieValue string) context.Context {
	return context.WithValue(ctx, ctxKeySSO, cookieValue)
}

func ssoFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeySSO).(string)

	return v
}

// ssoValues splits the cookie into its session values (at most maxSSOAccounts,
// no duplicates, nothing implausibly long).
func ssoValues(raw string) []string {
	if raw == "" {
		return nil
	}

	var out []string

	for _, v := range strings.Split(raw, ssoSeparator) {
		if v == "" || len(v) > 200 || slices.Contains(out, v) {
			continue
		}

		out = append(out, v)
		if len(out) == maxSSOAccounts {
			break
		}
	}

	return out
}

// ssoRow loads the live session of one session value (nil: none).
func (s *Store) ssoRow(value string) *ssoRow {
	if value == "" || len(value) > 200 {
		return nil
	}

	var row ssoRow

	err := s.app.ConcurrentDB().NewQuery(
		"SELECT [[id]], [[collectionRef]], [[subject]], [[sid]], [[amr]], [[acr]], [[authTime]], [[expires]] FROM {{" +
			core.CollectionNameOAuthSso + "}} WHERE [[ssoHash]] = {:hash} AND [[expires]] > {:now} LIMIT 1",
	).Bind(dbx.Params{"hash": hashToken(value), "now": nowString()}).One(&row)
	if err != nil {
		return nil
	}

	return &row
}

// why an account of the browser cannot carry a request (empty: it can)
type ssoReason string

const (
	reasonNone       ssoReason = ""
	reasonCollection ssoReason = "collection" // another kind of account than the client's
	reasonOrg        ssoReason = "org"        // not a member of the organization the client belongs to
	reasonReauth     ssoReason = "reauth"     // the sign-in is older than max_age
)

// ssoEntry is one live account of the browser.
type ssoEntry struct {
	row     ssoRow
	value   string
	account *core.Record
	reason  ssoReason
}

func (e ssoEntry) eligible() bool { return e.reason == reasonNone }

// ssoEntries returns the live accounts of the browser, the most recent sign-in
// first, each marked with the reason it cannot be used for the client (if any).
// An account whose Claviger session ended, or that is banned or gone, is not
// live: it is left out and its row dropped. c may be nil (nothing is filtered).
func (s *Store) ssoEntries(raw string, c *Client, maxAge *uint) []ssoEntry {
	var out []ssoEntry

	var colID string

	if c != nil {
		if col, err := c.collection(); err == nil {
			colID = col.Id
		}
	}

	for _, value := range ssoValues(raw) {
		row := s.ssoRow(value)
		if row == nil {
			continue
		}

		account, err := s.app.FindRecordById(row.CollectionRef, row.Subject)
		if err != nil || core.IsBanned(s.app, account) || core.ValidateSession(s.app, row.Sid, row.CollectionRef, row.Subject) != nil {
			s.ssoDrop(value)

			continue
		}

		e := ssoEntry{row: *row, value: value, account: account}

		switch {
		case c != nil && (colID == "" || row.CollectionRef != colID):
			e.reason = reasonCollection
		case c != nil && s.orgBoundOK(c, account) != nil:
			e.reason = reasonOrg
		case maxAge != nil && (*maxAge == 0 || time.Since(row.AuthTime.Time()) > time.Duration(*maxAge)*time.Second):
			e.reason = reasonReauth
		}

		out = append(out, e)
	}

	slices.Reverse(out)

	return out
}

// ssoDrop deletes the row of a session value.
func (s *Store) ssoDrop(value string) {
	if value == "" {
		return
	}

	if _, err := exec(s.app, "DELETE FROM {{"+core.CollectionNameOAuthSso+"}} WHERE [[ssoHash]] = {:hash}", dbx.Params{"hash": hashToken(value)}); err != nil {
		s.app.Logger().Debug("Failed to delete the OpenID Connect single sign-on session", "error", err)
	}
}

func filterEligible(entries []ssoEntry) []ssoEntry {
	var out []ssoEntry

	for _, e := range entries {
		if e.eligible() {
			out = append(out, e)
		}
	}

	return out
}

// hintMatches returns the entries a login_hint names: the email, the username
// or the record id of the account (case insensitive).
func hintMatches(entries []ssoEntry, hint string) []ssoEntry {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return nil
	}

	var out []ssoEntry

	for _, e := range entries {
		if strings.EqualFold(e.account.Email(), hint) || strings.EqualFold(e.account.Id, hint) ||
			(e.account.Collection().Fields.GetByName("username") != nil && strings.EqualFold(e.account.GetString("username"), hint)) {
			out = append(out, e)
		}
	}

	return out
}

type ssoAction int

const (
	actLogin    ssoAction = iota // ask for credentials
	actContinue                  // use decision.entry without asking
	actChoose                    // show the account chooser
)

type ssoDecision struct {
	action ssoAction
	entry  *ssoEntry
	// preselect is the entry a login_hint named (the chooser highlights it)
	preselect string
}

type decideParams struct {
	prompt []string
	hint   string
	// forceLogin ignores the browser's accounts ("use another account")
	forceLogin bool
	// alwaysChoose never continues silently (the device flow: the user has to
	// confirm who approves)
	alwaysChoose bool
}

// decideSSO says what the sign-in page does with the browser's accounts.
func decideSSO(entries []ssoEntry, p decideParams) ssoDecision {
	if p.forceLogin || slices.Contains(p.prompt, oidc.PromptLogin) {
		return ssoDecision{action: actLogin}
	}

	eligible := filterEligible(entries)
	matched := hintMatches(eligible, p.hint)

	pre := ""
	if len(matched) > 0 {
		pre = matched[0].row.Id
	}

	if slices.Contains(p.prompt, oidc.PromptSelectAccount) {
		if len(entries) == 0 {
			return ssoDecision{action: actLogin}
		}

		return ssoDecision{action: actChoose, preselect: pre}
	}

	if strings.TrimSpace(p.hint) != "" && !p.alwaysChoose {
		if len(matched) == 1 {
			return ssoDecision{action: actContinue, entry: &matched[0]}
		}

		// the hinted account is not signed in here
		return ssoDecision{action: actLogin}
	}

	switch {
	case len(eligible) == 0:
		return ssoDecision{action: actLogin}
	case len(eligible) == 1 && !p.alwaysChoose:
		return ssoDecision{action: actContinue, entry: &eligible[0]}
	}

	return ssoDecision{action: actChoose, preselect: pre}
}

// decideNone is prompt=none: no page may be shown, so the request needs
// exactly one account that can carry it. It returns the account, or the OAuth
// error code the client gets.
func decideNone(entries []ssoEntry, hint string) (*ssoEntry, string) {
	eligible := filterEligible(entries)

	if strings.TrimSpace(hint) != "" {
		if matched := hintMatches(eligible, hint); len(matched) == 1 {
			return &matched[0], ""
		}
	} else if len(eligible) == 1 {
		return &eligible[0], ""
	}

	if len(eligible) > 1 {
		return nil, "account_selection_required"
	}

	return nil, "login_required"
}

// ssoStart records the sign-in that just completed as one of the browser's
// accounts and sets the cookie. The account's earlier session, if the browser
// had one, is replaced; the oldest account is dropped when the browser is full.
func (m *module) ssoStart(e *core.RequestEvent, collectionRef, subject, sid, acr string, amr []string, authTime time.Time) {
	var kept []string

	for _, v := range ssoValues(ssoCookieValue(e.Request)) {
		row := m.store.ssoRow(v)
		if row == nil {
			continue // gone: the cookie forgets it
		}

		if row.CollectionRef == collectionRef && row.Subject == subject {
			m.store.ssoDrop(v) // replaced by this sign-in

			continue
		}

		kept = append(kept, v)
	}

	for len(kept) >= maxSSOAccounts {
		m.store.ssoDrop(kept[0])
		kept = kept[1:]
	}

	value := randomToken(32)
	expires := time.Now().Add(ssoLifetime)

	_, err := m.app.NonconcurrentDB().Insert(core.CollectionNameOAuthSso, dbx.Params{
		"id":            core.GenerateDefaultRandomId(),
		"ssoHash":       hashToken(value),
		"collectionRef": collectionRef,
		"subject":       subject,
		"sid":           sid,
		"amr":           jsonList(amr),
		"acr":           acr,
		"authTime":      dateString(authTime),
		"expires":       dateString(expires),
		"created":       nowString(),
	}).Execute()
	if err != nil {
		// the sign-in itself succeeded; only the convenience is lost
		m.app.Logger().Warn("Failed to start the OpenID Connect single sign-on session", "error", err)

		return
	}

	m.setSSOCookie(e.Response, append(kept, value))
}

// setSSOCookie writes the cookie for the session values (none: clears it).
func (m *module) setSSOCookie(w http.ResponseWriter, values []string) {
	c := &http.Cookie{
		Name:     ssoCookieName,
		Path:     mountPath,
		HttpOnly: true,
		Secure:   m.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	}

	if len(values) == 0 {
		c.MaxAge = -1
	} else {
		c.Value = strings.Join(values, ssoSeparator)
		c.MaxAge = int(ssoLifetime.Seconds())
	}

	http.SetCookie(w, c)
}

// pruneSSOCookie rewrites the cookie to the sessions that still exist. It is
// called once the request that ended some of them has done its work.
func (m *module) pruneSSOCookie(w http.ResponseWriter, r *http.Request) {
	old := ssoValues(ssoCookieValue(r))

	var live []string

	for _, v := range old {
		if m.store.ssoRow(v) != nil {
			live = append(live, v)
		}
	}

	if len(live) != len(old) {
		m.setSSOCookie(w, live)
	}
}

// pruneWriter prunes the cookie just before the response headers go out.
type pruneWriter struct {
	http.ResponseWriter
	prune func()
	done  bool
}

func (p *pruneWriter) once() {
	if !p.done {
		p.done = true
		p.prune()
	}
}

func (p *pruneWriter) WriteHeader(code int) {
	p.once()
	p.ResponseWriter.WriteHeader(code)
}

func (p *pruneWriter) Write(b []byte) (int, error) {
	p.once()

	return p.ResponseWriter.Write(b)
}

func (p *pruneWriter) Unwrap() http.ResponseWriter { return p.ResponseWriter }

func errConsentRequired() *oidc.Error {
	return &oidc.Error{ErrorType: "consent_required", Description: "the user has not consented to this client yet"}
}

func errAccountSelectionRequired() *oidc.Error {
	return &oidc.Error{ErrorType: "account_selection_required", Description: "more than one account is signed in; the user has to choose one"}
}
