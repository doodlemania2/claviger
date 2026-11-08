package core

import (
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
)

// IsPasskeyOnly reports whether WebAuthn/passkeys is the only sign-in method
// the auth options enable (no password, OAuth2 or OTP).
func (o *collectionAuthOptions) IsPasskeyOnly() bool {
	return o.WebAuthn.Enabled && !o.PasswordAuth.Enabled && !o.OAuth2.Enabled && !o.OTP.Enabled
}

func (o *collectionAuthOptions) hasSignInMethod() bool {
	return o.PasswordAuth.Enabled || o.OAuth2.Enabled || o.OTP.Enabled || o.WebAuthn.Enabled
}

// validateSignInMethods guards two things:
//
//  1. A change must not leave an auth collection with no sign-in method at
//     all. Collections that already had none when this check was introduced
//     stay saveable untouched (nobody is locked out by an unrelated edit),
//     but a new collection, or an edit that removes the last method, is
//     rejected.
//
//  2. A collection that becomes passkey-only needs a recovery path. A lost
//     authenticator cannot be replaced by a password, so the only way back is
//     the emailed passkey reset (request-passkey-reset). That flow looks the
//     account up by email, so the "email" field must be required (every
//     record has a reachable address) and the sender address must be set.
//     Mail transport itself cannot be verified at save time (an unset SMTP
//     falls back to sendmail and may be configured later), so it is not
//     checked here.
//
//     The recovery rule applies only on the transition into passkey-only
//     (or on creation): collections that were already passkey-only before
//     this rule existed are grandfathered so existing deployments can still
//     save unrelated changes.
func (o *collectionAuthOptions) validateSignInMethods(cv *collectionValidator) error {
	if cv.original == nil {
		return nil
	}

	wasNew := cv.original.IsNew()

	if !o.hasSignInMethod() && (wasNew || cv.original.hasSignInMethod()) {
		return validation.Errors{
			"passwordAuth": validation.Errors{
				"enabled": validation.NewError(
					"validation_no_auth_method",
					"At least one sign-in method (password, OAuth2, OTP or passkey) must be enabled.",
				),
			},
		}
	}

	if !o.IsPasskeyOnly() {
		return nil
	}

	if !wasNew && cv.original.IsPasskeyOnly() {
		return nil // grandfathered
	}

	var emailRequired bool
	if cv.new != nil {
		if f, ok := cv.new.Fields.GetByName(FieldNameEmail).(*EmailField); ok && f != nil {
			emailRequired = f.Required
		}
	}
	if !emailRequired {
		return validation.Errors{
			"webauthn": validation.Errors{
				"enabled": validation.NewError(
					"validation_passkey_only_requires_email",
					"A passkey-only collection needs the email field to be required so every user can recover a lost passkey by email.",
				),
			},
		}
	}

	if cv.app != nil && cv.app.Settings().Meta.SenderAddress == "" {
		return validation.Errors{
			"webauthn": validation.Errors{
				"enabled": validation.NewError(
					"validation_passkey_only_requires_mail",
					"A passkey-only collection needs a sender email address in the application settings so passkey reset emails can be sent.",
				),
			},
		}
	}

	return nil
}
