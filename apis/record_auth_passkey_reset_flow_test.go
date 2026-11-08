package apis_test

import (
	"errors"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
)

// These tests drive the emailed passkey-reset flow end to end with a software
// authenticator: token -> confirm-passkey-reset/begin -> /finish. They exist
// because the flow shipped dead (#39: FindAuthRecordByToken did not know the
// passkeyReset token type, so every real token was rejected) and nothing but
// guard-path tests noticed (#42).

const (
	resetBeginURL  = "/api/collections/users/confirm-passkey-reset/begin"
	resetFinishURL = "/api/collections/users/confirm-passkey-reset/finish"
)

func resetUser(t testing.TB, env *ceremonyEnv) *core.Record {
	t.Helper()

	user, err := env.app.FindRecordById("users", ceremonyUserID)
	if err != nil {
		t.Fatal(err)
	}

	return user
}

func passkeyCredentialRows(t testing.TB, env *ceremonyEnv, userID string) []*core.Record {
	t.Helper()

	rows, err := env.app.FindRecordsByFilter(
		core.CollectionNameWebAuthnCredentials,
		"recordRef = {:id}",
		"",
		0,
		0,
		map[string]any{"id": userID},
	)
	if err != nil {
		t.Fatal(err)
	}

	return rows
}

// resetBegin posts the token to /begin and returns status and body.
func resetBegin(env *ceremonyEnv, token string) (int, map[string]any) {
	return env.do(http.MethodPost, resetBeginURL, map[string]any{"token": token}, "")
}

// resetFinish runs begin + finish with the given authenticator and returns the
// finish response.
func resetFinish(t testing.TB, env *ceremonyEnv, token string, auth *softAuthenticator, name string) (int, map[string]any) {
	t.Helper()

	status, body := resetBegin(env, token)
	if status != http.StatusOK {
		t.Fatalf("reset begin: expected 200, got %d (%v)", status, body)
	}

	payload := auth.attestation(t, challengeFrom(t, body), ceremonyAppURL)
	payload["sessionToken"] = sessionTokenFrom(t, body)
	payload["token"] = token
	payload["name"] = name

	return env.do(http.MethodPost, resetFinishURL, payload, "")
}

func TestPasskeyResetFlowReplacesCredential(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	oldAuth := newSoftAuthenticator(t, "example.com")
	newAuth := newSoftAuthenticator(t, "example.com")

	oldCredID := registerPasskey(t, env, oldAuth, "old laptop")
	if rows := passkeyCredentialRows(t, env, ceremonyUserID); len(rows) != 1 {
		t.Fatalf("expected 1 credential before reset, got %d", len(rows))
	}

	token, err := resetUser(t, env).NewPasskeyResetToken()
	if err != nil {
		t.Fatal(err)
	}

	status, body := resetFinish(t, env, token, newAuth, "new phone")
	if status != http.StatusCreated {
		t.Fatalf("reset finish: expected 201, got %d (%v)", status, body)
	}
	if body["name"] != "new phone" {
		t.Fatalf("expected the new credential name in the response, got %v", body)
	}
	newCredID, _ := body["id"].(string)
	if newCredID == "" || newCredID == oldCredID {
		t.Fatalf("expected a fresh credential id, got %q (old %q)", newCredID, oldCredID)
	}

	// exactly one credential row remains: the new one
	rows := passkeyCredentialRows(t, env, ceremonyUserID)
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 credential after reset, got %d", len(rows))
	}
	if rows[0].Id != newCredID {
		t.Fatalf("expected the surviving row to be the new credential %q, got %q", newCredID, rows[0].Id)
	}
	if _, err := env.app.FindRecordById(core.CollectionNameWebAuthnCredentials, oldCredID); err == nil {
		t.Fatal("expected the old credential row to be deleted")
	}

	// the old key can no longer log in; the new one can
	if status, body := loginWithPasskey(t, env, oldAuth); status == http.StatusOK {
		t.Fatalf("expected the old passkey to be rejected after reset, got 200 (%v)", body)
	}
	if status, body := loginWithPasskey(t, env, newAuth); status != http.StatusOK {
		t.Fatalf("expected the new passkey to log in, got %d (%v)", status, body)
	}
}

