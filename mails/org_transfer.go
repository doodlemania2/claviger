package mails

import (
	"errors"
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

// Fork-local (Claviger #84, 84-4): the notice sent to a person when a transfer
// of their membership to another org is started. It says who proposed it,
// between which orgs and until when, and that nothing changes unless the
// person accepts while signed in. For a minor it goes to the guardians.

const (
	EmailPlaceholderTargetOrg = "{TARGET_ORG}"
	EmailPlaceholderStarter   = "{STARTER}"
	EmailPlaceholderPerson    = "{PERSON}"
)

func init() {
	core.OrgTransferNotifier = SendOrgTransferNotice
}

// BuiltinOrgTransferTemplate is the notice in the language, for the person
// (guardian false) or for a guardian of a minor.
func BuiltinOrgTransferTemplate(b *i18n.Bundle, lang string, guardian bool) core.EmailTemplate {
	blocks := []block{para("intro")}
	if guardian {
		blocks = append(blocks, para("guardian"))
	}

	blocks = append(blocks, para("consent"), button("button", core.EmailPlaceholderAppURL), italic("fine"))

	return core.EmailTemplate{
		Subject: b.Raw(lang, "email.orgTransfer.subject"),
		Body:    composeBody(b, lang, "email.orgTransfer.", blocks),
	}
}

// SendOrgTransferNotice emails each recipient of the notice. Recipients without
// an address are skipped. It returns the joined errors of the sends that failed.
func SendOrgTransferNotice(app core.App, n *core.OrgTransferNotice) error {
	bundle := core.I18n(app)
	meta := app.Settings().Meta

	var errs []error

	for _, to := range n.Recipients {
		if to.Email() == "" {
			continue
		}

		lang := recipientLanguage(app, to)

		person := n.Person.GetString("name")
		if person == "" {
			person = n.Person.Email()
		}

		starter := n.Starter
		if starter == "" {
			starter = bundle.Raw(lang, "email.invitation.someone")
		}

		raw := map[string]any{
			core.EmailPlaceholderAppName: meta.AppName,
			core.EmailPlaceholderAppURL:  strings.TrimRight(meta.AppURL, "/"),
			EmailPlaceholderOrgName:      n.FromOrg.GetString("name"),
			EmailPlaceholderTargetOrg:    n.ToOrg.GetString("name"),
			EmailPlaceholderStarter:      starter,
			EmailPlaceholderPerson:       person,
			EmailPlaceholderExpires:      invitationDate(n.Transfer.GetDateTime("expiresAt").Time().UTC().Truncate(time.Second), lang),
		}

		escaped := make(map[string]any, len(raw))
		for k, v := range raw {
			escaped[k] = html.EscapeString(v.(string))
		}

		tpl := BuiltinOrgTransferTemplate(bundle, lang, to.Id != n.Person.Id)
		subject, _ := core.EmailTemplate{Subject: tpl.Subject}.Resolve(raw)
		_, rawBody := core.EmailTemplate{Body: tpl.Body}.Resolve(escaped)

		body, err := resolveTemplateContent(struct{ HTMLContent template.HTML }{template.HTML(rawBody)},
			templates.Layout, templates.HTMLBody)
		if err == nil {
			err = app.NewMailClient().Send(&mailer.Message{
				From:    mail.Address{Name: meta.SenderName, Address: meta.SenderAddress},
				To:      []mail.Address{{Address: to.Email()}},
				Subject: subject,
				HTML:    body,
			})
		}

		if err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}
