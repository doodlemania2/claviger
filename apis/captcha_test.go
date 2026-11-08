package apis_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/captcha"
)

type fakeCaptcha struct {
	mu   sync.Mutex
	hits int
	last captcha.Request
	res  captcha.Result
	err  error
}

func (f *fakeCaptcha) Name() string { return "turnstile" }

func (f *fakeCaptcha) Verify(_ context.Context, req captcha.Request) (captcha.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	f.last = req
	return f.res, f.err
}

func (f *fakeCaptcha) Hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

type captchaEndpoint struct {
	action       string
	url          string
	body         string
	okStatus     int // status when the captcha passes (or is not required)
	enable       func(c *core.CaptchaActions)
	prepare      func(t testing.TB, app *tests.TestApp)
	authToken    string
	notRequireOn string
}

var captchaEndpoints = []captchaEndpoint{
	{
		action:   core.CaptchaActionSignUp,
		url:      "/api/collections/users/records",
		body:     `{"email":"newcomer@example.com","password":"1234567890","passwordConfirm":"1234567890"}`,
		okStatus: 200,
		enable:   func(c *core.CaptchaActions) { c.SignUp = true },
		prepare: func(t testing.TB, app *tests.TestApp) {
			col, err := app.FindCollectionByNameOrId("users")
			if err != nil {
				t.Fatal(err)
			}
			col.CreateRule = new(string) // public sign-up
			if err := app.Save(col); err != nil {
				t.Fatal(err)
			}
		},
	},
	{
		action:   core.CaptchaActionLogin,
		url:      "/api/collections/users/auth-with-password",
		body:     `{"identity":"test@example.com","password":"wrong-on-purpose"}`,
		okStatus: 400, // reaches the handler: wrong credentials (see below)
		enable:   func(c *core.CaptchaActions) { c.Login = true },
	},
	{
		action:   core.CaptchaActionOTPRequest,
		url:      "/api/collections/users/request-otp",
		body:     `{"email":"test@example.com"}`,
		okStatus: 200,
		enable:   func(c *core.CaptchaActions) { c.OTPRequest = true },
		prepare: func(t testing.TB, app *tests.TestApp) {
			col, _ := app.FindCollectionByNameOrId("users")
			col.OTP.Enabled = true
			if err := app.Save(col); err != nil {
				t.Fatal(err)
			}
		},
	},
	{
		action:   core.CaptchaActionResetRequest,
		url:      "/api/collections/users/request-password-reset",
		body:     `{"email":"test@example.com"}`,
		okStatus: 204,
		enable:   func(c *core.CaptchaActions) { c.ResetRequest = true },
	},
	{
		action:   core.CaptchaActionVerificationRequest,
		url:      "/api/collections/users/request-verification",
		body:     `{"email":"test@example.com"}`,
		okStatus: 204,
		enable:   func(c *core.CaptchaActions) { c.VerificationRequest = true },
	},
	{
		action:   core.CaptchaActionPasskeyLogin,
		url:      "/api/collections/users/auth-with-webauthn/login-begin",
		body:     `{"identity":"test@example.com"}`,
		okStatus: 200,
		enable:   func(c *core.CaptchaActions) { c.PasskeyLogin = true },
		prepare: func(t testing.TB, app *tests.TestApp) {
			col, _ := app.FindCollectionByNameOrId("users")
			col.WebAuthn.Enabled = true
			if err := app.Save(col); err != nil {
				t.Fatal(err)
			}
		},
	},
}

type captchaVariant struct {
	name       string
	enabled    bool
	failClosed bool
	headers    map[string]string
	body       func(string) string
	fake       *fakeCaptcha
	wantStatus func(ep captchaEndpoint) int
	wantHits   int
	wantFailed bool
}

func withTokenField(body string) string {
	return strings.Replace(body, "{", `{"captchaToken":"body-token",`, 1)
}

