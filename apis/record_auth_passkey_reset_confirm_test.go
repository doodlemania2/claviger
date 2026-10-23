package apis_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// Hardcoded password reset JWT for the seeded users/test@example.com
// (signed with that user's PasswordResetToken secret). Reused across the
// PocketBase test suite to verify token-type rejection in passkey reset.
const seededPasswordResetToken = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpZCI6IjRxMXhsY2xtZmxva3UzMyIsImV4cCI6MjUyNDYwNDQ2MSwidHlwZSI6InBhc3N3b3JkUmVzZXQiLCJjb2xsZWN0aW9uSWQiOiJfcGJfdXNlcnNfYXV0aF8iLCJlbWFpbCI6InRlc3RAZXhhbXBsZS5jb20ifQ.xR-xq1oHDy0D8Q4NDOAEyYKGHWd_swzoiSoL8FLFBHY"

func enablePasskeyResetWebAuthn(t testing.TB, app *tests.TestApp, _ *core.ServeEvent) {
	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	usersCol.WebAuthn.Enabled = true
	if err := app.Save(usersCol); err != nil {
		t.Fatal(err)
	}
}

func TestRecordConfirmPasskeyResetBegin(t *testing.T) {
	t.Parallel()

	scenarios := []tests.ApiScenario{
		{
			Name:            "not an auth collection",
			Method:          http.MethodPost,
			URL:             "/api/collections/demo1/confirm-passkey-reset/begin",
			Body:            strings.NewReader(`{"token":"x"}`),
			ExpectedStatus:  404,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "auth collection with disabled webauthn",
			Method:          http.MethodPost,
			URL:             "/api/collections/users/confirm-passkey-reset/begin",
			Body:            strings.NewReader(`{"token":"x"}`),
			ExpectedStatus:  403,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:           "empty body",
			Method:         http.MethodPost,
			URL:            "/api/collections/users/confirm-passkey-reset/begin",
			Body:           strings.NewReader(``),
			BeforeTestFunc: enablePasskeyResetWebAuthn,
			ExpectedStatus: 400,
			ExpectedContent: []string{
				`"data":{`,
				`"token":{"code":"validation_required"`,
			},
			ExpectedEvents: map[string]int{"*": 0},
		},
		{
			Name:            "invalid json",
			Method:          http.MethodPost,
			URL:             "/api/collections/users/confirm-passkey-reset/begin",
			Body:            strings.NewReader(`{"token`),
			BeforeTestFunc:  enablePasskeyResetWebAuthn,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:           "garbage token",
			Method:         http.MethodPost,
			URL:            "/api/collections/users/confirm-passkey-reset/begin",
			Body:           strings.NewReader(`{"token":"not-a-real-jwt"}`),
			BeforeTestFunc: enablePasskeyResetWebAuthn,
			ExpectedStatus: 400,
			ExpectedContent: []string{
				`"data":{`,
				`"token":{"code":"validation_invalid_token"`,
			},
			ExpectedEvents: map[string]int{"*": 0},
		},
		{
			Name:           "non-passkey reset token (password reset JWT rejected)",
			Method:         http.MethodPost,
			URL:            "/api/collections/users/confirm-passkey-reset/begin",
			Body:           strings.NewReader(`{"token":"` + seededPasswordResetToken + `"}`),
			BeforeTestFunc: enablePasskeyResetWebAuthn,
			ExpectedStatus: 400,
			ExpectedContent: []string{
				`"data":{`,
				`"token":{"code":"validation_invalid_token"`,
			},
			ExpectedEvents: map[string]int{"*": 0},
		},

		// rate limit checks
		// -----------------------------------------------------------
		{
			Name:   "RateLimit rule - users:confirmPasskeyResetBegin",
			Method: http.MethodPost,
			URL:    "/api/collections/users/confirm-passkey-reset/begin",
			Body:   strings.NewReader(`{"token":"x"}`),
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				enablePasskeyResetWebAuthn(t, app, e)
				app.Settings().RateLimits.Enabled = true
				app.Settings().RateLimits.Rules = []core.RateLimitRule{
					{MaxRequests: 100, Label: "abc"},
					{MaxRequests: 100, Label: "*:confirmPasskeyResetBegin"},
					{MaxRequests: 0, Label: "users:confirmPasskeyResetBegin"},
				}
			},
			ExpectedStatus:  429,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:   "RateLimit rule - *:confirmPasskeyResetBegin",
			Method: http.MethodPost,
			URL:    "/api/collections/users/confirm-passkey-reset/begin",
			Body:   strings.NewReader(`{"token":"x"}`),
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				enablePasskeyResetWebAuthn(t, app, e)
				app.Settings().RateLimits.Enabled = true
				app.Settings().RateLimits.Rules = []core.RateLimitRule{
					{MaxRequests: 100, Label: "abc"},
					{MaxRequests: 0, Label: "*:confirmPasskeyResetBegin"},
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

func TestRecordConfirmPasskeyResetFinish(t *testing.T) {
	t.Parallel()

	scenarios := []tests.ApiScenario{
		{
			Name:            "not an auth collection",
			Method:          http.MethodPost,
			URL:             "/api/collections/demo1/confirm-passkey-reset/finish",
			Body:            strings.NewReader(`{"token":"x","sessionToken":"y"}`),
			ExpectedStatus:  404,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "auth collection with disabled webauthn",
			Method:          http.MethodPost,
			URL:             "/api/collections/users/confirm-passkey-reset/finish",
			Body:            strings.NewReader(`{"token":"x","sessionToken":"y"}`),
			ExpectedStatus:  403,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:           "empty body",
			Method:         http.MethodPost,
			URL:            "/api/collections/users/confirm-passkey-reset/finish",
			Body:           strings.NewReader(``),
			BeforeTestFunc: enablePasskeyResetWebAuthn,
			ExpectedStatus: 400,
			ExpectedContent: []string{
				`"data":{`,
				`"token":{"code":"validation_required"`,
				`"sessionToken":{"code":"validation_required"`,
			},
			ExpectedEvents: map[string]int{"*": 0},
		},
		{
			Name:            "invalid json",
			Method:          http.MethodPost,
			URL:             "/api/collections/users/confirm-passkey-reset/finish",
			Body:            strings.NewReader(`{"token`),
			BeforeTestFunc:  enablePasskeyResetWebAuthn,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:           "garbage token",
			Method:         http.MethodPost,
			URL:            "/api/collections/users/confirm-passkey-reset/finish",
			Body:           strings.NewReader(`{"token":"bogus","sessionToken":"abc"}`),
			BeforeTestFunc: enablePasskeyResetWebAuthn,
			ExpectedStatus: 400,
			ExpectedContent: []string{
				`"data":{`,
				`"token":{"code":"validation_invalid_token"`,
			},
			ExpectedEvents: map[string]int{"*": 0},
		},
		{
			Name:           "non-passkey reset token (password reset JWT rejected)",
			Method:         http.MethodPost,
			URL:            "/api/collections/users/confirm-passkey-reset/finish",
			Body:           strings.NewReader(`{"token":"` + seededPasswordResetToken + `","sessionToken":"abc"}`),
			BeforeTestFunc: enablePasskeyResetWebAuthn,
			ExpectedStatus: 400,
			ExpectedContent: []string{
				`"data":{`,
				`"token":{"code":"validation_invalid_token"`,
			},
			ExpectedEvents: map[string]int{"*": 0},
		},

		// rate limit checks
		// -----------------------------------------------------------
		{
			Name:   "RateLimit rule - users:confirmPasskeyResetFinish",
			Method: http.MethodPost,
			URL:    "/api/collections/users/confirm-passkey-reset/finish",
			Body:   strings.NewReader(`{"token":"x","sessionToken":"y"}`),
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				enablePasskeyResetWebAuthn(t, app, e)
				app.Settings().RateLimits.Enabled = true
				app.Settings().RateLimits.Rules = []core.RateLimitRule{
					{MaxRequests: 100, Label: "abc"},
					{MaxRequests: 100, Label: "*:confirmPasskeyResetFinish"},
					{MaxRequests: 0, Label: "users:confirmPasskeyResetFinish"},
				}
			},
			ExpectedStatus:  429,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:   "RateLimit rule - *:confirmPasskeyResetFinish",
			Method: http.MethodPost,
			URL:    "/api/collections/users/confirm-passkey-reset/finish",
			Body:   strings.NewReader(`{"token":"x","sessionToken":"y"}`),
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				enablePasskeyResetWebAuthn(t, app, e)
				app.Settings().RateLimits.Enabled = true
				app.Settings().RateLimits.Rules = []core.RateLimitRule{
					{MaxRequests: 100, Label: "abc"},
					{MaxRequests: 0, Label: "*:confirmPasskeyResetFinish"},
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
