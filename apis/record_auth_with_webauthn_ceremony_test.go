package apis_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/hook"
)

// This file exercises the *successful* WebAuthn ceremonies end to end with a
// software authenticator that performs real ECDSA P-256 signing.
//
// The rest of the WebAuthn suite only covers guard and error paths (bad
// tokens, disabled collections, unknown identities), so before this file
// existed nothing verified that a passkey could actually be registered and
// then used to log in. That gap matters most during a go-webauthn upgrade:
// the library's ceremony entry points (BeginRegistration/FinishRegistration/
// BeginLogin/FinishLogin) can change shape without any existing test noticing,
// and the failure would only surface in production as "passkeys stopped
// working". These tests pin the happy path so such a regression fails here.

const (
	ceremonyAppURL = "https://example.com"
	ceremonyRPID   = "example.com"

	// users collection auth token for "4q1xlclmfloku33" (test_users fixture),
	// matching the token reused across the other WebAuthn scenarios.
	ceremonyUserToken = "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6IjRxMXhsY2xtZmxva3UzMyIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoiX3BiX3VzZXJzX2F1dGhfIiwiZXhwIjoyNTI0NjA0NDYxLCJyZWZyZXNoYWJsZSI6dHJ1ZX0.ZT3F0Z3iM-xbGgSG3LEKiEzHrPHr8t8IuHLZGGNuxLo"
	ceremonyUserID    = "4q1xlclmfloku33"
	ceremonyIdentity  = "test@example.com"
)

// authenticator data flag bits (WebAuthn §6.1).
const (
	flagUserPresent            = 0x01
	flagUserVerified           = 0x04
	flagBackupEligible         = 0x08
	flagBackupState            = 0x10
	flagAttestedCredentialData = 0x40
)

// softAuthenticator is a minimal in-process WebAuthn authenticator. It holds a
// real ECDSA P-256 key and produces attestation/assertion payloads in exactly
// the wire format the browser would send (see encodeAttestation/encodeAssertion
// in apis/passkeys_page.go).
type softAuthenticator struct {
	key       *ecdsa.PrivateKey
	credID    []byte
	aaguid    []byte
	signCount uint32
	rpID      string
}

func newSoftAuthenticator(t testing.TB, rpID string) *softAuthenticator {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate authenticator key: %v", err)
	}

	credID := make([]byte, 32)
	if _, err := rand.Read(credID); err != nil {
		t.Fatalf("failed to generate credential id: %v", err)
	}

	return &softAuthenticator{
		key:       key,
		credID:    credID,
		aaguid:    make([]byte, 16), // all-zero aaguid, as "none" attestation requires
		signCount: 1,
		rpID:      rpID,
	}
}

// coseKey returns the credential public key in COSE_Key form. The integer map
// keys mirror webauthncose.EC2PublicKeyData's `cbor:"N,keyasint"` tags so the
// library decodes it into an EC2 key: 1=kty, 3=alg, -1=crv, -2=x, -3=y.
func (a *softAuthenticator) coseKey(t testing.TB) []byte {
	t.Helper()

	// Take the coordinates from the encoded uncompressed point rather than the
	// big.Int X/Y fields, which are deprecated as of Go 1.26.
	// Bytes() returns SEC 1 uncompressed form: 0x04 || X(32) || Y(32).
	pub, err := a.key.PublicKey.Bytes()
	if err != nil {
		t.Fatalf("failed to encode public key: %v", err)
	}
	if len(pub) != 65 || pub[0] != 0x04 {
		t.Fatalf("unexpected P-256 public key encoding: len=%d prefix=%#x", len(pub), pub[0])
	}

	x := pub[1:33]
	y := pub[33:65]

	raw, err := webauthncbor.Marshal(map[int]any{
		1:  2,  // kty: EC2
		3:  -7, // alg: ES256
		-1: 1,  // crv: P-256
		-2: x,
		-3: y,
	})
	if err != nil {
		t.Fatalf("failed to encode COSE key: %v", err)
	}

	return raw
}

