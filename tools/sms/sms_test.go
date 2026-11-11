package sms_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/tools/sms"
)

func TestE164AndMask(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{"+14155550123", "+442071838750", "+1234567"} {
		if !sms.IsE164(ok) || sms.ValidateE164(ok) != nil {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "14155550123", "+0123456789", "+1 415 555 0123", "+1(415)5550123", "+123456", "+1234567890123456", "tel:+14155550123", "+14155550123\n", "+1415555012a"} {
		if sms.IsE164(bad) || !errors.Is(sms.ValidateE164(bad), sms.ErrInvalidNumber) {
			t.Errorf("%q should be invalid", bad)
		}
	}

	if got := sms.MaskNumber("+14155550123"); got != "+*********23" {
		t.Errorf("unexpected mask %q", got)
	}
	if got := sms.MaskNumber("+1"); strings.Contains(got, "1") {
		t.Errorf("short number leaked: %q", got)
	}
}

func TestTwilioSendSuccess(t *testing.T) {
	t.Parallel()

	var gotPath, gotUser, gotPass, gotCT string
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser, gotPass, _ = r.BasicAuth()
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(raw))
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"sid":"SM1","status":"queued"}`))
	}))
	defer srv.Close()

	tw, err := sms.NewTwilio(sms.TwilioConfig{AccountSID: "ACabc", AuthToken: "tok-123456", From: "+15005550006", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Send(context.Background(), &sms.Message{To: "+14155550123", Body: "hello & welcome"}); err != nil {
		t.Fatal(err)
	}

	if gotPath != "/2010-04-01/Accounts/ACabc/Messages.json" {
		t.Errorf("path %q", gotPath)
	}
	if gotUser != "ACabc" || gotPass != "tok-123456" {
		t.Errorf("basic auth %q %q", gotUser, gotPass)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("content type %q", gotCT)
	}
	if gotForm.Get("To") != "+14155550123" || gotForm.Get("From") != "+15005550006" || gotForm.Get("Body") != "hello & welcome" || gotForm.Get("MessagingServiceSid") != "" {
		t.Errorf("form %v", gotForm)
	}
}

func TestTwilioAPIKeyAndMessagingService(t *testing.T) {
	t.Parallel()

	var gotUser, gotPass string
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		raw, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(raw))
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	tw, err := sms.NewTwilio(sms.TwilioConfig{AccountSID: "ACabc", APIKeySID: "SKkey", APIKeySecret: "key-secret", MessagingServiceSID: "MG1", BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Send(context.Background(), &sms.Message{To: "+14155550123", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if gotUser != "SKkey" || gotPass != "key-secret" {
		t.Errorf("basic auth %q %q", gotUser, gotPass)
	}
	if gotForm.Get("MessagingServiceSid") != "MG1" || gotForm.Has("From") {
		t.Errorf("form %v", gotForm)
	}
}

func TestTwilioConfigValidation(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]sms.TwilioConfig{
		"no sid":       {AuthToken: "t", From: "+15005550006"},
		"bad sid":      {AccountSID: "AC/../x", AuthToken: "t", From: "+15005550006"},
		"no auth":      {AccountSID: "AC1", From: "+15005550006"},
		"half api key": {AccountSID: "AC1", APIKeySID: "SK1", From: "+15005550006"},
		"no sender":    {AccountSID: "AC1", AuthToken: "t"},
		"bad from":     {AccountSID: "AC1", AuthToken: "t", From: "555-0100"},
	} {
		if _, err := sms.NewTwilio(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestTwilioErrorMappingAndNoSecretLeak(t *testing.T) {
	t.Parallel()

	const token = "super-secret-token-value"
	const to = "+14155550123"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// an echoing response: the message repeats the token and the number
		fmt.Fprintf(w, `{"code":21211,"message":"The 'To' number %s is not valid, token %s","more_info":"https://www.twilio.com/docs/errors/21211","status":400}`, to, token)
	}))
	defer srv.Close()

	tw, _ := sms.NewTwilio(sms.TwilioConfig{AccountSID: "ACabc", AuthToken: token, From: "+15005550006", BaseURL: srv.URL})
	err := tw.Send(context.Background(), &sms.Message{To: to, Body: "x"})

	var se *sms.Error
	if !errors.As(err, &se) {
		t.Fatalf("expected *sms.Error, got %T %v", err, err)
	}
	if se.Provider != "twilio" || se.HTTPStatus != 400 || se.Code != "21211" {
		t.Errorf("unexpected error fields %+v", se)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), to) {
		t.Errorf("secret or full number leaked: %s", err)
	}
	if !strings.Contains(err.Error(), "21211") {
		t.Errorf("code missing: %s", err)
	}
}

func TestTwilioNonJSONError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>bad gateway</html>"))
	}))
	defer srv.Close()

	tw, _ := sms.NewTwilio(sms.TwilioConfig{AccountSID: "ACabc", AuthToken: "tok-123456", From: "+15005550006", BaseURL: srv.URL})
	err := tw.Send(context.Background(), &sms.Message{To: "+14155550123", Body: "x"})

	var se *sms.Error
	if !errors.As(err, &se) || se.HTTPStatus != 502 || se.Code != "" {
		t.Fatalf("unexpected %v", err)
	}
}

func TestSendRejectsBadInput(t *testing.T) {
	t.Parallel()

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()

	tw, _ := sms.NewTwilio(sms.TwilioConfig{AccountSID: "ACabc", AuthToken: "tok-123456", From: "+15005550006", BaseURL: srv.URL})
	wh, _ := sms.NewWebhook(sms.WebhookConfig{URL: srv.URL, Secret: "s"})

	for _, s := range []sms.Sender{tw, wh} {
		if err := s.Send(context.Background(), &sms.Message{To: "4155550123", Body: "x"}); !errors.Is(err, sms.ErrInvalidNumber) {
			t.Errorf("expected ErrInvalidNumber, got %v", err)
		}
		if err := s.Send(context.Background(), &sms.Message{To: "+14155550123", Body: "  "}); err == nil {
			t.Error("expected an error for an empty body")
		}
		if err := s.Send(context.Background(), nil); err == nil {
			t.Error("expected an error for a nil message")
		}
	}
	if called {
		t.Fatal("nothing should reach the provider for invalid input")
	}
}

func TestTwilioTimeout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	tw, _ := sms.NewTwilio(sms.TwilioConfig{AccountSID: "ACabc", AuthToken: "tok-123456", From: "+15005550006", BaseURL: srv.URL, Timeout: 50 * time.Millisecond})

	start := time.Now()
	err := tw.Send(context.Background(), &sms.Message{To: "+14155550123", Body: "x"})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("timeout not enforced: %v", time.Since(start))
	}
	if strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), "tok-123456") {
		t.Errorf("leak in %q", err)
	}
}

func TestContextCancel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()

	wh, _ := sms.NewWebhook(sms.WebhookConfig{URL: srv.URL, Secret: "s"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := wh.Send(ctx, &sms.Message{To: "+14155550123", Body: "x"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestWebhookSendAndSignature(t *testing.T) {
	t.Parallel()

	const secret = "hook-secret-value"
	now := time.Unix(1700000000, 0)

	var gotBody []byte
	var gotHeader http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeader = r.Header.Clone()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	wh, err := sms.NewWebhook(sms.WebhookConfig{URL: srv.URL, Secret: secret, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := wh.Send(context.Background(), &sms.Message{To: "+14155550123", Body: `hi "there"`}); err != nil {
		t.Fatal(err)
	}

	if string(gotBody) != `{"to":"+14155550123","body":"hi \"there\""}` {
		t.Errorf("body %s", gotBody)
	}
	if gotHeader.Get("Content-Type") != "application/json" || gotHeader.Get(sms.WebhookTimestampHeader) != "1700000000" {
		t.Errorf("headers %v", gotHeader)
	}

	// independent recomputation of the documented scheme
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("1700000000."))
	mac.Write(gotBody)
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	if got := gotHeader.Get(sms.WebhookSignatureHeader); got != want {
		t.Errorf("signature %q, want %q", got, want)
	}
	if want != sms.WebhookSignature(secret, 1700000000, gotBody) {
		t.Error("WebhookSignature disagrees")
	}
	if sms.WebhookSignature("other", 1700000000, gotBody) == want {
		t.Error("signature must depend on the secret")
	}
	if sms.WebhookSignature(secret, 1700000001, gotBody) == want {
		t.Error("signature must depend on the timestamp")
	}
}

func TestWebhookErrorsAndValidation(t *testing.T) {
	t.Parallel()

	const secret = "hook-secret-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, secret, " +14155550123")
	}))
	defer srv.Close()

	wh, _ := sms.NewWebhook(sms.WebhookConfig{URL: srv.URL + "/path?token=urltoken", Secret: secret})
	err := wh.Send(context.Background(), &sms.Message{To: "+14155550123", Body: "x"})

	var se *sms.Error
	if !errors.As(err, &se) || se.Provider != "webhook" || se.HTTPStatus != 500 {
		t.Fatalf("unexpected %v", err)
	}
	for _, leak := range []string{secret, "+14155550123", "urltoken"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("%q leaked: %s", leak, err)
		}
	}

	// connection failure: no URL (it may carry a token) in the text
	closed := httptest.NewServer(http.NotFoundHandler())
	u := closed.URL + "/x?token=urltoken"
	closed.Close()
	wh2, _ := sms.NewWebhook(sms.WebhookConfig{URL: u, Secret: secret})
	err = wh2.Send(context.Background(), &sms.Message{To: "+14155550123", Body: "x"})
	if err == nil || strings.Contains(err.Error(), "urltoken") || strings.Contains(err.Error(), secret) {
		t.Fatalf("unexpected %v", err)
	}

	for _, cfg := range []sms.WebhookConfig{{URL: "", Secret: "s"}, {URL: "ftp://x", Secret: "s"}, {URL: "/relative", Secret: "s"}, {URL: "https://x.example", Secret: ""}} {
		if _, err := sms.NewWebhook(cfg); err == nil {
			t.Errorf("expected an error for %+v", cfg)
		}
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	t.Parallel()

	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	tw, _ := sms.NewTwilio(sms.TwilioConfig{AccountSID: "ACabc", AuthToken: "tok-123456", From: "+15005550006", BaseURL: srv.URL})
	if err := tw.Send(context.Background(), &sms.Message{To: "+14155550123", Body: "x"}); err == nil {
		t.Fatal("a redirect must be an error")
	}
	if hit {
		t.Fatal("redirect was followed")
	}
}
