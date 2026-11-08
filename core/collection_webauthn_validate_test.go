package core_test

import (
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

func passkeyOnly(c *core.Collection) {
	c.PasswordAuth.Enabled = false
	c.OAuth2.Enabled = false
	c.OTP.Enabled = false
	c.MFA.Enabled = false
	c.WebAuthn.Enabled = true
}

func TestCollectionWebAuthnConfigBackwardCompat(t *testing.T) {
	t.Parallel()

	// options saved before the Discoverable option existed
	c := core.NewAuthCollection("legacy")
	if err := c.UnmarshalJSON([]byte(`{"type":"auth","name":"legacy","webauthn":{"enabled":true}}`)); err != nil {
		t.Fatal(err)
	}
	if !c.WebAuthn.Enabled {
		t.Fatal("expected webauthn to be enabled")
	}
	if c.WebAuthn.Discoverable {
		t.Fatal("expected Discoverable to default to false")
	}
}

func TestCollectionNoSignInMethod(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	// new collection with every method off is rejected
	c := core.NewAuthCollection("nomethod")
	c.PasswordAuth.Enabled = false
	if err := app.Save(c); err == nil {
		t.Fatal("expected a new collection without any sign-in method to be rejected")
	}

	// removing the last method of an existing collection is rejected
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	users.PasswordAuth.Enabled = false
	users.OAuth2.Enabled = false
	users.OTP.Enabled = false
	users.MFA.Enabled = false
	users.WebAuthn.Enabled = false
	if err := app.Save(users); err == nil {
		t.Fatal("expected disabling every sign-in method to be rejected")
	}
}

func TestCollectionPasskeyOnlyRecoveryRule(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}

	passkeyOnly(users)
	if !users.IsPasskeyOnly() {
		t.Fatal("expected IsPasskeyOnly")
	}

	// email field not required -> no recovery path
	users.Fields.GetByName(core.FieldNameEmail).(*core.EmailField).Required = false
	if err := app.Save(users); err == nil {
		t.Fatal("expected passkey-only without a required email field to be rejected")
	}

	// required email, but no sender address
	users.Fields.GetByName(core.FieldNameEmail).(*core.EmailField).Required = true
	app.Settings().Meta.SenderAddress = ""
	if err := app.Save(users); err == nil {
		t.Fatal("expected passkey-only without a sender address to be rejected")
	}

	// valid
	app.Settings().Meta.SenderAddress = "support@example.com"
	if err := app.Save(users); err != nil {
		t.Fatalf("expected a valid passkey-only collection to save: %v", err)
	}

	// grandfathering: an already passkey-only collection can have the email
	// requirement relaxed by an unrelated save only if it was already
	// passkey-only before (no re-validation of the recovery rule)
	users.Fields.GetByName(core.FieldNameEmail).(*core.EmailField).Required = false
	if err := app.Save(users); err != nil {
		t.Fatalf("expected an existing passkey-only collection to stay saveable: %v", err)
	}
}

func TestCollectionPasswordPlusPasskeyNeedsNoRecoveryRule(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	users, _ := app.FindCollectionByNameOrId("users")
	users.WebAuthn.Enabled = true
	users.Fields.GetByName(core.FieldNameEmail).(*core.EmailField).Required = false
	if err := app.Save(users); err != nil {
		t.Fatalf("password + passkey must not require a required email: %v", err)
	}
}
