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

// Fork-local (Claviger #79b): the invitation email.

// Placeholders of the invitation email. {APP_NAME} and {APP_URL} (from the
// settings) work as in the auth templates.
const (
	EmailPlaceholderOrgName   = "{ORG_NAME}"
	EmailPlaceholderOrgSlug   = "{ORG_SLUG}"
	EmailPlaceholderRole      = "{ROLE}"
	EmailPlaceholderInviter   = "{INVITER}"
	EmailPlaceholderInviteURL = "{INVITE_URL}"
	EmailPlaceholderExpires   = "{EXPIRES}"
)

// DefaultOrgInvitationTemplate is used when the org has no override
// (`settings.invitationEmail` with `subject` and/or `body`).
var DefaultOrgInvitationTemplate = core.EmailTemplate{
	Subject: "You are invited to join " + EmailPlaceholderOrgName + " on " + core.EmailPlaceholderAppName,
	Body: `<p>Hello,</p>
<p>` + EmailPlaceholderInviter + ` invited you to join <strong>` + EmailPlaceholderOrgName + `</strong> on ` + core.EmailPlaceholderAppName + ` as <strong>` + EmailPlaceholderRole + `</strong>.</p>
<p>
  <a class="btn" href="` + EmailPlaceholderInviteURL + `" target="_blank" rel="noopener">Accept the invitation</a>
</p>
<p>The link works once and expires on ` + EmailPlaceholderExpires + `.</p>
<p><i>If you did not expect this invitation, you can ignore this email.</i></p>
<p>
  Thanks,<br/>
  ` + core.EmailPlaceholderAppName + ` team
</p>`,
}

// InvitationURL is the link to the hosted accept page.
func InvitationURL(app core.App, token string) string {
	return strings.TrimRight(app.Settings().Meta.AppURL, "/") + "/orgs/invite/" + token
}

// SendOrgInvitation emails the invitation link. inviter is a display name
// ("" when unknown).
func SendOrgInvitation(app core.App, org, inv *core.Record, inviter, token string) error {
	// fork-local (#97): the invitation is sent in the language of the invitee's
	// account when there is one, otherwise in the app default
	bundle := core.I18n(app)
	lang := bundle.Negotiate(i18n.Preference{
		Record:  inviteeLanguage(app, inv.GetString("email")),
		Default: app.Settings().Meta.DefaultLang(),
	})

	if inviter == "" {
		inviter = bundle.Raw(lang, "email.invitation.someone")
	}

	tpl := BuiltinInvitationTemplate(bundle, lang)
	if s, b := core.InvitationEmailOverride(org); s != "" || b != "" {
		if s != "" {
			tpl.Subject = s
		}

		if b != "" {
			tpl.Body = b
		}
	}

	meta := app.Settings().Meta
	link := InvitationURL(app, token)
	expires := invitationDate(inv.GetDateTime("expires").Time().UTC(), lang)

	raw := map[string]any{
		core.EmailPlaceholderAppName: meta.AppName,
		core.EmailPlaceholderAppURL:  meta.AppURL,
		EmailPlaceholderOrgName:      org.GetString("name"),
		EmailPlaceholderOrgSlug:      org.GetString("slug"),
		EmailPlaceholderRole:         inv.GetString("role"),
		EmailPlaceholderInviter:      inviter,
		EmailPlaceholderInviteURL:    link,
		EmailPlaceholderExpires:      expires,
	}

	// the subject is plain text, the body is HTML: escape only for the body
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
		From: mail.Address{
			Name:    meta.SenderName,
			Address: meta.SenderAddress,
		},
		To:      []mail.Address{{Address: inv.GetString("email")}},
		Subject: subject,
		HTML:    body,
	})
}

// BuiltinInvitationTemplate is the invitation template without an org
// override, in the language. The English result equals DefaultOrgInvitationTemplate.
func BuiltinInvitationTemplate(b *i18n.Bundle, lang string) core.EmailTemplate {
	return core.EmailTemplate{
		Subject: b.Raw(lang, "email.invitation.subject"),
		Body: composeBody(b, lang, "email.invitation.", []block{
			para("intro"),
			button("button", EmailPlaceholderInviteURL),
			para("expiry"), italic("ignore"),
		}),
	}
}

// invitationDate formats the expiry: "2 January 2006" in English, ISO 8601
// elsewhere (month names are not part of the catalog, and a numeric date is
// unambiguous in every language).
func invitationDate(t time.Time, lang string) string {
	if i18n.Primary(lang) == "en" {
		return t.Format("2 January 2006")
	}

	return t.Format("2006-01-02")
}

// inviteeLanguage is the language preference of the account with the
// invitation's email, "" when there is no account or no preference.
func inviteeLanguage(app core.App, email string) string {
	col, err := core.OrgUserCollection(app)
	if err != nil {
		return ""
	}

	rec, err := app.FindAuthRecordByEmail(col, email)
	if err != nil {
		return ""
	}

	return core.RecordLanguage(app, rec)
}
