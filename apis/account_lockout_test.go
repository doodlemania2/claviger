package apis_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Per-account lockout of password sign-in (#139).
func TestAccountLockout(t *testing.T) {
	const url = "/api/collections/clients/auth-with-password"
	body := func(identity, password string) *strings.Reader {
		return strings.NewReader(`{"identity":"` + identity + `","password":"` + password + `"}`)
	}

	// one app and one router for the whole sequence: the counters live in the app
	type env struct {
		app *tests.TestApp
		mux http.Handler
	}
	newEnv := func(max, window int) *env {
		app, err := tests.NewTestApp()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(app.Cleanup)
		app.Settings().AccountLockout = core.AccountLockoutConfig{MaxFailures: max, WindowSeconds: window}
		if err := app.Save(app.Settings()); err != nil {
			t.Fatal(err)
		}

		baseRouter, err := apis.NewRouter(app)
		if err != nil {
			t.Fatal(err)
		}
		var mux http.Handler
		serveEvent := &core.ServeEvent{App: app, Router: baseRouter}
		if err := app.OnServe().Trigger(serveEvent, func(e *core.ServeEvent) error {
			m, err := e.Router.BuildMux()
			mux = m
			return err
		}); err != nil {
			t.Fatal(err)
		}

		return &env{app, mux}
	}

	run := func(t *testing.T, x *env, identity, password string, status int, content ...string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, url, body(identity, password))
		req.Header.Set("content-type", "application/json")
		x.mux.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("%s/%s: status %d, want %d: %s", identity, password, rec.Code, status, rec.Body.String())
		}
		for _, c := range content {
			if !strings.Contains(rec.Body.String(), c) {
				t.Fatalf("%s/%s: body %q lacks %q", identity, password, rec.Body.String(), c)
			}
		}
		return rec
	}

	t.Run("locks the identity after the failures, even for the right password", func(t *testing.T) {
		app := newEnv(3, 60)

		for i := 0; i < 3; i++ {
			run(t, app, "test@example.com", "wrong", 400, `"message":"Failed to authenticate."`)
		}
		rec := run(t, app, "test@example.com", "1234567890", 429, "Too many failed sign-in attempts")
		if rec.Header().Get("Retry-After") == "" {
			t.Fatal("missing Retry-After")
		}
		// another identity is not affected
		run(t, app, "other@example.com", "x", 400, `"message":"Failed to authenticate."`)
	})

	t.Run("an identity that does not exist is counted the same way", func(t *testing.T) {
		app := newEnv(2, 60)

		run(t, app, "nobody@example.com", "x", 400)
		run(t, app, "nobody@example.com", "x", 400)
		run(t, app, "nobody@example.com", "x", 429, "Too many failed sign-in attempts")
		// the same case-insensitive identity
		run(t, app, " NOBODY@example.com ", "x", 429, "Too many failed sign-in attempts")
	})

	t.Run("a success clears the failures", func(t *testing.T) {
		app := newEnv(3, 60)

		run(t, app, "test@example.com", "wrong", 400)
		run(t, app, "test@example.com", "wrong", 400)
		run(t, app, "test@example.com", "1234567890", 200, `"token"`)
		run(t, app, "test@example.com", "wrong", 400)
		run(t, app, "test@example.com", "wrong", 400)
		run(t, app, "test@example.com", "1234567890", 200, `"token"`)
	})

	t.Run("off by default", func(t *testing.T) {
		app := newEnv(0, 0)

		for i := 0; i < 10; i++ {
			run(t, app, "test@example.com", "wrong", 400)
		}
		run(t, app, "test@example.com", "1234567890", 200, `"token"`)
	})
}

func TestAccountLockoutSettingsValidation(t *testing.T) {
	for _, c := range []struct {
		cfg core.AccountLockoutConfig
		ok  bool
	}{
		{core.AccountLockoutConfig{}, true},
		{core.AccountLockoutConfig{MaxFailures: 5, WindowSeconds: 600}, true},
		{core.AccountLockoutConfig{MaxFailures: -1}, false},
		{core.AccountLockoutConfig{MaxFailures: 101}, false},
		{core.AccountLockoutConfig{MaxFailures: 5, WindowSeconds: 86401}, false},
	} {
		if err := c.cfg.Validate(); (err == nil) != c.ok {
			t.Errorf("%+v: ok=%v, got %v", c.cfg, c.ok, err)
		}
	}
}
