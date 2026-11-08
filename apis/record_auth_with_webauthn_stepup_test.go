package apis_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/hook"
)

const (
	stepUpBeginURL  = "/api/collections/users/auth-with-webauthn/step-up-begin"
	stepUpFinishURL = "/api/collections/users/auth-with-webauthn/step-up-finish"
)

// freshUserToken returns a token for the ceremony user that carries a passkey
// assertion made now. It builds the token from a throwaway app holding the
// same fixture data, so it can be used as a static scenario header.
func freshUserToken(t testing.TB) string {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	user, err := app.FindRecordById("users", ceremonyUserID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := user.NewPasskeyAuthToken(time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}

	return token
}

func userTokenAt(t testing.TB, env *ceremonyEnv, at time.Time) string {
	t.Helper()

	user := resetUser(t, env)
	token, err := user.NewPasskeyAuthToken(at, true)
	if err != nil {
		t.Fatal(err)
	}

	return token
}

// stepUp runs the full step-up ceremony and returns the replacement token.
func stepUp(t testing.TB, env *ceremonyEnv, auth *softAuthenticator, bearer string) string {
	t.Helper()

	status, body := env.do(http.MethodPost, stepUpBeginURL, map[string]any{}, bearer)
	if status != http.StatusOK {
		t.Fatalf("step-up begin: expected 200, got %d (%v)", status, body)
	}
	payload := auth.assertion(t, challengeFrom(t, body), ceremonyAppURL, ceremonyUserID)
	payload["sessionToken"] = sessionTokenFrom(t, body)

	status, body = env.do(http.MethodPost, stepUpFinishURL, payload, bearer)
	if status != http.StatusOK {
		t.Fatalf("step-up finish: expected 200, got %d (%v)", status, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("step-up finish returned no token: %v", body)
	}

	return token
}

func TestWebAuthnDeleteCredentialRequiresFreshPasskey(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	auth := newSoftAuthenticator(t, ceremonyRPID)
	credID := registerPasskey(t, env, auth, "laptop")
	deleteURL := credentialsPath + "/" + credID

	exists := func() bool {
		_, err := env.app.FindRecordById(core.CollectionNameWebAuthnCredentials, credID)
		return err == nil
	}

	// a plain session (no passkeyAt) is refused with the machine-readable code
	status, body := env.do(http.MethodDelete, deleteURL, nil, ceremonyUserToken)
	if status != http.StatusForbidden {
		t.Fatalf("plain token: expected 403, got %d (%v)", status, body)
	}
	data, _ := body["data"].(map[string]any)
	pk, _ := data["passkey"].(map[string]any)
	if pk["code"] != apis.StepUpRequiredCode {
		t.Fatalf("expected data.passkey.code=%s, got %v", apis.StepUpRequiredCode, body)
	}
	if !exists() {
		t.Fatal("credential was deleted without a fresh passkey")
	}

	// an assertion older than maxAge (5 minutes) is refused
	stale := userTokenAt(t, env, time.Now().Add(-6*time.Minute))
	if status, _ := env.do(http.MethodDelete, deleteURL, nil, stale); status != http.StatusForbidden {
		t.Fatalf("stale passkeyAt: expected 403, got %d", status)
	}
	// ... as is one claiming the future
	future := userTokenAt(t, env, time.Now().Add(10*time.Minute))
	if status, _ := env.do(http.MethodDelete, deleteURL, nil, future); status != http.StatusForbidden {
		t.Fatalf("future passkeyAt: expected 403, got %d", status)
	}
	if !exists() {
		t.Fatal("credential was deleted with a stale token")
	}

	// a claim signed with the wrong key must not be honoured
	forged := userTokenAt(t, env, time.Now())
	forged = forged[:len(forged)-3] + strings.Repeat("A", 3)
	if status, _ := env.do(http.MethodDelete, deleteURL, nil, forged); status == http.StatusNoContent {
		t.Fatal("forged token deleted the credential")
	}

	// step up with the passkey, then delete with the replacement token
	fresh := stepUp(t, env, auth, ceremonyUserToken)
	if status, body := env.do(http.MethodDelete, deleteURL, nil, fresh); status != http.StatusNoContent {
		t.Fatalf("fresh token: expected 204, got %d (%v)", status, body)
	}
	if exists() {
		t.Fatal("credential still exists after an authorized delete")
	}
}

// A passkey sign-in token is itself fresh.
func TestWebAuthnPasskeyLoginTokenIsFresh(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	auth := newSoftAuthenticator(t, ceremonyRPID)
	credID := registerPasskey(t, env, auth, "laptop")

	status, body := loginWithPasskey(t, env, auth)
	if status != http.StatusOK {
		t.Fatalf("login: %d %v", status, body)
	}
	token, _ := body["token"].(string)

	if status, body := env.do(http.MethodDelete, credentialsPath+"/"+credID, nil, token); status != http.StatusNoContent {
		t.Fatalf("delete with the passkey login token: expected 204, got %d (%v)", status, body)
	}
}

// Refreshing a token never extends freshness.
func TestWebAuthnAuthRefreshDropsPasskeyClaim(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	auth := newSoftAuthenticator(t, ceremonyRPID)
	credID := registerPasskey(t, env, auth, "laptop")

	status, body := loginWithPasskey(t, env, auth)
	if status != http.StatusOK {
		t.Fatal(status)
	}
	token, _ := body["token"].(string)

	status, body = env.do(http.MethodPost, "/api/collections/users/auth-refresh", nil, token)
	if status != http.StatusOK {
		t.Fatalf("auth-refresh: %d %v", status, body)
	}
	refreshed, _ := body["token"].(string)

	if status, _ := env.do(http.MethodDelete, credentialsPath+"/"+credID, nil, refreshed); status != http.StatusForbidden {
		t.Fatalf("refreshed token must not be fresh: expected 403, got %d", status)
	}
}

func TestWebAuthnStepUpRejections(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	auth := newSoftAuthenticator(t, ceremonyRPID)
	registerPasskey(t, env, auth, "laptop")

	// unauthenticated
	if status, _ := env.do(http.MethodPost, stepUpBeginURL, map[string]any{}, ""); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated begin: expected 401, got %d", status)
	}
	if status, _ := env.do(http.MethodPost, stepUpFinishURL, map[string]any{"sessionToken": "x"}, ""); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated finish: expected 401, got %d", status)
	}

	// wrong key
	impostor := newSoftAuthenticator(t, ceremonyRPID)
	impostor.credID = auth.credID
	status, body := env.do(http.MethodPost, stepUpBeginURL, map[string]any{}, ceremonyUserToken)
	if status != http.StatusOK {
		t.Fatal(status)
	}
	payload := impostor.assertion(t, challengeFrom(t, body), ceremonyAppURL, ceremonyUserID)
	payload["sessionToken"] = sessionTokenFrom(t, body)
	if status, _ := env.do(http.MethodPost, stepUpFinishURL, payload, ceremonyUserToken); status != http.StatusBadRequest {
		t.Fatalf("wrong key: expected 400, got %d", status)
	}

	// a login session cannot be finished as a step-up (purpose isolation)
	status, body = env.do(http.MethodPost, "/api/collections/users/auth-with-webauthn/login-begin", map[string]any{"identity": ceremonyIdentity}, "")
	if status != http.StatusOK {
		t.Fatal(status)
	}
	payload = auth.assertion(t, challengeFrom(t, body), ceremonyAppURL, ceremonyUserID)
	payload["sessionToken"] = sessionTokenFrom(t, body)
	if status, _ := env.do(http.MethodPost, stepUpFinishURL, payload, ceremonyUserToken); status != http.StatusBadRequest {
		t.Fatalf("login session at step-up finish: expected 400, got %d", status)
	}

	// ... and a step-up session cannot be finished as a sign-in
	status, body = env.do(http.MethodPost, stepUpBeginURL, map[string]any{}, ceremonyUserToken)
	if status != http.StatusOK {
		t.Fatal(status)
	}
	payload = auth.assertion(t, challengeFrom(t, body), ceremonyAppURL, ceremonyUserID)
	payload["sessionToken"] = sessionTokenFrom(t, body)
	payload["identity"] = ceremonyIdentity
	if status, _ := env.do(http.MethodPost, "/api/collections/users/auth-with-webauthn/login-finish", payload, ""); status != http.StatusBadRequest {
		t.Fatalf("step-up session at login-finish: expected 400, got %d", status)
	}
}

