package core

import (
	"sort"
	"strings"

	"github.com/doodlemania2/claviger/tools/sms"
)

// Fork-local (Claviger #83): a deliberately small, conservative national to
// E.164 normalizer. It is not a phone-number library: it knows a country code
// and whether a leading national trunk "0" is dropped, and nothing about
// numbering plans. Anything that does not fit counts as no phone.

type phoneRegion struct {
	// code is the country calling code, without "+".
	code string
	// trunk is the national trunk prefix to drop ("0"), or "".
	trunk string
	// nanp marks the +1 group: 10 digits, or 11 with a leading 1.
	nanp bool
}

var nanpRegion = phoneRegion{code: "1", nanp: true}

var phoneRegions = func() map[string]phoneRegion {
	m := map[string]phoneRegion{
		"GB": {code: "44", trunk: "0"},
		"IE": {code: "353", trunk: "0"},
		"DE": {code: "49", trunk: "0"},
		"FR": {code: "33", trunk: "0"},
		"ES": {code: "34"},
		"IT": {code: "39"}, // a leading 0 is part of the number
		"PT": {code: "351"},
		"NL": {code: "31", trunk: "0"},
		"BE": {code: "32", trunk: "0"},
		"MX": {code: "52"},
		"BR": {code: "55", trunk: "0"},
		"PH": {code: "63", trunk: "0"},
		"KR": {code: "82", trunk: "0"},
		"VN": {code: "84", trunk: "0"},
		"CN": {code: "86", trunk: "0"},
		"AU": {code: "61", trunk: "0"},
		"NZ": {code: "64", trunk: "0"},
	}

	// The +1 group (US, Canada and the Caribbean/Pacific NANP regions).
	for _, r := range []string{
		"US", "CA", "AG", "AI", "AS", "BB", "BM", "BS", "DM", "DO", "GD", "GU", "JM",
		"KN", "KY", "LC", "MP", "MS", "PR", "SX", "TC", "TT", "VC", "VG", "VI",
	} {
		m[r] = nanpRegion
	}

	return m
}()

// PhoneRegions returns the supported ISO 3166 alpha-2 codes, sorted.
func PhoneRegions() []string {
	out := make([]string, 0, len(phoneRegions))
	for r := range phoneRegions {
		out = append(out, r)
	}
	sort.Strings(out)

	return out
}

// IsPhoneRegion reports whether region (case-insensitive) is supported.
func IsPhoneRegion(region string) bool {
	_, ok := phoneRegions[strings.ToUpper(strings.TrimSpace(region))]

	return ok
}

func phoneDigitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

// NormalizePhoneRegion is [NormalizePhone] with a default region for numbers
// without a leading "+". An empty or unknown region keeps the strict
// behavior. It returns "" for anything it cannot place with confidence.
func NormalizePhoneRegion(s, region string) string {
	if p := NormalizePhone(s); p != "" {
		return p
	}

	r, ok := phoneRegions[strings.ToUpper(strings.TrimSpace(region))]
	if !ok {
		return ""
	}

	s = strings.Map(func(c rune) rune {
		switch c {
		case ' ', '-', '.', '(', ')', '\u00a0':
			return -1
		}

		return c
	}, strings.TrimSpace(s))

	// Digits only; "+", "00" (an international prefix we do not rewrite),
	// extensions and letters are all refused.
	if !phoneDigitsOnly(s) || strings.HasPrefix(s, "00") {
		return ""
	}

	var national string

	switch {
	case r.nanp:
		switch {
		case len(s) == 10:
			national = s
		case len(s) == 11 && s[0] == '1':
			national = s[1:]
		default:
			return ""
		}
		// NANP area and exchange codes never start with 0 or 1.
		if national[0] < '2' || national[3] < '2' {
			return ""
		}
	default:
		national = s
		if r.trunk != "" {
			national = strings.TrimPrefix(national, r.trunk)
		}
	}

	if national == "" {
		return ""
	}

	out := "+" + r.code + national
	if sms.ValidateE164(out) != nil {
		return ""
	}

	return out
}
