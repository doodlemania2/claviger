package apis_test

import (
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
)

// End to end for a passkey-only collection (issue #73): password, OAuth2 and
// OTP are off, so the only ways in are a passkey and the emailed passkey
// reset. The user is enrolled through the reset email, signs in, and later
// recovers from a lost device the same way.

var passkeyResetLink = regexp.MustCompile(`confirm-passkey-reset/([^"]+)"`)

func makePasskeyOnly(t testing.TB, env *ceremonyEnv) {
	t.Helper()

	users, err := env.app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	users.PasswordAuth.Enabled = false
	users.OAuth2.Enabled = false
	users.OTP.Enabled = false
	users.MFA.Enabled = false
	users.WebAuthn.Enabled = true
	users.Fields.GetByName(core.FieldNameEmail).(*core.EmailField).Required = true
	if err := env.app.Save(users); err != nil {
		t.Fatalf("a passkey-only collection with a required email must save: %v", err)
	}
}

// requestResetToken asks for a reset email like the user would and returns the
// token from the emailed link.
func requestResetToken(t testing.TB, env *ceremonyEnv) string {
	t.Helper()

	// the per-account resend lock (2 minutes) would swallow a second request
	users, _ := env.app.FindCollectionByNameOrId("users")
	env.app.Store().Remove("@limitPasskeyResetEmail_" + users.Id + ceremonyUserID)
	before := env.app.TestMailer.TotalSend()

	status, body := env.do(http.MethodPost, "/api/collections/users/request-passkey-reset", map[string]any{"email": ceremonyIdentity}, "")
	if status != http.StatusNoContent {
		t.Fatalf("request-passkey-reset: expected 204, got %d (%v)", status, body)
	}

	deadline := time.Now().Add(3 * time.Second)
	for env.app.TestMailer.TotalSend() == before {
		if time.Now().After(deadline) {
			t.Fatal("no passkey reset email was sent")
		}
		time.Sleep(10 * time.Millisecond)
	}

	m := passkeyResetLink.FindStringSubmatch(env.app.TestMailer.LastMessage().HTML)
	if len(m) != 2 {
		t.Fatalf("no reset link in email:\n%s", env.app.TestMailer.LastMessage().HTML)
	}

	return m[1]
}

func TestPasskeyOnlyCollectionEndToEnd(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	makePasskeyOnly(t, env)

	// auth-methods reports the mode
	status, methods := env.do(http.MethodGet, "/api/collections/users/auth-methods", nil, "")
	if status != http.StatusOK {
		t.Fatal(status)
	}
	if pw, _ := methods["password"].(map[string]any); pw["enabled"] != false {
		t.Fatalf("password must be reported disabled: %v", methods["password"])
	}
	if wa, _ := methods["webauthn"].(map[string]any); wa["enabled"] != true || wa["passkeyOnly"] != true {
		t.Fatalf("webauthn must be reported enabled + passkeyOnly: %v", methods["webauthn"])
	}

	// the password route is closed
	if status, _ := env.do(http.MethodPost, "/api/collections/users/auth-with-password", map[string]any{"identity": ceremonyIdentity, "password": "1234567890"}, ""); status == http.StatusOK {
		t.Fatal("password sign-in must be disabled")
	}

	// 1. register: the user has no passkey yet, so the emailed link enrolls the first one
	laptop := newSoftAuthenticator(t, ceremonyRPID)
	token := requestResetToken(t, env)
	if status, body := resetFinish(t, env, token, laptop, "laptop"); status != http.StatusCreated {
		t.Fatalf("enrollment via email: expected 201, got %d (%v)", status, body)
	}

	// 2. sign in with it (identity-bound, and usernameless when enabled)
	status, body := loginWithPasskey(t, env, laptop)
	if status != http.StatusOK {
		t.Fatalf("passkey sign-in: expected 200, got %d (%v)", status, body)
	}
	if tok, _ := body["token"].(string); tok == "" {
		t.Fatalf("no token: %v", body)
	}

	// 3. recover: the laptop is lost, a new device takes over through the email link
	phone := newSoftAuthenticator(t, ceremonyRPID)
	token = requestResetToken(t, env)
	if status, body := resetFinish(t, env, token, phone, "phone"); status != http.StatusCreated {
		t.Fatalf("recovery via email: expected 201, got %d (%v)", status, body)
	}
	if status, _ := loginWithPasskey(t, env, laptop); status == http.StatusOK {
		t.Fatal("the lost device must no longer sign in after recovery")
	}
	if status, body := loginWithPasskey(t, env, phone); status != http.StatusOK {
		t.Fatalf("recovered device sign-in: expected 200, got %d (%v)", status, body)
	}
}

// Usernameless sign-in works in a passkey-only collection too, with the
// registered credential marked discoverable.
func TestPasskeyOnlyDiscoverableEndToEnd(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	makePasskeyOnly(t, env)
	enableDiscoverable(t, env)

	phone := newSoftAuthenticator(t, ceremonyRPID)
	if status, body := resetFinish(t, env, requestResetToken(t, env), phone, "phone"); status != http.StatusCreated {
		t.Fatalf("enrollment: %d %v", status, body)
	}

	challenge, session := discoverableBegin(t, env)
	payload := phone.assertion(t, challenge, ceremonyAppURL, ceremonyUserID)
	payload["sessionToken"] = session
	if status, body := env.do(http.MethodPost, discFinishURL, payload, ""); status != http.StatusOK {
		t.Fatalf("usernameless sign-in: expected 200, got %d (%v)", status, body)
	}
}
