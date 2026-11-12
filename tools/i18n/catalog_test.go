package i18n_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/tools/i18n"
)

var shipped = []string{"en", "es", "pt-BR", "fr", "tl", "vi", "ko", "zh-Hans", "ar"}

var tagRe = regexp.MustCompile(`<[^>]+>`)

func TestShippedCatalogs(t *testing.T) {
	b := i18n.Default()

	have := map[string]i18n.Locale{}
	for _, l := range b.Locales() {
		have[l.Tag] = l
	}

	for _, tag := range shipped {
		l, ok := have[tag]
		if !ok {
			t.Errorf("the catalog %s is missing", tag)
			continue
		}

		if l.Name == "" || l.Name == tag {
			t.Errorf("%s: _name is missing", tag)
		}

		// English is the source; every translation starts as a draft until a native speaker reviews it
		if tag != "en" && l.Status != i18n.StatusDraft && l.Status != i18n.StatusReviewed {
			t.Errorf("%s: _status %q must be draft or reviewed", tag, l.Status)
		}
	}

	if have["ar"].Dir != "rtl" || have["en"].Dir != "ltr" {
		t.Error("directions are wrong")
	}
}

// TestEveryCatalogHasEveryKey: a translation may be a draft, it may not be
// incomplete. A missing key would silently show English (or the key) on a page.
func TestEveryCatalogHasEveryKey(t *testing.T) {
	b := i18n.Default()
	en := b.Messages("en")

	if len(en) < 100 {
		t.Fatalf("suspiciously few English messages: %d", len(en))
	}

	for _, tag := range shipped[1:] {
		got := b.Messages(tag)

		var missing, extra []string

		for k := range en {
			if _, ok := got[k]; !ok {
				missing = append(missing, k)
			}
		}

		for k := range got {
			if _, ok := en[k]; !ok {
				extra = append(extra, k)
			}
		}

		sort.Strings(missing)
		sort.Strings(extra)

		if len(missing) > 0 {
			t.Errorf("%s misses %d keys: %v", tag, len(missing), missing)
		}

		if len(extra) > 0 {
			t.Errorf("%s has keys English does not: %v", tag, extra)
		}
	}
}

// TestPlaceholdersAndMarkupMatchEnglish: a translation must keep the
// {placeholders} (a lost one drops data from a page or email) and the HTML
// tags of the English message (a broken tag breaks the page).
func TestPlaceholdersAndMarkupMatchEnglish(t *testing.T) {
	b := i18n.Default()
	en := b.Messages("en")

	for _, tag := range shipped[1:] {
		for k, msg := range b.Messages(tag) {
			want, ok := en[k]
			if !ok {
				continue
			}

			if strings.TrimSpace(msg) == "" {
				t.Errorf("%s %s is empty", tag, k)
			}

			if a, c := i18n.Placeholders(want), i18n.Placeholders(msg); !reflect.DeepEqual(a, c) {
				t.Errorf("%s %s: placeholders %v, English has %v", tag, k, c, a)
			}

			if a, c := tagRe.FindAllString(want, -1), tagRe.FindAllString(msg, -1); !reflect.DeepEqual(a, c) {
				t.Errorf("%s %s: markup %v, English has %v", tag, k, c, a)
			}
		}
	}
}

// TestOnlyHTMLKeysCarryMarkup: text keys are escaped by the templates, so
// markup is only allowed where the key says so (…Html) and in the emails,
// whose messages are HTML fragments.
func TestOnlyHTMLKeysCarryMarkup(t *testing.T) {
	for _, tag := range shipped {
		for k, msg := range i18n.Default().Messages(tag) {
			if !strings.ContainsAny(msg, "<>") {
				continue
			}

			if !strings.HasSuffix(k, "Html") && !strings.HasPrefix(k, "email.") {
				t.Errorf("%s %s carries markup but is not an Html key", tag, k)
			}
		}
	}
}

func TestEnglishEmailsKeepTheirCharacters(t *testing.T) {
	// emails escape nothing: an English message with a raw ampersand would
	// not be valid HTML; apostrophes are fine
	for k, msg := range i18n.Default().Messages("en") {
		if strings.HasPrefix(k, "email.") && regexp.MustCompile(`&(?:[^a-z#]|$)`).MatchString(msg) {
			t.Errorf("%s has a bare ampersand", k)
		}
	}
}

// TestKeysInUseAreInTheCatalog scans the sources for catalog keys: a typo in
// a key would show the key on a page. Keys built at run time are covered by
// their prefix.
func TestKeysInUseAreInTheCatalog(t *testing.T) {
	root := filepath.Join("..", "..")
	keyRe := regexp.MustCompile(`"((?:js|oidc|orgs|passkeys|email|a11y|sms\.recovery)\.[A-Za-z0-9_.]*)"`)
	en := i18n.Default().Messages("en")

	used := map[string]bool{}
	prefixes := map[string]bool{}

	for _, dir := range []string{"apis", "apis/orgs_pages", "oidc", "mails", "core"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}

		for _, ent := range entries {
			name := ent.Name()
			if ent.IsDir() || strings.HasSuffix(name, "_test.go") || (!strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".js")) {
				continue
			}

			raw, err := os.ReadFile(filepath.Join(root, dir, name))
			if err != nil {
				t.Fatal(err)
			}

			for _, m := range keyRe.FindAllStringSubmatch(string(raw), -1) {
				if strings.HasSuffix(m[1], ".") {
					prefixes[m[1]] = true
				} else {
					used[m[1]] = true
				}
			}

			// the page templates and scripts also use @{key} markers
			for _, m := range regexp.MustCompile(`@\{([A-Za-z0-9_.]+)\}`).FindAllStringSubmatch(string(raw), -1) {
				used[m[1]] = true
			}
		}
	}

	for k := range used {
		if _, ok := en[k]; !ok && k != "lang" && k != "datablock" {
			t.Errorf("the code uses the key %q that English does not have", k)
		}
	}

	// the other way round: no dead messages for translators to work on
	dynamic := []string{"email.", "oidc.scope.", "js.role.", "js.status.", "js.admin.youAre."}
	prefixes["email."] = true

	for k := range en {
		if used[k] {
			continue
		}

		ok := false

		for _, p := range append(dynamic, keys(prefixes)...) {
			if strings.HasPrefix(k, p) {
				ok = true
			}
		}

		if !ok {
			t.Errorf("the key %q is in the catalog but nothing uses it", k)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}
