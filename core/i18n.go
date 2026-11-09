package core

import (
	"path/filepath"
	"regexp"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/tools/i18n"
)

// Fork-local (Claviger #97): language helpers shared by the hosted pages and
// the emails. See docs/localization.md.

var languageFieldRegex = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,99}$`)

func validateLanguageTag(value any) error {
	s, _ := value.(string)
	if s != "" && i18n.Canonical(s) == "" {
		return validation.NewError("validation_invalid_language", "Must be a language tag like en or pt-BR.")
	}

	return nil
}

const i18nStoreKey = "__clvI18nBundle__"

// I18n returns the message catalog of the app: the languages embedded in the
// binary plus the overrides in `<pb_data>/i18n/<tag>.json` (read once).
func I18n(app App) *i18n.Bundle {
	if b, ok := app.Store().Get(i18nStoreKey).(*i18n.Bundle); ok {
		return b
	}

	b := i18n.Default().Clone()

	if dir := app.DataDir(); dir != "" {
		// a missing directory is the normal case
		_ = b.LoadDirOS(filepath.Join(dir, "i18n"))
	}

	got, _ := app.Store().GetOrSet(i18nStoreKey, func() any { return b }).(*i18n.Bundle)

	return got
}

// DefaultLang is meta.defaultLanguage, "en" when unset.
func (c MetaConfig) DefaultLang() string {
	if c.DefaultLanguage == "" {
		return i18n.FallbackLanguage
	}

	return c.DefaultLanguage
}

// LanguageFieldName is meta.languageField, "lang" when unset.
func (c MetaConfig) LanguageFieldName() string {
	if c.LanguageField == "" {
		return "lang"
	}

	return c.LanguageField
}

// RecordLanguage returns the preferred language stored on an auth record (the
// language field of its collection), or "" when the collection has no such
// field or the record has no preference.
func RecordLanguage(app App, record *Record) string {
	if record == nil {
		return ""
	}

	name := app.Settings().Meta.LanguageFieldName()
	if record.Collection().Fields.GetByName(name) == nil {
		return ""
	}

	return record.GetString(name)
}