func TestPasskeyResetFlowTokenIsSingleUse(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	token, err := resetUser(t, env).NewPasskeyResetToken()
	if err != nil {
		t.Fatal(err)
	}

	firstAuth := newSoftAuthenticator(t, "example.com")
	if status, body := resetFinish(t, env, token, firstAuth, "first"); status != http.StatusCreated {
		t.Fatalf("first use: expected 201, got %d (%v)", status, body)
	}

	// replay at begin
	if status, body := resetBegin(env, token); status != http.StatusBadRequest {
		t.Fatalf("replayed token at begin: expected 400, got %d (%v)", status, body)
	}

	// replay at finish with a fresh authenticator must not add or replace anything
	before := passkeyCredentialRows(t, env, ceremonyUserID)
	payload := newSoftAuthenticator(t, "example.com").attestation(t, "AAAA", ceremonyAppURL)
	payload["sessionToken"] = "does-not-matter"
	payload["token"] = token
	if status, body := env.do(http.MethodPost, resetFinishURL, payload, ""); status != http.StatusBadRequest {
		t.Fatalf("replayed token at finish: expected 400, got %d (%v)", status, body)
	}
	after := passkeyCredentialRows(t, env, ceremonyUserID)
	if len(before) != 1 || len(after) != 1 || before[0].Id != after[0].Id {
		t.Fatalf("replay changed credentials: before=%d after=%d", len(before), len(after))
	}

	// the first authenticator still works
	if status, body := loginWithPasskey(t, env, firstAuth); status != http.StatusOK {
		t.Fatalf("expected first passkey to still log in, got %d (%v)", status, body)
	}
}

func TestPasskeyResetFlowRejectsWrongTokens(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	user := resetUser(t, env)

	passwordReset, err := user.NewPasswordResetToken()
	if err != nil {
		t.Fatal(err)
	}
	authTok, err := user.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	verification, err := user.NewVerificationToken()
	if err != nil {
		t.Fatal(err)
	}

	// valid passkey reset token for a record of a different auth collection
	client, err := env.app.FindAuthRecordByEmail("clients", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	otherCollection, err := client.NewPasskeyResetToken()
	if err != nil {
		t.Fatal(err)
	}

	// a passkey reset token signed with the wrong secret (tampered key)
	valid, err := user.NewPasskeyResetToken()
	if err != nil {
		t.Fatal(err)
	}
	tampered := valid[:len(valid)-2] + "xx"

	// expired passkey reset token
	user.Collection().PasskeyResetToken.Duration = 1
	expired, err := user.NewPasskeyResetToken()
	if err != nil {
		t.Fatal(err)
	}
	user.Collection().PasskeyResetToken.Duration = 1800
	time.Sleep(1100 * time.Millisecond)

	scenarios := map[string]string{
		"passwordReset token": passwordReset,
		"auth token":          authTok,
		"verification token":  verification,
		"other collection":    otherCollection,
		"tampered signature":  tampered,
		"expired":             expired,
		"garbage":             "not-a-jwt",
	}

	for name, token := range scenarios {
		t.Run(name, func(t *testing.T) {
			if status, body := resetBegin(env, token); status != http.StatusBadRequest {
				t.Fatalf("begin: expected 400, got %d (%v)", status, body)
			}

			payload := map[string]any{"token": token, "sessionToken": "x"}
			if status, body := env.do(http.MethodPost, resetFinishURL, payload, ""); status != http.StatusBadRequest {
				t.Fatalf("finish: expected 400, got %d (%v)", status, body)
			}
		})
	}

	// nothing was touched
	if rows := passkeyCredentialRows(t, env, ceremonyUserID); len(rows) != 0 {
		t.Fatalf("expected no credentials to be created, got %d", len(rows))
	}
}

// A reset session started with user A's token must not be finishable with
// user B's token (and vice versa), and a rejected finish must not wipe B's
// credentials.
func TestPasskeyResetFlowSessionBoundToTokenOwner(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	userA := resetUser(t, env)
	userB, err := env.app.FindAuthRecordByEmail("users", "test2@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if userA.Id == userB.Id {
		t.Fatal("expected two distinct users")
	}

	registerPasskey(t, env, newSoftAuthenticator(t, "example.com"), "a's key")

	tokenA, _ := userA.NewPasskeyResetToken()
	tokenB, _ := userB.NewPasskeyResetToken()

	// begin as A, try to finish with B's token
	status, body := resetBegin(env, tokenA)
	if status != http.StatusOK {
		t.Fatalf("begin: expected 200, got %d (%v)", status, body)
	}
	attacker := newSoftAuthenticator(t, "example.com")
	payload := attacker.attestation(t, challengeFrom(t, body), ceremonyAppURL)
	payload["sessionToken"] = sessionTokenFrom(t, body)
	payload["token"] = tokenB
	if status, body := env.do(http.MethodPost, resetFinishURL, payload, ""); status != http.StatusBadRequest {
		t.Fatalf("cross-user finish: expected 400, got %d (%v)", status, body)
	}

	if rows := passkeyCredentialRows(t, env, userA.Id); len(rows) != 1 {
		t.Fatalf("expected A's credential to be untouched, got %d rows", len(rows))
	}
	if rows := passkeyCredentialRows(t, env, userB.Id); len(rows) != 0 {
		t.Fatalf("expected no credential for B, got %d rows", len(rows))
	}
}

// Full path including the request endpoint: the token in the emailed message
// (not one minted by the test) must drive the whole flow.
func TestPasskeyResetFlowFromEmail(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)

	status, body := env.do(http.MethodPost, "/api/collections/users/request-passkey-reset",
		map[string]any{"email": ceremonyIdentity}, "")
	if status != http.StatusNoContent {
		t.Fatalf("request: expected 204, got %d (%v)", status, body)
	}

	// the mail is sent from a goroutine
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && env.app.TestMailer.TotalSend() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if env.app.TestMailer.TotalSend() != 1 {
		t.Fatalf("expected 1 email, got %d", env.app.TestMailer.TotalSend())
	}
	m := regexp.MustCompile(`confirm-passkey-reset/([A-Za-z0-9_\-.]+)"`).FindStringSubmatch(env.app.TestMailer.FirstMessage().HTML)
	if m == nil {
		t.Fatalf("no reset link in email: %s", env.app.TestMailer.FirstMessage().HTML)
	}
	token := m[1]

	if status, body := resetFinish(t, env, token, newSoftAuthenticator(t, "example.com"), "from email"); status != http.StatusCreated {
		t.Fatalf("finish: expected 201, got %d (%v)", status, body)
	}
}

