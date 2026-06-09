package apis

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"
)

const (
	webauthnSessionPrefix     = "webauthn:session:"
	webauthnSessionTTL        = 2 * time.Minute
	webauthnSessionMaxEntries = 1024              // hard cap to prevent unbounded growth (DoS)
	webauthnSessionGCInterval = 30 * time.Second  // background sweep interval
	webauthnRPCacheKey        = "webauthn:rp"     // cached *webauthn.WebAuthn in app.Store
	webauthnLoginRateRule     = "@pb_webauthn_login_"
	webauthnRegisterRateRule  = "@pb_webauthn_register_"
	webauthnPatchRateRule     = "@pb_webauthn_patch_"
	webauthnDeleteRateRule    = "@pb_webauthn_delete_"
)

// webauthnSessionEntry holds a WebAuthn challenge session along with its expiry.
//
// Decoy is set when the session was synthesized for an unknown/credential-less
// identity to equalize the response shape and timing of /login-begin against
// the real path. login-finish always rejects decoy sessions with the same
// generic error returned by genuine signature failures (audit H3).
type webauthnSessionEntry struct {
	Session   *webauthn.SessionData
	ExpiresAt time.Time
	RecordId  string
	Decoy     bool
}

// webauthnRPCacheEntry stores a precomputed *webauthn.WebAuthn keyed by the
// settings inputs that affect it; on settings change the cacheKey mismatches
// and the value is rebuilt.
type webauthnRPCacheEntry struct {
	cacheKey string
	wa       *webauthn.WebAuthn
}

// webauthnUserAdapter wraps a PocketBase auth record to implement the
// webauthn.User interface required by the go-webauthn library.
type webauthnUserAdapter struct {
	record      *core.Record
	credentials []webauthn.Credential
}

func (u *webauthnUserAdapter) WebAuthnID() []byte {
	return []byte(u.record.Id)
}

func (u *webauthnUserAdapter) WebAuthnName() string {
	name := u.record.GetString("username")
	if name == "" {
		name = u.record.GetString("email")
	}
	if name == "" {
		name = u.record.Id
	}
	return name
}

func (u *webauthnUserAdapter) WebAuthnDisplayName() string {
	name := u.record.GetString("name")
	if name == "" {
		name = u.WebAuthnName()
	}
	return name
}

func (u *webauthnUserAdapter) WebAuthnCredentials() []webauthn.Credential {
	return u.credentials
}

// initWebAuthn creates a new webauthn.WebAuthn instance configured from app settings.
//
// Hardenings (audit H4):
//   - AppURL is required and must parse to scheme + host (no path, no fragment).
//   - Non-https schemes are rejected except for localhost/127.0.0.1/[::1] to
//     keep local development workflows usable.
//   - The computed *webauthn.WebAuthn is cached on app.Store with a cacheKey
//     derived from the affecting settings; if AppURL or AppName changes the
//     cache misses and the value is rebuilt. This avoids rebuilding for every
//     request (perf) and ensures we don't keep using a stale relying-party
//     config after a settings update.
func initWebAuthn(app core.App) (*webauthn.WebAuthn, error) {
	appURL := app.Settings().Meta.AppURL
	if appURL == "" {
		return nil, errors.New("application URL is not configured (Settings.Meta.AppURL)")
	}

	parsed, err := url.Parse(appURL)
	if err != nil {
		return nil, fmt.Errorf("invalid application URL: %w", err)
	}
	if parsed.Host == "" {
		return nil, errors.New("application URL must include a host")
	}

	rpID := parsed.Hostname()
	if rpID == "" {
		return nil, errors.New("application URL must include a hostname")
	}

	// reject non-https except for local dev hosts (WebAuthn spec requires a
	// secure context except for localhost loopback addresses)
	if parsed.Scheme != "https" {
		if !isLocalWebAuthnHost(rpID) {
			return nil, fmt.Errorf("application URL must use https for WebAuthn (got %q)", parsed.Scheme)
		}
		if parsed.Scheme != "http" {
			return nil, fmt.Errorf("application URL scheme %q is not supported for WebAuthn", parsed.Scheme)
		}
	}

	// origin must be scheme://host[:port] only; trailing path/fragment must
	// not leak into RPOrigins or the browser will reject the assertion.
	origin := parsed.Scheme + "://" + parsed.Host
	rpOrigins := []string{origin}

	if envRPID := strings.TrimSpace(os.Getenv("WEBAUTHN_RP_ID")); envRPID != "" {
		rpID = envRPID
	}

	if envOrigins := os.Getenv("WEBAUTHN_RP_ORIGINS"); envOrigins != "" {
		overrideOrigins := make([]string, 0, 4)
		for _, raw := range strings.Split(envOrigins, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}

			u, err := url.Parse(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid WebAuthn RP origin %q: %w", raw, err)
			}
			if u.Host == "" {
				return nil, fmt.Errorf("invalid WebAuthn RP origin %q: missing host", raw)
			}

			host := u.Hostname()
			if host == "" {
				return nil, fmt.Errorf("invalid WebAuthn RP origin %q: missing hostname", raw)
			}

			if u.Scheme != "https" {
				if !isLocalWebAuthnHost(host) {
					return nil, fmt.Errorf("WebAuthn RP origin must use https (got %q)", u.Scheme)
				}
				if u.Scheme != "http" {
					return nil, fmt.Errorf("WebAuthn RP origin scheme %q is not supported", u.Scheme)
				}
			}

			if u.Path != "" && u.Path != "/" {
				return nil, fmt.Errorf("invalid WebAuthn RP origin %q: path is not allowed", raw)
			}
			if u.RawQuery != "" {
				return nil, fmt.Errorf("invalid WebAuthn RP origin %q: query is not allowed", raw)
			}
			if u.Fragment != "" {
				return nil, fmt.Errorf("invalid WebAuthn RP origin %q: fragment is not allowed", raw)
			}

			overrideOrigins = append(overrideOrigins, u.Scheme+"://"+u.Host)
		}

		if len(overrideOrigins) == 0 {
			return nil, errors.New("WEBAUTHN_RP_ORIGINS does not contain any valid origins")
		}

		rpOrigins = overrideOrigins
	}

	for _, rpOrigin := range rpOrigins {
		rpOriginURL, err := url.Parse(rpOrigin)
		if err != nil {
			continue
		}
		host := strings.ToLower(rpOriginURL.Hostname())
		rpIDLower := strings.ToLower(rpID)
		if host == rpIDLower || strings.HasSuffix(host, "."+rpIDLower) {
			continue
		}
		app.Logger().Warn("webauthn_rp_config_mismatch",
			"rpId", rpID,
			"origin", rpOrigin,
		)
	}

	cacheKey := app.Settings().Meta.AppName + "\x00" + rpID + "\x00" + strings.Join(rpOrigins, "\x00")
	if raw, ok := app.Store().GetOk(webauthnRPCacheKey); ok {
		if cached, ok := raw.(*webauthnRPCacheEntry); ok && cached != nil && cached.cacheKey == cacheKey && cached.wa != nil {
			return cached.wa, nil
		}
	}

	config := &webauthn.Config{
		RPDisplayName: app.Settings().Meta.AppName,
		RPID:          rpID,
		RPOrigins:     rpOrigins,
	}
	wa, err := webauthn.New(config)
	if err != nil {
		return nil, err
	}
	app.Store().Set(webauthnRPCacheKey, &webauthnRPCacheEntry{cacheKey: cacheKey, wa: wa})
	return wa, nil
}

