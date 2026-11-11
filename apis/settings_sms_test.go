package apis_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #83): SMS settings and the test-send route.

func TestSettingsTestSMS(t *testing.T) {
	t.Parallel()

	var lastBody string
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		lastBody = string(raw)
		if strings.HasSuffix(r.URL.Path, "/fail") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer receiver.Close()

	enable := func(path string) func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
		return func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
			app.Settings().SMS = core.SMSConfig{
				Enabled: true, Provider: core.SMSProviderWebhook,
				Webhook: core.SMSWebhookConfig{URL: receiver.URL + path, Secret: "hook-top-secret"},
			}
		}
	}

	superuser := map[string]string{"Authorization": auditSuperuserToken}

	scenarios := []tests.ApiScenario{
		{
			Name:            "unauthorized",
			Method:          http.MethodPost,
			URL:             "/api/settings/test/sms",
			Body:            strings.NewReader(`{"to":"+14155550123"}`),
			ExpectedStatus:  401,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "regular user",
			Method:          http.MethodPost,
			URL:             "/api/settings/test/sms",
			Body:            strings.NewReader(`{"to":"+14155550123"}`),
			Headers:         map[string]string{"Authorization": auditUserToken},
			ExpectedStatus:  403,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "invalid body",
			Method:          http.MethodPost,
			URL:             "/api/settings/test/sms",
			Body:            strings.NewReader(`{`),
			Headers:         superuser,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "missing number",
			Method:          http.MethodPost,
			URL:             "/api/settings/test/sms",
			Body:            strings.NewReader(`{}`),
			Headers:         superuser,
			BeforeTestFunc:  enable("/ok"),
			ExpectedStatus:  400,
			ExpectedContent: []string{`"to":{"code":"validation_required"`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "number is not E.164",
			Method:          http.MethodPost,
			URL:             "/api/settings/test/sms",
			Body:            strings.NewReader(`{"to":"415 555 0123"}`),
			Headers:         superuser,
			BeforeTestFunc:  enable("/ok"),
			ExpectedStatus:  400,
			ExpectedContent: []string{`"to":{"code":"validation_invalid_phone"`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "sms not configured",
			Method:          http.MethodPost,
			URL:             "/api/settings/test/sms",
			Body:            strings.NewReader(`{"to":"+14155550123"}`),
			Headers:         superuser,
			ExpectedStatus:  400,
			ExpectedContent: []string{`SMS is not configured`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:           "success is audited with a masked number",
			Method:         http.MethodPost,
			URL:            "/api/settings/test/sms",
			Body:           strings.NewReader(`{"to":"+14155550123"}`),
			Headers:        superuser,
			BeforeTestFunc: enable("/ok"),
			ExpectedStatus: 204,
			ExpectedEvents: map[string]int{},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				if !strings.Contains(lastBody, `"to":"+14155550123"`) {
					t.Fatalf("receiver got %s", lastBody)
				}

				entries := auditEntries(t, app, "action='sms.test'")
				if len(entries) != 1 {
					t.Fatalf("Expected 1 audit entry, got %d", len(entries))
				}
				requireSuperuserEntry(t, entries[0], "sms.test", "_params")

				dump := tests.CompactJSON(entries[0].GetString("meta")) + tests.CompactJSON(entries[0].GetString("before")) + tests.CompactJSON(entries[0].GetString("after")) + entries[0].GetString("changedFields")
				if strings.Contains(dump, "+14155550123") || strings.Contains(dump, "4155550123") || strings.Contains(dump, "hook-top-secret") {
					t.Fatalf("full number or secret in the audit entry: %s", dump)
				}
				if !strings.Contains(dump, `"to":"+*********23"`) || !strings.Contains(dump, `"success":true`) {
					t.Fatalf("unexpected meta: %s", dump)
				}
			},
		},
		{
			Name:           "provider failure is reported and audited",
			Method:         http.MethodPost,
			URL:            "/api/settings/test/sms",
			Body:           strings.NewReader(`{"to":"+14155550123"}`),
			Headers:        superuser,
			BeforeTestFunc: enable("/fail?token=urltoken"),
			ExpectedStatus: 400,
			ExpectedContent: []string{
				`Failed to send the test SMS`, `HTTP 500`,
			},
			NotExpectedContent: []string{"hook-top-secret", "urltoken", "+14155550123"},
			ExpectedEvents:     map[string]int{},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				entries := auditEntries(t, app, "action='sms.test'")
				if len(entries) != 1 || !strings.Contains(tests.CompactJSON(entries[0].GetString("meta")), `"success":false`) {
					t.Fatalf("unexpected audit entries: %v", entries)
				}
			},
		},
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}

func TestSettingsSMSPatch(t *testing.T) {
	t.Parallel()

	rejectedSettingsEvents := map[string]int{"OnSettingsUpdateRequest": 1, "OnModelUpdate": 1, "OnModelValidate": 1, "OnModelAfterUpdateError": 1}

	superuser := map[string]string{"Authorization": auditSuperuserToken}

	(&tests.ApiScenario{
		Name:   "patch stores the secrets but never returns them",
		Method: http.MethodPatch,
		URL:    "/api/settings",
		Body: strings.NewReader(`{"sms":{"enabled":true,"provider":"twilio","twilio":{
			"accountSid":"ACpublic","authToken":"tw-top-secret","apiKeySid":"SKpub","apiKeySecret":"key-top-secret","from":"+15005550006"},
			"webhook":{"url":"https://hooks.example.com/x","secret":"wh-top-secret"}}}`),
		Headers: superuser,
		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			c := app.Settings().SMS
			if c.Twilio.AuthToken != "tw-top-secret" || c.Twilio.APIKeySecret != "key-top-secret" || c.Webhook.Secret != "wh-top-secret" {
				t.Fatalf("secrets not stored: %+v", c)
			}
		},
		ExpectedStatus:     200,
		ExpectedContent:    []string{`"accountSid":"ACpublic"`, `"apiKeySid":"SKpub"`, `"provider":"twilio"`},
		NotExpectedContent: []string{"tw-top-secret", "key-top-secret", "wh-top-secret", "authToken", "apiKeySecret"},
		ExpectedEvents:     map[string]int{},
	}).Test(t)

	(&tests.ApiScenario{
		Name:            "enabled twilio without credentials is rejected",
		Method:          http.MethodPatch,
		URL:             "/api/settings",
		Body:            strings.NewReader(`{"sms":{"enabled":true,"provider":"twilio","twilio":{"accountSid":"ACpublic"}}}`),
		Headers:         superuser,
		ExpectedStatus:  400,
		ExpectedContent: []string{`"sms":{`, `"twilio":{`},
		ExpectedEvents:  rejectedSettingsEvents,
	}).Test(t)

	(&tests.ApiScenario{
		Name:            "enabled without a provider is rejected",
		Method:          http.MethodPatch,
		URL:             "/api/settings",
		Body:            strings.NewReader(`{"sms":{"enabled":true}}`),
		Headers:         superuser,
		ExpectedStatus:  400,
		ExpectedContent: []string{`"provider":{"code":"validation_required"`},
		ExpectedEvents:  rejectedSettingsEvents,
	}).Test(t)

	(&tests.ApiScenario{
		Name:            "plain http webhook is rejected",
		Method:          http.MethodPatch,
		URL:             "/api/settings",
		Body:            strings.NewReader(`{"sms":{"enabled":true,"provider":"webhook","webhook":{"url":"http://hooks.example.com/x","secret":"s"}}}`),
		Headers:         superuser,
		ExpectedStatus:  400,
		ExpectedContent: []string{`"webhook":{`},
		ExpectedEvents:  rejectedSettingsEvents,
	}).Test(t)
}
