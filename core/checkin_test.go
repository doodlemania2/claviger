package core_test

import (
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #82): the pure parts. The routes and the collections
// run in apis/checkin_test.go.

func TestCheckinRulesNameTheStaffTagsAndNoOpenWrites(t *testing.T) {
	rules := core.CheckinRules()

	for _, name := range []string{core.CheckinEventsCollection, core.CheckinSessionCollection, core.PickupAuthCollection, core.CheckinLogCollection, core.CheckinFlagsCollection} {
		r, ok := rules[name]
		if !ok || r.List == nil || r.View == nil {
			t.Fatalf("%s needs list and view rules", name)
		}
	}

	// written by the routes only
	for _, name := range []string{core.CheckinSessionCollection, core.CheckinLogCollection, core.CheckinFlagsCollection} {
		r := rules[name]
		if r.Create != nil || r.Update != nil || r.Delete != nil {
			t.Fatalf("%s must have null write rules", name)
		}
	}

	if !strings.Contains(*rules[core.CheckinLogCollection].List, core.CheckinStaffTag) {
		t.Fatal("the log is read by staff")
	}

	if strings.Contains(*rules[core.CheckinFlagsCollection].List, `name ?= "`+core.CheckinStaffTag+`"`) ||
		!strings.Contains(*rules[core.CheckinFlagsCollection].List, core.CheckinSupervisorTag) {
		t.Fatal("the flags are for supervisors only")
	}
}

func TestNormalizeCheckinCode(t *testing.T) {
	for in, want := range map[string]string{"ab-cd2": "ABCD2", " ab cd2 ": "ABCD2", "ABCD2": "ABCD2", "ab_cd2": "ABCD2"} {
		if got := core.NormalizeCheckinCode(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestCheckinAlphabetHasNoLookAlikes(t *testing.T) {
	if strings.ContainsAny(core.CheckinCodeAlphabet, "01OIL") {
		t.Fatal("ambiguous characters in the alphabet")
	}

	if len(core.CheckinCodeAlphabet) < 30 {
		t.Fatal("the alphabet is too small for a 5 character code")
	}
}

func TestKioskTokenFormat(t *testing.T) {
	if !core.IsKioskToken(core.KioskTokenPrefix+"abc") || core.IsKioskToken("clv_abc") || core.IsKioskToken("") {
		t.Fatal("IsKioskToken must only accept the kiosk prefix")
	}
}