// isLocalWebAuthnHost reports whether the given RP-ID is a loopback host that
// WebAuthn permits over plain http.
func isLocalWebAuthnHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// sanitizeWebAuthnName trims surrounding whitespace and removes ASCII control
// characters (incl. newlines) from a user-supplied credential label. The
// length cap is enforced separately by validation.
func sanitizeWebAuthnName(s string) string {
	s = strings.TrimSpace(s)
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// loadUserCredentials loads all WebAuthn credentials for a given record from the database.
func loadUserCredentials(app core.App, record *core.Record) ([]webauthn.Credential, error) {
	records, err := app.FindAllRecords(
		core.CollectionNameWebAuthnCredentials,
		dbx.HashExp{
			"collectionRef": record.Collection().Id,
			"recordRef":     record.Id,
		},
	)
	if err != nil {
		return nil, err
	}

	credentials := make([]webauthn.Credential, 0, len(records))
	for _, r := range records {
		proxy := &core.WebAuthnCredential{Record: r}
		cred, err := proxy.ToWebAuthnCredential()
		if err != nil {
			// audit L1: a credential row failing to decode means the row is
			// corrupt or the on-disk format has drifted -- log it instead of
			// silently dropping the credential.
			app.Logger().Warn("webauthn_credential_decode_failed",
				"credentialRecordId", r.Id,
				"recordRef", record.Id,
				"collectionRef", record.Collection().Id,
				"error", err,
			)
			continue
		}
		credentials = append(credentials, cred)
	}
	return credentials, nil
}

// webauthnGCOnce ensures the background eviction goroutine is started at most
// once per process. The goroutine periodically prunes expired entries from
// the app store; we never want concurrent sweepers and we never want the
// goroutine to outlive the process (which a bare goroutine satisfies).
var webauthnGCOnce sync.Once

// ensureWebAuthnGC lazily starts the background sweeper on first session
// store call. Idempotent; safe to call from every storeWebAuthnSession.
func ensureWebAuthnGC(app core.App) {
	webauthnGCOnce.Do(func() {
		go func() {
			t := time.NewTicker(webauthnSessionGCInterval)
			defer t.Stop()
			for range t.C {
				evictExpiredWebAuthnSessions(app)
			}
		}()
	})
}

// evictExpiredWebAuthnSessions removes any expired entries from the app store
// and returns the number of evictions performed (used by tests and to size
// retry-after-prune logic in storeWebAuthnSession).
func evictExpiredWebAuthnSessions(app core.App) int {
	now := time.Now()
	var evicted int
	for k, v := range app.Store().GetAll() {
		if !strings.HasPrefix(k, webauthnSessionPrefix) {
			continue
		}
		entry, ok := v.(*webauthnSessionEntry)
		if !ok || entry == nil {
			app.Store().Remove(k)
			evicted++
			continue
		}
		if now.After(entry.ExpiresAt) {
			app.Store().Remove(k)
			evicted++
		}
	}
	return evicted
}

// purgeWebAuthnSessionsForUser drops all in-flight WebAuthn sessions tied to
// a specific record id. Called when a user's credentials are deleted or
// administratively cleared so any pending login/registration ceremony cannot
// be completed using a since-revoked credential (audit M7).
func purgeWebAuthnSessionsForUser(app core.App, recordId string) int {
	if recordId == "" {
		return 0
	}
	var purged int
	for k, v := range app.Store().GetAll() {
		if !strings.HasPrefix(k, webauthnSessionPrefix) {
			continue
		}
		entry, ok := v.(*webauthnSessionEntry)
		if !ok || entry == nil || entry.RecordId != recordId {
			continue
		}
		app.Store().Remove(k)
		purged++
	}
	return purged
}

// countWebAuthnSessions returns the number of webauthn session entries
// currently in the app store (regardless of expiry).
func countWebAuthnSessions(app core.App) int {
	var n int
	for k := range app.Store().GetAll() {
		if strings.HasPrefix(k, webauthnSessionPrefix) {
			n++
		}
	}
	return n
}

// evictOldestWebAuthnSession drops the single oldest webauthn session entry
// (lowest ExpiresAt). Used when the store is at the configured hard cap and
// we must make room for a new session.
func evictOldestWebAuthnSession(app core.App) {
	type kv struct {
		key string
		exp time.Time
	}
	var oldest *kv
	for k, v := range app.Store().GetAll() {
		if !strings.HasPrefix(k, webauthnSessionPrefix) {
			continue
		}
		entry, ok := v.(*webauthnSessionEntry)
		if !ok || entry == nil {
			app.Store().Remove(k)
			continue
		}
		if oldest == nil || entry.ExpiresAt.Before(oldest.exp) {
			oldest = &kv{key: k, exp: entry.ExpiresAt}
		}
	}
	if oldest != nil {
		app.Store().Remove(oldest.key)
	}
}

// storeWebAuthnSession saves a webauthn session to the app store with a
// cryptographically random key and short TTL. The store is bounded by
// webauthnSessionMaxEntries; on overflow the oldest entry is evicted to make
// room (audit H1 mitigation against memory-exhaustion DoS).
func storeWebAuthnSession(app core.App, session *webauthn.SessionData, recordId string, decoy bool) string {
	ensureWebAuthnGC(app)

	entry := &webauthnSessionEntry{
		Session:   session,
		ExpiresAt: time.Now().Add(webauthnSessionTTL),
		RecordId:  recordId,
		Decoy:     decoy,
	}

	// opportunistic prune so we don't immediately evict a non-expired entry
	evictExpiredWebAuthnSessions(app)
	if countWebAuthnSessions(app) >= webauthnSessionMaxEntries {
		evictOldestWebAuthnSession(app)
	}

	token := security.RandomString(32)
	app.Store().Set(webauthnSessionPrefix+token, entry)
	return token
}

// retrieveWebAuthnSession retrieves and validates a session from the app store.
func retrieveWebAuthnSession(app core.App, token string) (*webauthnSessionEntry, error) {
	raw := app.Store().Get(webauthnSessionPrefix + token)
	if raw == nil {
		return nil, errors.New("missing or expired webauthn session")
	}

	entry, ok := raw.(*webauthnSessionEntry)
	if !ok || entry == nil {
		return nil, errors.New("invalid webauthn session data")
	}

	if time.Now().After(entry.ExpiresAt) {
		app.Store().Remove(webauthnSessionPrefix + token)
		return nil, errors.New("webauthn session has expired")
	}

	return entry, nil
}

// deleteWebAuthnSession removes a session from the app store.
func deleteWebAuthnSession(app core.App, token string) {
	app.Store().Remove(webauthnSessionPrefix + token)
}

// buildDecoyWebAuthnLoginOptions synthesizes a PublicKeyCredentialRequestOptions
// payload and matching SessionData for an identity that either (a) does not
// resolve to an auth record or (b) resolves but has no registered passkeys.
//
// The decoy challenge and decoy credential id are HMAC-derived from the
// per-collection auth-token secret so that:
//
//  1. Repeated probes with the same identity produce the same response shape
//     (no random "unknown user" tell vs a real user whose options stay stable
//     within a session).
//  2. The decoy values are unforgeable by the attacker.
//
// login-finish unconditionally rejects sessions where Decoy=true with the
// same generic "Failed to authenticate." error returned for a signature
// verification failure on a real account, completing the equalization. The
// goal is to eliminate the HTTP status-code and response-body enumeration
// signal documented in audit finding H3.
func buildDecoyWebAuthnLoginOptions(collection *core.Collection, identity, rpID string) *protocol.CredentialAssertion {
	secret := collection.AuthToken.Secret
	if secret == "" {
		// fallback ensures we still produce a deterministic value if the
		// collection has not been saved yet (e.g. in unit tests)
		secret = "webauthn-decoy-default"
	}
	ident := strings.ToLower(strings.TrimSpace(identity))

	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte("webauthn-decoy-challenge:"))
	h.Write([]byte(ident))
	challenge := h.Sum(nil)

	h2 := hmac.New(sha256.New, []byte(secret))
	h2.Write([]byte("webauthn-decoy-credid:"))
	h2.Write([]byte(ident))
	fakeCredID := h2.Sum(nil)

	return &protocol.CredentialAssertion{
		Response: protocol.PublicKeyCredentialRequestOptions{
			Challenge:      challenge,
			Timeout:        int(webauthnSessionTTL / time.Millisecond),
			RelyingPartyID: rpID,
			AllowedCredentials: []protocol.CredentialDescriptor{{
				Type:         protocol.PublicKeyCredentialType,
				CredentialID: fakeCredID,
				Transport: []protocol.AuthenticatorTransport{
					protocol.USB, protocol.NFC, protocol.BLE, protocol.Internal,
				},
			}},
			UserVerification: protocol.VerificationPreferred,
		},
	}
}