// authData builds the authenticator data structure (WebAuthn §6.1).
func (a *softAuthenticator) authData(t testing.TB, includeAttestedCredential bool) []byte {
	t.Helper()

	rpIDHash := sha256.Sum256([]byte(a.rpID))

	// Backup eligible/state are set so the credential looks like a modern
	// multi-device passkey; the fork stores these flags on the credential.
	flags := byte(flagUserPresent | flagUserVerified | flagBackupEligible | flagBackupState)
	if includeAttestedCredential {
		flags |= flagAttestedCredentialData
	}

	buf := new(bytes.Buffer)
	buf.Write(rpIDHash[:])
	buf.WriteByte(flags)

	counter := make([]byte, 4)
	binary.BigEndian.PutUint32(counter, a.signCount)
	buf.Write(counter)

	if includeAttestedCredential {
		buf.Write(a.aaguid)

		credIDLen := make([]byte, 2)
		binary.BigEndian.PutUint16(credIDLen, uint16(len(a.credID)))
		buf.Write(credIDLen)
		buf.Write(a.credID)
		buf.Write(a.coseKey(t))
	}

	return buf.Bytes()
}

func (a *softAuthenticator) clientDataJSON(t testing.TB, ceremonyType, challenge, origin string) []byte {
	t.Helper()

	raw, err := json.Marshal(map[string]any{
		"type":        ceremonyType,
		"challenge":   challenge,
		"origin":      origin,
		"crossOrigin": false,
	})
	if err != nil {
		t.Fatalf("failed to encode clientDataJSON: %v", err)
	}

	return raw
}

// attestation produces a registration response using the "none" attestation
// format, which is what a browser returns when the RP does not request
// attestation conveyance (the fork leaves AttestationPreference unset).
func (a *softAuthenticator) attestation(t testing.TB, challenge, origin string) map[string]any {
	t.Helper()

	clientData := a.clientDataJSON(t, "webauthn.create", challenge, origin)
	rawAuthData := a.authData(t, true)

	attObj, err := webauthncbor.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": rawAuthData,
	})
	if err != nil {
		t.Fatalf("failed to encode attestation object: %v", err)
	}

	credIDb64 := base64.RawURLEncoding.EncodeToString(a.credID)

	return map[string]any{
		"id":                     credIDb64,
		"rawId":                  credIDb64,
		"type":                   "public-key",
		"clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"attestationObject": base64.RawURLEncoding.EncodeToString(attObj),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
		},
	}
}

// assertion produces a login response, signing over
// authenticatorData || SHA256(clientDataJSON) as the spec requires.
func (a *softAuthenticator) assertion(t testing.TB, challenge, origin, userHandle string) map[string]any {
	t.Helper()

	a.signCount++

	clientData := a.clientDataJSON(t, "webauthn.get", challenge, origin)
	rawAuthData := a.authData(t, false)

	clientDataHash := sha256.Sum256(clientData)
	signed := append(append([]byte{}, rawAuthData...), clientDataHash[:]...)
	digest := sha256.Sum256(signed)

	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatalf("failed to sign assertion: %v", err)
	}

	credIDb64 := base64.RawURLEncoding.EncodeToString(a.credID)

	return map[string]any{
		"id":                     credIDb64,
		"rawId":                  credIDb64,
		"type":                   "public-key",
		"clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"authenticatorData": base64.RawURLEncoding.EncodeToString(rawAuthData),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
			"signature":         base64.RawURLEncoding.EncodeToString(sig),
			"userHandle":        base64.RawURLEncoding.EncodeToString([]byte(userHandle)),
		},
	}
}

// ceremonyEnv wires a test app + router so a sequence of dependent requests can
// share state (the session token from "begin" must reach "finish").
type ceremonyEnv struct {
	t   testing.TB
	app *tests.TestApp
	mux http.Handler
}

// newCeremonyEnv builds an environment with MFA disabled.
func newCeremonyEnv(t testing.TB) *ceremonyEnv {
	t.Helper()

	return newCeremonyEnvWithMFA(t, false)
}

