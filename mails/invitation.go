package mails

import (
	"html"
	"html/template"
	"net/mail"
	"strings"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/mails/templates"
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
	if inviter == "" {
		inviter = "Someone"
	}

	tpl := DefaultOrgInvitationTemplate
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
	expires := inv.GetDateTime("expires").Time().UTC().Format("2 January 2006")

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