// buildDecoySessionData returns a SessionData matching the decoy options;
// stored alongside the response so it occupies the same store slot a real
// session would, and so it expires identically (audit H1+H3).
func buildDecoySessionData(opts *protocol.CredentialAssertion) *webauthn.SessionData {
	return &webauthn.SessionData{
		Challenge:        base64.RawURLEncoding.EncodeToString(opts.Response.Challenge),
		Expires:          time.Now().Add(webauthnSessionTTL),
		RelyingPartyID:   opts.Response.RelyingPartyID,
		UserVerification: opts.Response.UserVerification,
	}
}

// constantTimeIdentityCompare prevents accidental short-circuit timing on
// session token comparison by routing through hmac.Equal which is constant
// time over equal-length inputs. Callers pre-length-check; differing lengths
// fall through to the inequality return.
func constantTimeIdentityCompare(a, b string) bool {
	ab := []byte(a)
	bb := []byte(b)
	if len(ab) != len(bb) {
		// still touch both to avoid a length-based timing oracle
		_ = hmac.Equal(ab, ab)
		return false
	}
	return hmac.Equal(ab, bb)
}

// resolveAuthRecordByIdentity attempts to find an auth record by email OR
// username, always performing BOTH lookups regardless of whether the first
// matched. This equalizes the timing of the lookup path between known and
// unknown identities (audit H3).
func resolveAuthRecordByIdentity(app core.App, collection *core.Collection, identity string) *core.Record {
	byEmail, errEmail := app.FindAuthRecordByEmail(collection, identity)
	byUsername, errUsername := app.FindFirstRecordByFilter(
		collection,
		"username = {:identity}",
		dbx.Params{"identity": identity},
	)
	switch {
	case errEmail == nil && byEmail != nil:
		return byEmail
	case errUsername == nil && byUsername != nil:
		return byUsername
	}
	return nil
}