func newCeremonyEnvWithMFA(t testing.TB, mfa bool) *ceremonyEnv {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("failed to create test app: %v", err)
	}
	t.Cleanup(app.Cleanup)

	// Deterministic relying-party config. https + a non-loopback host keeps us
	// on the strict path in initWebAuthn (no localhost special-casing).
	app.Settings().Meta.AppURL = ceremonyAppURL
	if err := app.Save(app.Settings()); err != nil {
		t.Fatalf("failed to set AppURL: %v", err)
	}

	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("failed to find users collection: %v", err)
	}
	usersCol.WebAuthn.Enabled = true
	// The fixture enables MFA, which would turn a successful ceremony into a
	// 401 + mfaId second-factor challenge. These tests isolate the WebAuthn
	// ceremony itself; TestWebAuthnCeremonyMFAChallenge covers the MFA path.
	usersCol.MFA.Enabled = mfa
	if err := app.Save(usersCol); err != nil {
		t.Fatalf("failed to configure users collection: %v", err)
	}

	baseRouter, err := apis.NewRouter(app)
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}

	serveEvent := new(core.ServeEvent)
	serveEvent.App = app
	serveEvent.Router = baseRouter

	var mux http.Handler
	err = app.OnServe().Trigger(serveEvent, func(e *core.ServeEvent) error {
		// Disable rate limiting; the login-finish handler applies a per-record
		// limit of 5 attempts that the multi-login test would otherwise trip.
		e.Router.Bind(&hook.Handler[*core.RequestEvent]{
			Func: func(re *core.RequestEvent) error {
				return re.Next()
			},
			Priority: -9999,
		})

		built, err := e.Router.BuildMux()
		if err != nil {
			return err
		}
		mux = built

		return nil
	})
	if err != nil {
		t.Fatalf("failed to build mux: %v", err)
	}

	return &ceremonyEnv{t: t, app: app, mux: mux}
}

// do issues a JSON request and returns the status plus decoded body.
func (env *ceremonyEnv) do(method, url string, body any, authToken string) (int, map[string]any) {
	env.t.Helper()

	var reader *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			env.t.Fatalf("failed to encode request body: %v", err)
		}
		reader = strings.NewReader(string(raw))
	} else {
		reader = strings.NewReader("")
	}

	req := httptest.NewRequest(method, url, reader)
	req.Header.Set("content-type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", authToken)
	}

	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)

	res := rec.Result()

	decoded := map[string]any{}
	if rec.Body.Len() > 0 {
		// A non-JSON body is not fatal here; callers assert on status too.
		_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	}

	return res.StatusCode, decoded
}

// challengeFrom digs the challenge out of a begin response. The options value
// is the library's CredentialCreation/CredentialAssertion, which serializes as
// {"publicKey": {"challenge": "...", ...}}.
func challengeFrom(t testing.TB, body map[string]any) string {
	t.Helper()

	options, ok := body["options"].(map[string]any)
	if !ok {
		t.Fatalf("response has no options object: %v", body)
	}

	publicKey, ok := options["publicKey"].(map[string]any)
	if !ok {
		t.Fatalf("options has no publicKey object: %v", options)
	}

	challenge, ok := publicKey["challenge"].(string)
	if !ok || challenge == "" {
		t.Fatalf("publicKey has no challenge: %v", publicKey)
	}

	return challenge
}

func sessionTokenFrom(t testing.TB, body map[string]any) string {
	t.Helper()

	token, ok := body["sessionToken"].(string)
	if !ok || token == "" {
		t.Fatalf("response has no sessionToken: %v", body)
	}

	return token
}

// registerPasskey runs the full registration ceremony and returns the stored
// credential id.
func registerPasskey(t testing.TB, env *ceremonyEnv, auth *softAuthenticator, name string) string {
	t.Helper()

	status, body := env.do(
		http.MethodPost,
		"/api/collections/users/auth-with-webauthn/register-begin",
		map[string]any{},
		ceremonyUserToken,
	)
	if status != http.StatusOK {
		t.Fatalf("register-begin: expected 200, got %d (%v)", status, body)
	}

	challenge := challengeFrom(t, body)
	sessionToken := sessionTokenFrom(t, body)

	payload := auth.attestation(t, challenge, ceremonyAppURL)
	payload["sessionToken"] = sessionToken
	payload["name"] = name

	status, body = env.do(
		http.MethodPost,
		"/api/collections/users/auth-with-webauthn/register-finish",
		payload,
		ceremonyUserToken,
	)
	if status != http.StatusCreated {
		t.Fatalf("register-finish: expected 201, got %d (%v)", status, body)
	}

	recordID, _ := body["id"].(string)

	return recordID
}

// loginWithPasskey runs the full login ceremony and returns status + body.
func loginWithPasskey(t testing.TB, env *ceremonyEnv, auth *softAuthenticator) (int, map[string]any) {
	t.Helper()

	status, body := env.do(
		http.MethodPost,
		"/api/collections/users/auth-with-webauthn/login-begin",
		map[string]any{"identity": ceremonyIdentity},
		"",
	)
	if status != http.StatusOK {
		t.Fatalf("login-begin: expected 200, got %d (%v)", status, body)
	}

	challenge := challengeFrom(t, body)
	sessionToken := sessionTokenFrom(t, body)

	payload := auth.assertion(t, challenge, ceremonyAppURL, ceremonyUserID)
	payload["sessionToken"] = sessionToken
	payload["identity"] = ceremonyIdentity

	return env.do(
		http.MethodPost,
		"/api/collections/users/auth-with-webauthn/login-finish",
		payload,
		"",
	)
}

