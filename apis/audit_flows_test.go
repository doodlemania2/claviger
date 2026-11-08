package apis_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/auth"
	"golang.org/x/oauth2"
)

// Fork-local (Claviger #110): every audited write done on behalf of an API
// request carries an actor and the client IP, including the flows that run
// without an authenticated session (the actor is then the record itself,
// flagged meta.self=true).

func clearAuditLog(t testing.TB, app core.App) {
	t.Helper()
	if _, err := core.DeleteExpiredAuditEntries(app, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// requireAttributed fails if any entry in the log is attributed to system or
// has no IP, and returns the entries.
func requireAttributed(t testing.TB, app core.App, minEntries int) []*core.Record {
	t.Helper()

	entries, err := app.FindAllRecords(core.CollectionNameAuditLog)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < minEntries {
		t.Fatalf("Expected at least %d entries, got %d", minEntries, len(entries))
	}
	for _, e := range entries {
		if e.GetString("actorType") == core.AuditActorTypeSystem || e.GetString("actorId") == "" || e.GetString("ip") == "" {
			t.Fatalf("Entry without an actor/ip: %s", e.PublicExport())
		}
	}

	return entries
}

func requireEntry(t testing.TB, entries []*core.Record, action, target string) *core.Record {
	t.Helper()
	for _, e := range entries {
		if e.GetString("action") == action && e.GetString("targetCollection") == target {
			return e
		}
	}
	t.Fatalf("No %s entry for %s among %d entries", action, target, len(entries))
	return nil
}

func requireSelf(t testing.TB, entries []*core.Record, action, target, actorId string) {
	t.Helper()
	e := requireEntry(t, entries, action, target)
	if e.GetString("actorId") != actorId || e.GetString("actorType") != core.AuditActorTypeRecord ||
		e.GetString("actorCollection") != "users" || !strings.Contains(e.GetString("meta"), `"self":true`) {
		t.Fatalf("Expected a self actor %s, got %s", actorId, e.PublicExport())
	}
}

func TestAuditFlowsConfirmTokens(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	user, err := env.app.FindAuthRecordByEmail("users", ceremonyIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if user.Verified() {
		t.Fatal("expected the fixture user to be unverified")
	}
	clearAuditLog(t, env.app)

	token, _ := user.NewVerificationToken()
	if s, b := env.do(http.MethodPost, "/api/collections/users/confirm-verification", map[string]any{"token": token}, ""); s != http.StatusNoContent {
		t.Fatalf("verification: %d %v", s, b)
	}
	requireSelf(t, requireAttributed(t, env.app, 1), "update", "users", user.Id)
	clearAuditLog(t, env.app)

	user, _ = env.app.FindAuthRecordByEmail("users", ceremonyIdentity)
	token, _ = user.NewEmailChangeToken("audit-new@example.com")
	if s, b := env.do(http.MethodPost, "/api/collections/users/confirm-email-change", map[string]any{"token": token, "password": "1234567890"}, ""); s != http.StatusNoContent {
		t.Fatalf("email change: %d %v", s, b)
	}
	requireSelf(t, requireAttributed(t, env.app, 1), "update", "users", user.Id)
	clearAuditLog(t, env.app)

	user, _ = env.app.FindAuthRecordByEmail("users", "audit-new@example.com")
	token, _ = user.NewPasswordResetToken()
	if s, b := env.do(http.MethodPost, "/api/collections/users/confirm-password-reset", map[string]any{
		"token": token, "password": "newpassword123", "passwordConfirm": "newpassword123",
	}, ""); s != http.StatusNoContent {
		t.Fatalf("password reset: %d %v", s, b)
	}
	entries := requireAttributed(t, env.app, 1)
	requireSelf(t, entries, "update", "users", user.Id)
	if strings.Contains(requireEntry(t, entries, "update", "users").GetString("after"), "newpassword123") {
		t.Fatal("password leaked into the entry")
	}
}

func TestAuditFlowsOTPVerifiedUpgrade(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	user, _ := env.app.FindAuthRecordByEmail("users", ceremonyIdentity)
	otp := core.NewOTP(env.app)
	otp.SetCollectionRef(user.Collection().Id)
	otp.SetRecordRef(user.Id)
	otp.SetSentTo(user.Email())
	otp.SetPassword("123456")
	if err := env.app.Save(otp); err != nil {
		t.Fatal(err)
	}
	clearAuditLog(t, env.app)

	if s, b := env.do(http.MethodPost, "/api/collections/users/auth-with-otp", map[string]any{"otpId": otp.Id, "password": "123456"}, ""); s != http.StatusOK {
		t.Fatalf("otp: %d %v", s, b)
	}
	requireSelf(t, requireAttributed(t, env.app, 1), "update", "users", user.Id)
}

func TestAuditFlowsPasskeys(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	clearAuditLog(t, env.app)

	authenticator := newSoftAuthenticator(t, "example.com")
	credId := registerPasskey(t, env, authenticator, "laptop")

	// deleting a passkey needs a fresh passkey assertion
	s, b := loginWithPasskey(t, env, authenticator)
	if s != http.StatusOK {
		t.Fatalf("login: %d %v", s, b)
	}
	freshToken, _ := b["token"].(string)
	clearAuditLog(t, env.app)
	registerPasskey(t, env, newSoftAuthenticator(t, "example.com"), "second")
	created := requireEntry(t, requireAttributed(t, env.app, 1), "create", core.CollectionNameWebAuthnCredentials)
	if created.GetString("actorId") != ceremonyUserID {
		t.Fatalf("Unexpected actor: %s", created.PublicExport())
	}
	clearAuditLog(t, env.app)

	credURL := "/api/collections/users/auth-with-webauthn/credentials/" + credId
	if s, b := env.do(http.MethodPatch, credURL, map[string]any{"name": "renamed"}, freshToken); s != http.StatusOK {
		t.Fatalf("rename: %d %v", s, b)
	}
	if s, b := env.do(http.MethodDelete, credURL, nil, freshToken); s != http.StatusNoContent {
		t.Fatalf("delete: %d %v", s, b)
	}

	entries := requireAttributed(t, env.app, 2)
	for _, action := range []string{"update", "delete"} {
		e := requireEntry(t, entries, action, core.CollectionNameWebAuthnCredentials)
		if e.GetString("actorId") != ceremonyUserID || e.GetString("actorType") != core.AuditActorTypeRecord {
			t.Fatalf("Unexpected actor: %s", e.PublicExport())
		}
	}

	// passkey reset confirm (unauthenticated, authorized by the emailed token)
	registerPasskey(t, env, newSoftAuthenticator(t, "example.com"), "old")
	user, _ := env.app.FindAuthRecordByEmail("users", ceremonyIdentity)
	token, _ := user.NewPasskeyResetToken()
	clearAuditLog(t, env.app)

	if s, b := resetFinish(t, env, token, newSoftAuthenticator(t, "example.com"), "new"); s != http.StatusCreated {
		t.Fatalf("reset: %d %v", s, b)
	}
	entries = requireAttributed(t, env.app, 3)
	requireSelf(t, entries, "delete", core.CollectionNameWebAuthnCredentials, ceremonyUserID)
	requireSelf(t, entries, "create", core.CollectionNameWebAuthnCredentials, ceremonyUserID)
	requireSelf(t, entries, "update", "users", ceremonyUserID)
}

func TestAuditFlowsTOTP(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	bearer := totpBearer(t, env, "users", ceremonyIdentity)
	clearAuditLog(t, env.app)

	secret, codes := totpEnroll(t, env, "users", bearer)
	entries := requireAttributed(t, env.app, 2)
	requireEntry(t, entries, "create", core.CollectionNameTOTPs)
	requireEntry(t, entries, "update", core.CollectionNameTOTPs)
	clearAuditLog(t, env.app)

	// backup code used at sign-in: authorized by the password + code, no session
	s, b := totpPasswordSignIn(env, "users", ceremonyIdentity)
	if s != http.StatusUnauthorized {
		t.Fatalf("expected a challenge, got %d", s)
	}
	if s, b = env.do(http.MethodPost, totpURL("users", "auth-with-totp"), map[string]any{"mfaId": b["mfaId"], "code": codes[0]}, ""); s != http.StatusOK {
		t.Fatalf("backup code sign-in: %d %v", s, b)
	}
	requireSelf(t, requireAttributed(t, env.app, 1), "update", core.CollectionNameTOTPs, ceremonyUserID)
	clearAuditLog(t, env.app)

	if s, b := env.do(http.MethodPost, totpURL("users", "totp/disable"), map[string]any{"code": totpCode(t, secret, 1)}, bearer); s != http.StatusNoContent {
		t.Fatalf("disable: %d %v", s, b)
	}
	requireEntry(t, requireAttributed(t, env.app, 1), "delete", core.CollectionNameTOTPs)
}

func TestAuditFlowsOAuth2(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	const providerName = "auditflowoauth"
	auth.Providers[providerName] = func() auth.Provider {
		return &oauth2MockProvider{
			AuthUser: &auth.AuthUser{Id: "audit_flow_id", Email: "audit-oauth-new@example.com", Name: "New"},
			Token:    &oauth2.Token{AccessToken: "abc"},
		}
	}

	users, _ := env.app.FindCollectionByNameOrId("users")
	users.MFA.Enabled = false
	users.OAuth2.Enabled = true
	users.OAuth2.Providers = []core.OAuth2ProviderConfig{{Name: providerName, ClientId: "123", ClientSecret: "456"}}
	if err := env.app.Save(users); err != nil {
		t.Fatal(err)
	}
	clearAuditLog(t, env.app)

	// sign-up: the created record is its own actor
	if s, b := env.do(http.MethodPost, "/api/collections/users/auth-with-oauth2", map[string]any{
		"provider": providerName, "code": "123", "redirectURL": "https://example.com",
	}, ""); s != http.StatusOK {
		t.Fatalf("oauth2 sign-up: %d %v", s, b)
	}
	created, err := env.app.FindAuthRecordByEmail("users", "audit-oauth-new@example.com")
	if err != nil {
		t.Fatal(err)
	}
	entries := requireAttributed(t, env.app, 2)
	requireSelf(t, entries, "create", "users", created.Id)
	requireSelf(t, entries, "create", core.CollectionNameExternalAuths, created.Id)

	// link to an existing, unverified record without a session
	existing, _ := env.app.FindAuthRecordByEmail("users", ceremonyIdentity)
	auth.Providers[providerName] = func() auth.Provider {
		return &oauth2MockProvider{
			AuthUser: &auth.AuthUser{Id: "audit_flow_id_2", Email: existing.Email()},
			Token:    &oauth2.Token{AccessToken: "abc"},
		}
	}
	clearAuditLog(t, env.app)
	if s, b := env.do(http.MethodPost, "/api/collections/users/auth-with-oauth2", map[string]any{
		"provider": providerName, "code": "123", "redirectURL": "https://example.com",
	}, ""); s != http.StatusOK {
		t.Fatalf("oauth2 link: %d %v", s, b)
	}
	entries = requireAttributed(t, env.app, 2)
	requireSelf(t, entries, "update", "users", existing.Id)
	requireSelf(t, entries, "create", core.CollectionNameExternalAuths, existing.Id)
}