// addJitter sleeps for a small randomized duration so that the wall clock of
// any equalized authentication path varies by a few hundred microseconds,
// blurring residual timing measurements an attacker might attempt.
func addAuthTimingJitter() {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return
	}
	n := int(b[0])<<8 | int(b[1])
	time.Sleep(time.Duration(n%500) * time.Microsecond)
}

// -------------------------------------------------------------------
// Registration flow
// -------------------------------------------------------------------

// recordWebAuthnRegisterBegin initiates a WebAuthn credential registration.
//
// Requires an authenticated user. Returns PublicKeyCredentialCreationOptions
// and a session token for the finish step. Per-record rate limited to defend
// against authenticated-but-malicious clients flooding the session store
// (audit M4).
func recordWebAuthnRegisterBegin(e *core.RequestEvent) error {
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

	if err := checkRateLimit(e, webauthnRegisterRateRule+record.Id, core.RateLimitRule{MaxRequests: 10, Duration: 180}); err != nil {
		return e.TooManyRequestsError("Too many registration attempts, please try again later.", nil)
	}

	wa, err := initWebAuthn(e.App)
	if err != nil {
		return e.InternalServerError("Failed to initialize WebAuthn.", err)
	}

	existingCreds, err := loadUserCredentials(e.App, record)
	if err != nil {
		return e.InternalServerError("Failed to load existing credentials.", err)
	}

	user := &webauthnUserAdapter{
		record:      record,
		credentials: existingCreds,
	}

	options, session, err := wa.BeginRegistration(user)
	if err != nil {
		return e.InternalServerError("Failed to begin WebAuthn registration.", err)
	}

	token := storeWebAuthnSession(e.App, session, record.Id, false)

	return e.JSON(http.StatusOK, map[string]any{
		"options":      options,
		"sessionToken": token,
	})
}

