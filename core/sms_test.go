package core_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/security"
	"github.com/doodlemania2/claviger/tools/sms"
)

func TestSMSSettingsDefaultOff(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	if app.Settings().SMS.Enabled {
		t.Fatal("SMS must be off by default")
	}
	if _, err := core.NewSMSSender(app); !errors.Is(err, core.ErrSMSNotConfigured) {
		t.Fatalf("expected ErrSMSNotConfigured, got %v", err)
	}
}

func TestSMSSettingsSecretsAreRedacted(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	app.Settings().SMS = core.SMSConfig{
		Enabled:  true,
		Provider: core.SMSProviderTwilio,
		Twilio: core.SMSTwilioConfig{
			AccountSID: "ACpublic", AuthToken: "twilio-top-secret", APIKeySID: "SKpublic",
			APIKeySecret: "apikey-top-secret", From: "+15005550006",
		},
		Webhook: core.SMSWebhookConfig{URL: "https://hooks.example.com/sms", Secret: "webhook-top-secret"},
	}

	raw, err := app.Settings().MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"twilio-top-secret", "apikey-top-secret", "webhook-top-secret", "authToken", "apiKeySecret", `"secret"`} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("%q leaked: %s", leak, raw)
		}
	}
	for _, visible := range []string{`"accountSid":"ACpublic"`, `"apiKeySid":"SKpublic"`, `"url":"https://hooks.example.com/sms"`} {
		if !strings.Contains(string(raw), visible) {
			t.Fatalf("%s should be visible: %s", visible, raw)
		}
	}

	// Clone keeps the secrets so a PATCH without them does not erase them
	clone, err := app.Settings().Clone()
	if err != nil {
		t.Fatal(err)
	}
	if clone.SMS.Twilio.AuthToken != "twilio-top-secret" || clone.SMS.Twilio.APIKeySecret != "apikey-top-secret" || clone.SMS.Webhook.Secret != "webhook-top-secret" {
		t.Fatal("clone must keep the secrets")
	}
}

func TestSMSSettingsEncryptedAtRest(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	os.Setenv(app.EncryptionEnv(), security.RandomString(32))
	defer os.Unsetenv(app.EncryptionEnv())

	app.Settings().SMS.Twilio.AuthToken = "twilio-top-secret"
	data, err := app.Settings().DBExport(app)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(data["value"].(string), "twilio-top-secret") {
		t.Fatal("secret stored in plaintext despite the encryption env")
	}
}

func TestSMSSettingsValidation(t *testing.T) {
	t.Parallel()

	twilio := func(mod func(c *core.SMSConfig)) core.SMSConfig {
		c := core.SMSConfig{Enabled: true, Provider: core.SMSProviderTwilio, Twilio: core.SMSTwilioConfig{
			AccountSID: "ACabc", AuthToken: "tok", From: "+15005550006",
		}}
		mod(&c)
		return c
	}
	webhook := func(mod func(c *core.SMSConfig)) core.SMSConfig {
		c := core.SMSConfig{Enabled: true, Provider: core.SMSProviderWebhook, Webhook: core.SMSWebhookConfig{
			URL: "https://hooks.example.com/sms", Secret: "s",
		}}
		mod(&c)
		return c
	}

	scenarios := []struct {
		name  string
		cfg   core.SMSConfig
		valid bool
	}{
		{"disabled and empty", core.SMSConfig{}, true},
		{"disabled with junk provider values left over", core.SMSConfig{Provider: "twilio"}, true},
		{"disabled with an unknown provider", core.SMSConfig{Provider: "nope"}, false},
		{"enabled without provider", core.SMSConfig{Enabled: true}, false},
		{"enabled with unknown provider", core.SMSConfig{Enabled: true, Provider: "nope"}, false},
		{"twilio token and from", twilio(func(*core.SMSConfig) {}), true},
		{"twilio api key and messaging service", twilio(func(c *core.SMSConfig) {
			c.Twilio.AuthToken, c.Twilio.From = "", ""
			c.Twilio.APIKeySID, c.Twilio.APIKeySecret, c.Twilio.MessagingServiceSID = "SKx", "sec", "MGx"
		}), true},
		{"twilio no sid", twilio(func(c *core.SMSConfig) { c.Twilio.AccountSID = "" }), false},
		{"twilio malformed sid", twilio(func(c *core.SMSConfig) { c.Twilio.AccountSID = "XX1" }), false},
		{"twilio no auth", twilio(func(c *core.SMSConfig) { c.Twilio.AuthToken = "" }), false},
		{"twilio api key sid without secret", twilio(func(c *core.SMSConfig) { c.Twilio.APIKeySID = "SKx" }), false},
		{"twilio no sender", twilio(func(c *core.SMSConfig) { c.Twilio.From = "" }), false},
		{"twilio bad from", twilio(func(c *core.SMSConfig) { c.Twilio.From = "555-0100" }), false},
		{"webhook ok", webhook(func(*core.SMSConfig) {}), true},
		{"webhook localhost http", webhook(func(c *core.SMSConfig) { c.Webhook.URL = "http://localhost:9000/x" }), true},
		{"webhook plain http", webhook(func(c *core.SMSConfig) { c.Webhook.URL = "http://hooks.example.com/x" }), false},
		{"webhook no url", webhook(func(c *core.SMSConfig) { c.Webhook.URL = "" }), false},
		{"webhook no secret", webhook(func(c *core.SMSConfig) { c.Webhook.Secret = "" }), false},
		{"webhook other provider's fields are not required", webhook(func(c *core.SMSConfig) { c.Twilio = core.SMSTwilioConfig{} }), true},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			err := s.cfg.Validate()
			if s.valid && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if !s.valid && err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestNewSMSSenderBuildsProviders(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	var got struct{ path, body string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got.path, got.body = r.URL.Path, string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	app.Settings().SMS = core.SMSConfig{
		Enabled: true, Provider: core.SMSProviderWebhook,
		Webhook: core.SMSWebhookConfig{URL: srv.URL + "/sms", Secret: "s"},
	}
	sender, err := core.NewSMSSender(app)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sender.(*sms.Webhook); !ok {
		t.Fatalf("expected a webhook sender, got %T", sender)
	}
	if err := sender.Send(context.Background(), &sms.Message{To: "+14155550123", Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	if got.path != "/sms" || !strings.Contains(got.body, `"to":"+14155550123"`) {
		t.Fatalf("unexpected request %+v", got)
	}

	app.Settings().SMS = core.SMSConfig{
		Enabled: true, Provider: core.SMSProviderTwilio,
		Twilio: core.SMSTwilioConfig{AccountSID: "ACabc", AuthToken: "tok", From: "+15005550006"},
	}
	sender, err = core.NewSMSSender(app)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sender.(*sms.Twilio); !ok {
		t.Fatalf("expected a twilio sender, got %T", sender)
	}

	// enabled but incomplete (settings edited in place, bypassing validation)
	app.Settings().SMS.Twilio.AuthToken = ""
	if _, err := core.NewSMSSender(app); err == nil {
		t.Fatal("expected a build error for incomplete twilio settings")
	}
}
