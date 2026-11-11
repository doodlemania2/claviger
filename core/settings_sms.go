package core

import (
	"errors"
	"net/url"
	"regexp"
	"strings"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/tools/sms"
)

// SMS providers (#83).
const (
	SMSProviderTwilio  = "twilio"
	SMSProviderWebhook = "webhook"
)

// SMSConfig defines the optional SMS sender (#83). With Enabled false (the
// default) nothing changes and [NewSMSSender] returns [ErrSMSNotConfigured].
type SMSConfig struct {
	Enabled bool `form:"enabled" json:"enabled"`

	// Provider is one of twilio, webhook.
	Provider string `form:"provider" json:"provider"`

	Twilio  SMSTwilioConfig  `form:"twilio" json:"twilio"`
	Webhook SMSWebhookConfig `form:"webhook" json:"webhook"`
}

// SMSTwilioConfig holds the Twilio credentials and sender.
//
// Authenticate with AccountSID + AuthToken, or with APIKeySID + APIKeySecret
// (the AccountSID is still needed for the request path). Send from a number
// (From) or a Messaging Service (MessagingServiceSID).
type SMSTwilioConfig struct {
	AccountSID string `form:"accountSid" json:"accountSid"`

	// AuthToken is write-only: it is stripped from the settings API responses.
	AuthToken string `form:"authToken" json:"authToken,omitempty"`

	APIKeySID string `form:"apiKeySid" json:"apiKeySid"`

	// APIKeySecret is write-only: it is stripped from the settings API responses.
	APIKeySecret string `form:"apiKeySecret" json:"apiKeySecret,omitempty"`

	// From is the sending number in E.164 format.
	From string `form:"from" json:"from"`

	MessagingServiceSID string `form:"messagingServiceSid" json:"messagingServiceSid"`
}

// SMSWebhookConfig holds the generic webhook sender settings.
type SMSWebhookConfig struct {
	URL string `form:"url" json:"url"`

	// Secret is write-only: it is stripped from the settings API responses.
	Secret string `form:"secret" json:"secret,omitempty"`
}

// Validate makes SMSConfig validatable by implementing [validation.Validatable] interface.
func (c SMSConfig) Validate() error {
	return validation.ValidateStruct(&c,
		validation.Field(&c.Provider,
			validation.When(c.Enabled, validation.Required),
			validation.In(SMSProviderTwilio, SMSProviderWebhook),
		),
		validation.Field(&c.Twilio, validation.When(c.Enabled && c.Provider == SMSProviderTwilio, validation.By(func(any) error {
			return c.Twilio.validateRequired()
		}))),
		validation.Field(&c.Webhook, validation.When(c.Enabled && c.Provider == SMSProviderWebhook, validation.By(func(any) error {
			return c.Webhook.validateRequired()
		}))),
	)
}

// Validate makes SMSTwilioConfig validatable (the format checks that apply
// whenever a value is set, even while the provider is not selected).
func (c SMSTwilioConfig) Validate() error {
	return validation.ValidateStruct(&c,
		validation.Field(&c.From, validation.By(func(v any) error {
			if s, _ := v.(string); s != "" && !sms.IsE164(s) {
				return errors.New("must be a phone number in E.164 format, for example +14155550123")
			}
			return nil
		})),
	)
}

func (c SMSTwilioConfig) validateRequired() error {
	hasKey := c.APIKeySID != "" || c.APIKeySecret != ""

	return validation.ValidateStruct(&c,
		validation.Field(&c.AccountSID, validation.Required, validation.Match(smsSIDRegex)),
		validation.Field(&c.AuthToken, validation.When(!hasKey, validation.Required.Error("cannot be blank unless an API key is set"))),
		validation.Field(&c.APIKeySID, validation.When(hasKey, validation.Required)),
		validation.Field(&c.APIKeySecret, validation.When(hasKey, validation.Required)),
		validation.Field(&c.From, validation.When(c.MessagingServiceSID == "", validation.Required.Error("set a from number or a messaging service SID"))),
	)
}

// Validate makes SMSWebhookConfig validatable.
func (c SMSWebhookConfig) Validate() error {
	return validation.ValidateStruct(&c,
		validation.Field(&c.URL, validation.By(func(v any) error {
			s, _ := v.(string)
			if s == "" {
				return nil
			}
			return checkSMSWebhookURL(s)
		})),
	)
}

func (c SMSWebhookConfig) validateRequired() error {
	return validation.ValidateStruct(&c,
		validation.Field(&c.URL, validation.Required),
		validation.Field(&c.Secret, validation.Required),
	)
}

func checkSMSWebhookURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("must be an absolute http(s) URL")
	}
	if u.Scheme == "http" {
		switch strings.ToLower(u.Hostname()) {
		case "localhost", "127.0.0.1", "::1":
		default:
			return errors.New("must use https (plain http is only accepted for localhost)")
		}
	}
	return nil
}

var smsSIDRegex = regexp.MustCompile(`^AC[0-9A-Za-z]+$`)