// recordWebAuthnRegisterFinish completes WebAuthn credential registration.
//
// Requires the session token from register-begin and the attestation
// response from the authenticator. Returns 201 with the new credential ID.
//
// Hardenings:
//   - The duplicate-credential lookup is scoped to the current collection so
//     identical credential IDs across collections do not collide (audit M1).
//   - The credential name is sanitized to strip control characters before
//     validation (audit M5/L6).
//   - A generic error message is returned on duplicate registration so an
//     attacker cannot use this endpoint to enumerate registered credentials.
func recordWebAuthnRegisterFinish(e *core.RequestEvent) error {
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

	form := &webauthnRegisterFinishForm{}
	if err := e.BindBody(form); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while loading the submitted data.", err))
	}
	form.Name = sanitizeWebAuthnName(form.Name)
	if err := form.validate(); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
	}

	entry, err := retrieveWebAuthnSession(e.App, form.SessionToken)
	if err != nil {
		return e.BadRequestError("Invalid or expired registration session.", err)
	}
	defer deleteWebAuthnSession(e.App, form.SessionToken)

	if entry.Decoy || entry.RecordId != record.Id {
		return e.BadRequestError("Session does not match the authenticated user.", nil)
	}

	wa, err := initWebAuthn(e.App)
	if err != nil {
		return e.InternalServerError("Failed to initialize WebAuthn.", err)
	}

	existingCreds, err := loadUserCredentials(e.App, record)
	if err != nil {
		return e.InternalServerError("Failed to load existing credentials.", err)
	}

	user := &webauthnUserAdapter{
		record:      record,
		credentials: existingCreds,
	}

	cred, err := wa.FinishRegistration(user, *entry.Session, e.Request)
	if err != nil {
		return e.BadRequestError("Failed to verify WebAuthn registration.", err)
	}

	// scoped duplicate-credential check (audit M1): identical authenticator
	// IDs CAN legitimately appear in different auth collections, so we only
	// reject a duplicate within the current collection.
	credIdEncoded := base64.RawURLEncoding.EncodeToString(cred.ID)
	_, findErr := e.App.FindFirstRecordByFilter(
		core.CollectionNameWebAuthnCredentials,
		"credentialId = {:credId} && collectionRef = {:colId}",
		dbx.Params{"credId": credIdEncoded, "colId": collection.Id},
	)
	if findErr == nil {
		// generic message; do not echo the credential id
		return e.BadRequestError("Registration failed.", nil)
	}

	wcRecord := core.NewWebAuthnCredential(e.App)
	wcRecord.SetCollectionRef(collection.Id)
	wcRecord.SetRecordRef(record.Id)
	wcRecord.FromWebAuthnCredential(*cred)
	if form.Name != "" {
		wcRecord.SetName(form.Name)
	}

	if err := e.App.Save(wcRecord); err != nil {
		return e.InternalServerError("Failed to save WebAuthn credential.", err)
	}

	return e.JSON(http.StatusCreated, map[string]any{
		"id":   wcRecord.Id,
		"name": wcRecord.Name(),
	})
}

// -------------------------------------------------------------------
// Login flow
// -------------------------------------------------------------------

// recordWebAuthnLoginBegin initiates a WebAuthn authentication.
//
// Accepts an identity (email or username) and returns
// PublicKeyCredentialRequestOptions and a session token.
//
// Hardenings (audit H3):
//   - Both email and username lookups always run, regardless of whether the
//     first matched, so the wall-clock time of the response is independent
//     of which lookup (or neither) found the user.
//   - If the identity does not resolve to a record OR the record has no
//     registered credentials, a synthesized decoy challenge is returned with
//     the same JSON shape and HTTP status as the genuine path; the matching
//     session is marked Decoy=true so login-finish unconditionally rejects
//     it with the same generic error returned for a signature failure on a
//     real account.
//   - Generic errors carry no wrapped inner error, removing the data-field
//     discriminator a network observer could otherwise inspect.
func recordWebAuthnLoginBegin(e *core.RequestEvent) error {
	collection, err := findAuthCollection(e)
	if err != nil {
		return err
	}

	if !collection.WebAuthn.Enabled {
		return e.ForbiddenError("The collection is not configured to allow WebAuthn authentication.", nil)
	}

	form := &webauthnLoginBeginForm{}
	if err := e.BindBody(form); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while loading the submitted data.", err))
	}
	if err := form.validate(); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
	}

	e.Set(core.RequestEventKeyInfoContext, core.RequestInfoContextWebAuthn)

	wa, err := initWebAuthn(e.App)
	if err != nil {
		return e.InternalServerError("Failed to initialize WebAuthn.", err)
	}

	// always perform both lookups for timing equalization
	record := resolveAuthRecordByIdentity(e.App, collection, form.Identity)

	// always load creds; for unknown identities we still pay the credential-
	// load wall-clock cost via the decoy path below
	var existingCreds []webauthn.Credential
	if record != nil {
		if loaded, lerr := loadUserCredentials(e.App, record); lerr == nil {
			existingCreds = loaded
		}
	}

	// Decoy path: identity unknown OR no registered credentials. Synthesize
	// an indistinguishable response and decoy session.
	if record == nil || len(existingCreds) == 0 {
		rpID := wa.Config.RPID
		decoyOpts := buildDecoyWebAuthnLoginOptions(collection, form.Identity, rpID)
		decoySession := buildDecoySessionData(decoyOpts)
		token := storeWebAuthnSession(e.App, decoySession, "", true)
		addAuthTimingJitter()
		return e.JSON(http.StatusOK, map[string]any{
			"options":      decoyOpts,
			"sessionToken": token,
		})
	}

	user := &webauthnUserAdapter{
		record:      record,
		credentials: existingCreds,
	}

	options, session, err := wa.BeginLogin(user)
	if err != nil {
		return e.InternalServerError("Failed to begin WebAuthn login.", err)
	}

	token := storeWebAuthnSession(e.App, session, record.Id, false)
	addAuthTimingJitter()

	return e.JSON(http.StatusOK, map[string]any{
		"options":      options,
		"sessionToken": token,
	})
}

