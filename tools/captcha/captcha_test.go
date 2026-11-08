package captcha_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/tools/captcha"
)

// fake provider: replies with the given JSON and records the form it got.
type fake struct {
	srv  *httptest.Server
	form map[string]string
	hits int
}

func newFake(t *testing.T, status int, body string) *fake {
	t.Helper()
	f := &fake{form: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits++
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		_ = r.ParseForm()
		for k := range r.PostForm {
			f.form[k] = r.PostForm.Get(k)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newVerifier(t *testing.T, provider string, f *fake, minScore float64) captcha.Verifier {
	t.Helper()
	v, err := captcha.New(captcha.Config{Provider: provider, SecretKey: "sekret", MinScore: minScore, Endpoint: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifiers(t *testing.T) {
	t.Parallel()

	score := func(s string) string {
		return `{"success":true,"hostname":"church.example","action":"login","score":` + s + `}`
	}

	scenarios := []struct {
		name      string
		provider  string
		status    int
		body      string
		minScore  float64
		req       captcha.Request
		wantOK    bool
		wantReas  string
		wantError bool
	}{
		{"turnstile success", captcha.ProviderTurnstile, 200, `{"success":true,"hostname":"church.example"}`, 0, captcha.Request{Token: "t", RemoteIP: "1.2.3.4"}, true, "", false},
		{"turnstile failure", captcha.ProviderTurnstile, 200, `{"success":false,"error-codes":["invalid-input-response"]}`, 0, captcha.Request{Token: "t"}, false, "invalid-input-response", false},
		{"turnstile wrong hostname", captcha.ProviderTurnstile, 200, `{"success":true,"hostname":"evil.example"}`, 0, captcha.Request{Token: "t", Hostnames: []string{"church.example"}}, false, "hostname-mismatch", false},
		{"turnstile hostname case-insensitive", captcha.ProviderTurnstile, 200, `{"success":true,"hostname":"Church.Example"}`, 0, captcha.Request{Token: "t", Hostnames: []string{"church.example"}}, true, "", false},
		{"turnstile hostname not reported", captcha.ProviderTurnstile, 200, `{"success":true}`, 0, captcha.Request{Token: "t", Hostnames: []string{"church.example"}}, true, "", false},
		{"turnstile wrong action", captcha.ProviderTurnstile, 200, `{"success":true,"action":"signup"}`, 0, captcha.Request{Token: "t", Action: "login"}, false, "action-mismatch", false},
		{"turnstile action not reported", captcha.ProviderTurnstile, 200, `{"success":true}`, 0, captcha.Request{Token: "t", Action: "login"}, true, "", false},
		{"hcaptcha success", captcha.ProviderHCaptcha, 200, `{"success":true,"hostname":"church.example"}`, 0, captcha.Request{Token: "t"}, true, "", false},
		{"hcaptcha failure", captcha.ProviderHCaptcha, 200, `{"success":false,"error-codes":["invalid-or-already-seen-response"]}`, 0, captcha.Request{Token: "t"}, false, "invalid-or-already-seen-response", false},
		{"hcaptcha wrong hostname", captcha.ProviderHCaptcha, 200, `{"success":true,"hostname":"evil.example"}`, 0, captcha.Request{Token: "t", Hostnames: []string{"church.example"}}, false, "hostname-mismatch", false},
		{"recaptcha v2 success", captcha.ProviderRecaptchaV2, 200, `{"success":true,"hostname":"church.example"}`, 0, captcha.Request{Token: "t"}, true, "", false},
		{"recaptcha v2 failure", captcha.ProviderRecaptchaV2, 200, `{"success":false,"error-codes":["timeout-or-duplicate"]}`, 0, captcha.Request{Token: "t"}, false, "timeout-or-duplicate", false},
		{"recaptcha v2 ignores score", captcha.ProviderRecaptchaV2, 200, score("0.1"), 0, captcha.Request{Token: "t"}, true, "", false},
		{"recaptcha v3 default min score ok", captcha.ProviderRecaptchaV3, 200, score("0.5"), 0, captcha.Request{Token: "t", Action: "login"}, true, "", false},
		{"recaptcha v3 low score", captcha.ProviderRecaptchaV3, 200, score("0.3"), 0, captcha.Request{Token: "t", Action: "login"}, false, "low-score", false},
		{"recaptcha v3 custom min score fail", captcha.ProviderRecaptchaV3, 200, score("0.7"), 0.9, captcha.Request{Token: "t", Action: "login"}, false, "low-score", false},
		{"recaptcha v3 custom min score ok", captcha.ProviderRecaptchaV3, 200, score("0.95"), 0.9, captcha.Request{Token: "t", Action: "login"}, true, "", false},
		{"recaptcha v3 wrong action", captcha.ProviderRecaptchaV3, 200, score("0.9"), 0, captcha.Request{Token: "t", Action: "signup"}, false, "action-mismatch", false},
		{"recaptcha v3 missing score", captcha.ProviderRecaptchaV3, 200, `{"success":true,"action":"login"}`, 0, captcha.Request{Token: "t", Action: "login"}, false, "missing-score", false},
		{"missing token", captcha.ProviderTurnstile, 200, `{"success":true}`, 0, captcha.Request{Token: " "}, false, "missing-input-response", false},
		{"5xx is an outage", captcha.ProviderTurnstile, 503, `oops`, 0, captcha.Request{Token: "t"}, false, "", true},
		{"429 is an outage", captcha.ProviderHCaptcha, 429, `{}`, 0, captcha.Request{Token: "t"}, false, "", true},
		{"garbage body is an outage", captcha.ProviderRecaptchaV2, 200, `<html>`, 0, captcha.Request{Token: "t"}, false, "", true},
		{"internal-error is an outage", captcha.ProviderTurnstile, 200, `{"success":false,"error-codes":["internal-error"]}`, 0, captcha.Request{Token: "t"}, false, "", true},
		{"bad secret is an outage", captcha.ProviderTurnstile, 200, `{"success":false,"error-codes":["invalid-input-secret"]}`, 0, captcha.Request{Token: "t"}, false, "", true},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			f := newFake(t, s.status, s.body)
			v := newVerifier(t, s.provider, f, s.minScore)

			res, err := v.Verify(context.Background(), s.req)
			if (err != nil) != s.wantError {
				t.Fatalf("error = %v, want error %v", err, s.wantError)
			}
			if res.Success != s.wantOK {
				t.Fatalf("success = %v (%s), want %v", res.Success, res.Reason, s.wantOK)
			}
			if res.Reason != s.wantReas {
				t.Fatalf("reason = %q, want %q", res.Reason, s.wantReas)
			}
			if v.Name() != s.provider {
				t.Fatalf("name = %q", v.Name())
			}
		})
	}
}

func TestVerifierSendsSecretTokenAndIP(t *testing.T) {
	t.Parallel()

	f := newFake(t, 200, `{"success":true}`)
	v := newVerifier(t, captcha.ProviderHCaptcha, f, 0)

	if _, err := v.Verify(context.Background(), captcha.Request{Token: "tok", RemoteIP: "9.9.9.9"}); err != nil {
		t.Fatal(err)
	}
	if f.form["secret"] != "sekret" || f.form["response"] != "tok" || f.form["remoteip"] != "9.9.9.9" {
		t.Fatalf("unexpected form %v", f.form)
	}

	// no IP: the field is omitted
	f.form = map[string]string{}
	if _, err := v.Verify(context.Background(), captcha.Request{Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.form["remoteip"]; ok {
		t.Fatal("remoteip must be omitted when unknown")
	}
}

func TestVerifierMissingTokenDoesNotCallProvider(t *testing.T) {
	t.Parallel()

	f := newFake(t, 200, `{"success":true}`)
	v := newVerifier(t, captcha.ProviderTurnstile, f, 0)
	_, _ = v.Verify(context.Background(), captcha.Request{})
	if f.hits != 0 {
		t.Fatal("provider must not be called without a token")
	}
}

func TestVerifierTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer srv.Close()

	v, err := captcha.New(captcha.Config{Provider: captcha.ProviderTurnstile, SecretKey: "s", Endpoint: srv.URL, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	res, err := v.Verify(context.Background(), captcha.Request{Token: "t"})
	if err == nil || res.Success {
		t.Fatalf("expected a timeout outage, got %+v, %v", res, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout was not enforced")
	}
}

func TestNewValidation(t *testing.T) {
	t.Parallel()

	if _, err := captcha.New(captcha.Config{Provider: "nope", SecretKey: "s"}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported provider error, got %v", err)
	}
	if _, err := captcha.New(captcha.Config{Provider: captcha.ProviderTurnstile}); err == nil {
		t.Fatal("expected missing secret error")
	}
}
