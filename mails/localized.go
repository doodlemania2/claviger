package mails

import (
	"strings"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/i18n"
)

// Fork-local (Claviger #97): the built-in emails in the language of the
// recipient.
//
// A collection stores its email templates. A template that is still the
// English built-in default is replaced by the catalog version in the language
// of the recipient (the language field of the record, then meta.defaultLanguage).
// A template the admin edited is never touched: it is their override.

// block is one element of the body of a built-in email.
type block struct {
	kind string // "p", "i" (italic paragraph), "btn" (button link)
	key  string // catalog key below "email.<kind>."; for "btn" the label key
	href string // for "btn": the link, placeholders allowed
}

func para(key string) block   { return block{kind: "p", key: key} }
func italic(key string) block { return block{kind: "i", key: key} }
func button(key, href string) block {
	return block{kind: "btn", key: key, href: href}
}

// raw is a paragraph that is not translated (it only holds a placeholder).
func raw(html string) block { return block{kind: "raw", href: html} }

var layouts = map[string][]block{
	core.EmailKindVerification: {
		para("intro"), para("action"),
		button("button", core.EmailPlaceholderAppURL+"/_/#/auth/confirm-verification/"+core.EmailPlaceholderToken),
		italic("ignore"),
	},
	core.EmailKindPasswordReset: {
		para("action"),
		button("button", core.EmailPlaceholderAppURL+"/_/#/auth/confirm-password-reset/"+core.EmailPlaceholderToken),
		italic("ignore"),
	},
	core.EmailKindEmailChange: {
		para("action"),
		button("button", core.EmailPlaceholderAppURL+"/_/#/auth/confirm-email-change/"+core.EmailPlaceholderToken),
		italic("ignore"),
	},
	core.EmailKindOTP: {
		para("code"), italic("ignore"),
	},
	core.EmailKindAuthAlert: {
		para("notice"), raw("<p><em>" + core.EmailPlaceholderAlertInfo + "</em></p>"),
		{kind: "strong", key: "warning"}, para("fine"),
	},
	core.EmailKindPasskeyReset: {
		para("action"),
		button("button", core.EmailPlaceholderAppURL+"/_/#/auth/confirm-passkey-reset/"+core.EmailPlaceholderToken),
		italic("ignore"),
	},
}

// BuiltinTemplate composes the built-in template of an email kind in a language.
// The English result is byte-identical to the default stored in new collections.
func BuiltinTemplate(b *i18n.Bundle, lang, kind string) (core.EmailTemplate, bool) {
	blocks, ok := layouts[kind]
	if !ok {
		return core.EmailTemplate{}, false
	}

	return core.EmailTemplate{
		Subject: b.Raw(lang, "email."+kind+".subject"),
		Body:    composeBody(b, lang, "email."+kind+".", blocks),
	}, true
}

// composeBody lays the blocks out like the English defaults: a greeting, the
// blocks, and the signature, in a wrapper that carries lang and dir when the
// language is not the fallback one.
func composeBody(b *i18n.Bundle, lang, prefix string, blocks []block) string {
	var sb strings.Builder

	sb.WriteString("<p>" + b.Raw(lang, "email.hello") + "</p>")

	for _, bl := range blocks {
		sb.WriteString("\n")

		switch bl.kind {
		case "p":
			sb.WriteString("<p>" + b.Raw(lang, prefix+bl.key) + "</p>")
		case "i":
			sb.WriteString("<p><i>" + b.Raw(lang, prefix+bl.key) + "</i></p>")
		case "strong":
			sb.WriteString("<p><strong>" + b.Raw(lang, prefix+bl.key) + "</strong></p>")
		case "raw":
			sb.WriteString(bl.href)
		case "btn":
			sb.WriteString("<p>\n  <a class=\"btn\" href=\"" + bl.href + "\" target=\"_blank\" rel=\"noopener\">" +
				b.Raw(lang, prefix+bl.key) + "</a>\n</p>")
		}
	}

	sb.WriteString("\n<p>\n  " + b.Raw(lang, "email.thanks") + "<br/>\n  " + b.Raw(lang, "email.team") + "\n</p>")

	body := sb.String()

	if lang == i18n.FallbackLanguage {
		return body
	}

	open := `<div lang="` + lang + `"`
	if i18n.Dir(lang) == "rtl" {
		open += ` dir="rtl"`
	}

	return open + ">\n" + body + "\n</div>"
}

// recipientLanguage is the language of the recipient: the preference stored on
// the record, then meta.defaultLanguage.
func recipientLanguage(app core.App, record *core.Record) string {
	return core.I18n(app).Negotiate(i18n.Preference{
		Record:  core.RecordLanguage(app, record),
		Default: app.Settings().Meta.DefaultLang(),
	})
}

// localizeTemplate returns the template to send: the localized built-in one
// when the stored template is still the English default, otherwise the
// stored (customized) one.
func localizeTemplate(app core.App, record *core.Record, kind string, stored core.EmailTemplate) core.EmailTemplate {
	def, ok := core.DefaultEmailTemplate(kind)
	if !ok || stored != def {
		return stored
	}

	if local, ok := BuiltinTemplate(core.I18n(app), recipientLanguage(app, record), kind); ok {
		return local
	}

	return stored
}
