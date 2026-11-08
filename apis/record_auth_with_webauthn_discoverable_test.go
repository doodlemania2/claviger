package apis_test

import (
	"net/http"
	"reflect"
	"testing"
)

const (
	discBeginURL  = "/api/collections/users/auth-with-webauthn/login-begin-discoverable"
	discFinishURL = "/api/collections/users/auth-with-webauthn/login-finish-discoverable"
)

func enableDiscoverable(t testing.TB, env *ceremonyEnv) {
	t.Helper()

	users, err := env.app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	users.WebAuthn.Discoverable = true
	if err := env.app.Save(users); err != nil {
		t.Fatal(err)
	}
}

func discoverableBegin(t testing.TB, env *ceremonyEnv) (challenge, session string) {
	t.Helper()

	status, body := env.do(http.MethodPost, discBeginURL, map[string]any{}, "")
	if status != http.StatusOK {
		t.Fatalf("discoverable begin: expected 200, got %d (%v)", status, body)
	}

	return challengeFrom(t, body), sessionTokenFrom(t, body)
}

func TestWebAuthnDiscoverableLogin(t *testing.T) {
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
		t.Fatalf("discoverable finish: expected 200, got %d (%v)", status, body)
	}
	if tok, _ := body["token"].(string); tok == "" {
		t.Fatalf("expected a token, got %v", body)
	}
	if rec, _ := body["record"].(map[string]any); rec["id"] != ceremonyUserID {
		t.Fatalf("authenticated as %v, want %s", rec["id"], ceremonyUserID)
	}

	// lastUsed was recorded by the shared tail
	if items := listPasskeys(t, env); items[0]["lastUsed"] == nil {
		t.Fatal("expected lastUsed to be set after a discoverable sign-in")
	}

	// the session is single-use
	if status, _ := env.do(http.MethodPost, discFinishURL, payload, ""); status == http.StatusOK {
		t.Fatal("replayed discoverable assertion was accepted")
	}
}

func TestWebAuthnDiscoverableBeginHasNoIdentityOracle(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	enableDiscoverable(t, env)

	status, body := env.do(http.MethodPost, discBeginURL, map[string]any{}, "")
	if status != http.StatusOK {
		t.Fatalf("begin: %d %v", status, body)
	}
	pk := body["options"].(map[string]any)["publicKey"].(map[string]any)
	if allow, ok := pk["allowCredentials"]; ok && len(allow.([]any)) != 0 {
		t.Fatalf("allowCredentials must be empty, got %v", allow)
	}

	// an identity in the body changes nothing: it is ignored, so the response
	// shape carries no per-account information
	status, body2 := env.do(http.MethodPost, discBeginURL, map[string]any{"identity": ceremonyIdentity}, "")
	if status != http.StatusOK {
		t.Fatalf("begin with identity: %d", status)
	}
	pk2 := body2["options"].(map[string]any)["publicKey"].(map[string]any)
	delete(pk, "challenge")
	delete(pk2, "challenge")
	if !reflect.DeepEqual(pk, pk2) {
		t.Fatalf("options differ with an identity present:\n%v\n%v", pk, pk2)
	}
}

func TestWebAuthnDiscoverableDisabledByDefault(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t) // Discoverable not set
	if status, _ := env.do(http.MethodPost, discBeginURL, map[string]any{}, ""); status != http.StatusForbidden {
		t.Fatalf("begin: expected 403, got %d", status)
	}
	if status, _ := env.do(http.MethodPost, discFinishURL, map[string]any{"sessionToken": "x"}, ""); status != http.StatusForbidden {
		t.Fatalf("finish: expected 403, got %d", status)
	}
}

