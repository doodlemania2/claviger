package apis

import (
	"html/template"
	"net/http"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #80, 80-6): the hosted "My household" page.
//
//	GET /households/mine           the signed-in person's households: the people in them and, for a
//	                               guardian, forms to change the details the API allows
//
// It sits at the root like /recovery, because it is a page of the person and
// not of one org (a person may belong to several households of several orgs).
// It follows the conventions of the other hosted pages: no inline script or
// style (the same strict Content-Security-Policy; the script is
// /orgs/assets/households.js, served by the org assets route), strings from
// the message catalog, accessible mode, everything through /api/households.
// The page is a shell: it looks nothing up, so it reveals nothing to a
// stranger. It only answers 404 when households are not available at all.

var householdsPageTemplates = template.Must(template.Must(orgsPageTemplates.Clone()).Parse(`
{{define "households"}}{{template "head" .}}
<main class="card wide" id="app" data-page="households" data-collection="{{.Collection}}">
<p id="hstatus" class="msg err" role="alert" tabindex="-1" hidden></p>
<div id="captchaBox" hidden></div>

{{template "signin" .}}

<div id="main" hidden>
<header class="bar">
<div><h1>{{.Text.T "orgs.households.title"}}</h1><p class="muted">{{.Text.T "orgs.households.intro"}}</p></div>
<button id="signout" type="button" class="secondary">{{.Text.T "orgs.signOut"}}</button>
</header>
<p id="loading" class="muted" role="status">{{.Text.T "orgs.households.loading"}}</p>
<div id="households" aria-live="polite"></div>
</div>
</main>
<script type="module" src="/orgs/assets/households.js"></script>
</body></html>{{end}}
`))

func bindHouseholdsPages(app core.App) {
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id:       "__clvHouseholdsPages__",
		Priority: 9998,
		Func: func(se *core.ServeEvent) error {
			se.Router.GET("/households/mine", householdsMinePage)

			return se.Next()
		},
	})
}

func householdsMinePage(e *core.RequestEvent) error {
	if !core.HouseholdsAvailable(e.App) {
		return orgsErrorPage(e, http.StatusNotFound, "orgs.err.notFound")
	}

	col, err := core.OrgUserCollection(e.App)
	if err != nil {
		return orgsErrorPage(e, http.StatusNotFound, "orgs.err.orgsUnavailable")
	}

	orgsPageHeaders(e)

	text := NewPageText(e, "")
	e.Response.WriteHeader(http.StatusOK)

	return householdsPageTemplates.ExecuteTemplate(e.Response, "households", orgsPageData{
		Text: text, Title: text.T("orgs.households.title"), Collection: col.Name,
	})
}
