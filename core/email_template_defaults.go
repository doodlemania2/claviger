package core

// Fork-local (Claviger #97): the built-in email templates by kind, so the
// mails package can tell a collection that still carries the English default
// (which it localizes) from one the admin customized (which it never touches).

// Kinds of the built-in auth emails.
const (
	EmailKindVerification  = "verification"
	EmailKindPasswordReset = "passwordReset"
	EmailKindEmailChange   = "emailChange"
	EmailKindOTP           = "otp"
	EmailKindAuthAlert     = "authAlert"
	EmailKindPasskeyReset  = "passkeyReset"
)

// DefaultEmailTemplate returns the built-in English template of an email kind.
func DefaultEmailTemplate(kind string) (EmailTemplate, bool) {
	switch kind {
	case EmailKindVerification:
		return defaultVerificationTemplate, true
	case EmailKindPasswordReset:
		return defaultResetPasswordTemplate, true
	case EmailKindEmailChange:
		return defaultConfirmEmailChangeTemplate, true
	case EmailKindOTP:
		return defaultOTPTemplate, true
	case EmailKindAuthAlert:
		return defaultAuthAlertTemplate, true
	case EmailKindPasskeyReset:
		return defaultPasskeyResetTemplate, true
	}

	return EmailTemplate{}, false
}