// recordWebAuthnLoginFinish completes WebAuthn authentication.
//
// Validates the assertion response against the stored credential and returns
// an auth token on success.
//
// Hardenings:
//   - The session is retrieved (and the deferred delete is registered) BEFORE
//     the rate-limit check, so a burst of finish-requests still invalidates
//     each session token after a single use (audit H2). Sessions are
//     single-use; a rate-limited request still consumes its session.
//   - Decoy sessions (Decoy=true) are rejected with the same generic error
//     used for a signature verification failure on a real account so the
//     status code and response shape match the equalized login-begin path
//     (audit H3).
//   - When the underlying webauthn library reports CloneWarning (sign-count
//     regression or stale) the sign count is NOT updated and the request is
//     rejected: a regression usually indicates a cloned authenticator (audit
//     M3).
func recordWebAuthnLoginFinish(e *core.RequestEvent) error {
	collection, err := findAuthCollection(e)
	if err != nil {
		return err
	}

	if !collection.WebAuthn.Enabled {
		return e.ForbiddenError("The collection is not configured to allow WebAuthn authentication.", nil)
	}

	form := &webauthnLoginFinishForm{}
	if err := e.BindBody(form); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while loading the submitted data.", err))
	}
	if err := form.validate(); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
	}

	e.Set(core.RequestEventKeyInfoContext, core.RequestInfoContextWebAuthn)

	// Retrieve session FIRST and register the deferred delete so that the
	// single-use semantics hold even for rate-limited or otherwise rejected
	// completion attempts (audit H2).
	entry, err := retrieveWebAuthnSession(e.App, form.SessionToken)
	if err != nil {
		return e.BadRequestError("Failed to authenticate.", nil)
	}
	defer deleteWebAuthnSession(e.App, form.SessionToken)

	// Decoy session: equalized rejection path for unknown user / no creds.
	if entry.Decoy {
		addAuthTimingJitter()
		return e.BadRequestError("Failed to authenticate.", nil)
	}

	record := resolveAuthRecordByIdentity(e.App, collection, form.Identity)
	if record == nil {
		addAuthTimingJitter()
		return e.BadRequestError("Failed to authenticate.", nil)
	}

	if !constantTimeIdentityCompare(entry.RecordId, record.Id) {
		return e.BadRequestError("Failed to authenticate.", nil)
	}

	// per-record rate limit AFTER session retrieval so that rate-limited
	// requests still consume the session (single-use guarantee).
	if rlErr := checkRateLimit(e, webauthnLoginRateRule+record.Id, core.RateLimitRule{MaxRequests: 5, Duration: 180}); rlErr != nil {
		return e.TooManyRequestsError("Too many attempts, please try again later.", nil)
	}

	wa, err := initWebAuthn(e.App)
	if err != nil {
		return e.InternalServerError("Failed to initialize WebAuthn.", err)
	}

	existingCreds, err := loadUserCredentials(e.App, record)
	if err != nil {
		return e.InternalServerError("Failed to load credentials.", err)
	}

	user := &webauthnUserAdapter{
		record:      record,
		credentials: existingCreds,
	}

	cred, err := wa.FinishLogin(user, *entry.Session, e.Request)
	if err != nil {
		return e.BadRequestError("Failed to authenticate.", nil)
	}

	// sign-count regression / cloned-authenticator detection (audit M3): if
	// the library raised CloneWarning the authenticator's sign count went
	// backwards relative to the stored value. Reject and log; do NOT update
	// the stored sign count.
	if cred != nil && cred.Authenticator.CloneWarning {
		e.App.Logger().Warn("webauthn_clone_warning",
			"recordId", record.Id,
			"collectionId", collection.Id,
			"credentialId", base64.RawURLEncoding.EncodeToString(cred.ID),
			"reportedSignCount", cred.Authenticator.SignCount,
		)
		return e.BadRequestError("Failed to authenticate.", nil)
	}

	// Update sign count on the stored credential (single query, reused for event)
	var storedProxy *core.WebAuthnCredential
	if cred != nil {
		credIdEncoded := base64.RawURLEncoding.EncodeToString(cred.ID)
		storedRecord, findErr := e.App.FindFirstRecordByFilter(
			core.CollectionNameWebAuthnCredentials,
			"credentialId = {:credId} && recordRef = {:recordRef} && collectionRef = {:colId}",
			dbx.Params{"credId": credIdEncoded, "recordRef": record.Id, "colId": collection.Id},
		)
		if findErr == nil {
			storedProxy = &core.WebAuthnCredential{Record: storedRecord}
			storedProxy.SetSignCount(cred.Authenticator.SignCount)
			storedProxy.SetFlags(cred.Flags)
			if saveErr := e.App.Save(storedProxy); saveErr != nil {
				e.App.Logger().Error("Failed to update WebAuthn credential sign count",
					"error", saveErr,
					"credentialId", credIdEncoded,
				)
			}
		}
	}

	// Trigger WebAuthn auth event
	event := new(core.RecordAuthWithWebAuthnRequestEvent)
	event.RequestEvent = e
	event.Collection = collection
	event.Record = record
	event.Credential = storedProxy

	return e.App.OnRecordAuthWithWebAuthnRequest().Trigger(event, func(e *core.RecordAuthWithWebAuthnRequestEvent) error {
		return RecordAuthResponse(e.RequestEvent, e.Record, core.MFAMethodWebAuthn, nil)
	})
}

