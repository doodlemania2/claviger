package apis_test

import (
	"net/http"
	"testing"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #93): sessions under the passkey flows.

func ceremonySession(t testing.TB, env *ceremonyEnv, token string) *core.Record {
	t.Helper()
	sid := sidOf(t, token)
	if sid == "" {
		t.Fatal("the token has no sid")
	}
	r, err := env.app.FindRecordById(core.CollectionNameSessions, sid)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSessionPasskeyLoginRefreshAndStepUp(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	auth := newSoftAuthenticator(t, ceremonyRPID)
	registerPasskey(t, env, auth, "laptop")

	status, body := loginWithPasskey(t, env, auth)
	if status != http.StatusOK {
		t.Fatalf("login: %d %v", status, body)
	}
	token, _ := body["token"].(string)
	sid := sidOf(t, token)
	if m := ceremonySession(t, env, token).GetString("authMethod"); m != "webauthn" {
		t.Fatalf("method %q", m)
	}
	if _, ok := claimsOf(t, token)["passkeyAt"]; !ok {
		t.Fatal("the passkey token lost passkeyAt")
	}

	// refresh: same sid, no passkeyAt
	status, body = env.do(http.MethodPost, "/api/collections/users/auth-refresh", nil, token)
	if status != http.StatusOK {
		t.Fatalf("refresh: %d %v", status, body)
	}
	refreshed, _ := body["token"].(string)
	if sidOf(t, refreshed) != sid {
		t.Fatal("the refresh changed the sid")
	}
	if _, ok := claimsOf(t, refreshed)["passkeyAt"]; ok {
		t.Fatal("the refresh carried passkeyAt")
	}

	// step-up: same sid, fresh passkeyAt again
	stepped := stepUp(t, env, auth, refreshed)
	if sidOf(t, stepped) != sid {
		t.Fatal("the step-up changed the sid")
	}
	if _, ok := claimsOf(t, stepped)["passkeyAt"]; !ok {
		t.Fatal("the step-up token has no passkeyAt")
	}
	if n, _ := env.app.CountRecords(core.CollectionNameSessions); n != 1 {
		t.Fatalf("expected one session for the whole story, got %d", n)
	}

	// revoking the session kills the original, the refreshed and the stepped-up token
	if _, err := core.RevokeSessions(env.app, ceremonySession(t, env, token).GetString("collectionRef"), ceremonyUserID, "test", ""); err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"login": token, "refreshed": refreshed, "stepped": stepped} {
		if status, _ := env.do(http.MethodPost, "/api/collections/users/auth-refresh", nil, tok); status != http.StatusUnauthorized {
			t.Fatalf("%s token after the revocation: %d", name, status)
		}
	}
}

func TestSessionDiscoverableLogin(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	enableDiscoverable(t, env)
	auth := newSoftAuthenticator(t, ceremonyRPID)
	registerPasskey(t, env, auth, "phone")

	challenge, session := discoverableBegin(t, env)
	payload := auth.assertion(t, challenge, ceremonyAppURL, ceremonyUserID)
	payload["sessionToken"] = session
	status, body := env.do(http.MethodPost, discFinishURL, payload, "")
	if status != http.StatusOK {
		t.Fatalf("finish: %d %v", status, body)
	}
	token, _ := body["token"].(string)
	if m := ceremonySession(t, env, token).GetString("authMethod"); m != "discoverable" {
		t.Fatalf("method %q", m)
	}
}
