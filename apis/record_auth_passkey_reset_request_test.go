package apis_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestRecordRequestPasskeyReset(t *testing.T) {
	t.Parallel()

	enableWebAuthn := func(t testing.TB, app *tests.TestApp, _ *core.ServeEvent) {
		usersCol, err := app.FindCollectionByNameOrId("users")
		if err != nil {
			t.Fatal(err)
		}
		usersCol.WebAuthn.Enabled = true
		if err := app.Save(usersCol); err != nil {
			t.Fatal(err)
		}
	}

	scenarios := []tests.ApiScenario{
		{
			Name:            "not an auth collection",
			Method:          http.MethodPost,
			URL:             "/api/collections/demo1/request-passkey-reset",
			Body:            strings.NewReader(`{"email":"test@example.com"}`),
			ExpectedStatus:  404,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "auth collection with disabled webauthn",
			Method:          http.MethodPost,
			URL:             "/api/collections/users/request-passkey-reset",
			Body:            strings.NewReader(`{"email":"test@example.com"}`),
			ExpectedStatus:  400,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:           "empty data",
			Method:         http.MethodPost,
			URL:            "/api/collections/users/request-passkey-reset",
			Body:           strings.NewReader(``),
			BeforeTestFunc: enableWebAuthn,
			ExpectedStatus: 400,
			ExpectedContent: []string{
				`"data":{`,
				`"email":{"code":"validation_required"`,
			},
			ExpectedEvents: map[string]int{"*": 0},
		},
		{
			Name:            "invalid json",
			Method:          http.MethodPost,
			URL:             "/api/collections/users/request-passkey-reset",
			Body:            strings.NewReader(`{"email`),
			BeforeTestFunc:  enableWebAuthn,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "invalid email format",
			Method:          http.MethodPost,
			URL:             "/api/collections/users/request-passkey-reset",
			Body:            strings.NewReader(`{"email":"not-an-email"}`),
			BeforeTestFunc:  enableWebAuthn,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"data":{`, `"email":{`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:           "missing auth record",
			Method:         http.MethodPost,
			URL:            "/api/collections/users/request-passkey-reset",
			Body:           strings.NewReader(`{"email":"missing@example.com"}`),
			BeforeTestFunc: enableWebAuthn,
			Delay:          100 * time.Millisecond,
			ExpectedStatus: 204,
			ExpectedEvents: map[string]int{"*": 0},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				if app.TestMailer.TotalSend() != 0 {
					t.Fatalf("Expected zero emails, got %d", app.TestMailer.TotalSend())
				}
			},
		},
		{
			Name:           "existing auth record",
			Method:         http.MethodPost,
			URL:            "/api/collections/users/request-passkey-reset",
			Body:           strings.NewReader(`{"email":"test@example.com"}`),
			BeforeTestFunc: enableWebAuthn,
			Delay:          100 * time.Millisecond,
			ExpectedStatus: 204,
			ExpectedEvents: map[string]int{
				"*":                              0,
				"OnMailerSend":                   1,
				"OnMailerRecordPasskeyResetSend": 1,
			},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				if !strings.Contains(app.TestMailer.LastMessage().HTML, "/auth/confirm-passkey-reset/") {
					t.Fatalf("Expected passkey reset email, got\n%v", app.TestMailer.LastMessage().HTML)
				}
			},
		},
		{
			Name:           "existing auth record (already sent / resend lock)",
			Method:         http.MethodPost,
			URL:            "/api/collections/users/request-passkey-reset",
			Body:           strings.NewReader(`{"email":"test@example.com"}`),
			Delay:          100 * time.Millisecond,
			ExpectedStatus: 204,
			ExpectedEvents: map[string]int{"*": 0},
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				enableWebAuthn(t, app, e)

				authRecord, err := app.FindAuthRecordByEmail("users", "test@example.com")
				if err != nil {
					t.Fatal(err)
				}
				resendKey := "@limitPasskeyResetEmail_" + authRecord.Collection().Id + authRecord.Id
				app.Store().Set(resendKey, struct{}{})
			},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				if app.TestMailer.TotalSend() != 0 {
					t.Fatalf("Expected zero emails (resend locked), got %d", app.TestMailer.TotalSend())
				}
			},
		},

		// rate limit checks
		// -----------------------------------------------------------
		{
			Name:   "RateLimit rule - users:requestPasskeyReset",
			Method: http.MethodPost,
			URL:    "/api/collections/users/request-passkey-reset",
			Body:   strings.NewReader(`{"email":"missing@example.com"}`),
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				enableWebAuthn(t, app, e)
				app.Settings().RateLimits.Enabled = true
				app.Settings().RateLimits.Rules = []core.RateLimitRule{
					{MaxRequests: 100, Label: "abc"},
					{MaxRequests: 100, Label: "*:requestPasskeyReset"},
					{MaxRequests: 0, Label: "users:requestPasskeyReset"},
				}
			},
			ExpectedStatus:  429,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:   "RateLimit rule - *:requestPasskeyReset",
			Method: http.MethodPost,
			URL:    "/api/collections/users/request-passkey-reset",
			Body:   strings.NewReader(`{"email":"missing@example.com"}`),
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				enableWebAuthn(t, app, e)
				app.Settings().RateLimits.Enabled = true
				app.Settings().RateLimits.Rules = []core.RateLimitRule{
					{MaxRequests: 100, Label: "abc"},
					{MaxRequests: 0, Label: "*:requestPasskeyReset"},
				}
			},
			ExpectedStatus:  429,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}
