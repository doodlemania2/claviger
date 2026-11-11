// Package sms is a small provider interface for sending transactional text
// messages (Claviger #83). It depends on the standard library only.
//
// Implementations: [Twilio] (REST API) and [Webhook] (signed JSON POST).
package sms

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Message is a single text message.
type Message struct {
	// To is the recipient in E.164 format (for example +14155550123).
	To string

	// Body is the message text.
	Body string
}

// Sender sends text messages.
type Sender interface {
	Send(ctx context.Context, m *Message) error
}

// ErrInvalidNumber is returned for a recipient that is not E.164.
var ErrInvalidNumber = errors.New("sms: the phone number must be in E.164 format (+ and up to 15 digits)")

var e164Regex = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// IsE164 reports whether s is an E.164 number: "+", a non-zero country code
// digit and 7 to 14 more digits, with no spaces or punctuation.
func IsE164(s string) bool {
	return e164Regex.MatchString(s)
}

// ValidateE164 returns [ErrInvalidNumber] unless s is an E.164 number.
func ValidateE164(s string) error {
	if !IsE164(s) {
		return ErrInvalidNumber
	}
	return nil
}

// MaskNumber hides all but the last 2 digits of a phone number, for logs and
// audit entries (+14155550123 becomes +*********23). Anything shorter than 3
// characters is fully masked.
func MaskNumber(s string) string {
	if len(s) < 3 {
		return strings.Repeat("*", len(s))
	}

	prefix := ""
	rest := s
	if strings.HasPrefix(s, "+") {
		prefix = "+"
		rest = s[1:]
	}

	if len(rest) <= 2 {
		return prefix + strings.Repeat("*", len(rest))
	}

	return prefix + strings.Repeat("*", len(rest)-2) + rest[len(rest)-2:]
}

func validateMessage(m *Message) error {
	if m == nil {
		return errors.New("sms: nil message")
	}
	if err := ValidateE164(m.To); err != nil {
		return err
	}
	if strings.TrimSpace(m.Body) == "" {
		return errors.New("sms: the message body is empty")
	}
	return nil
}

// Error is a failure reported by a provider. Its text never contains the
// configured credentials or the full recipient number.
type Error struct {
	// Provider is "twilio" or "webhook".
	Provider string

	// HTTPStatus is the HTTP status of the provider response (0 if none).
	HTTPStatus int

	// Code is the provider error code (for Twilio, the numeric "code"
	// such as 21211), empty if none.
	Code string

	// Message is the sanitized provider message.
	Message string
}

// Error implements the error interface.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("sms: ")
	b.WriteString(e.Provider)
	b.WriteString(" request failed")
	if e.HTTPStatus != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.HTTPStatus)
	}
	if e.Code != "" {
		fmt.Fprintf(&b, " code %s", e.Code)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

// scrub removes every secret and the full recipient number from s.
func scrub(s, to string, secrets ...string) string {
	for _, v := range secrets {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	if to != "" {
		s = strings.ReplaceAll(s, to, MaskNumber(to))
		s = strings.ReplaceAll(s, strings.TrimPrefix(to, "+"), MaskNumber(to))
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// transportError wraps a transport failure without leaking the request URL
// (which for the webhook sender may carry a token).
func transportError(provider string, err error, to string, secrets ...string) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Provider: provider, Message: "request timed out"}
	case errors.Is(err, context.Canceled):
		return &Error{Provider: provider, Message: "request canceled"}
	}
	return &Error{Provider: provider, Message: "could not reach the provider: " + scrub(unwrapURLError(err), to, secrets...)}
}