func TestCaptchaProtectedActions(t *testing.T) {
	t.Parallel()

	okFake := func() *fakeCaptcha { return &fakeCaptcha{res: captcha.Result{Success: true}} }
	rejectFake := func() *fakeCaptcha { return &fakeCaptcha{res: captcha.Result{Reason: "invalid-input-response"}} }
	outageFake := func() *fakeCaptcha { return &fakeCaptcha{err: errors.New("provider down")} }

	identity := func(s string) string { return s }
	ok := func(ep captchaEndpoint) int { return ep.okStatus }
	bad := func(captchaEndpoint) int { return http.StatusBadRequest }
	tok := map[string]string{apis.CaptchaHeader: "header-token"}

	variants := []captchaVariant{
		{"disabled: no token needed, provider never called", false, false, nil, identity, okFake(), ok, 0, false},
		{"enabled, no token", true, false, nil, identity, okFake(), bad, 0, true},
		{"enabled, header token", true, false, tok, identity, okFake(), ok, 1, false},
		{"enabled, body token", true, false, nil, withTokenField, okFake(), ok, 1, false},
		{"enabled, provider rejects", true, false, tok, identity, rejectFake(), bad, 1, true},
		{"enabled, outage fails open by default", true, false, tok, identity, outageFake(), ok, 1, false},
		{"enabled, outage with failClosed", true, true, tok, identity, outageFake(), bad, 1, true},
	}

	for _, ep := range captchaEndpoints {
		for _, v := range variants {
			ep, v := ep, v
			// every scenario gets its own fake
			fake := &fakeCaptcha{res: v.fake.res, err: v.fake.err}

			sc := tests.ApiScenario{
				Name:   ep.action + ": " + v.name,
				Method: http.MethodPost,
				URL:    ep.url,
				Body:   strings.NewReader(v.body(ep.body)),
				Headers: func() map[string]string {
					h := map[string]string{"Content-Type": "application/json"}
					for k, val := range v.headers {
						h[k] = val
					}
					return h
				}(),
				BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
					if ep.prepare != nil {
						ep.prepare(t, app)
					}
					app.Settings().Captcha = core.CaptchaConfig{
						Provider: core.CaptchaProviderTurnstile, SiteKey: "site", SecretKey: "secret",
						FailClosed: v.failClosed, Hostnames: []string{"church.example"},
					}
					if v.enabled {
						ep.enable(&app.Settings().Captcha.Actions)
					}
					if v.enabled {
						apis.SetCaptchaVerifierForTest(app, fake)
					}
				},
				AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
					if fake.Hits() != v.wantHits {
						t.Fatalf("expected %d provider calls, got %d", v.wantHits, fake.Hits())
					}
					if v.wantHits > 0 {
						if fake.last.Action != ep.action {
							t.Fatalf("expected action %q, got %q", ep.action, fake.last.Action)
						}
						if len(fake.last.Hostnames) != 1 || fake.last.Hostnames[0] != "church.example" {
							t.Fatalf("hostnames not forwarded: %v", fake.last.Hostnames)
						}
						wantToken := "body-token"
						if v.headers != nil {
							wantToken = "header-token"
						}
						if fake.last.Token != wantToken {
							t.Fatalf("expected token %q, got %q", wantToken, fake.last.Token)
						}
					}
				},
				ExpectedStatus: v.wantStatus(ep),
				ExpectedEvents: map[string]int{}, // not asserted
			}

			if v.wantFailed {
				sc.ExpectedContent = []string{`"captchaToken":{"code":"captcha_failed"`}
			} else {
				sc.NotExpectedContent = []string{`captcha_failed`}
				if v.wantStatus(ep) == 204 {
					sc.NotExpectedContent = nil
				}
			}

			sc.Test(t)
		}
	}
}

func TestCaptchaSuperuserSkipsAndSignUpOnlyForAuthCollections(t *testing.T) {
	t.Parallel()

	fake := &fakeCaptcha{res: captcha.Result{Reason: "x"}}

	scenarios := []tests.ApiScenario{
		{
			Name:   "superuser creating a user needs no captcha",
			Method: http.MethodPost,
			URL:    "/api/collections/users/records",
			Body:   strings.NewReader(`{"email":"made@example.com","password":"1234567890","passwordConfirm":"1234567890"}`),
			Headers: map[string]string{
				"Authorization": "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6InN5d2JoZWNuaDQ2cmhtMCIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoicGJjXzMxNDI2MzU4MjMiLCJleHAiOjI1MjQ2MDQ0NjEsInJlZnJlc2hhYmxlIjp0cnVlfQ.UXgO3j-0BumcugrFjbd7j0M4MQvbrLggLlcu_YNGjoY",
			},
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				app.Settings().Captcha = core.CaptchaConfig{
					Provider: core.CaptchaProviderTurnstile, SiteKey: "s", SecretKey: "k",
					Actions: core.CaptchaActions{SignUp: true},
				}
				apis.SetCaptchaVerifierForTest(app, fake)
			},
			ExpectedStatus:     200,
			NotExpectedContent: []string{"captcha_failed"},
			ExpectedEvents:     map[string]int{},
		},
		{
			Name:   "base collection records are not affected",
			Method: http.MethodPost,
			URL:    "/api/collections/demo2/records",
			Body:   strings.NewReader(`{"title":"hello"}`),
			Headers: map[string]string{
				"Authorization": "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6InN5d2JoZWNuaDQ2cmhtMCIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoicGJjXzMxNDI2MzU4MjMiLCJleHAiOjI1MjQ2MDQ0NjEsInJlZnJlc2hhYmxlIjp0cnVlfQ.UXgO3j-0BumcugrFjbd7j0M4MQvbrLggLlcu_YNGjoY",
			},
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				app.Settings().Captcha = core.CaptchaConfig{
					Provider: core.CaptchaProviderTurnstile, SiteKey: "s", SecretKey: "k",
					Actions: core.CaptchaActions{SignUp: true},
				}
				apis.SetCaptchaVerifierForTest(app, fake)
			},
			ExpectedStatus:     200,
			NotExpectedContent: []string{"captcha_failed"},
			ExpectedEvents:     map[string]int{},
		},
	}

	for _, s := range scenarios {
		s.Test(t)
	}

	if fake.Hits() != 0 {
		t.Fatalf("provider must not be called, got %d", fake.Hits())
	}
}

