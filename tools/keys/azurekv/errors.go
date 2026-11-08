package azurekv

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrKeyMismatch is returned by Unwrap when the key id belongs to another
	// vault or another key than the one the client is configured for.
	ErrKeyMismatch = errors.New("the key id belongs to a different Key Vault key")

	// ErrNoCredential is returned when no authentication method is configured.
	ErrNoCredential = errors.New("no Azure credential is configured")
)

// Error is a failed call to Azure. It says whether the failure is definite
// (the service answered and refused: bad credentials, missing permission, key
// not found, wrong key) or indeterminate (nothing conclusive was learned:
// timeout, network error, throttling, 5xx).
type Error struct {
	// Op names the step ("get key", "wrapkey", "token", ...).
	Op string
	// Status is the HTTP status, or 0 when there was no response.
	Status int
	// Code and Message are the service's error code and (shortened) message.
	Code    string
	Message string
	// Definite is true when the service gave a conclusive answer.
	Definite bool
	// Err is the underlying transport error, if any.
	Err error

	retryAfter time.Duration
}

// public strips the internal retry bookkeeping.
func (e *Error) public() *Error {
	if e == nil {
		return &Error{Op: "request"}
	}
	c := *e
	c.retryAfter = 0
	return &c
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("azure key vault: ")
	b.WriteString(e.Op)
	b.WriteString(" failed")

	if e.Status != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.Status)
	}
	if e.Code != "" {
		b.WriteString(": " + e.Code)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	if e.Err != nil {
		b.WriteString(": " + e.Err.Error())
	}
	if !e.Definite {
		b.WriteString(" (inconclusive: the service could not be reached or is unavailable)")
	}

	return b.String()
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// Indeterminate reports whether the failure says nothing about whether the
// credentials and the key are right.
func (e *Error) Indeterminate() bool { return !e.Definite }

// IsIndeterminate reports whether err (or an error it wraps) is an
// indeterminate failure.
func IsIndeterminate(err error) bool {
	var i interface{ Indeterminate() bool }
	return errors.As(err, &i) && i.Indeterminate()
}

// shorten keeps service messages short and on one line.
func shorten(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 240 {
		s = s[:240] + "..."
	}
	return s
}
