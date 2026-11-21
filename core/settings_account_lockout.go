package core

import (
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
)

const (
	// AccountLockoutDefaultWindowSeconds is the lock duration used when none is set.
	AccountLockoutDefaultWindowSeconds = 900
	// AccountLockoutMaxFailuresLimit caps MaxFailures.
	AccountLockoutMaxFailuresLimit = 100
	// AccountLockoutMaxWindowSeconds caps WindowSeconds (one day).
	AccountLockoutMaxWindowSeconds = 86400
)

// AccountLockoutConfig defines the optional per-account lockout of password
// sign-in (#139). With MaxFailures 0 (the default) nothing changes.
//
// The count is keyed by the collection and the submitted identity, whether or
// not such an account exists, so the response never reveals which identities
// are registered. It is independent of the per-IP rate limits: it is what
// protects a short secret (a numeric PIN) from being guessed from many addresses.
type AccountLockoutConfig struct {
	// MaxFailures is the number of failed password sign-ins an identity may
	// make inside the window before it is locked. 0 turns the feature off.
	MaxFailures int `form:"maxFailures" json:"maxFailures"`

	// WindowSeconds is the length of the fixed window in which failures are
	// counted, and so the longest an identity stays locked. 0 means 900 (15 minutes).
	WindowSeconds int `form:"windowSeconds" json:"windowSeconds"`
}

// Enabled reports whether the lockout is on.
func (c AccountLockoutConfig) Enabled() bool { return c.MaxFailures > 0 }

// Window returns the effective window in seconds.
func (c AccountLockoutConfig) Window() int {
	if c.WindowSeconds <= 0 {
		return AccountLockoutDefaultWindowSeconds
	}

	return c.WindowSeconds
}

// Validate makes AccountLockoutConfig validatable by implementing [validation.Validatable] interface.
func (c AccountLockoutConfig) Validate() error {
	return validation.ValidateStruct(&c,
		validation.Field(&c.MaxFailures, validation.Min(0), validation.Max(AccountLockoutMaxFailuresLimit)),
		validation.Field(&c.WindowSeconds, validation.Min(0), validation.Max(AccountLockoutMaxWindowSeconds)),
	)
}
