package apis

import (
	"encoding/json"
	"net/http"
	"time"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/security"
	"github.com/go-webauthn/webauthn/protocol"
)

// Passkey step-up
// ---------------
//
// Some actions (removing a passkey, and anything an operator chooses to gate)
// should not run on the strength of a bearer token alone: a stolen or
// left-open session would be enough. Step-up requires that the session also
// shows a passkey assertion made recently.
//
// Design: the time of the last passkey assertion rides in the auth token as
// the signed "passkeyAt" claim (unix seconds).
//
//   - Auth tokens are already per-collection JWTs signed with
//     record.tokenKey + collection.authToken.secret, so the claim is
//     tamper-proof, is revoked by the same token-key rotation that revokes the
//     session (password change, passkey reset), and needs no new server state.
//   - A server-side store was rejected: it would be lost on restart (the
//     reference deployment restarts on every rollout), would need its own
//     eviction, and could not be seen by stateless JWT consumers.
//   - Token refresh (auth-refresh) mints a token WITHOUT the claim, on purpose:
//     refreshing proves nothing about the user, so freshness never extends by
//     refreshing. A client simply steps up again when a gated route asks.
//   - A passkey sign-in token carries the claim, so the session that just
//     authenticated with a passkey is fresh without a second prompt.
//
// The step-up ceremony (POST .../auth-with-webauthn/step-up-begin and
// .../step-up-finish) is a normal passkey assertion for the already signed-in
// user, restricted to that user's own credentials; on success it returns a
// replacement token carrying a new passkeyAt.

const (
	// webauthnStepUpMaxAge is how recent a passkey assertion must be to
	// delete a passkey: 5 minutes. Long enough to rename a few devices or
	// delete several in one sitting after a single prompt, short enough that
	// a session left open on a shared computer does not stay privileged.
	webauthnStepUpMaxAge = 5 * time.Minute

	// StepUpRequiredCode is the machine-readable code in the 403 response data
	// when a route requires a fresh passkey assertion.
	StepUpRequiredCode = "passkey_step_up_required"

	DefaultRequireFreshPasskeyMiddlewareId = "pbRequireFreshPasskey"

	webauthnStepUpRateRule = "@pb_webauthn_stepup_"

	// tolerated clock skew for a claim that is slightly in the future
	webauthnStepUpSkew = 30 * time.Second
)

// PasskeyAssertedAt returns the time of the passkey assertion recorded in the
// request's auth token, or the zero time when the token does not carry a valid
// one (no auth, password/OAuth2/OTP sign-in, a refreshed token, ...).
//
// The token is re-verified against the authenticated record's own signing key
// and must belong to that record, so a claim is only honoured when it was
// signed for the auth context the request actually runs as.
func PasskeyAssertedAt(e *core.RequestEvent) time.Time {
	if e.Auth == nil {
		return time.Time{}
	}

	token := getAuthTokenFromRequest(e)
	if token == "" {
		return time.Time{}
	}

	key := e.Auth.TokenKey() + e.Auth.Collection().AuthToken.Secret
	claims, err := security.ParseJWT(token, key)
	if err != nil {
		return time.Time{}
	}

	if id, _ := claims[core.TokenClaimId].(string); id != e.Auth.Id {
		return time.Time{}
	}
	if typ, _ := claims[core.TokenClaimType].(string); typ != core.TokenTypeAuth {
		return time.Time{}
	}

	at, ok := claims[core.TokenClaimPasskeyAt].(float64)
	if !ok || at <= 0 {
		return time.Time{}
	}

	return time.Unix(int64(at), 0)
}

// HasFreshPasskey reports whether the request's auth token carries a passkey
// assertion made within maxAge. Use it from Go route handlers and hooks when a
// middleware is not a fit.
func HasFreshPasskey(e *core.RequestEvent, maxAge time.Duration) bool {
	at := PasskeyAssertedAt(e)
	if at.IsZero() {
		return false
	}

	age := time.Since(at)

	return age <= maxAge && age >= -webauthnStepUpSkew
}

