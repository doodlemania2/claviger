package core

import (
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
)

// Fork-local (Claviger #88, 88-4): the erasure settings.

const (
	// ErasureDefaultGraceDays is the grace period between the confirmation of
	// an erasure and the job (spec 10.6, owner decision 2026-11-14).
	ErasureDefaultGraceDays = 14

	// ErasureMaxGraceDays is the longest grace period that can be set.
	ErasureMaxGraceDays = 30
)

// ErasureConfig defines how erasure requests are handled.
type ErasureConfig struct {
	// GraceDays is how long an account stays locked (and the erasure can be
	// cancelled) after the request was confirmed: 0 to 30. Left unset (null) it
	// is 14. 0 is a real value: the job starts at once.
	GraceDays *int `form:"graceDays" json:"graceDays"`
}

// Validate makes ErasureConfig validatable by implementing [validation.Validatable] interface.
func (c ErasureConfig) Validate() error {
	return validation.ValidateStruct(&c,
		validation.Field(&c.GraceDays, validation.Min(0), validation.Max(ErasureMaxGraceDays)),
	)
}

// ErasureGraceDays returns the effective grace period in days.
func ErasureGraceDays(app App) int {
	if d := app.Settings().Erasure.GraceDays; d != nil {
		return min(max(*d, 0), ErasureMaxGraceDays)
	}

	return ErasureDefaultGraceDays
}
