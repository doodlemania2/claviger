package sms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TwilioBaseURL is the default Twilio REST API base URL.
const TwilioBaseURL = "https://api.twilio.com"

// TwilioConfig configures the [Twilio] sender.
type TwilioConfig struct {
	// AccountSID is required (it is part of the request path). It is also
	// the basic-auth user unless APIKeySID is set.
	AccountSID string

	// AuthToken is the basic-auth password when no API key is used.
	AuthToken string

	// APIKeySID and APIKeySecret, when both are set, are used for the
	// basic auth instead of AccountSID and AuthToken.
	APIKeySID    string
	APIKeySecret string

	// From is the sending number in E.164 format. Exactly one of From and
	// MessagingServiceSID is required (MessagingServiceSID wins).
	From                string
	MessagingServiceSID string

	// BaseURL overrides [TwilioBaseURL] (tests).
	BaseURL string

	// Timeout is the per-request timeout (default 15s).
	Timeout time.Duration

	// Client optionally supplies the transport (tests).
	Client *http.Client
}

// Twilio sends messages through the Twilio Messages REST API.
type Twilio struct {
	cfg    TwilioConfig
	client *http.Client
	user   string
	pass   string
	url    string
}

var _ Sender = (*Twilio)(nil)

// NewTwilio validates cfg and returns a Twilio sender.
func NewTwilio(cfg TwilioConfig) (*Twilio, error) {
	if cfg.AccountSID == "" {
		return nil, errors.New("sms: twilio account SID is required")
	}
	if cfg.AccountSID != url.PathEscape(cfg.AccountSID) {
		return nil, errors.New("sms: twilio account SID is invalid")
	}

	t := &Twilio{cfg: cfg, client: newClient(cfg.Client, cfg.Timeout)}

	switch {
	case cfg.APIKeySID != "" || cfg.APIKeySecret != "":
		if cfg.APIKeySID == "" || cfg.APIKeySecret == "" {
			return nil, errors.New("sms: twilio API key SID and secret must be set together")
		}
		t.user, t.pass = cfg.APIKeySID, cfg.APIKeySecret
	case cfg.AuthToken != "":
		t.user, t.pass = cfg.AccountSID, cfg.AuthToken
	default:
		return nil, errors.New("sms: twilio auth token or API key is required")
	}

	if cfg.MessagingServiceSID == "" {
		if cfg.From == "" {
			return nil, errors.New("sms: twilio from number or messaging service SID is required")
		}
		if err := ValidateE164(cfg.From); err != nil {
			return nil, errors.New("sms: twilio from number must be in E.164 format")
		}
	}

	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = TwilioBaseURL
	}
	t.url = base + "/2010-04-01/Accounts/" + url.PathEscape(cfg.AccountSID) + "/Messages.json"

	return t, nil
}

func (t *Twilio) secrets() []string {
	return []string{t.cfg.AuthToken, t.cfg.APIKeySecret}
}

// Send implements [Sender].
func (t *Twilio) Send(ctx context.Context, m *Message) error {
	if err := validateMessage(m); err != nil {
		return err
	}

	form := url.Values{}
	form.Set("To", m.To)
	form.Set("Body", m.Body)
	if t.cfg.MessagingServiceSID != "" {
		form.Set("MessagingServiceSid", t.cfg.MessagingServiceSID)
	} else {
		form.Set("From", t.cfg.From)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, strings.NewReader(form.Encode()))
	if err != nil {
		return &Error{Provider: "twilio", Message: "could not build the request"}
	}
	req.SetBasicAuth(t.user, t.pass)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	res, err := t.client.Do(req)
	if err != nil {
		return transportError("twilio", err, m.To, t.secrets()...)
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxResponse))

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}

	return twilioError(res.StatusCode, raw, m.To, t.secrets())
}

func twilioError(status int, raw []byte, to string, secrets []string) error {
	e := &Error{Provider: "twilio", HTTPStatus: status}

	var body struct {
		Code    json.Number `json:"code"`
		Message string      `json:"message"`
	}
	if err := json.Unmarshal(raw, &body); err == nil {
		e.Code = body.Code.String()
		e.Message = scrub(body.Message, to, secrets...)
	}
	if _, err := strconv.Atoi(e.Code); err != nil {
		e.Code = ""
	}
	if e.Message == "" {
		e.Message = fmt.Sprintf("unexpected response (HTTP %d)", status)
	}

	return e
}
