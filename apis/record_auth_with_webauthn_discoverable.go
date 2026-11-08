package apis

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// webauthnRegistrationOptions returns the extra registration options for a
// collection. With the Discoverable option on, the authenticator is asked
// (not required, so older security keys still enroll) for a discoverable
// credential, which is what usernameless sign-in needs to find the account
// from the credential alone.
func webauthnRegistrationOptions(collection *core.Collection) []webauthn.RegistrationOption {
	if collection.WebAuthn.Discoverable {
		return []webauthn.RegistrationOption{
			webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred),
		}
	}

	return nil
}

// trimWebAuthnSessionsWithPurpose evicts the oldest sessions of the given
// purpose until fewer than max remain, leaving sessions of other purposes alone.
func trimWebAuthnSessionsWithPurpose(app core.App, purpose string, max int) {
	type kv struct {
		key string
		exp time.Time
	}

	var all []kv
	for k, v := range app.Store().GetAll() {
		if !strings.HasPrefix(k, webauthnSessionPrefix) {
			continue
		}
		entry, ok := v.(*webauthnSessionEntry)
		if !ok || entry == nil || entry.Purpose != purpose {
			continue
		}
		all = append(all, kv{key: k, exp: entry.ExpiresAt})
	}

	for len(all) >= max {
		oldest := 0
		for i := range all {
			if all[i].exp.Before(all[oldest].exp) {
				oldest = i
			}
		}
		app.Store().Remove(all[oldest].key)
		all = append(all[:oldest], all[oldest+1:]...)
	}
}

// recordWebAuthnLoginBeginDiscoverable starts a usernameless sign-in
// (WebAuthn "conditional mediation" / autofill). The request carries no
// identity and the options carry an empty allowCredentials, so the response is
// the same for every caller and cannot be used to learn which accounts exist.
//
// It spends from the same always-on per-IP budget as login-begin. Unlike the
// decoy responses of login-begin, a real challenge must be remembered for the
// finish step, so those sessions are stored; they are capped separately
// (webauthnDiscoverableMaxEntries) so anonymous callers can only evict other
// anonymous discoverable sessions.
func recordWebAuthnLoginBeginDiscoverable(e *core.RequestEvent) error {
	collection, err := findAuthCollection(e)
	if err != nil {
		return err
	}

	if !collection.WebAuthn.Enabled || !collection.WebAuthn.Discoverable {
		return e.ForbiddenError("The collection is not configured to allow usernameless WebAuthn authentication.", nil)
	}

	if err := checkWebAuthnLoginBeginIPLimit(e); err != nil {
		return err
	}

	e.Set(core.RequestEventKeyInfoContext, core.RequestInfoContextWebAuthn)

	wa, err := initWebAuthn(e.App)
	if err != nil {
		return e.InternalServerError("Failed to initialize WebAuthn.", err)
	}

	options, session, err := wa.BeginDiscoverableLogin()
	if err != nil {
		return e.InternalServerError("Failed to begin WebAuthn login.", err)
	}

	trimWebAuthnSessionsWithPurpose(e.App, webauthnPurposeDiscoverable, webauthnDiscoverableMaxEntries)
	token := storeWebAuthnSessionFor(e.App, session, "", false, webauthnPurposeDiscoverable)

	return e.JSON(http.StatusOK, map[string]any{
		"options":      options,
		"sessionToken": token,
	})
}

type webauthnLoginFinishDiscoverableForm struct {
	SessionToken string `form:"sessionToken" json:"sessionToken"`
}

func (form *webauthnLoginFinishDiscoverableForm) validate() error {
	return validation.ValidateStruct(form,
		validation.Field(&form.SessionToken, validation.Required, validation.Length(1, 255)),
	)
}

// recordWebAuthnLoginFinishDiscoverable completes a usernameless sign-in. The
// account is resolved from the credential's userHandle (the record id the
// authenticator stored at registration), not from anything the client names.
//
// As in login-finish, the session is consumed before anything else can fail,
// every rejection carries the same generic message, and the per-account
// attempt budget applies.
func recordWebAuthnLoginFinishDiscoverable(e *core.RequestEvent) error {
	collection, err := findAuthCollection(e)
	if err != nil {
		return err
	}

	if !collection.WebAuthn.Enabled || !collection.WebAuthn.Discoverable {
		return e.ForbiddenError("The collection is not configured to allow usernameless WebAuthn authentication.", nil)
	}

	bodyBytes, err := readWebAuthnFinishBody(e)
	if err != nil {
		return err
	}

	form := &webauthnLoginFinishDiscoverableForm{}
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, form); err != nil {
			return e.BadRequestError("An error occurred while loading the submitted data.", err)
		}
	}
	if err := form.validate(); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
	}

	e.Set(core.RequestEventKeyInfoContext, core.RequestInfoContextWebAuthn)

	entry, err := retrieveWebAuthnSession(e.App, form.SessionToken)
	if err != nil {
		addAuthTimingJitter()
		return e.BadRequestError("Failed to authenticate.", nil)
	}
	defer deleteWebAuthnSession(e.App, form.SessionToken)

	if entry.Purpose != webauthnPurposeDiscoverable {
		addAuthTimingJitter()
		return e.BadRequestError("Failed to authenticate.", nil)
	}

	wa, err := initWebAuthn(e.App)
	if err != nil {
		return e.InternalServerError("Failed to initialize WebAuthn.", err)
	}

	parsedResponse, err := protocol.ParseCredentialRequestResponseBytes(bodyBytes)
	if err != nil {
		return e.BadRequestError("Failed to authenticate.", nil)
	}

	var (
		record      *core.Record
		rateLimited bool
	)
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		found, err := e.App.FindRecordById(collection, string(userHandle))
		if err != nil {
			return nil, errors.New("unknown user handle")
		}

		// per-account attempt budget, same as login-finish; checked before the
		// signature work so guessing handles cannot be used to burn CPU
		if rlErr := checkRateLimit(e, webauthnLoginRateRule+found.Id, core.RateLimitRule{MaxRequests: 5, Duration: 180}); rlErr != nil {
			rateLimited = true
			return nil, errors.New("rate limited")
		}

		creds, err := loadUserCredentials(e.App, found)
		if err != nil {
			return nil, err
		}

		record = found

		return &webauthnUserAdapter{record: found, credentials: creds}, nil
	}

	_, cred, err := wa.ValidatePasskeyLogin(handler, *entry.Session, parsedResponse)
	if rateLimited {
		return e.TooManyRequestsError("Too many attempts, please try again later.", nil)
	}
	if err != nil || record == nil {
		addAuthTimingJitter()
		return e.BadRequestError("Failed to authenticate.", nil)
	}

	e.Set(requestEventKeySessionMethod, core.SessionAuthMethodDiscover) // session label (#93)

	return finishWebAuthnLogin(e, collection, record, cred)
}
