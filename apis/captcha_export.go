package apis

// CaptchaWidgetSources returns the narrow Content-Security-Policy sources
// (each with a leading space, or empty) the widget of a captcha provider needs
// for scripts, frames, XHR and styles. It is empty for an unknown provider.
//
// It lets the hosted OpenID Connect sign-in page (oidc/) extend its own strict
// policy the same way /passkeys does, without duplicating the provider table.
func CaptchaWidgetSources(provider string) (script, frame, connect, style string) {
	s := captchaSourcesFor(provider)

	return s.script, s.frame, s.connect, s.style
}
