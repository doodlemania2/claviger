package core_test

import (
	"os"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/security"
)

func TestCaptchaSettingsDefaultOff(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	c := app.Settings().Captcha
	if c.Provider != "" || c.FailClosed || len(c.Required()) != 0 {
		t.Fatalf("captcha must be off by default (fail-open), got %+v", c)
	}
}

func TestCaptchaSettingsSecretIsRedacted(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	app.Settings().Captcha = core.CaptchaConfig{
		Provider: core.CaptchaProviderTurnstile, SiteKey: "site-pub", SecretKey: "top-secret-value",
	}

	raw, err := app.Settings().MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "top-secret-value") || strings.Contains(string(raw), "secretKey") {
		t.Fatalf("secret leaked: %s", raw)
	}
	if !strings.Contains(string(raw), `"siteKey":"site-pub"`) {
		t.Fatalf("site key should be visible: %s", raw)
	}

	// Clone (used by the settings update) keeps the secret
	clone, err := app.Settings().Clone()
	if err != nil {
		t.Fatal(err)
	}
	if clone.Captcha.SecretKey != "top-secret-value" {
		t.Fatal("clone must keep the secret so a PATCH without it does not erase it")
	}
}

func TestCaptchaSettingsEncryptedAtRest(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	os.Setenv(app.EncryptionEnv(), security.RandomString(32))
	defer os.Unsetenv(app.EncryptionEnv())

	app.Settings().Captcha.SecretKey = "top-secret-value"
	data, err := app.Settings().DBExport(app)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := data["value"].(string); v == "" || strings.Contains(v, "top-secret-value") {
		t.Fatalf("the secret must not be stored in plain text: %v", data["value"])
	}
}

func TestCaptchaSettingsValidation(t *testing.T) {
	scenarios := []struct {
		name    string
		cfg     core.CaptchaConfig
		wantErr bool
	}{
		{"empty is valid", core.CaptchaConfig{}, false},
		{"provider needs keys", core.CaptchaConfig{Provider: "turnstile"}, true},
		{"valid turnstile", core.CaptchaConfig{Provider: "turnstile", SiteKey: "a", SecretKey: "b"}, false},
		{"unknown provider", core.CaptchaConfig{Provider: "nope", SiteKey: "a", SecretKey: "b"}, true},
		{"score out of range", core.CaptchaConfig{Provider: "recaptchaV3", SiteKey: "a", SecretKey: "b", MinScore: 1.5}, true},
		{"score ok", core.CaptchaConfig{Provider: "recaptchaV3", SiteKey: "a", SecretKey: "b", MinScore: 0.7}, false},
		{"hostname with scheme", core.CaptchaConfig{Provider: "hcaptcha", SiteKey: "a", SecretKey: "b", Hostnames: []string{"https://x.org"}}, true},
		{"bare hostname", core.CaptchaConfig{Provider: "hcaptcha", SiteKey: "a", SecretKey: "b", Hostnames: []string{"x.org"}}, false},
		{"action without provider", core.CaptchaConfig{Actions: core.CaptchaActions{Login: true}}, true},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			err := s.cfg.Validate()
			if (err != nil) != s.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, s.wantErr)
			}
		})
	}
}

func TestCaptchaSettingsRequired(t *testing.T) {
	c := core.CaptchaConfig{Provider: "turnstile", Actions: core.CaptchaActions{SignUp: true, PasskeyLogin: true}}
	got := strings.Join(c.Required(), ",")
	if got != "signup,passkey_login" {
		t.Fatalf("got %q", got)
	}
	if !c.IsRequired(core.CaptchaActionSignUp) || c.IsRequired(core.CaptchaActionLogin) {
		t.Fatal("unexpected IsRequired")
	}
	c.Provider = ""
	if len(c.Required()) != 0 {
		t.Fatal("no provider means nothing is required")
	}
}
