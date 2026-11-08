package apis_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
)

// listPasskeys calls the owner-facing credentials list endpoint.
func listPasskeys(t testing.TB, env *ceremonyEnv) []map[string]any {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/api/collections/users/auth-with-webauthn/credentials", strings.NewReader(""))
	req.Header.Set("Authorization", ceremonyUserToken)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list credentials: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	var items []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("list credentials: bad body %q: %v", rec.Body.String(), err)
	}

	return items
}

func TestWebAuthnLastUsedUpdatedOnLogin(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	auth := newSoftAuthenticator(t, ceremonyRPID)
	registerPasskey(t, env, auth, "laptop")

	items := listPasskeys(t, env)
	if len(items) != 1 {
		t.Fatalf("expected 1 credential, got %d", len(items))
	}
	if items[0]["lastUsed"] != nil {
		t.Fatalf("expected lastUsed to be null before the first sign-in, got %v", items[0]["lastUsed"])
	}
	for _, k := range []string{"id", "name", "created", "signCount", "transports"} {
		if _, ok := items[0][k]; !ok {
			t.Fatalf("list item missing %q: %v", k, items[0])
		}
	}
	// hidden authenticator material must never be listed
	for _, k := range []string{"publicKey", "credentialId", "aaguid", "flags", "attestationType"} {
		if _, ok := items[0][k]; ok {
			t.Fatalf("list item must not expose %q", k)
		}
	}

	before := time.Now().UTC().Add(-2 * time.Second)
	auth.signCount = 20
	if status, body := loginWithPasskey(t, env, auth); status != http.StatusOK {
		t.Fatalf("login: expected 200, got %d (%v)", status, body)
	}

	items = listPasskeys(t, env)
	lastUsed, _ := items[0]["lastUsed"].(string)
	if lastUsed == "" {
		t.Fatalf("expected lastUsed after sign-in, got %v", items[0]["lastUsed"])
	}
	parsed, err := time.Parse("2006-01-02 15:04:05.000Z", lastUsed)
	if err != nil {
		t.Fatalf("unparseable lastUsed %q: %v", lastUsed, err)
	}
	if parsed.Before(before) {
		t.Fatalf("lastUsed %v is not recent", parsed)
	}
	if sc, _ := items[0]["signCount"].(float64); sc < 20 {
		t.Fatalf("expected signCount to be updated with lastUsed, got %v", items[0]["signCount"])
	}
}

// A failed sign-in must not touch lastUsed.
func TestWebAuthnLastUsedUntouchedOnFailedLogin(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	auth := newSoftAuthenticator(t, ceremonyRPID)
	registerPasskey(t, env, auth, "laptop")

	impostor := newSoftAuthenticator(t, ceremonyRPID)
	impostor.credID = auth.credID
	if status, _ := loginWithPasskey(t, env, impostor); status == http.StatusOK {
		t.Fatal("impostor must not sign in")
	}

	if items := listPasskeys(t, env); items[0]["lastUsed"] != nil {
		t.Fatalf("lastUsed must stay null after a failed sign-in, got %v", items[0]["lastUsed"])
	}
}

// The migration adds a hidden, system datetime field and old rows (no value)
// still load.
func TestWebAuthnLastUsedMigration(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)

	col, err := env.app.FindCollectionByNameOrId(core.CollectionNameWebAuthnCredentials)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := col.Fields.GetByName("lastUsed").(*core.DateField)
	if !ok || f == nil {
		t.Fatal("expected a lastUsed date field")
	}
	if !f.Hidden || !f.System {
		t.Fatalf("lastUsed must be hidden and system (Hidden=%v System=%v)", f.Hidden, f.System)
	}

	// the hidden-fields lock from 1779672000 still holds
	for _, name := range []string{"credentialId", "aaguid", "flags", "signCount", "transport", "attestationType", "publicKey"} {
		if !col.Fields.GetByName(name).GetHidden() {
			t.Fatalf("field %q must stay hidden", name)
		}
	}

	// a legacy row without lastUsed
	row := core.NewWebAuthnCredential(env.app)
	row.SetCollectionRef("_pb_users_auth_")
	row.SetRecordRef(ceremonyUserID)
	row.SetCredentialId("legacy")
	row.SetPublicKey("AAAA")
	if err := env.app.Save(row); err != nil {
		t.Fatal(err)
	}
	reloaded, err := env.app.FindRecordById(core.CollectionNameWebAuthnCredentials, row.Id)
	if err != nil {
		t.Fatal(err)
	}
	if !(&core.WebAuthnCredential{Record: reloaded}).LastUsed().IsZero() {
		t.Fatal("expected a zero lastUsed on a legacy row")
	}
}
