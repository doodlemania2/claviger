package sms

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Header names set by the [Webhook] sender.
const (
	WebhookTimestampHeader = "X-Claviger-Timestamp"
	WebhookSignatureHeader = "X-Claviger-Signature"
)

// WebhookConfig configures the [Webhook] sender.
type WebhookConfig struct {
	// URL is the http(s) endpoint that receives the message.
	URL string

	// Secret signs the request body (HMAC-SHA256). Required.
	Secret string

	// Timeout is the per-request timeout (default 15s).
	Timeout time.Duration

	// Client optionally supplies the transport (tests).
	Client *http.Client

	// Now overrides the clock (tests).
	Now func() time.Time
}

// Webhook POSTs {"to": "...", "body": "..."} as JSON to a configured URL.
//
// The request carries X-Claviger-Timestamp (unix seconds) and
// X-Claviger-Signature: "v1=" plus the hex HMAC-SHA256 of "<timestamp>.<raw
// body>" keyed with the secret (see [WebhookSignature]). Receivers should
// recompute it, compare in constant time and reject old timestamps. Any 2xx
// response is a success.
type Webhook struct {
	cfg    WebhookConfig
	client *http.Client
}

var _ Sender = (*Webhook)(nil)

// NewWebhook validates cfg and returns a Webhook sender.
func NewWebhook(cfg WebhookConfig) (*Webhook, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("sms: the webhook URL must be an absolute http(s) URL")
	}
	if cfg.Secret == "" {
		return nil, errors.New("sms: the webhook secret is required")
	}

	return &Webhook{cfg: cfg, client: newClient(cfg.Client, cfg.Timeout)}, nil
}

// WebhookSignature returns the X-Claviger-Signature value for the timestamp
// (unix seconds) and raw body.
func WebhookSignature(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", timestamp)
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Send implements [Sender].
func (w *Webhook) Send(ctx context.Context, m *Message) error {
	if err := validateMessage(m); err != nil {
		return err
	}

	payload, err := json.Marshal(struct {
		To   string `json:"to"`
		Body string `json:"body"`
	}{m.To, m.Body})
	if err != nil {
		return &Error{Provider: "webhook", Message: "could not encode the message"}
	}

	now := time.Now
	if w.cfg.Now != nil {
		now = w.cfg.Now
	}
	ts := now().Unix()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.URL, bytes.NewReader(payload))
	if err != nil {
		return &Error{Provider: "webhook", Message: "could not build the request"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(WebhookTimestampHeader, strconv.FormatInt(ts, 10))
	req.Header.Set(WebhookSignatureHeader, WebhookSignature(w.cfg.Secret, ts, payload))

	res, err := w.client.Do(req)
	if err != nil {
		return transportError("webhook", err, m.To, w.cfg.Secret, w.cfg.URL)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxResponse))

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}

	return &Error{Provider: "webhook", HTTPStatus: res.StatusCode, Message: "the endpoint rejected the message"}
}