func TestWebAuthnStepUpNoPasskeyRegistered(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	if status, _ := env.do(http.MethodPost, stepUpBeginURL, map[string]any{}, ceremonyUserToken); status != http.StatusBadRequest {
		t.Fatalf("expected 400 without a registered passkey, got %d", status)
	}
}

// RequireFreshPasskey is reusable on any route.
func TestRequireFreshPasskeyMiddleware(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)

	// build a second mux with a custom gated route on the same app
	router, err := apis.NewRouter(env.app)
	if err != nil {
		t.Fatal(err)
	}
	router.GET("/api/test/danger", func(e *core.RequestEvent) error {
		return e.String(http.StatusOK, "ok")
	}).Bind(apis.RequireFreshPasskey(time.Minute))
	router.Bind(&hook.Handler[*core.RequestEvent]{
		Func: func(re *core.RequestEvent) error { return re.Next() },
	})
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	gated := &ceremonyEnv{t: t, app: env.app, mux: mux}

	if status, _ := gated.do(http.MethodGet, "/api/test/danger", nil, ""); status != http.StatusUnauthorized {
		t.Fatalf("no auth: expected 401, got %d", status)
	}
	if status, _ := gated.do(http.MethodGet, "/api/test/danger", nil, ceremonyUserToken); status != http.StatusForbidden {
		t.Fatalf("no claim: expected 403, got %d", status)
	}
	if status, _ := gated.do(http.MethodGet, "/api/test/danger", nil, userTokenAt(t, env, time.Now().Add(-2*time.Minute))); status != http.StatusForbidden {
		t.Fatalf("older than maxAge: expected 403, got %d", status)
	}
	if status, _ := gated.do(http.MethodGet, "/api/test/danger", nil, userTokenAt(t, env, time.Now())); status != http.StatusOK {
		t.Fatalf("fresh: expected 200, got %d", status)
	}
}
