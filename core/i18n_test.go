package core_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #97).

func TestMetaLanguageSettingsValidate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		lang, field string
		ok          bool
	}{
		{"", "", true},
		{"en", "lang", true},
		{"pt-BR", "preferred_language", true},
		{"zh-Hans", "locale", true},
		{"not a tag", "lang", false},
		{"en", "bad field!", false},
		{"en", "1lang", false},
	} {
		meta := core.MetaConfig{
			AppName: "a", AppURL: "https://example.com", SenderName: "s", SenderAddress: "s@example.com",
			DefaultLanguage: tc.lang, LanguageField: tc.field,
		}

		if err := meta.Validate(); (err == nil) != tc.ok {
			t.Errorf("%q/%q: ok=%v, err=%v", tc.lang, tc.field, tc.ok, err)
		}
	}

	if (core.MetaConfig{}).DefaultLang() != "en" || (core.MetaConfig{}).LanguageFieldName() != "lang" {
		t.Error("the defaults must be en and lang")
	}
}

func TestNewInstallsDefaultToEnglishAndLangField(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	if got := app.Settings().Meta.DefaultLang(); got != "en" {
		t.Fatalf("got %q", got)
	}
}

func TestRecordLanguage(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	users, _ := app.FindCollectionByNameOrId("users")
	user, _ := app.FindFirstRecordByData("users", "email", "test@example.com")

	// no field: no preference
	if got := core.RecordLanguage(app, user); got != "" {
		t.Fatalf("got %q", got)
	}

	users.Fields.Add(&core.TextField{Name: "lang"})

	if err := app.Save(users); err != nil {
		t.Fatal(err)
	}

	user, _ = app.FindFirstRecordByData("users", "email", "test@example.com")
	user.Set("lang", "es")

	if got := core.RecordLanguage(app, user); got != "es" {
		t.Fatalf("got %q", got)
	}

	if got := core.RecordLanguage(app, nil); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestI18nOverridesInTheDataDirectory(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	dir := filepath.Join(app.DataDir(), "i18n")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// a partial override of Spanish, and a brand new language
	files := map[string]string{
		"es.json":    `{"js.signIn":"Entrar ya"}`,
		"pt-PT.json": `{"_name":"Português (Portugal)","_status":"draft","js.signIn":"Iniciar sessão"}`,
	}
	for name, doc := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	b := core.I18n(app)

	if b != core.I18n(app) {
		t.Fatal("the bundle must be built once per app")
	}

	if got := b.T("es", "js.signIn"); got != "Entrar ya" {
		t.Fatalf("override not applied: %q", got)
	}

	if got := b.T("es", "js.verify"); got == "js.verify" || got == "Verify" {
		t.Fatalf("keys the override does not set keep the shipped translation, got %q", got)
	}

	if got, ok := b.Resolve("pt-PT"); !ok || got != "pt-PT" {
		t.Fatalf("the new language is not available: %q %v", got, ok)
	}

	// pt-PT falls back to pt-BR? No: its chain is pt-PT, pt, en; keys it does not set come from English
	if got := b.T("pt-PT", "js.signIn"); got != "Iniciar sessão" {
		t.Fatalf("got %q", got)
	}

	// the shipped catalogs are untouched for other apps
	other, _ := tests.NewTestApp()
	defer other.Cleanup()

	if got := core.I18n(other).T("es", "js.signIn"); got == "Entrar ya" {
		t.Fatal("an override leaked into another app")
	}
}
