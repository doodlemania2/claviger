package core

import (
	"errors"

	"github.com/doodlemania2/claviger/tools/sms"
)

// ErrSMSNotConfigured is returned by [NewSMSSender] while SMS is disabled.
var ErrSMSNotConfigured = errors.New("SMS is not configured")

// NewSMSSender builds an [sms.Sender] from the current app settings (#83).
//
// It returns [ErrSMSNotConfigured] when Settings.SMS is disabled. It is a
// plain function rather than a core.App method on purpose, so the App
// interface (and the jsvm binds) stay untouched. The sender is cheap to
// build; callers should not cache it across settings changes.
func NewSMSSender(app App) (sms.Sender, error) {
	c := app.Settings().SMS

	if !c.Enabled {
		return nil, ErrSMSNotConfigured
	}

	switch c.Provider {
	case SMSProviderTwilio:
		return sms.NewTwilio(sms.TwilioConfig{
			AccountSID:          c.Twilio.AccountSID,
			AuthToken:           c.Twilio.AuthToken,
			APIKeySID:           c.Twilio.APIKeySID,
			APIKeySecret:        c.Twilio.APIKeySecret,
			From:                c.Twilio.From,
			MessagingServiceSID: c.Twilio.MessagingServiceSID,
		})
	case SMSProviderWebhook:
		return sms.NewWebhook(sms.WebhookConfig{
			URL:    c.Webhook.URL,
			Secret: c.Webhook.Secret,
		})
	}

	return nil, ErrSMSNotConfigured
}