// A failure while finishing the reset must not report success, must leave the
// user's existing passkey in place, and must leave the link usable for a retry
// (the old code returned 201 when rotating the token key failed, leaving the
// link replayable).
func TestPasskeyResetFlowFailureIsAtomic(t *testing.T) {
	t.Parallel()

	scenarios := map[string]func(env *ceremonyEnv){
		"token key rotation fails": func(env *ceremonyEnv) {
			env.app.OnRecordUpdate("users").BindFunc(func(e *core.RecordEvent) error {
				return errors.New("forced users update failure")
			})
		},
		"credential save fails": func(env *ceremonyEnv) {
			env.app.OnRecordCreate(core.CollectionNameWebAuthnCredentials).BindFunc(func(e *core.RecordEvent) error {
				return errors.New("forced credential create failure")
			})
		},
	}

	for name, breakIt := range scenarios {
		t.Run(name, func(t *testing.T) {
			env := newCeremonyEnv(t)
			oldAuth := newSoftAuthenticator(t, "example.com")
			oldCredID := registerPasskey(t, env, oldAuth, "old")

			token, err := resetUser(t, env).NewPasskeyResetToken()
			if err != nil {
				t.Fatal(err)
			}

			status, body := resetBegin(env, token)
			if status != http.StatusOK {
				t.Fatalf("begin: expected 200, got %d (%v)", status, body)
			}
			payload := newSoftAuthenticator(t, "example.com").attestation(t, challengeFrom(t, body), ceremonyAppURL)
			payload["sessionToken"] = sessionTokenFrom(t, body)
			payload["token"] = token

			breakIt(env)

			status, body = env.do(http.MethodPost, resetFinishURL, payload, "")
			if status != http.StatusInternalServerError {
				t.Fatalf("finish: expected 500, got %d (%v)", status, body)
			}

			rows := passkeyCredentialRows(t, env, ceremonyUserID)
			if len(rows) != 1 || rows[0].Id != oldCredID {
				t.Fatalf("expected the old credential to survive a failed reset, got %d rows", len(rows))
			}
			if status, body := loginWithPasskey(t, env, oldAuth); status != http.StatusOK {
				t.Fatalf("expected the old passkey to still log in, got %d (%v)", status, body)
			}

			// the link was not spent
			if status, body := resetBegin(env, token); status != http.StatusOK {
				t.Fatalf("expected the token to remain valid, got %d (%v)", status, body)
			}
		})
	}
}