func TestCaptchaAuthMethodsExposure(t *testing.T) {
	t.Parallel()

	scenarios := []tests.ApiScenario{
		{
			Name:   "off: response unchanged (no captcha key)",
			Method: http.MethodGet,
			URL:    "/api/collections/users/auth-methods",
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				// provider configured but no action enabled
				app.Settings().Captcha = core.CaptchaConfig{Provider: "turnstile", SiteKey: "site", SecretKey: "topsecret"}
			},
			ExpectedStatus:     200,
			NotExpectedContent: []string{`"captcha"`, "topsecret"},
			ExpectedEvents:     map[string]int{"*": 0},
		},
		{
			Name:   "on: provider, site key and required actions without the secret",
			Method: http.MethodGet,
			URL:    "/api/collections/users/auth-methods",
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				app.Settings().Captcha = core.CaptchaConfig{
					Provider: "recaptchaV3", SiteKey: "site-123", SecretKey: "topsecret", MinScore: 0.7,
					Actions: core.CaptchaActions{Login: true, SignUp: true},
				}
			},
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"captcha":{"provider":"recaptchaV3","siteKey":"site-123","required":["signup","login"]}`,
			},
			NotExpectedContent: []string{"topsecret", "minScore"},
			ExpectedEvents:     map[string]int{"*": 0},
		},
	}

	for _, s := range scenarios {
		s.Test(t)
	}
}

func TestCaptchaSettingsAPIRedactsSecret(t *testing.T) {
	t.Parallel()

	su := "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6InN5d2JoZWNuaDQ2cmhtMCIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoicGJjXzMxNDI2MzU4MjMiLCJleHAiOjI1MjQ2MDQ0NjEsInJlZnJlc2hhYmxlIjp0cnVlfQ.UXgO3j-0BumcugrFjbd7j0M4MQvbrLggLlcu_YNGjoY"

	(&tests.ApiScenario{
		Name:    "patch stores the secret but never returns it",
		Method:  http.MethodPatch,
		URL:     "/api/settings",
		Body:    strings.NewReader(`{"captcha":{"provider":"hcaptcha","siteKey":"pub","secretKey":"very-secret","actions":{"login":true}}}`),
		Headers: map[string]string{"Authorization": su},
		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			if got := app.Settings().Captcha.SecretKey; got != "very-secret" {
				t.Fatalf("secret not stored, got %q", got)
			}
		},
		ExpectedStatus:     200,
		ExpectedContent:    []string{`"siteKey":"pub"`, `"provider":"hcaptcha"`},
		NotExpectedContent: []string{"very-secret", "secretKey"},
		ExpectedEvents:     map[string]int{},
	}).Test(t)

	(&tests.ApiScenario{
		Name:    "patch without the secret keeps the stored one",
		Method:  http.MethodPatch,
		URL:     "/api/settings",
		Body:    strings.NewReader(`{"captcha":{"minScore":0.8}}`),
		Headers: map[string]string{"Authorization": su},
		BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
			app.Settings().Captcha = core.CaptchaConfig{Provider: "recaptchaV3", SiteKey: "pub", SecretKey: "kept"}
			if err := app.Save(app.Settings()); err != nil {
				t.Fatal(err)
			}
		},
		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			if got := app.Settings().Captcha.SecretKey; got != "kept" {
				t.Fatalf("secret was lost, got %q", got)
			}
		},
		ExpectedStatus:     200,
		NotExpectedContent: []string{"kept"},
		ExpectedEvents:     map[string]int{},
	}).Test(t)

	(&tests.ApiScenario{
		Name:            "an invalid config is refused",
		Method:          http.MethodPatch,
		URL:             "/api/settings",
		Body:            strings.NewReader(`{"captcha":{"provider":"hcaptcha"}}`),
		Headers:         map[string]string{"Authorization": su},
		ExpectedStatus:  400,
		ExpectedContent: []string{`"captcha"`},
		ExpectedEvents:  map[string]int{},
	}).Test(t)
}
