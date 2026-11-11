package apis

import (
	"embed"
	"html/template"
	"path"
	"strings"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #83, 83-3): the static files shared by the hosted pages.
//
//	GET /a11y/{file}                    a11y-mode.css, a11y-mode.js: accessible mode (larger text, high contrast)
//	GET /passkeys/assets/{file}         passkeys-page.css, passkeys-page.js: the /passkeys page
//
// They are files, never inline, so the pages can keep a Content-Security-Policy
// without 'unsafe-inline'. The accessible-mode files are used by /passkeys,
// /orgs/* and /oidc/*; see docs/accessibility.md.

//go:embed hosted_assets/*
var hostedAssetsFS embed.FS

const (
	a11yMount     = "/a11y"
	passkeysMount = "/passkeys/assets"
)

// A11yHead is the stylesheet and script of accessible mode, for the end of <head>
// (after the page's own stylesheet). The script is not a module and not deferred:
// it applies the saved choice before the first paint.
func (p *PageText) A11yHead() template.HTML {
	return `<link rel="stylesheet" href="/a11y/a11y-mode.css">
<script src="/a11y/a11y-mode.js"></script>
`
}

// A11yBar is the skip link and the toggle, for the start of <body>. The target
// of the skip link is the element with id "app" (the <main> of each page). The
// toggle starts hidden: the script shows it, so without script there is no
// dead button.
func (p *PageText) A11yBar() template.HTML {
	return template.HTML(`<a class="skip" href="#app">` + template.HTMLEscapeString(p.T("a11y.skip")) + `</a>` + //nolint:gosec // the label is escaped
		"\n" + `<header class="a11y-bar"><button type="button" id="a11yToggle" class="a11y-toggle" aria-pressed="false" hidden>` +
		template.HTMLEscapeString(p.T("a11y.toggle")) + "</button></header>\n")
}

func bindHostedAssets(app core.App) {
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id:       "__clvHostedAssets__",
		Priority: 9998,
		Func: func(se *core.ServeEvent) error {
			se.Router.GET(a11yMount+"/{file}", func(e *core.RequestEvent) error {
				return serveHostedAsset(e, e.Request.PathValue("file"), "a11y-mode.css", "a11y-mode.js")
			})
			se.Router.GET(passkeysMount+"/{file}", func(e *core.RequestEvent) error {
				return serveHostedAsset(e, e.Request.PathValue("file"), "passkeys-page.css", "passkeys-page.js")
			})

			return se.Next()
		},
	})
}

// serveHostedAsset serves one of the allowed embedded files.
func serveHostedAsset(e *core.RequestEvent, file string, allowed ...string) error {
	name := path.Base(file)

	ok := false

	for _, a := range allowed {
		ok = ok || a == name
	}

	if !ok {
		return e.NotFoundError("", nil)
	}

	raw, err := hostedAssetsFS.ReadFile("hosted_assets/" + name)
	if err != nil {
		return e.NotFoundError("", nil)
	}

	contentType := "application/javascript; charset=utf-8"
	if strings.HasSuffix(name, ".css") {
		contentType = "text/css; charset=utf-8"
	}

	h := e.Response.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("X-Content-Type-Options", "nosniff")

	_, err = e.Response.Write(raw)

	return err
}