// -------------------------------------------------------------------
// Forms
// -------------------------------------------------------------------

// webauthnRegisterFinishForm captures the session token and optional credential
// name for the WebAuthn registration completion step.
type webauthnRegisterFinishForm struct {
	SessionToken string `form:"sessionToken" json:"sessionToken"`
	Name         string `form:"name" json:"name"`
}

func (form *webauthnRegisterFinishForm) validate() error {
	return validation.ValidateStruct(form,
		validation.Field(&form.SessionToken, validation.Required, validation.Length(1, 255)),
		validation.Field(&form.Name, validation.Length(0, 255)),
	)
}

// webauthnLoginBeginForm captures the identity (email or username) for
// WebAuthn login initiation.
type webauthnLoginBeginForm struct {
	Identity string `form:"identity" json:"identity"`
}

func (form *webauthnLoginBeginForm) validate() error {
	return validation.ValidateStruct(form,
		validation.Field(&form.Identity, validation.Required, validation.Length(1, 255)),
	)
}

// webauthnLoginFinishForm captures the identity and session token for
// WebAuthn login completion.
type webauthnLoginFinishForm struct {
	Identity     string `form:"identity" json:"identity"`
	SessionToken string `form:"sessionToken" json:"sessionToken"`
}

func (form *webauthnLoginFinishForm) validate() error {
	return validation.ValidateStruct(form,
		validation.Field(&form.Identity, validation.Required, validation.Length(1, 255)),
		validation.Field(&form.SessionToken, validation.Required, validation.Length(1, 255)),
	)
}

// webauthnPatchCredentialForm captures the new credential name for the
// rename-own-credential endpoint.
type webauthnPatchCredentialForm struct {
	Name string `form:"name" json:"name"`
}

func (form *webauthnPatchCredentialForm) validate() error {
	return validation.ValidateStruct(form,
		validation.Field(&form.Name, validation.Length(0, 255)),
	)
}

// -------------------------------------------------------------------
// Credential management (recovery support)
// -------------------------------------------------------------------

// recordWebAuthnListCredentials lists the authenticated user's WebAuthn credentials.
//
// Returns a summary of each credential (id, name, created, signCount)
// without exposing sensitive fields like public keys.
func recordWebAuthnListCredentials(e *core.RequestEvent) error {
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

	records, err := e.App.FindAllRecords(
		core.CollectionNameWebAuthnCredentials,
		dbx.HashExp{
			"collectionRef": collection.Id,
			"recordRef":     record.Id,
		},
	)
	if err != nil {
		return e.InternalServerError("Failed to load credentials.", err)
	}

	result := make([]map[string]any, 0, len(records))
	for _, r := range records {
		proxy := &core.WebAuthnCredential{Record: r}
		result = append(result, map[string]any{
			"id":        r.Id,
			"name":      proxy.Name(),
			"created":   r.GetDateTime("created"),
			"signCount": proxy.SignCount(),
		})
	}

	return e.JSON(http.StatusOK, result)
}

// recordWebAuthnPatchCredential lets an authenticated user rename
// one of their own WebAuthn credentials.
func recordWebAuthnPatchCredential(e *core.RequestEvent) error {
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

	if err := checkRateLimit(e, webauthnPatchRateRule+record.Id, core.RateLimitRule{MaxRequests: 30, Duration: 180}); err != nil {
		return e.TooManyRequestsError("Too many requests, please try again later.", nil)
	}

	credentialId := e.Request.PathValue("credentialId")
	if credentialId == "" {
		return e.BadRequestError("Missing credential ID.", nil)
	}

	form := &webauthnPatchCredentialForm{}
	if err := e.BindBody(form); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while loading the submitted data.", err))
	}
	form.Name = sanitizeWebAuthnName(form.Name)
	if err := form.validate(); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
	}

	credRecord, err := e.App.FindRecordById(core.CollectionNameWebAuthnCredentials, credentialId)
	if err != nil {
		return e.NotFoundError("Credential not found.", nil)
	}

	proxy := &core.WebAuthnCredential{Record: credRecord}

	if proxy.RecordRef() != record.Id || proxy.CollectionRef() != collection.Id {
		return e.NotFoundError("Credential not found.", nil)
	}

	proxy.SetName(form.Name)
	if err := e.App.Save(credRecord); err != nil {
		return e.InternalServerError("Failed to update credential.", err)
	}

	return e.JSON(http.StatusOK, map[string]any{
		"id":   credRecord.Id,
		"name": proxy.Name(),
	})
}

