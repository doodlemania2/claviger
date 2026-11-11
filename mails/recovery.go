package mails

import (
	"html"
	"html/template"
	"net/mail"
	"strings"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/mails/templates"
	"github.com/doodlemania2/claviger/tools/i18n"
	"github.com/doodlemania2/claviger/tools/mailer"
)

// Fork-local (Claviger #83): the notice sent to a person when staff issue a
// recovery code for their account. It says when, and carries a link to report
// a code the person did not expect. It never contains the code.

const (
	EmailPlaceholderRecoveryTime      = "{TIME}"
	EmailPlaceholderRecoveryReportURL = "{REPORT_URL}"
)

// RecoveryReportURL is the hosted page that reports an unexpected code. The
// token rides in the fragment, so it is not sent to the server in a request
// line or written to access logs.
func RecoveryReportURL(app core.App, reportToken string) string {
	return strings.TrimRight(app.Settings().Meta.AppURL, "/") + "/recovery#report=" + reportToken
}

// BuiltinRecoveryNoticeTemplate is the notice in the language.
func BuiltinRecoveryNoticeTemplate(b *i18n.Bundle, lang string) core.EmailTemplate {
	return core.EmailTemplate{
		Subject: b.Raw(lang, "email.recoveryNotice.subject"),
		Body: composeBody(b, lang, "email.recoveryNotice.", []block{
			para("intro"),
			button("button", EmailPlaceholderRecoveryReportURL),
			italic("fine"),
		}),
	}
}

// SendRecoveryNotice emails the target that staff of org issued a recovery code
// at the given time. It does nothing, and returns nil, when the account has no
// email address (an SMS notice goes through the function the apis package
// names sendRecoveryNoticeSMS).
func SendRecoveryNotice(app core.App, org, target *core.Record, at time.Time, reportToken string) error {
	to := target.Email()
	if to == "" {
		return nil
	}

	bundle := core.I18n(app)
	lang := recipientLanguage(app, target)
	tpl := BuiltinRecoveryNoticeTemplate(bundle, lang)
	meta := app.Settings().Meta

	raw := map[string]any{
		core.EmailPlaceholderAppName:      meta.AppName,
		core.EmailPlaceholderAppURL:       meta.AppURL,
		EmailPlaceholderOrgName:           org.GetString("name"),
		EmailPlaceholderRecoveryTime:      recoveryTime(at.UTC(), lang),
		EmailPlaceholderRecoveryReportURL: RecoveryReportURL(app, reportToken),
	}

	escaped := make(map[string]any, len(raw))
	for k, v := range raw {
		escaped[k] = html.EscapeString(v.(string))
	}

	subject, _ := core.EmailTemplate{Subject: tpl.Subject}.Resolve(raw)
	_, rawBody := core.EmailTemplate{Body: tpl.Body}.Resolve(escaped)

	body, err := resolveTemplateContent(struct{ HTMLContent template.HTML }{template.HTML(rawBody)},
		templates.Layout, templates.HTMLBody)
	if err != nil {
		return err
	}

	return app.NewMailClient().Send(&mailer.Message{
		From:    mail.Address{Name: meta.SenderName, Address: meta.SenderAddress},
		To:      []mail.Address{{Address: to}},
		Subject: subject,
		HTML:    body,
	})
}

// recoveryTime is "15:04 UTC on 2 January 2006" in English, ISO 8601 elsewhere.
func recoveryTime(t time.Time, lang string) string {
	if i18n.Primary(lang) == "en" {
		return t.Format("15:04 MST on 2 January 2006")
	}

	return t.Format("2006-01-02 15:04 MST")
}
