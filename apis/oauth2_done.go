package apis

import (
	"html/template"
	"net/http"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local: the page the OAuth2 sign-in window lands on when the provider has
// answered (GET /api/oauth2-done?r=success|failure).
//
// Upstream sends that window to the admin UI (/_/#/auth/oauth2-redirect-*). Behind
// an edge gateway that guards /_ (the fragment never reaches the edge, so it cannot
// tell that route from the dashboard) every person's window would show the gateway's
// login. This page is public, has no dependency on the admin UI, and just closes the
// window, with a line to read if the browser keeps it open. The script is a file and
// the policy forbids everything else, like the other hosted pages.

const oauth2DoneCSP = "default-src 'none'; script-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

const oauth2DoneJS = `// closes the sign-in window; the page text is the fallback when the browser refuses
try { window.close(); } catch (e) { /* the text stays */ }
`

var oauth2DoneTemplate = template.Must(template.New("done").Parse(`<!doctype html>
<html lang="{{.Text.Lang}}"{{.Text.Attrs}}>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
<link rel="icon" href="data:,">
</head>
<body>
<main id="app">
<p role="status">{{.Message}}</p>
</main>
<script src="/api/oauth2-done.js"></script>
</body>
</html>`))

func oauth2DonePage(e *core.RequestEvent) error {
	text := NewPageText(e, "")

	msg := text.T("oidc.oauth2Done.failure")
	if e.Request.URL.Query().Get("r") == "success" {
		msg = text.T("oidc.oauth2Done.success")
	}

	h := e.Response.Header()
	h.Set("Content-Security-Policy", oauth2DoneCSP)
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Type", "text/html; charset=utf-8")
	e.Response.WriteHeader(http.StatusOK)

	return oauth2DoneTemplate.Execute(e.Response, map[string]any{
		"Text": text, "Title": msg, "Message": msg,
	})
}

func oauth2DoneScript(e *core.RequestEvent) error {
	h := e.Response.Header()
	h.Set("Content-Type", "application/javascript; charset=utf-8")
	h.Set("Cache-Control", "public, max-age=86400")
	h.Set("X-Content-Type-Options", "nosniff")
	_, err := e.Response.Write([]byte(oauth2DoneJS))

	return err
}
