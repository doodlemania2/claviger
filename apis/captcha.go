package apis

import (
	"strings"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/captcha"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/telemetry"
)

// Optional CAPTCHA protection of the unauthenticated auth endpoints (#91).
//
// Nothing happens unless a superuser selects a provider AND enables the action
// in Settings.Captcha, so a default install behaves exactly as before.
//
// The client sends the token in the X-Captcha-Token header or in the
// "captchaToken" body field (both are accepted; the header wins). A missing,
// rejected or action/hostname-mismatched token is a 400 whose data carries the
// stable code `captcha_failed`:
//
//	{"status":400,"message":"Captcha verification failed.",
//	 "data":{"captchaToken":{"code":"captcha_failed","message":"..."}}}
//
// Provider outage (network error, timeout, 5xx, unparsable reply, bad secret)
// is NOT a verification failure. By default it fails OPEN: for a church site a
// captcha outage locking every member out of sign-in is worse than a brief
// exposure of the (still rate limited) endpoints. The outage is logged at WARN
// and counted as claviger_captcha_verifications_total{outcome="outage_open"}.
// Settings.Captcha.FailClosed flips it to refuse (outcome="outage_closed").
const (
	// CaptchaHeader is the request header that carries the captcha token.
	CaptchaHeader = "X-Captcha-Token"

	// CaptchaBodyField is the request body field that carries the captcha token.
	CaptchaBodyField = "captchaToken"

	// CaptchaFailedCode is the stable validation error code of a failed captcha.
	CaptchaFailedCode = "captcha_failed"

	captchaVerifierStoreKey = "__clvCaptchaVerifier__"
)

// captcha outcomes (metric label values)
const (
	captchaOutcomeSuccess      = "success"
	captchaOutcomeFailed       = "failed"
	captchaOutcomeMissing      = "missing"
	captchaOutcomeOutageOpen   = "outage_open"
	captchaOutcomeOutageClosed = "outage_closed"
)

// requireCaptcha is a route middleware enforcing the captcha for the action
// (when it is enabled in the settings).
func requireCaptcha(action string) *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id: "clvCaptcha_" + action,
		Func: func(e *core.RequestEvent) error {
			if err := checkCaptcha(e, action); err != nil {
				return err
			}

			return e.Next()
		},
	}
}

func captchaFailedError(e *core.RequestEvent) error {
	return e.BadRequestError("Captcha verification failed.", validation.Errors{
		CaptchaBodyField: validation.NewError(CaptchaFailedCode, "Captcha verification failed."),
	})
}

// captchaToken extracts the token from the header or the (already cached) body.
func captchaToken(e *core.RequestEvent) string {
	if v := strings.TrimSpace(e.Request.Header.Get(CaptchaHeader)); v != "" {
		return v
	}

	info, err := e.RequestInfo()
	if err != nil || info == nil {
		return ""
	}

	if v, ok := info.Body[CaptchaBodyField].(string); ok {
		return strings.TrimSpace(v)
	}

	return ""
}

// captchaVerifierFor builds the configured verifier. A verifier stored under
// captchaVerifierStoreKey replaces it (tests).
func captchaVerifierFor(app core.App, cfg core.CaptchaConfig) (captcha.Verifier, error) {
	if v, ok := app.Store().Get(captchaVerifierStoreKey).(captcha.Verifier); ok && v != nil {
		return v, nil
	}

	return captcha.New(captcha.Config{
		Provider:  cfg.Provider,
		SecretKey: cfg.SecretKey,
		MinScore:  cfg.MinScore,
	})
}