// RequireFreshPasskey returns a middleware that lets the request through only
// when the signed-in user made a passkey assertion within maxAge. Otherwise it
// answers 403 with data.passkey.code == "passkey_step_up_required", so a client can run the step-up ceremony and retry. It
// answers 401 when there is no auth at all.
//
// Go usage:
//
//	se.Router.DELETE("/api/myapp/danger", handler).Bind(
//		apis.RequireAuth("users"),
//		apis.RequireFreshPasskey(5*time.Minute),
//	)
//
// JS hooks (pb_hooks) cannot import this, but can enforce the same rule with
// existing bindings, because the claim is an ordinary signed JWT claim:
//
//	routerAdd("DELETE", "/api/myapp/danger", (e) => {
//	  const rec = e.auth
//	  const token = e.request.header.get("Authorization").replace(/^Bearer /i, "")
//	  let claims
//	  try {
//	    claims = $security.parseJWT(token, rec.tokenKey() + rec.collection().authToken.secret)
//	  } catch (_) { throw new ForbiddenError("passkey_step_up_required") }
//	  const age = Date.now() / 1000 - (claims.passkeyAt || 0)
//	  if (!claims.passkeyAt || age > 300 || age < -30) {
//	    throw new ForbiddenError("passkey_step_up_required")
//	  }
//	  // ... fresh passkey, proceed
//	}, $apis.requireAuth("users"))
func RequireFreshPasskey(maxAge time.Duration) *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id: DefaultRequireFreshPasskeyMiddlewareId,
		Func: func(e *core.RequestEvent) error {
			if e.Auth == nil {
				return e.UnauthorizedError("The request requires valid record authorization token.", nil)
			}

			if !HasFreshPasskey(e, maxAge) {
				return e.ForbiddenError("A recent passkey verification is required for this action.", validation.Errors{
					"passkey": validation.NewError(StepUpRequiredCode, "A recent passkey verification is required."),
				})
			}

			return e.Next()
		},
	}
}

// recordWebAuthnStepUpBegin starts a passkey assertion for the signed-in user.
//
// It is authenticated, so there is no enumeration concern and no decoy: the
// allowed credentials are the caller's own. The session is bound to the caller
// and to the step-up purpose.
func recordWebAuthnStepUpBegin(e *core.RequestEvent) error {
	collection, err := findAuthCollection(e)
	if err != nil {
		return err
	}

	if !collection.WebAuthn.Enabled {
		return e.ForbiddenError("The collection is not configured to allow WebAuthn authentication.", nil)
	}

	record := e.Auth
	if record == nil {
		return e.UnauthorizedError("Authentication is required.", nil)
	}

	if err := checkRateLimit(e, webauthnStepUpRateRule+record.Id, core.RateLimitRule{MaxRequests: 10, Duration: 180}); err != nil {
		return e.TooManyRequestsError("Too many requests, please try again later.", nil)
	}

	wa, err := initWebAuthn(e.App)
	if err != nil {
		return e.InternalServerError("Failed to initialize WebAuthn.", err)
	}

	creds, err := loadUserCredentials(e.App, record)
	if err != nil {
		return e.InternalServerError("Failed to load credentials.", err)
	}
	if len(creds) == 0 {
		return e.BadRequestError("No passkey is registered for this account.", nil)
	}

	options, session, err := beginWebAuthnLogin(wa, &webauthnUserAdapter{record: record, credentials: creds})
	if err != nil {
		return e.InternalServerError("Failed to begin WebAuthn step-up.", err)
	}

	token := storeWebAuthnSessionFor(e.App, session, record.Id, false, webauthnPurposeStepUp)

	return e.JSON(http.StatusOK, map[string]any{
		"options":      options,
		"sessionToken": token,
	})
}

type webauthnStepUpFinishForm struct {
	SessionToken string `form:"sessionToken" json:"sessionToken"`
}

