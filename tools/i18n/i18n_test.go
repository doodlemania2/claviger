package i18n_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/doodlemania2/claviger/tools/i18n"
)

func testBundle(t *testing.T) *i18n.Bundle {
	t.Helper()

	b := i18n.New()
	for tag, doc := range map[string]string{
		"en":      `{"_name":"English","hello":"Hello","only.en":"Only English","items.one":"{count} item","items.other":"{count} items","greet":"Hi {name}, {unknown}"}`,
		"pt":      `{"_name":"Português","hello":"Olá","only.pt":"Só pt"}`,
		"pt-BR":   `{"_name":"Português (Brasil)","hello":"Oi"}`,
		"es":      `{"_name":"Español","hello":"Hola"}`,
		"ar":      `{"_name":"العربية","items.zero":"لا عناصر","items.one":"عنصر","items.two":"عنصران","items.few":"{count} عناصر","items.many":"{count} عنصرًا","items.other":"{count} عنصر"}`,
		"zh-Hans": `{"_name":"简体中文","hello":"你好"}`,
		"tl":      `{"_name":"Tagalog","hello":"Kumusta"}`,
	} {
		if err := b.LoadJSON(tag, []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}

	return b
}

func TestFallbackChain(t *testing.T) {
	b := testBundle(t)

	if got := b.Chain("pt-BR"); !reflect.DeepEqual(got, []string{"pt-BR", "pt", "en"}) {
		t.Fatalf("chain %v", got)
	}

	for _, tc := range []struct{ tag, key, want string }{
		{"pt-BR", "hello", "Oi"},             // the tag itself
		{"pt-BR", "only.pt", "Só pt"},        // then the primary language
		{"pt-BR", "only.en", "Only English"}, // then English
		{"es", "only.pt", "only.pt"},         // nowhere: the key, never empty
		{"pt-PT", "hello", "Olá"},            // an unknown region falls back to pt
		{"xx", "hello", "Hello"},             // an unknown language falls back to English
	} {
		if got := b.T(tc.tag, tc.key); got != tc.want {
			t.Errorf("T(%q,%q) = %q, want %q", tc.tag, tc.key, got, tc.want)
		}
	}
}

func TestPlaceholders(t *testing.T) {
	b := testBundle(t)

	if got := b.T("en", "greet", i18n.Args{"name": "Ana"}); got != "Hi Ana, {unknown}" {
		t.Fatalf("unknown placeholders must stay: %q", got)
	}

	if got := i18n.Format("{a}{b}{a}", i18n.Args{"a": 1}, i18n.Args{"b": "x"}); got != "1x1" {
		t.Fatalf("got %q", got)
	}

	if got := i18n.Placeholders("x {b} {a} {b} { not } {APP_NAME}"); !reflect.DeepEqual(got, []string{"APP_NAME", "a", "b"}) {
		t.Fatalf("got %v", got)
	}
}

func TestPlurals(t *testing.T) {
	b := testBundle(t)

	for _, tc := range []struct {
		tag  string
		n    int
		want string
	}{
		{"en", 1, "1 item"}, {"en", 0, "0 items"}, {"en", 2, "2 items"},
		{"ar", 0, "لا عناصر"}, {"ar", 1, "عنصر"}, {"ar", 2, "عنصران"}, {"ar", 5, "5 عناصر"}, {"ar", 11, "11 عنصرًا"}, {"ar", 100, "100 عنصر"},
		{"es", 1, "1 item"}, // no forms in es: English forms through the chain
	} {
		if got := b.Plural(tc.tag, "items", tc.n); got != tc.want {
			t.Errorf("Plural(%s,%d) = %q, want %q", tc.tag, tc.n, got, tc.want)
		}
	}

	cats := map[string]map[int]string{
		"en": {0: "other", 1: "one", 2: "other"},
		"fr": {0: "one", 1: "one", 2: "other", 1000000: "many"},
		"pt": {0: "one", 1: "one", 2: "other"},
		"es": {1: "one", 0: "other", 3: "other"},
		"tl": {1: "one", 2: "one", 3: "one", 4: "other", 5: "one", 6: "other", 9: "other", 10: "one"},
		"ar": {0: "zero", 1: "one", 2: "two", 3: "few", 10: "few", 11: "many", 99: "many", 100: "other", 102: "other"},
		"ko": {0: "other", 1: "other"}, "vi": {1: "other"}, "zh-Hans": {1: "other"},
	}
	for tag, byN := range cats {
		for n, want := range byN {
			if got := i18n.PluralCategory(tag, n); got != want {
				t.Errorf("PluralCategory(%s,%d) = %s, want %s", tag, n, got, want)
			}
		}
	}
}

func TestResolve(t *testing.T) {
	b := testBundle(t)

	for in, want := range map[string]string{
		"pt-BR": "pt-BR", "pt_br": "pt-BR", "PT-br": "pt-BR",
		"pt": "pt", "pt-PT": "pt", "es-MX": "es", "ES": "es",
		"zh": "zh-Hans", "zh-CN": "zh-Hans", "zh-TW": "zh-Hans", "zh-Hans-CN": "zh-Hans",
		"fil": "tl", "fil-PH": "tl",
	} {
		if got, ok := b.Resolve(in); !ok || got != want {
			t.Errorf("Resolve(%q) = %q,%v want %q", in, got, ok, want)
		}
	}

	for _, in := range []string{"", "xx", "de", "*", "not a tag", "en/../x"} {
		if got, ok := b.Resolve(in); ok && in != "" {
			t.Errorf("Resolve(%q) = %q, want no match", in, got)
		}
	}
}

func TestAcceptLanguage(t *testing.T) {
	got := i18n.AcceptLanguage("de;q=0.7, pt-br, es;q=0.9, *;q=0.1, fr;q=0, en-US;q=0.9")
	if want := []string{"pt-BR", "es", "en-US", "de"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	if got := i18n.AcceptLanguage(""); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestNegotiationOrder(t *testing.T) {
	b := testBundle(t)

	// explicit > cookie > record > Accept-Language > default
	full := i18n.Preference{Explicit: "es", Cookie: "pt-BR", Record: "ar", AcceptLanguage: "zh-CN", Default: "tl"}

	steps := []struct {
		drop func(p *i18n.Preference)
		want string
	}{
		{func(p *i18n.Preference) {}, "es"},
		{func(p *i18n.Preference) { p.Explicit = "" }, "pt-BR"},
		{func(p *i18n.Preference) { p.Cookie = "" }, "ar"},
		{func(p *i18n.Preference) { p.Record = "" }, "zh-Hans"},
		{func(p *i18n.Preference) { p.AcceptLanguage = "" }, "tl"},
		{func(p *i18n.Preference) { p.Default = "" }, "en"},
	}

	p := full
	for i, s := range steps {
		s.drop(&p)

		if got := b.Negotiate(p); got != s.want {
			t.Errorf("step %d: got %s, want %s", i, got, s.want)
		}
	}

	// values that no catalog matches are skipped, not fatal
	if got := b.Negotiate(i18n.Preference{Explicit: "xx", Cookie: "!!", Record: "de", AcceptLanguage: "ja, fr;q=0.5, pt-PT;q=0.4", Default: "es"}); got != "pt" {
		t.Errorf("got %s, want pt (the first Accept-Language entry some catalog serves)", got)
	}

	if got := b.Negotiate(i18n.Preference{Explicit: "xx", Default: "yy"}); got != "en" {
		t.Errorf("got %s, want the fallback language", got)
	}
}

func TestFromRequestAndCookie(t *testing.T) {
	b := testBundle(t)

	r := httptest.NewRequest(http.MethodGet, "/x?lang=es", nil)
	r.Header.Set("Accept-Language", "ar")
	r.AddCookie(&http.Cookie{Name: i18n.CookieName, Value: "pt-BR"})

	if got := b.FromRequest(r, "zh-Hans", "tl"); got != "es" {
		t.Fatalf("?lang= must win, got %s", got)
	}

	w := httptest.NewRecorder()
	b.RememberChoice(w, r)

	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "lang" || cookies[0].Value != "es" || !cookies[0].HttpOnly {
		t.Fatalf("unexpected cookie %v", cookies)
	}

	// no explicit choice: the cookie, and nothing is written
	r2 := httptest.NewRequest(http.MethodGet, "/x", nil)
	r2.AddCookie(&http.Cookie{Name: i18n.CookieName, Value: "pt-BR"})

	if got := b.FromRequest(r2, "ar", "tl"); got != "pt-BR" {
		t.Fatalf("cookie must come before the record, got %s", got)
	}

	w2 := httptest.NewRecorder()
	b.RememberChoice(w2, r2)

	if len(w2.Result().Cookies()) != 0 {
		t.Fatal("a cookie must only be written for an explicit ?lang=")
	}

	// an unknown ?lang= is ignored and not remembered
	r3 := httptest.NewRequest(http.MethodGet, "/x?lang=xx", nil)
	w3 := httptest.NewRecorder()
	b.RememberChoice(w3, r3)

	if len(w3.Result().Cookies()) != 0 || b.FromRequest(r3, "", "") != "en" {
		t.Fatal("an unsupported ?lang= must be ignored")
	}
}

func TestDirAndCanonical(t *testing.T) {
	for tag, want := range map[string]string{"ar": "rtl", "ar-EG": "rtl", "he": "rtl", "fa": "rtl", "ur-PK": "rtl", "en": "ltr", "pt-BR": "ltr", "ko": "ltr"} {
		if got := i18n.Dir(tag); got != want {
			t.Errorf("Dir(%s) = %s", tag, got)
		}
	}

	for in, want := range map[string]string{"pt_br": "pt-BR", "ZH-hans": "zh-Hans", "en": "en", "es-419": "es-419", "": "", "a b": "", "-x": ""} {
		if got := i18n.Canonical(in); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOverrideAndClone(t *testing.T) {
	b := testBundle(t)
	c := b.Clone()

	c.Override("pt-BR", map[string]string{"hello": "E aí"})
	c.Override("fr-CA", map[string]string{"hello": "Allô"})

	if b.T("pt-BR", "hello") != "Oi" || c.T("pt-BR", "hello") != "E aí" {
		t.Fatal("overrides must apply to the clone only")
	}

	if got, ok := c.Resolve("fr"); !ok || got != "fr-CA" {
		t.Fatalf("a new language is matched by its primary language, got %q %v", got, ok)
	}

	sub := c.Subset("pt-BR", "only.")
	if sub["only.pt"] != "Só pt" || sub["only.en"] != "Only English" {
		t.Fatalf("subset must resolve along the chain: %v", sub)
	}
}
