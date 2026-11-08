// Package captcha verifies CAPTCHA tokens with a third party provider.
//
// Every provider implements the small [Verifier] interface, so more can be
// added without touching the callers. A verifier distinguishes two outcomes:
//
//   - the provider answered and rejected the token: Result.Success is false;
//   - the provider could not be asked or answered nonsense (network error,
//     timeout, 5xx, bad JSON): Verify returns an error (an "outage"). The
//     caller decides whether an outage fails open or closed.
package captcha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Provider names (the values of Settings.Captcha.Provider).
const (
	ProviderTurnstile   = "turnstile"
	ProviderHCaptcha    = "hcaptcha"
	ProviderRecaptchaV2 = "recaptchaV2"
	ProviderRecaptchaV3 = "recaptchaV3"
)

// Providers lists the supported provider names.
var Providers = []string{ProviderTurnstile, ProviderHCaptcha, ProviderRecaptchaV2, ProviderRecaptchaV3}

// DefaultMinScore is the reCAPTCHA v3 score below which a token is rejected
// when no minimum is configured.
const DefaultMinScore = 0.5

// DefaultTimeout bounds one verification call.
const DefaultTimeout = 10 * time.Second

// Default provider siteverify endpoints.
const (
	TurnstileEndpoint = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	HCaptchaEndpoint  = "https://api.hcaptcha.com/siteverify"
	RecaptchaEndpoint = "https://www.google.com/recaptcha/api/siteverify"
)

// Request is a single verification request.
type Request struct {
	// Token is the client response token.
	Token string

	// RemoteIP is the end user IP (optional but recommended).
	RemoteIP string

	// Action is the action the token must have been issued for. It is only
	// enforced when the provider reports an action (reCAPTCHA v3 always does,
	// Turnstile when the widget was rendered with one).
	Action string

	// Hostnames, when not empty, restricts the hostname the provider reports
	// the challenge was solved on. It is only enforced when the provider
	// reports a hostname.
	Hostnames []string
}

// Result is the provider verdict.
type Result struct {
	Success bool

	// Reason is a short, non sensitive explanation of a rejection
	// (e.g. "invalid-input-response", "low-score", "hostname-mismatch").
	Reason string
}

// Verifier validates CAPTCHA tokens.
type Verifier interface {
	// Name returns the provider name (one of Providers).
	Name() string

	// Verify checks the token. A non-nil error means the provider was
	// unavailable (outage), not that the token was rejected.
	Verify(ctx context.Context, req Request) (Result, error)
}

// Config describes a verifier to build.
type Config struct {
	Provider  string
	SecretKey string

	// MinScore applies to reCAPTCHA v3 only (0 means DefaultMinScore).
	MinScore float64

	// Timeout bounds one call (0 means DefaultTimeout).
	Timeout time.Duration

	// Endpoint overrides the provider siteverify URL (tests, proxies).
	Endpoint string

	// Client overrides the HTTP client (tests).
	Client *http.Client
}

// New builds the verifier for the configured provider.
func New(cfg Config) (Verifier, error) {
	if cfg.SecretKey == "" {
		return nil, errors.New("captcha: missing secret key")
	}

	v := &siteVerify{
		name:     cfg.Provider,
		secret:   cfg.SecretKey,
		endpoint: cfg.Endpoint,
		timeout:  cfg.Timeout,
		client:   cfg.Client,
	}

	if v.timeout <= 0 {
		v.timeout = DefaultTimeout
	}

	switch cfg.Provider {
	case ProviderTurnstile:
		if v.endpoint == "" {
			v.endpoint = TurnstileEndpoint
		}
	case ProviderHCaptcha:
		if v.endpoint == "" {
			v.endpoint = HCaptchaEndpoint
		}
	case ProviderRecaptchaV2:
		if v.endpoint == "" {
			v.endpoint = RecaptchaEndpoint
		}
	case ProviderRecaptchaV3:
		if v.endpoint == "" {
			v.endpoint = RecaptchaEndpoint
		}
		v.scored = true
		v.minScore = cfg.MinScore
		if v.minScore <= 0 {
			v.minScore = DefaultMinScore
		}
	default:
		return nil, fmt.Errorf("captcha: unsupported provider %q", cfg.Provider)
	}

	return v, nil
}

// siteVerify implements the "siteverify" protocol shared by Turnstile,
// hCaptcha and reCAPTCHA (form POST of secret/response/remoteip, JSON reply).
type siteVerify struct {
	name     string
	secret   string
	endpoint string
	timeout  time.Duration
	client   *http.Client

	scored   bool
	minScore float64
}

func (v *siteVerify) Name() string { return v.name }

type siteVerifyResponse struct {
	Success    bool     `json:"success"`
	Hostname   string   `json:"hostname"`
	Action     string   `json:"action"`
	Score      *float64 `json:"score"`
	ErrorCodes []string `json:"error-codes"`
}

// maxResponseBytes caps what is read from a provider.
const maxResponseBytes = 64 << 10

func (v *siteVerify) Verify(ctx context.Context, req Request) (Result, error) {
	if strings.TrimSpace(req.Token) == "" {
		return Result{Reason: "missing-input-response"}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	form := url.Values{}
	form.Set("secret", v.secret)
	form.Set("response", req.Token)
	if req.RemoteIP != "" {
		form.Set("remoteip", req.RemoteIP)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")

	client := v.client
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", v.name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return Result{}, fmt.Errorf("%s: provider status %d", v.name, resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", v.name, err)
	}

	var out siteVerifyResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("%s: invalid provider response: %w", v.name, err)
	}

	if !out.Success {
		// "internal-error" / "bad-request" style codes are the provider's
		// problem, not a verdict on the token
		for _, code := range out.ErrorCodes {
			if code == "internal-error" {
				return Result{}, fmt.Errorf("%s: provider internal error", v.name)
			}
			if code == "missing-input-secret" || code == "invalid-input-secret" {
				// misconfiguration: surface as an outage so it is loud in the
				// logs and honors the fail-open/closed choice
				return Result{}, fmt.Errorf("%s: %s (check the secret key)", v.name, code)
			}
		}

		reason := "rejected"
		if len(out.ErrorCodes) > 0 {
			reason = out.ErrorCodes[0]
		}

		return Result{Reason: reason}, nil
	}

	if len(req.Hostnames) > 0 && out.Hostname != "" && !hostnameAllowed(req.Hostnames, out.Hostname) {
		return Result{Reason: "hostname-mismatch"}, nil
	}

	if req.Action != "" && out.Action != "" && out.Action != req.Action {
		return Result{Reason: "action-mismatch"}, nil
	}

	if v.scored {
		if out.Score == nil {
			return Result{Reason: "missing-score"}, nil
		}
		if *out.Score < v.minScore {
			return Result{Reason: "low-score"}, nil
		}
	}

	return Result{Success: true}, nil
}

func hostnameAllowed(allowed []string, got string) bool {
	for _, h := range allowed {
		if strings.EqualFold(strings.TrimSpace(h), got) {
			return true
		}
	}

	return false
}
