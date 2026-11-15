package mails

import (
	"html"
	"html/template"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/mails/templates"
	"github.com/doodlemania2/claviger/tools/mailer"
)

// Fork-local (Claviger #88, 88-4): the three emails of an erasure.
//
//	confirm  the link that confirms the request (it can be ignored: nothing happens)
//	grace    the request was confirmed: the date it will run, and the link to cancel
//	result   what was done and what was kept, and how to complain (Article 12(4)).
//	         It is sent before the address is anonymized.
//
// The catalog keys are email.erasureConfirm.*, email.erasureGrace.* and
// email.erasureResult.*. The links go to the hosted pages /erasure/confirm and
// /erasure/cancel, which only show a button: a mail scanner that opens a link
// changes nothing.

const (
	EmailPlaceholderGraceDate = "{GRACE_DATE}"
	EmailPlaceholderGraceDays = "{GRACE_DAYS}"
	EmailPlaceholderCount     = "{COUNT}"
	EmailPlaceholderErased    = "{ERASED}"
	EmailPlaceholderAnon      = "{ANONYMIZED}"
	EmailPlaceholderPseudo    = "{PSEUDONYMIZED}"
	EmailPlaceholderRetained  = "{RETAINED_LIST}"
)

func init() {
	core.ErasureNotifier = SendErasureNotice
}

// SendErasureNotice sends the email of the notice in the language of the
// recipient.
func SendErasureNotice(app core.App, n *core.ErasureNotice) error {
	meta := app.Settings().Meta
	appURL := strings.TrimRight(meta.AppURL, "/")
	bundle := core.I18n(app)
	lang := recipientLanguage(app, n.Recipient)

	raw := map[string]string{
		core.EmailPlaceholderAppName: meta.AppName,
		core.EmailPlaceholderAppURL:  appURL,
	}

	var (
		prefix  string
		blocks  []block
		rawHTML = map[string]string{}
	)

	intro := "intro"
	if n.OnBehalf {
		intro = "introGuardian"
	}

	switch n.Kind {
	case "confirm":
		prefix = "erasureConfirm"
		blocks = []block{para(intro), para("action"), button("button", appURL+"/erasure/confirm?token="+url.QueryEscape(n.Token)), italic("fine")}
		raw[EmailPlaceholderGraceDays] = strconv.Itoa(n.GraceDays)
	case "grace":
		prefix = "erasureGrace"
		blocks = []block{para(intro), para("action"), button("button", appURL+"/erasure/cancel?token="+url.QueryEscape(n.Token)), italic("fine")}
		raw[EmailPlaceholderGraceDate] = n.GraceUntil.UTC().Format(time.DateOnly)
		raw[EmailPlaceholderGraceDays] = strconv.Itoa(n.GraceDays)
	case "result":
		prefix = "erasureResult"
		blocks = []block{para(intro), para("counts"), {kind: "raw", href: EmailPlaceholderRetained}, para("complaint"), italic("fine")}

		res := n.Result
		if res == nil {
			res = &core.ErasureResult{}
		}

		c := res.Counts()
		raw[EmailPlaceholderErased] = strconv.Itoa(c["erased"])
		raw[EmailPlaceholderAnon] = strconv.Itoa(c["anonymized"])
		raw[EmailPlaceholderPseudo] = strconv.Itoa(c["pseudonymized"])
		rawHTML[EmailPlaceholderRetained] = retainedListHTML(bundle, lang, res)
	default:
		return nil
	}

	escaped := make(map[string]any, len(raw))
	plain := make(map[string]any, len(raw))

	for k, v := range raw {
		escaped[k] = html.EscapeString(v)
		plain[k] = v
	}

	for k, v := range rawHTML {
		escaped[k] = v
	}

	key := "email." + prefix + "."
	tpl := core.EmailTemplate{
		Subject: bundle.Raw(lang, key+"subject"),
		Body:    composeBody(bundle, lang, key, blocks),
	}

	subject, _ := core.EmailTemplate{Subject: tpl.Subject}.Resolve(plain)
	_, rawBody := core.EmailTemplate{Body: tpl.Body}.Resolve(escaped)

	// the link carries the token: it is already URL-escaped and sits in an href
	body, err := resolveTemplateContent(struct{ HTMLContent template.HTML }{template.HTML(rawBody)}, //nolint:gosec // every value is escaped above
		templates.Layout, templates.HTMLBody)
	if err != nil {
		return err
	}

	return app.NewMailClient().Send(&mailer.Message{
		From:    mail.Address{Name: meta.SenderName, Address: meta.SenderAddress},
		To:      []mail.Address{{Address: n.Recipient.Email()}},
		Subject: subject,
		HTML:    body,
	})
}

// retainedListHTML is the list of what was kept, each with its reason. The
// reasons come from the erasure job; they are escaped here.
func retainedListHTML(b interface {
	Raw(lang, key string) string
}, lang string, res *core.ErasureResult) string {
	var sb strings.Builder

	if res.Refused != "" {
		sb.WriteString("<p><strong>" + html.EscapeString(b.Raw(lang, "email.erasureResult.refused")) + "</strong></p>")

		return sb.String()
	}

	if len(res.Retained) == 0 {
		return "<p>" + b.Raw(lang, "email.erasureResult.nothingKept") + "</p>"
	}

	sb.WriteString("<p>" + b.Raw(lang, "email.erasureResult.kept") + "</p><ul>")

	kept := append([]core.ErasureRetained(nil), res.Retained...)
	sort.Slice(kept, func(i, j int) bool { return kept[i].Collection < kept[j].Collection })

	for _, k := range kept {
		sb.WriteString("<li>" + html.EscapeString(strconv.Itoa(k.Count)) + " &times; " + html.EscapeString(k.Collection) + ": " + html.EscapeString(k.Reason) + "</li>")
	}

	sb.WriteString("</ul>")

	return sb.String()
}
