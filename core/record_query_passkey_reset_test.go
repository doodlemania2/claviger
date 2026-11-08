package core_test

import (
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Regression test for #39: FindAuthRecordByToken must verify passkeyReset
// tokens against the collection's PasskeyResetToken secret. core/record_query.go
// is upstream-owned, so an upstream merge can silently drop the fork's case.
func TestFindAuthRecordByTokenPasskeyReset(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	user, err := app.FindAuthRecordByEmail("users", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}

	token, err := user.NewPasskeyResetToken()
	if err != nil {
		t.Fatal(err)
	}

	got, err := app.FindAuthRecordByToken(token, core.TokenTypePasskeyReset)
	if err != nil {
		t.Fatalf("expected the passkey reset token to verify, got %v", err)
	}
	if got.Id != user.Id {
		t.Fatalf("expected record %q, got %q", user.Id, got.Id)
	}

	// no explicit type list: still resolved via the token's own type claim
	if _, err := app.FindAuthRecordByToken(token); err != nil {
		t.Fatalf("expected the token to verify without a type filter, got %v", err)
	}

	// must not satisfy other token types
	for _, typ := range []string{core.TokenTypeAuth, core.TokenTypePasswordReset, core.TokenTypeFile} {
		if _, err := app.FindAuthRecordByToken(token, typ); err == nil {
			t.Fatalf("expected the passkey reset token to be rejected for type %q", typ)
		}
	}

	// other token types must not satisfy passkeyReset
	pw, _ := user.NewPasswordResetToken()
	if _, err := app.FindAuthRecordByToken(pw, core.TokenTypePasskeyReset); err == nil {
		t.Fatal("expected a password reset token to be rejected as passkeyReset")
	}

	// the signature is bound to the collection's PasskeyResetToken secret:
	// rotating it invalidates outstanding links
	user.Collection().PasskeyResetToken.Secret += "rotated"
	if _, err := app.FindAuthRecordByToken(token, core.TokenTypePasskeyReset); err == nil {
		t.Fatal("expected the token to be rejected after the secret changed")
	}
}

// The PasskeyResetToken secret must never leave through the collection JSON.
func TestCollectionPasskeyResetSecretNotSerialized(t *testing.T) {
	t.Parallel()

	const marker = "PASSKEY-RESET-SECRET-MARKER-0123456789"

	c := core.NewAuthCollection("serialize_test")
	c.PasskeyResetToken.Secret = marker

	raw, err := c.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), marker) || strings.Contains(c.String(), marker) {
		t.Fatal("PasskeyResetToken.Secret leaked through collection serialization")
	}
	if c.PasskeyResetToken.Secret != marker {
		t.Fatal("serialization must not modify the in-memory secret")
	}
}