func (form *webauthnStepUpFinishForm) validate() error {
	return validation.ValidateStruct(form,
		validation.Field(&form.SessionToken, validation.Required, validation.Length(1, 255)),
	)
}

// recordWebAuthnStepUpFinish verifies the assertion and returns a replacement
// auth token carrying a fresh passkeyAt. The previous token stays valid (JWTs
// cannot be recalled individually) but does not become fresh.
//
// The assertion goes through the same checks as a sign-in: single-use session,
// signature, origin, sign-count clone detection, and it updates
// signCount/lastUsed.
func recordWebAuthnStepUpFinish(e *core.RequestEvent) error {
	collection, err := findAuthCollection(e)
	if err != nil {
		return err
	}

	if !collection.WebAuthn.Enabled {
		return e.ForbiddenError("The collection is not configured to allow WebAuthn authentication.", nil)
	}

	record := e.Auth
	if record == nil {
		return e.UnauthorizedError("Authentication is required.", nil)
	}

	bodyBytes, err := readWebAuthnFinishBody(e)
	if err != nil {
		return err
	}

	form := &webauthnStepUpFinishForm{}
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, form); err != nil {
			return e.BadRequestError("An error occurred while loading the submitted data.", err)
		}
	}
	if err := form.validate(); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
	}

	entry, err := retrieveWebAuthnSession(e.App, form.SessionToken)
	if err != nil {
		return e.BadRequestError("Failed to verify the passkey.", nil)
	}
	defer deleteWebAuthnSession(e.App, form.SessionToken)

	if entry.Decoy || entry.Purpose != webauthnPurposeStepUp || !constantTimeIdentityCompare(entry.RecordId, record.Id) {
		return e.BadRequestError("Failed to verify the passkey.", nil)
	}

	wa, err := initWebAuthn(e.App)
	if err != nil {
		return e.InternalServerError("Failed to initialize WebAuthn.", err)
	}

	creds, err := loadUserCredentials(e.App, record)
	if err != nil {
		return e.InternalServerError("Failed to load credentials.", err)
	}

	parsed, err := protocol.ParseCredentialRequestResponseBytes(bodyBytes)
	if err != nil {
		return e.BadRequestError("Failed to verify the passkey.", nil)
	}

	cred, err := wa.ValidateLogin(&webauthnUserAdapter{record: record, credentials: creds}, *entry.Session, parsed)
	if err != nil || cred == nil {
		return e.BadRequestError("Failed to verify the passkey.", nil)
	}
	if cred.Authenticator.CloneWarning {
		e.App.Logger().Warn("webauthn_clone_warning",
			"recordId", record.Id,
			"collectionId", collection.Id,
			"reportedSignCount", cred.Authenticator.SignCount,
			"context", "step-up",
		)
		return e.BadRequestError("Failed to verify the passkey.", nil)
	}

	recordWebAuthnCredentialUse(e.App, collection, record, cred)

	// keep the non-refreshable property of a static token across the step-up
	refreshable := true
	if claims, err := security.ParseJWT(getAuthTokenFromRequest(e), record.TokenKey()+collection.AuthToken.Secret); err == nil {
		if v, ok := claims[core.TokenClaimRefreshable].(bool); ok {
			refreshable = v
		}
	}

	now := time.Now()
	token, err := record.NewPasskeyAuthToken(now, refreshable)
	if err != nil {
		return e.InternalServerError("Failed to create auth token.", err)
	}

	// the step-up keeps the session of the token it replaces (#93)
	token, err = carrySession(e, record, getAuthTokenFromRequest(e), token, refreshable)
	if err != nil {
		return err
	}

	e.App.Logger().Info("webauthn_step_up",
		"recordId", record.Id,
		"collectionId", collection.Id,
	)

	return e.JSON(http.StatusOK, map[string]any{
		"token":     token,
		"passkeyAt": now.Unix(),
		"maxAge":    int64(webauthnStepUpMaxAge / time.Second),
	})
}