// checkCaptcha enforces the captcha for the action. It returns nil when the
// action does not require one.
func checkCaptcha(e *core.RequestEvent, action string) error {
	cfg := e.App.Settings().Captcha

	if !cfg.IsRequired(action) {
		return nil
	}

	// superusers (e.g. creating users from the dashboard) are not bots
	if e.HasSuperuserAuth() {
		return nil
	}

	provider := cfg.Provider
	record := func(outcome string) {
		telemetry.Active().RecordCaptchaVerification(provider, outcome)
	}

	token := captchaToken(e)
	if token == "" {
		record(captchaOutcomeMissing)
		return captchaFailedError(e)
	}

	verifier, err := captchaVerifierFor(e.App, cfg)
	if err == nil {
		var res captcha.Result

		res, err = verifier.Verify(e.Request.Context(), captcha.Request{
			Token:     token,
			RemoteIP:  e.RealIP(),
			Action:    action,
			Hostnames: cfg.Hostnames,
		})
		if err == nil {
			if res.Success {
				record(captchaOutcomeSuccess)
				return nil
			}

			record(captchaOutcomeFailed)
			e.App.Logger().Debug("Captcha rejected", "provider", provider, "action", action, "reason", res.Reason)

			return captchaFailedError(e)
		}
	}

	// provider outage / misconfiguration
	if cfg.FailClosed {
		record(captchaOutcomeOutageClosed)
		e.App.Logger().Warn("Captcha provider unavailable, refusing the request (failClosed)",
			"provider", provider, "action", action, "error", err)

		return captchaFailedError(e)
	}

	record(captchaOutcomeOutageOpen)
	e.App.Logger().Warn("Captcha provider unavailable, letting the request through (fail-open)",
		"provider", provider, "action", action, "error", err)

	return nil
}

// captchaInfo is the public (secret free) part of the captcha settings that
// clients need to render the widget.
type captchaInfo struct {
	Provider string   `json:"provider"`
	SiteKey  string   `json:"siteKey"`
	Required []string `json:"required"`
}

// publicCaptchaInfo returns nil when no action requires a captcha, so the
// auth-methods response is unchanged for installs that do not use it.
func publicCaptchaInfo(app core.App) *captchaInfo {
	cfg := app.Settings().Captcha

	required := cfg.Required()
	if len(required) == 0 {
		return nil
	}

	return &captchaInfo{Provider: cfg.Provider, SiteKey: cfg.SiteKey, Required: required}
}

// captchaCSPSources are the extra Content-Security-Policy sources (each value
// has a leading space, or is empty) a provider widget needs.
type captchaCSPSources struct {
	script, frame, connect, style string
}

// captchaSourcesFor returns the narrow set of provider domains, nothing for an
// unknown or empty provider.
func captchaSourcesFor(provider string) captchaCSPSources {
	switch provider {
	case core.CaptchaProviderTurnstile:
		return captchaCSPSources{
			script:  " https://challenges.cloudflare.com",
			frame:   " https://challenges.cloudflare.com",
			connect: " https://challenges.cloudflare.com",
		}
	case core.CaptchaProviderHCaptcha:
		return captchaCSPSources{
			script:  " https://hcaptcha.com https://*.hcaptcha.com",
			frame:   " https://hcaptcha.com https://*.hcaptcha.com",
			connect: " https://hcaptcha.com https://*.hcaptcha.com",
			style:   " https://hcaptcha.com https://*.hcaptcha.com",
		}
	case core.CaptchaProviderRecaptchaV2, core.CaptchaProviderRecaptchaV3:
		return captchaCSPSources{
			script:  " https://www.google.com/recaptcha/ https://www.gstatic.com/recaptcha/",
			frame:   " https://www.google.com/recaptcha/ https://recaptcha.google.com/recaptcha/",
			connect: " https://www.google.com/recaptcha/",
		}
	}

	return captchaCSPSources{}
}

// adminUICSP returns the Content-Security-Policy of the embedded admin UI:
// upstream's defaultCSP, plus the provider domains only when the superuser
// login requires a captcha.
func adminUICSP(app core.App) string {
	cfg := app.Settings().Captcha
	if !cfg.IsRequired(core.CaptchaActionLogin) {
		return defaultCSP
	}

	src := captchaSourcesFor(cfg.Provider)
	if src.script == "" {
		return defaultCSP
	}

	csp := defaultCSP
	csp = strings.Replace(csp, "style-src 'self' 'unsafe-inline'", "style-src 'self' 'unsafe-inline'"+src.style, 1)
	csp = strings.Replace(csp, "connect-src 'self' http://127.0.0.1:* https://nominatim.openstreetmap.org",
		"connect-src 'self' http://127.0.0.1:* https://nominatim.openstreetmap.org"+src.connect, 1)
	csp = strings.Replace(csp, "script-src 'self' http://127.0.0.1:*", "script-src 'self' http://127.0.0.1:*"+src.script, 1)
	csp = strings.Replace(csp, "frame-ancestors 'none'", "frame-src 'self'"+src.frame+"; frame-ancestors 'none'", 1)

	return csp
}
