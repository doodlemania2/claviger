package apis

import (
	"html/template"
	"net/http"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #81, 81-4): the hosted "My credentials" page.
//
//	GET /credentials/mine          the signed-in person's safeguarding credentials, their dates and
//	                               why a list might look empty
//
// It sits at the root like /households/mine, because it is a page of the
// person and not of one org. It is read-only, with the same strict
// Content-Security-Policy as the other hosted pages (the script is
// /orgs/assets/credentials.js), strings from the message catalog and accessible
// mode. Everything comes from GET /api/me/access-status. The page is a shell:
// it looks nothing up, so it reveals nothing to a stranger. It answers 404 only
// when credentials are not available at all.

var credentialsPageTemplates = template.Must(template.Must(orgsPageTemplates.Clone()).Parse(`
{{define "credentials"}}{{template "head" .}}
<main class="card wide" id="app" data-page="credentials" data-collection="{{.Collection}}">
<p id="cstatus" class="msg err" role="alert" tabindex="-1" hidden></p>
<div id="captchaBox" hidden></div>

{{template "signin" .}}

<div id="main" hidden>
<header class="bar">
<div><h1>{{.Text.T "orgs.credentials.title"}}</h1><p class="muted">{{.Text.T "orgs.credentials.intro"}}</p></div>
<button id="signout" type="button" class="secondary">{{.Text.T "orgs.signOut"}}</button>
</header>
<p id="loading" class="muted" role="status">{{.Text.T "orgs.credentials.loading"}}</p>
<div id="credentials" aria-live="polite"></div>
</div>
</main>
<script type="module" src="/orgs/assets/credentials.js"></script>
</body></html>{{end}}
`))

func bindCredentialsPages(app core.App) {
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id:       "__clvCredentialsPages__",
		Priority: 9998,
		Func: func(se *core.ServeEvent) error {
			se.Router.GET("/credentials/mine", credentialsMinePage)

			return se.Next()
		},
	})
}

func credentialsMinePage(e *core.RequestEvent) error {
	if !core.CredentialsAvailable(e.App) {
		return orgsErrorPage(e, http.StatusNotFound, "orgs.err.notFound")
	}

	col, err := core.OrgUserCollection(e.App)
	if err != nil {
		return orgsErrorPage(e, http.StatusNotFound, "orgs.err.orgsUnavailable")
	}

	orgsPageHeaders(e)

	text := NewPageText(e, "")
	e.Response.WriteHeader(http.StatusOK)

	return credentialsPageTemplates.ExecuteTemplate(e.Response, "credentials", orgsPageData{
		Text: text, Title: text.T("orgs.credentials.title"), Collection: col.Name,
	})
}