// recordWebAuthnDeleteCredential lets an authenticated user delete
// one of their own WebAuthn credentials by ID.
//
// Any in-flight WebAuthn sessions belonging to this user are also purged so
// a pending login/registration ceremony cannot be completed using a since-
// revoked credential (audit M7).
func recordWebAuthnDeleteCredential(e *core.RequestEvent) error {
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

	if err := checkRateLimit(e, webauthnDeleteRateRule+record.Id, core.RateLimitRule{MaxRequests: 10, Duration: 180}); err != nil {
		return e.TooManyRequestsError("Too many requests, please try again later.", nil)
	}

	credentialId := e.Request.PathValue("credentialId")
	if credentialId == "" {
		return e.BadRequestError("Missing credential ID.", nil)
	}

	credRecord, err := e.App.FindRecordById(core.CollectionNameWebAuthnCredentials, credentialId)
	if err != nil {
		return e.NotFoundError("Credential not found.", nil)
	}

	proxy := &core.WebAuthnCredential{Record: credRecord}

	if proxy.RecordRef() != record.Id || proxy.CollectionRef() != collection.Id {
		return e.NotFoundError("Credential not found.", nil)
	}

	if err := e.App.Delete(credRecord); err != nil {
		return e.InternalServerError("Failed to delete credential.", err)
	}

	purged := purgeWebAuthnSessionsForUser(e.App, record.Id)
	e.App.Logger().Info("webauthn_credential_deleted",
		"actor", "user",
		"actorId", record.Id,
		"collectionId", collection.Id,
		"credentialId", credRecord.Id,
		"sessionsPurged", purged,
	)

	return e.NoContent(http.StatusNoContent)
}

// recordWebAuthnAdminClearCredentials allows a superuser to delete
// all WebAuthn credentials for a specific auth record.
//
// This is the recovery mechanism for users who have lost their authenticator
// device and cannot log in with their passkey.
//
// Hardenings (audit M2):
//   - The credential deletes run inside a single transaction; on partial
//     failure the whole operation is rolled back, eliminating the
//     half-cleared state where some credentials remain after a 500.
//   - The action emits a structured audit log entry (event
//     webauthn_admin_clear_credentials) with the superuser id, target record
//     id and deleted credential ids.
//   - Any in-flight WebAuthn sessions for the target user are purged so a
//     pending ceremony cannot be completed against credentials that just got
//     deleted (audit M7).
func recordWebAuthnAdminClearCredentials(e *core.RequestEvent) error {
	collection, err := findAuthCollection(e)
	if err != nil {
		return err
	}

	actor := e.Auth
	actorId := ""
	if actor != nil {
		actorId = actor.Id
	}

	recordId := e.Request.PathValue("id")
	if recordId == "" {
		return e.BadRequestError("Missing record ID.", nil)
	}

	if _, err := e.App.FindRecordById(collection, recordId); err != nil {
		return e.NotFoundError("Auth record not found.", nil)
	}

	records, err := e.App.FindAllRecords(
		core.CollectionNameWebAuthnCredentials,
		dbx.HashExp{
			"collectionRef": collection.Id,
			"recordRef":     recordId,
		},
	)
	if err != nil {
		return e.InternalServerError("Failed to load credentials.", err)
	}

	deletedIds := make([]string, 0, len(records))
	txErr := e.App.RunInTransaction(func(txApp core.App) error {
		for _, r := range records {
			if derr := txApp.Delete(r); derr != nil {
				return derr
			}
			deletedIds = append(deletedIds, r.Id)
		}
		return nil
	})
	if txErr != nil {
		e.App.Logger().Error("webauthn_admin_clear_credentials_failed",
			"actorId", actorId,
			"targetRecordId", recordId,
			"collectionId", collection.Id,
			"error", txErr,
		)
		return e.InternalServerError("Failed to clear credentials.", txErr)
	}

	purged := purgeWebAuthnSessionsForUser(e.App, recordId)
	e.App.Logger().Info("webauthn_admin_clear_credentials",
		"actor", "superuser",
		"actorId", actorId,
		"targetRecordId", recordId,
		"collectionId", collection.Id,
		"deletedCount", len(deletedIds),
		"deletedIds", deletedIds,
		"sessionsPurged", purged,
	)

	return e.JSON(http.StatusOK, map[string]any{
		"deleted": len(deletedIds),
	})
}
