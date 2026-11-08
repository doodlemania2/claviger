package oidc

import (
	"context"
	"net/http"
	"slices"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
	"github.com/zitadel/oidc/v3/pkg/oidc"
)

// Single sign-on session of the hosted sign-in page (#114).
//
// After a sign-in on /oidc/login the browser gets an opaque cookie. The cookie
// is 32 random bytes and nothing else: no token, no id, no claim. The server
// maps its SHA-256 to a row of _oauthSso, which names the Claviger session
// (sid) the sign-in created. A later /oidc/authorize from the same browser can
// then skip the credentials: the sign-in page finds the row, checks that the
// Claviger session behind it is still alive, and completes the request as if the
// user had just signed in, with the authentication time, acr and amr of the real
// sign-in. Consent is still asked for any client that is not first party.
//
// The session ends when
//   - the Claviger session it is bound to is revoked or expires (the user
//     revoked it, a password change or a ban revoked it, an administrator did),
//     because it is re-checked on every use;
//   - the account is banned or deleted;
//   - end_session is called from the browser (the row and the Claviger session
//     go, the cookie is cleared);
//   - its own lifetime of 12 hours since sign-in runs out. That is deliberately
//     shorter than a Claviger session: the cookie sits in a browser that may be
//     shared, while the long-lived token belongs to the relying party.
//
// A request with prompt=login, with max_age older than the sign-in (max_age=0
// always), or with prompt=select_account (there is no account chooser: one
// session per browser) ignores the session and asks for credentials; that
// sign-in then replaces the session. prompt=none uses the session or fails with
// login_required (or consent_required), without showing a page.
const (
	ssoCookieName = "claviger_oidc_sso"
	ssoLifetime   = 12 * time.Hour
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

// ssoRow loads the live session of a cookie value (nil: none).
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

// ssoAccount returns the account of the session when the session can carry an
// authorize request for the client; otherwise nil and the reason.
func (s *Store) ssoAccount(value string, c *Client, maxAge *uint, prompt []string) (*core.Record, string) {
	row := s.ssoRow(value)
	if row == nil {
		return nil, "no session"
	}

	if slices.Contains(prompt, oidc.PromptLogin) || slices.Contains(prompt, oidc.PromptSelectAccount) {
		return nil, "re-authentication requested"
	}

	if maxAge != nil && (*maxAge == 0 || time.Since(row.AuthTime.Time()) > time.Duration(*maxAge)*time.Second) {
		return nil, "the sign-in is older than max_age"
	}

	col, err := c.collection()
	if err != nil || col.Id != row.CollectionRef {
		return nil, "the session belongs to another kind of account"
	}

	account, err := s.app.FindRecordById(row.CollectionRef, row.Subject)
	if err != nil || core.IsBanned(s.app, account) {
		return nil, "account unavailable"
	}

	if core.ValidateSession(s.app, row.Sid, row.CollectionRef, row.Subject) != nil {
		return nil, "the Claviger session ended"
	}

	return account, ""
}

// ssoStart records the sign-in that just completed as the browser's session
// and sets the cookie. A session the browser already had is dropped.
func (m *module) ssoStart(e *core.RequestEvent, collectionRef, subject, sid, acr string, amr []string, authTime time.Time) {
	if old, err := e.Request.Cookie(ssoCookieName); err == nil {
		m.ssoDelete(old.Value)
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

	http.SetCookie(e.Response, &http.Cookie{
		Name:     ssoCookieName,
		Value:    value,
		Path:     mountPath,
		MaxAge:   int(ssoLifetime.Seconds()),
		HttpOnly: true,
		Secure:   m.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (m *module) ssoDelete(value string) {
	if value == "" {
		return
	}

	if _, err := exec(m.app, "DELETE FROM {{"+core.CollectionNameOAuthSso+"}} WHERE [[ssoHash]] = {:hash}", dbx.Params{"hash": hashToken(value)}); err != nil {
		m.app.Logger().Debug("Failed to delete the OpenID Connect single sign-on session", "error", err)
	}
}

// clearSSOCookie tells the browser to forget the session.
func (m *module) clearSSOCookie(e *core.RequestEvent) {
	http.SetCookie(e.Response, &http.Cookie{
		Name:     ssoCookieName,
		Value:    "",
		Path:     mountPath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   m.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func errConsentRequired() *oidc.Error {
	return &oidc.Error{ErrorType: "consent_required", Description: "the user has not consented to this client yet"}
}