func TestWebAuthnDiscoverableRejections(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	enableDiscoverable(t, env)
	auth := newSoftAuthenticator(t, ceremonyRPID)
	registerPasskey(t, env, auth, "phone")

	finish := func(a *softAuthenticator, userHandle, origin string, mutate func(map[string]any)) (int, map[string]any) {
		challenge, session := discoverableBegin(t, env)
		payload := a.assertion(t, challenge, origin, userHandle)
		payload["sessionToken"] = session
		if mutate != nil {
			mutate(payload)
		}
		return env.do(http.MethodPost, discFinishURL, payload, "")
	}

	impostor := newSoftAuthenticator(t, ceremonyRPID)
	impostor.credID = auth.credID

	cases := map[string]func() (int, map[string]any){
		"unknown user handle": func() (int, map[string]any) { return finish(auth, "nosuchrecord000", ceremonyAppURL, nil) },
		"wrong key":           func() (int, map[string]any) { return finish(impostor, ceremonyUserID, ceremonyAppURL, nil) },
		"wrong origin":        func() (int, map[string]any) { return finish(auth, ceremonyUserID, "https://evil.example.net", nil) },
		"missing user handle": func() (int, map[string]any) {
			return finish(auth, ceremonyUserID, ceremonyAppURL, func(p map[string]any) {
				p["response"].(map[string]any)["userHandle"] = nil
			})
		},
		"unknown session": func() (int, map[string]any) {
			return finish(auth, ceremonyUserID, ceremonyAppURL, func(p map[string]any) { p["sessionToken"] = "nope" })
		},
	}

	var bodies []map[string]any
	for name, run := range cases {
		status, body := run()
		if status != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d (%v)", name, status, body)
		}
		bodies = append(bodies, body)
	}
	// every refusal is byte-identical: no oracle on why it failed
	for _, b := range bodies[1:] {
		if !reflect.DeepEqual(bodies[0], b) {
			t.Fatalf("refusals differ: %v vs %v", bodies[0], b)
		}
	}
}

func TestWebAuthnDiscoverableSessionPurposeIsolation(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	enableDiscoverable(t, env)
	auth := newSoftAuthenticator(t, ceremonyRPID)
	registerPasskey(t, env, auth, "phone")

	// a discoverable session cannot finish an identity-bound login
	challenge, session := discoverableBegin(t, env)
	payload := auth.assertion(t, challenge, ceremonyAppURL, ceremonyUserID)
	payload["sessionToken"] = session
	payload["identity"] = ceremonyIdentity
	if status, _ := env.do(http.MethodPost, "/api/collections/users/auth-with-webauthn/login-finish", payload, ""); status != http.StatusBadRequest {
		t.Fatalf("discoverable session used at login-finish: expected 400, got %d", status)
	}

	// an identity-bound session cannot finish a discoverable login
	status, body := env.do(http.MethodPost, "/api/collections/users/auth-with-webauthn/login-begin", map[string]any{"identity": ceremonyIdentity}, "")
	if status != http.StatusOK {
		t.Fatal(status)
	}
	payload = auth.assertion(t, challengeFrom(t, body), ceremonyAppURL, ceremonyUserID)
	payload["sessionToken"] = sessionTokenFrom(t, body)
	if status, _ := env.do(http.MethodPost, discFinishURL, payload, ""); status != http.StatusBadRequest {
		t.Fatalf("login session used at discoverable finish: expected 400, got %d", status)
	}
}

// Discoverable begin spends from the same per-IP budget as login-begin.
func TestWebAuthnDiscoverableSharesPerIPBudget(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	enableDiscoverable(t, env)

	for i := 0; i < 20; i++ {
		if status, _ := env.do(http.MethodPost, discBeginURL, map[string]any{}, ""); status != http.StatusOK {
			t.Fatalf("begin %d: expected 200, got %d", i, status)
		}
	}
	if status, _ := env.do(http.MethodPost, discBeginURL, map[string]any{}, ""); status != http.StatusTooManyRequests {
		t.Fatalf("21st discoverable begin: expected 429, got %d", status)
	}
	// the identity-bound begin from the same IP is also refused: one shared budget
	if status, _ := env.do(http.MethodPost, "/api/collections/users/auth-with-webauthn/login-begin", map[string]any{"identity": ceremonyIdentity}, ""); status != http.StatusTooManyRequests {
		t.Fatalf("login-begin after the budget was spent: expected 429, got %d", status)
	}
}

// Registration asks for a discoverable credential only when the option is on.
func TestWebAuthnRegistrationResidentKeyOnlyWhenDiscoverable(t *testing.T) {
	t.Parallel()

	residentKey := func(env *ceremonyEnv) any {
		status, body := env.do(http.MethodPost, "/api/collections/users/auth-with-webauthn/register-begin", map[string]any{}, ceremonyUserToken)
		if status != http.StatusOK {
			t.Fatalf("register-begin: %d %v", status, body)
		}
		pk := body["options"].(map[string]any)["publicKey"].(map[string]any)
		sel, _ := pk["authenticatorSelection"].(map[string]any)
		return sel["residentKey"]
	}

	off := newCeremonyEnv(t)
	if got := residentKey(off); got != nil && got != "" {
		t.Fatalf("default config must not request a resident key, got %v", got)
	}

	on := newCeremonyEnv(t)
	enableDiscoverable(t, on)
	if got := residentKey(on); got != "preferred" {
		t.Fatalf("expected residentKey=preferred, got %v", got)
	}
}
