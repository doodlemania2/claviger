package core

import "time"

// SetHouseholdClockForTest replaces the clock of the household model hook and
// returns a function that restores it.
func SetHouseholdClockForTest(now func() time.Time) (restore func()) {
	old := householdNow
	householdNow = now

	return func() { householdNow = old }
}
