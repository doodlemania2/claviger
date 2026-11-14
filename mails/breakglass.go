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

// Fork-local (Claviger #86, 86-2): the notice sent when break-glass access to
// a pastoral note is approved. It goes to the note's author and the org
// owners (and to the other holders of the breakglass tag when the author has
// no usable address), says who has access, who approved it, why and until
// when. It cannot be switched off and there is no deferred variant.

const (
	EmailPlaceholderGrantee  = "{GRANTEE}"
	EmailPlaceholderApprover = "{APPROVER}"
	EmailPlaceholderBasis    = "{BASIS}"
	EmailPlaceholderReason   = "{REASON}"
)

func init() {
	core.BreakGlassNotifier = SendBreakGlassNotice
}

// BuiltinBreakGlassTemplate is the notice in the language, for the author or
// for an owner or holder (author false). fallback adds the line that says why
// the other holders are told.
func BuiltinBreakGlassTemplate(b *i18n.Bundle, lang string, author, fallback bool) core.EmailTemplate {
	blocks := []block{para("intro"), para("reason"), para("expires")}

	if author {
		blocks = append(blocks, para("author"))
	} else {
		blocks = append(blocks, para("owner"))
	}

	if fallback {
		blocks = append(blocks, para("fallback"))
	}

	blocks = append(blocks, para("revoke"), button("button", core.EmailPlaceholderAppURL), italic("fine"))

	return core.EmailTemplate{
		Subject: b.Raw(lang, "email.breakGlass.subject"),
		Body:    composeBody(b, lang, "email.breakGlass.", blocks),
	}
}

// SendBreakGlassNotice emails each recipient and returns how many it reached.
// Recipients without an address are skipped.
func SendBreakGlassNotice(app core.App, n *core.BreakGlassNotice) (int, error) {
	bundle := core.I18n(app)
	meta := app.Settings().Meta

	var (
		errs []error
		sent int
	)

	name := func(r *core.Record) string {
		if v := r.GetString("name"); v != "" {
			return v
		}

		return r.Email()
	}

	for _, to := range n.Recipients {
		if to.Email() == "" {
			continue
		}

		lang := recipientLanguage(app, to)

		raw := map[string]any{
			core.EmailPlaceholderAppName: meta.AppName,
			core.EmailPlaceholderAppURL:  strings.TrimRight(meta.AppURL, "/"),
			EmailPlaceholderOrgName:      n.Org.GetString("name"),
			EmailPlaceholderGrantee:      name(n.Grantee),
			EmailPlaceholderApprover:     name(n.Approver),
			EmailPlaceholderBasis:        bundle.Raw(lang, "email.breakGlass.basis."+n.Grant.GetString("basis")),
			EmailPlaceholderReason:       n.Grant.GetString("reason"),
			EmailPlaceholderExpires:      invitationDate(n.Expires.UTC().Truncate(time.Second), lang) + n.Expires.UTC().Format(" 15:04 UTC"),
		}

		escaped := make(map[string]any, len(raw))
		for k, v := range raw {
			escaped[k] = html.EscapeString(v.(string))
		}

		isAuthor := to.Id == n.Note.GetString("author")
		tpl := BuiltinBreakGlassTemplate(bundle, lang, isAuthor, !n.AuthorReachable)

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
