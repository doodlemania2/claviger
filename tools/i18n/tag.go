package i18n

import (
	"sort"
	"strconv"
	"strings"
)

// Canonical normalizes a language tag: "pt_br" -> "pt-BR", "ZH-hans" -> "zh-Hans".
// It returns "" for something that is not a tag.
func Canonical(tag string) string {
	tag = strings.ReplaceAll(strings.TrimSpace(tag), "_", "-")
	if tag == "" || len(tag) > 35 {
		return ""
	}

	parts := strings.Split(tag, "-")
	for i, p := range parts {
		if p == "" {
			return ""
		}

		for _, r := range p {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
				return ""
			}
		}

		switch {
		case i == 0:
			parts[i] = strings.ToLower(p)
		case len(p) == 4 && isAlpha(p): // script
			parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
		case len(p) == 2 && isAlpha(p), len(p) == 3 && isDigit(p): // region
			parts[i] = strings.ToUpper(p)
		default:
			parts[i] = strings.ToLower(p)
		}
	}

	return strings.Join(parts, "-")
}

func isAlpha(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}

	return true
}

func isDigit(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

// Primary returns the language subtag: "pt-BR" -> "pt".
func Primary(tag string) string {
	tag = Canonical(tag)
	if i := strings.IndexByte(tag, '-'); i >= 0 {
		return tag[:i]
	}

	return tag
}

// aliases maps tags that name an available language under another code.
var aliases = map[string]string{
	"fil": "tl", // Filipino is served by the Tagalog catalog
	"iw":  "he",
}

var rtlLanguages = map[string]bool{
	"ar": true, "he": true, "fa": true, "ur": true, "ps": true, "sd": true, "yi": true, "dv": true, "ug": true,
}

// Dir is the text direction of a language: "rtl" for Arabic, Hebrew, Persian,
// Urdu and a few others, "ltr" otherwise.
func Dir(tag string) string {
	if rtlLanguages[Primary(tag)] {
		return "rtl"
	}

	return "ltr"
}

// Resolve maps a requested tag to the best available language, or reports
// false. The match is exact first, then by dropping subtags from the right,
// then to any available language with the same primary language (pt-PT is
// served by pt-BR, zh-TW by zh-Hans).
func (b *Bundle) Resolve(tag string) (string, bool) {
	tag = Canonical(tag)
	if tag == "" {
		return "", false
	}

	if a, ok := aliases[tag]; ok {
		tag = a
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	if _, ok := b.msgs[tag]; ok {
		return tag, true
	}

	parts := strings.Split(tag, "-")
	for i := len(parts) - 1; i >= 1; i-- {
		if cand := strings.Join(parts[:i], "-"); b.msgs[cand] != nil {
			return cand, true
		}
	}

	primary := parts[0]
	if a, ok := aliases[primary]; ok {
		primary = a
	}

	var same []string

	for t := range b.msgs {
		if Primary(t) == primary {
			same = append(same, t)
		}
	}

	if len(same) == 0 {
		return "", false
	}

	sort.Strings(same)

	return same[0], true
}

// AcceptLanguage parses an Accept-Language header into tags ordered by
// descending quality (ties keep their order). q=0 entries and "*" are dropped.
func AcceptLanguage(header string) []string {
	type entry struct {
		tag string
		q   float64
	}

	var list []entry

	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")

		tag := Canonical(fields[0])
		if tag == "" {
			continue
		}

		q := 1.0

		for _, p := range fields[1:] {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(p, "q=") {
				if v, err := strconv.ParseFloat(p[2:], 64); err == nil {
					q = v
				} else {
					q = 0
				}
			}
		}

		if q > 0 {
			list = append(list, entry{tag, q})
		}
	}

	sort.SliceStable(list, func(i, j int) bool { return list[i].q > list[j].q })

	out := make([]string, len(list))
	for i, e := range list {
		out[i] = e.tag
	}

	return out
}

// Preference is what the language of a response is negotiated from, in order
// of priority. Empty values are skipped, and so are values that no available
// language matches.
type Preference struct {
	Explicit       string // ?lang=
	Cookie         string // the lang cookie
	Record         string // the preferred language of the signed-in or addressed record
	AcceptLanguage string // the Accept-Language header
	Default        string // the app default (meta.defaultLanguage)
}

// Negotiate returns the available language for the preference.
func (b *Bundle) Negotiate(p Preference) string {
	for _, v := range []string{p.Explicit, p.Cookie, p.Record} {
		if t, ok := b.Resolve(v); ok {
			return t
		}
	}

	for _, v := range AcceptLanguage(p.AcceptLanguage) {
		if t, ok := b.Resolve(v); ok {
			return t
		}
	}

	if t, ok := b.Resolve(p.Default); ok {
		return t
	}

	return FallbackLanguage
}
