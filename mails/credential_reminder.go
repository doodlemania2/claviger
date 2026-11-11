package mails

import (
	"errors"
	"html"
	"html/template"
	"net/mail"
	"strconv"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/mails/templates"
	"github.com/doodlemania2/claviger/tools/i18n"
	"github.com/doodlemania2/claviger/tools/mailer"
)

// Fork-local (Claviger #81): the reminder sent before a safeguarding
// credential expires, to the holder and to the coordinators of the org. It
// names the credential type and the expiry date, nothing more. Sent in the
// default language of the org.

const (
	EmailPlaceholderCredential = "{CREDENTIAL}"
	EmailPlaceholderHolder     = "{HOLDER}"
	EmailPlaceholderDaysLeft   = "{DAYS}"
)

func init() {
	core.CredentialReminderSender = SendCredentialReminder
}

// BuiltinCredentialReminderTemplate is the reminder in the language, for the
// holder (coordinator false) or for a coordinator.
func BuiltinCredentialReminderTemplate(b *i18n.Bundle, lang string, coordinator bool) core.EmailTemplate {
	blocks := []block{para("holder"), para("holderAction")}
	if coordinator {
		blocks = []block{para("coordinator"), para("coordinatorAction")}
	}

	return core.EmailTemplate{
		Subject: b.Raw(lang, "email.credentialReminder.subject"),
		Body:    composeBody(b, lang, "email.credentialReminder.", blocks),
	}
}

// SendCredentialReminder emails the holder and each coordinator of the
// reminder. A recipient without an email address (a minor without a login, for
// one) is skipped; the holder is never mailed twice when they also coordinate.
// It returns the joined errors of the sends that failed.
func SendCredentialReminder(app core.App, r *core.CredentialReminder) error {
	bundle := core.I18n(app)
	lang := core.OrgDefaultLanguage(app, r.Org)
	meta := app.Settings().Meta

	holderName := r.Holder.GetString("name")
	if holderName == "" {
		holderName = r.Holder.Email()
	}

	if holderName == "" {
		holderName = bundle.Raw(lang, "email.invitation.someone")
	}

	raw := map[string]any{
		core.EmailPlaceholderAppName: meta.AppName,
		core.EmailPlaceholderAppURL:  meta.AppURL,
		EmailPlaceholderOrgName:      r.Org.GetString("name"),
		EmailPlaceholderCredential:   r.TypeName,
		EmailPlaceholderHolder:       holderName,
		EmailPlaceholderExpires:      invitationDate(r.Expires.UTC(), lang),
		EmailPlaceholderDaysLeft:     strconv.Itoa(r.DaysLeft),
	}

	escaped := make(map[string]any, len(raw))
	for k, v := range raw {
		escaped[k] = html.EscapeString(v.(string))
	}

	var errs []error

	send := func(to string, coordinator bool) {
		if to == "" {
			return
		}

		tpl := BuiltinCredentialReminderTemplate(bundle, lang, coordinator)
		subject, _ := core.EmailTemplate{Subject: tpl.Subject}.Resolve(raw)
		_, rawBody := core.EmailTemplate{Body: tpl.Body}.Resolve(escaped)

		body, err := resolveTemplateContent(struct{ HTMLContent template.HTML }{template.HTML(rawBody)},
			templates.Layout, templates.HTMLBody)
		if err == nil {
			err = app.NewMailClient().Send(&mailer.Message{
				From:    mail.Address{Name: meta.SenderName, Address: meta.SenderAddress},
				To:      []mail.Address{{Address: to}},
				Subject: subject,
				HTML:    body,
			})
		}

		if err != nil {
			errs = append(errs, err)
		}
	}

	send(r.Holder.Email(), false)

	for _, c := range r.Coordinators {
		if c.Id != r.Holder.Id {
			send(c.Email(), true)
		}
	}

	return errors.Join(errs...)
}
