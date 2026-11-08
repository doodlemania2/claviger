package core

import (
	"errors"
	"slices"
	"strings"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
)

// Captcha providers (mirrors tools/captcha; duplicated here so core does not
// import the HTTP verifier package).
const (
	CaptchaProviderTurnstile   = "turnstile"
	CaptchaProviderHCaptcha    = "hcaptcha"
	CaptchaProviderRecaptchaV2 = "recaptchaV2"
	CaptchaProviderRecaptchaV3 = "recaptchaV3"
)

// Captcha action identifiers. They double as the "action" name sent to the
// provider (reCAPTCHA v3 / Turnstile), so clients should render the widget
// with the same name.
const (
	CaptchaActionSignUp              = "signup"
	CaptchaActionLogin               = "login"
	CaptchaActionOTPRequest          = "otp_request"
	CaptchaActionResetRequest        = "reset_request"
	CaptchaActionVerificationRequest = "verification"
	CaptchaActionPasskeyLogin        = "passkey_login"
)

// CaptchaConfig defines the optional CAPTCHA protection of the unauthenticated
// auth endpoints (#91). With no Provider (the default) nothing changes.
type CaptchaConfig struct {
	// Provider is one of turnstile, hcaptcha, recaptchaV2, recaptchaV3.
	// Empty disables the feature.
	Provider string `form:"provider" json:"provider"`

	SiteKey string `form:"siteKey" json:"siteKey"`

	// SecretKey is write-only: it is stripped from the settings API responses.
	SecretKey string `form:"secretKey" json:"secretKey,omitempty"`

	// MinScore is the reCAPTCHA v3 minimum score (0.0 - 1.0, default 0.5).
	MinScore float64 `form:"minScore" json:"minScore"`

	// Hostnames optionally restricts the hostname the provider reports for
	// the solved challenge (the site that rendered the widget). Empty means
	// not checked.
	Hostnames []string `form:"hostnames" json:"hostnames"`

	// FailClosed rejects the request when the provider cannot be reached.
	// The default (false) fails OPEN: a provider outage must not lock a
	// congregation out of its own sign-in; the outage is logged as a WARN and
	// counted as outage_open.
	FailClosed bool `form:"failClosed" json:"failClosed"`

	// Actions selects the endpoints that require a valid captcha token.
	Actions CaptchaActions `form:"actions" json:"actions"`
}

// CaptchaActions are the per-endpoint toggles (all off by default).
type CaptchaActions struct {
	SignUp              bool `form:"signUp" json:"signUp"`
	Login               bool `form:"login" json:"login"`
	OTPRequest          bool `form:"otpRequest" json:"otpRequest"`
	ResetRequest        bool `form:"resetRequest" json:"resetRequest"`
	VerificationRequest bool `form:"verificationRequest" json:"verificationRequest"`
	PasskeyLogin        bool `form:"passkeyLogin" json:"passkeyLogin"`
}

// Required returns the action identifiers that currently require a token.
func (c CaptchaConfig) Required() []string {
	if c.Provider == "" {
		return []string{}
	}

	result := []string{}
	for _, item := range []struct {
		on     bool
		action string
	}{
		{c.Actions.SignUp, CaptchaActionSignUp},
		{c.Actions.Login, CaptchaActionLogin},
		{c.Actions.OTPRequest, CaptchaActionOTPRequest},
		{c.Actions.ResetRequest, CaptchaActionResetRequest},
		{c.Actions.VerificationRequest, CaptchaActionVerificationRequest},
		{c.Actions.PasskeyLogin, CaptchaActionPasskeyLogin},
	} {
		if item.on {
			result = append(result, item.action)
		}
	}

	return result
}

// IsRequired reports whether the action needs a captcha token.
func (c CaptchaConfig) IsRequired(action string) bool {
	return slices.Contains(c.Required(), action)
}

// Validate makes CaptchaConfig validatable by implementing [validation.Validatable] interface.
func (c CaptchaConfig) Validate() error {
	hasProvider := c.Provider != ""

	return validation.ValidateStruct(&c,
		validation.Field(&c.Provider, validation.In(
			CaptchaProviderTurnstile,
			CaptchaProviderHCaptcha,
			CaptchaProviderRecaptchaV2,
			CaptchaProviderRecaptchaV3,
		)),
		validation.Field(&c.SiteKey, validation.When(hasProvider, validation.Required)),
		validation.Field(&c.SecretKey, validation.When(hasProvider, validation.Required)),
		validation.Field(&c.MinScore, validation.Min(0.0), validation.Max(1.0)),
		validation.Field(&c.Hostnames, validation.Each(validation.By(func(v any) error {
			s, _ := v.(string)
			if strings.TrimSpace(s) == "" || strings.ContainsAny(s, "/ :") {
				return errors.New("must be a bare hostname")
			}
			return nil
		}))),
		validation.Field(&c.Actions, validation.By(func(any) error {
			if !hasProvider && len(c.requiredIgnoringProvider()) > 0 {
				return errors.New("select a provider to require a captcha")
			}
			return nil
		})),
	)
}

func (c CaptchaConfig) requiredIgnoringProvider() []string {
	cp := c
	cp.Provider = "x"
	return cp.Required()
}
