package mails

import (
	"errors"
	"html"
	"html/template"
	"net/mail"
	"strings"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/mails/templates"
	"github.com/doodlemania2/claviger/tools/mailer"
)

// Fork-local (Claviger #86): two short notices that carry no note content and
// no reason: a break-glass request is waiting for a second holder, and a held
// note is waiting for an owner's review.

const EmailPlaceholderOrgId = "{ORG_ID}"

func init() {
	core.BreakGlassRequestNotifier = SendBreakGlassRequestNotice
	core.PastoralHoldNotifier = SendPastoralHoldNotice
}

// sendPastoralNotice sends the template "email.<prefix>." to each recipient.
func sendPastoralNotice(app core.App, prefix string, org *core.Record, who string, recipients []*core.Record) (int, error) {
	bundle := core.I18n(app)
	meta := app.Settings().Meta

	var (
		errs []error
		sent int
	)

	for _, to := range recipients {
		if to.Email() == "" {
			continue
		}

		lang := recipientLanguage(app, to)

		raw := map[string]any{
			core.EmailPlaceholderAppName: meta.AppName,
			core.EmailPlaceholderAppURL:  strings.TrimRight(meta.AppURL, "/"),
			EmailPlaceholderOrgName:      org.GetString("name"),
			EmailPlaceholderOrgId:        org.Id,
			EmailPlaceholderGrantee:      who,
		}

		escaped := make(map[string]any, len(raw))
		for k, v := range raw {
			escaped[k] = html.EscapeString(v.(string))
		}

		tpl := core.EmailTemplate{
			Subject: bundle.Raw(lang, "email."+prefix+".subject"),
			Body: composeBody(bundle, lang, "email."+prefix+".", []block{
				para("intro"), para("action"), button("button", core.EmailPlaceholderAppURL), italic("fine"),
			}),
		}

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

			continue
		}

		sent++
	}

	return sent, errors.Join(errs...)
}

// SendBreakGlassRequestNotice tells the other holders that a request waits.
func SendBreakGlassRequestNotice(app core.App, n *core.BreakGlassRequestNotice) (int, error) {
	who := n.Requester.GetString("name")
	if who == "" {
		who = n.Requester.Email()
	}

	return sendPastoralNotice(app, "breakGlassRequest", n.Org, who, n.Recipients)
}

// SendPastoralHoldNotice tells the org owners that a held note waits.
func SendPastoralHoldNotice(app core.App, n *core.PastoralHoldNotice) (int, error) {
	return sendPastoralNotice(app, "pastoralHold", n.Org, "", n.Recipients)
}