// TestWebAuthnCeremonyRegisterThenLogin is the core happy-path guarantee:
// a passkey can be registered and then used to authenticate.
func TestWebAuthnCeremonyRegisterThenLogin(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	auth := newSoftAuthenticator(t, ceremonyRPID)

	credID := registerPasskey(t, env, auth, "primary key")
	if credID == "" {
		t.Fatal("expected a credential id from register-finish")
	}

	// the credential must be persisted and bound to the right record
	rec, err := env.app.FindFirstRecordByFilter(
		core.CollectionNameWebAuthnCredentials,
		"credentialId = {:id}",
		map[string]any{"id": base64.RawURLEncoding.EncodeToString(auth.credID)},
	)
	if err != nil {
		t.Fatalf("credential was not persisted: %v", err)
	}
	if got := rec.GetString("recordRef"); got != ceremonyUserID {
		t.Fatalf("credential bound to %q, want %q", got, ceremonyUserID)
	}
	if got := rec.GetString("name"); got != "primary key" {
		t.Fatalf("credential name = %q, want %q", got, "primary key")
	}

	status, body := loginWithPasskey(t, env, auth)
	if status != http.StatusOK {
		t.Fatalf("login-finish: expected 200, got %d (%v)", status, body)
	}

	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("expected an auth token in the login response, got %v", body)
	}

	record, ok := body["record"].(map[string]any)
	if !ok {
		t.Fatalf("expected a record in the login response, got %v", body)
	}
	if id, _ := record["id"].(string); id != ceremonyUserID {
		t.Fatalf("authenticated as %q, want %q", id, ceremonyUserID)
	}
}

// TestWebAuthnCeremonyLoginRejectsWrongKey ensures the signature is actually
// verified: a different private key must not authenticate, even though the
// credential id matches a registered credential.
func TestWebAuthnCeremonyLoginRejectsWrongKey(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	auth := newSoftAuthenticator(t, ceremonyRPID)

	registerPasskey(t, env, auth, "primary key")

	// swap in a different key while keeping the registered credential id
	impostor := newSoftAuthenticator(t, ceremonyRPID)
	impostor.credID = auth.credID

	status, body := loginWithPasskey(t, env, impostor)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for a bad signature, got %d (%v)", status, body)
	}
}

// TestWebAuthnCeremonyLoginRejectsWrongOrigin ensures origin binding is
// enforced, which is the anti-phishing property of WebAuthn.
func TestWebAuthnCeremonyLoginRejectsWrongOrigin(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	auth := newSoftAuthenticator(t, ceremonyRPID)

	registerPasskey(t, env, auth, "primary key")

	status, body := env.do(
		http.MethodPost,
		"/api/collections/users/auth-with-webauthn/login-begin",
		map[string]any{"identity": ceremonyIdentity},
		"",
	)
	if status != http.StatusOK {
		t.Fatalf("login-begin: expected 200, got %d (%v)", status, body)
	}

	challenge := challengeFrom(t, body)
	sessionToken := sessionTokenFrom(t, body)

	// sign over an attacker-controlled origin
	payload := auth.assertion(t, challenge, "https://evil.example.net", ceremonyUserID)
	payload["sessionToken"] = sessionToken
	payload["identity"] = ceremonyIdentity

	status, body = env.do(
		http.MethodPost,
		"/api/collections/users/auth-with-webauthn/login-finish",
		payload,
		"",
	)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for a mismatched origin, got %d (%v)", status, body)
	}
}

// TestWebAuthnCeremonyLoginRejectsReplayedChallenge ensures a session token is
// single-use, so a captured assertion cannot be replayed.
func TestWebAuthnCeremonyLoginRejectsReplayedChallenge(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	auth := newSoftAuthenticator(t, ceremonyRPID)

	registerPasskey(t, env, auth, "primary key")

	status, body := env.do(
		http.MethodPost,
		"/api/collections/users/auth-with-webauthn/login-begin",
		map[string]any{"identity": ceremonyIdentity},
		"",
	)
	if status != http.StatusOK {
		t.Fatalf("login-begin: expected 200, got %d (%v)", status, body)
	}

	challenge := challengeFrom(t, body)
	sessionToken := sessionTokenFrom(t, body)

	payload := auth.assertion(t, challenge, ceremonyAppURL, ceremonyUserID)
	payload["sessionToken"] = sessionToken
	payload["identity"] = ceremonyIdentity

	status, _ = env.do(
		http.MethodPost,
		"/api/collections/users/auth-with-webauthn/login-finish",
		payload,
		"",
	)
	if status != http.StatusOK {
		t.Fatalf("first login: expected 200, got %d", status)
	}

	// replaying the same assertion must fail (session already consumed)
	status, body = env.do(
		http.MethodPost,
		"/api/collections/users/auth-with-webauthn/login-finish",
		payload,
		"",
	)
	if status == http.StatusOK {
		t.Fatalf("replayed assertion was accepted: %v", body)
	}
}

