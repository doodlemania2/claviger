package apis_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPasskeysPageAndClientCoverNewFlows(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)

	get := func(path string) string {
		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}

	page := get("/passkeys")
	for _, want := range []string{
		`autocomplete="username webauthn"`, // conditional UI hook
		"autofillLogin",
		"requestReset", // recovery for passkey-only collections
		"passkeyOnly",
		"lastUsed",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("passkeys page is missing %q", want)
		}
	}

	client := get("/api/passkeys-client.js")
	for _, want := range []string{
		"login-begin-discoverable",
		"login-finish-discoverable",
		`mediation: "conditional"`,
		"step-up-begin",
		"step-up-finish",
		"passkey_step_up_required",
		"authWithPasskeyAutofill",
		"stepUpPasskey",
	} {
		if !strings.Contains(client, want) {
			t.Errorf("passkeys client is missing %q", want)
		}
	}
}
