package apis

import (
	"errors"
	"net/http"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/security"
	"github.com/golang-jwt/jwt/v5"
)

// Fork-local (Claviger #93): the glue between the auth flows and the
// server-side sessions of core/session.go.
//
// The upstream-owned files call into this file at four places only:
//   - loadAuthToken:        checkSessionClaim  (per-request session check)
//   - recordAuthResponse:   checkNotBanned before the MFA step and
//     attachSession after it (create the session, add "sid" to the token)
//   - recordAuthRefresh:    carrySession (keep the sid, extend the session)
//   - password reset request: silentlyRefuseBanned

const (
	requestEventKeySessionId     = "__clavigerSessionId"
	requestEventKeySessionMethod = "__clavigerSessionMethod"
	requestEventKeySessionClaims = "__clavigerSessionClaims"
	requestEventKeySessionSkip   = "__clavigerSessionSkip"
)

// SessionIdFromRequest returns the id of the session behind the request's
// auth token, or "" for guests, API keys and tokens issued before sessions.
func SessionIdFromRequest(e *core.RequestEvent) string {
	sid, _ := e.Get(requestEventKeySessionId).(string)
	return sid
}

// checkSessionClaim rejects a (signature-verified) auth token whose session
// is gone, revoked or expired. Tokens without a sid are accepted unchanged:
// that is every token issued before this feature was deployed.
func checkSessionClaim(e *core.RequestEvent, token string, record *core.Record) error {
	claims, err := security.ParseUnverifiedJWT(token)
	if err != nil {
		return err
	}

	sid, _ := claims[core.TokenClaimSessionId].(string)
	if sid == "" {
		return nil
	}

	if err := core.ValidateSession(e.App, sid, record.Collection().Id, record.Id); err != nil {
		return err
	}

	e.Set(requestEventKeySessionId, sid)

	return nil
}

// checkNotBanned refuses the sign-in of a banned account with the same
// response as a failed credential check (the client is not told why).
// A superuser (impersonating) gets the actual reason.
func checkNotBanned(e *core.RequestEvent, authRecord *core.Record) error {
	if !core.IsBanned(e.App, authRecord) {
		return nil
	}

	if e.HasSuperuserAuth() {
		return e.BadRequestError("The account is banned.", nil)
	}

	return e.BadRequestError("Failed to authenticate.", errors.New("account is banned"))
}

// silentlyRefuseBanned answers like a request for an unknown address would
// (so a ban is not revealed) and reports whether it did.
func silentlyRefuseBanned(e *core.RequestEvent, record *core.Record) bool {
	if !core.IsBanned(e.App, record) {
		return false
	}

	_ = e.NoContent(http.StatusNoContent)

	return true
}

// skipSessionAttach makes recordAuthResponse leave the token as it is
// (a refresh that did not issue a new token).
func skipSessionAttach(e *core.RequestEvent) {
	e.Set(requestEventKeySessionSkip, true)
}

func sessionMethodFor(e *core.RequestEvent, record *core.Record, authMethod string) string {
	if v, _ := e.Get(requestEventKeySessionMethod).(string); v != "" {
		return v
	}

	// the second step of an MFA sign-in carries the id of the first one
	if record.Collection().MFA.Enabled && authMethod != "" {
		if info, err := e.RequestInfo(); err == nil {
			if id, _ := info.Body["mfaId"].(string); id != "" {
				return core.SessionAuthMethodMFA
			}
		}
		if e.Request.URL.Query().Get("mfaId") != "" {
			return core.SessionAuthMethodMFA
		}
	}

	return authMethod
}

// attachSession creates the session of a freshly authenticated token and
// returns the token re-signed with the sid claim (same expiration and claims).
// It is a no-op for tokens that are not auth tokens of the record or that
// already carry a session.
func attachSession(e *core.RequestEvent, record *core.Record, token, authMethod string) (string, error) {
	if skip, _ := e.Get(requestEventKeySessionSkip).(bool); skip {
		return token, nil
	}

	claims, err := security.ParseUnverifiedJWT(token)
	if err != nil {
		return token, nil // a custom (non-JWT) token set by a hook: not ours
	}

	if t, _ := claims[core.TokenClaimType].(string); t != core.TokenTypeAuth {
		return token, nil
	}
	if id, _ := claims[core.TokenClaimId].(string); id != record.Id {
		return token, nil
	}
	if sid, _ := claims[core.TokenClaimSessionId].(string); sid != "" {
		return token, nil
	}

	exp, ok := claims["exp"].(float64)
	if !ok {
		return token, nil
	}

	method := sessionMethodFor(e, record, authMethod)
	if method == "" {
		method = core.SessionAuthMethodOther
	}

	sid, err := core.CreateSession(e.App, record, method, e.Request.UserAgent(), e.RealIP(), time.Unix(int64(exp), 0))
	if err != nil {
		return "", e.InternalServerError("Failed to create the session.", err)
	}

	extra := jwt.MapClaims{core.TokenClaimSessionId: sid}
	if v, ok := e.Get(requestEventKeySessionClaims).(map[string]any); ok {
		for k, val := range v {
			extra[k] = val
		}
	}

	resigned, err := resignAuthToken(record, claims, extra)
	if err != nil {
		return "", e.InternalServerError("Failed to create auth token.", err)
	}

	return resigned, nil
}

// carrySession moves the session of the current request token to the token
// that replaces it (auth-refresh, passkey step-up). With extend the session
// expiration follows the new token (a refresh); without it only the id moves.
//
// A request token without a session returns newToken unchanged; for a
// refresh that lets recordAuthResponse start a session, which is how a
// pre-deploy token migrates.
func carrySession(e *core.RequestEvent, record *core.Record, oldToken, newToken string, extend bool) (string, error) {
	oldClaims, err := security.ParseUnverifiedJWT(oldToken)
	if err != nil {
		return newToken, nil
	}

	sid, _ := oldClaims[core.TokenClaimSessionId].(string)
	if sid == "" || newToken == oldToken {
		return newToken, nil
	}

	newClaims, err := security.ParseUnverifiedJWT(newToken)
	if err != nil {
		return newToken, nil
	}

	resigned, err := resignAuthToken(record, newClaims, jwt.MapClaims{core.TokenClaimSessionId: sid})
	if err != nil {
		return "", e.InternalServerError("Failed to refresh auth token.", err)
	}

	if extend {
		if exp, ok := newClaims["exp"].(float64); ok {
			if err := core.SetSessionExpires(e.App, sid, time.Unix(int64(exp), 0)); err != nil {
				return "", e.InternalServerError("Failed to extend the session.", err)
			}
		}
	}

	return resigned, nil
}

// resignAuthToken signs claims+extra with the key of the record. The
// original exp claim is preserved (it is part of claims).
func resignAuthToken(record *core.Record, claims, extra jwt.MapClaims) (string, error) {
	key := record.TokenKey() + record.Collection().AuthToken.Secret
	if key == "" {
		return "", core.ErrMissingSigningKey
	}

	merged := jwt.MapClaims{}
	for k, v := range claims {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}

	return security.NewJWT(merged, key, 0)
}