// TestWebAuthnCeremonySignCountRegressionRejected covers the fork's own
// cloned-authenticator hardening (audit M3): a sign count that goes backwards
// must be refused.
func TestWebAuthnCeremonySignCountRegressionRejected(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	auth := newSoftAuthenticator(t, ceremonyRPID)

	registerPasskey(t, env, auth, "primary key")

	// advance the stored sign count via a legitimate login
	auth.signCount = 50
	if status, body := loginWithPasskey(t, env, auth); status != http.StatusOK {
		t.Fatalf("priming login: expected 200, got %d (%v)", status, body)
	}

	// now present a lower counter, as a cloned authenticator would
	auth.signCount = 2
	status, body := loginWithPasskey(t, env, auth)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for a sign-count regression, got %d (%v)", status, body)
	}
}

// TestWebAuthnCeremonyStoredCredentialShape verifies the credential fields the
// fork persists survive a real registration. FromWebAuthnCredential is the
// boundary most exposed to a library struct change, so assert on it directly.
func TestWebAuthnCeremonyStoredCredentialShape(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	auth := newSoftAuthenticator(t, ceremonyRPID)

	registerPasskey(t, env, auth, "shape check")

	rec, err := env.app.FindFirstRecordByFilter(
		core.CollectionNameWebAuthnCredentials,
		"credentialId = {:id}",
		map[string]any{"id": base64.RawURLEncoding.EncodeToString(auth.credID)},
	)
	if err != nil {
		t.Fatalf("credential was not persisted: %v", err)
	}

	if got := rec.GetString("publicKey"); got == "" {
		t.Fatal("publicKey was not stored")
	}

	// the stored public key must be exactly the COSE key the authenticator
	// generated, base64url-encoded (see FromWebAuthnCredential)
	stored, err := base64.RawURLEncoding.DecodeString(rec.GetString("publicKey"))
	if err != nil {
		t.Fatalf("stored publicKey is not decodable: %v", err)
	}
	if !bytes.Equal(stored, auth.coseKey(t)) {
		t.Error("stored public key does not match the authenticator's COSE key")
	}

	// the persisted record must rebuild into a usable library credential,
	// which is the boundary most exposed to a library struct change
	wc := core.NewWebAuthnCredential(env.app)
	wc.SetProxyRecord(rec)

	cred, err := wc.ToWebAuthnCredential()
	if err != nil {
		t.Fatalf("stored credential does not round-trip: %v", err)
	}
	if !bytes.Equal(cred.ID, auth.credID) {
		t.Error("round-tripped credential id mismatch")
	}
	if !bytes.Equal(cred.PublicKey, auth.coseKey(t)) {
		t.Error("round-tripped public key mismatch")
	}

	// the backup flags the authenticator set must survive the round trip
	if !cred.Flags.BackupEligible || !cred.Flags.BackupState {
		t.Errorf("backup flags lost in round trip: %+v", cred.Flags)
	}
}

// TestWebAuthnCeremonyMFAChallenge verifies that a passkey works as the first
// factor when the collection requires MFA: the ceremony must still succeed and
// the response must be a second-factor challenge rather than a full session.
func TestWebAuthnCeremonyMFAChallenge(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnvWithMFA(t, true)
	auth := newSoftAuthenticator(t, ceremonyRPID)

	registerPasskey(t, env, auth, "mfa key")

	status, body := loginWithPasskey(t, env, auth)
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 MFA challenge, got %d (%v)", status, body)
	}

	if mfaID, _ := body["mfaId"].(string); mfaID == "" {
		t.Fatalf("expected an mfaId in the MFA challenge, got %v", body)
	}

	// a full session must NOT be issued on the first factor alone
	if token, _ := body["token"].(string); token != "" {
		t.Fatal("an auth token was issued despite MFA being required")
	}
}
