package apis

import (
	"html/template"
	"net/http"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #87, 87-5): the hosted registers page of an org.
//
//	GET /orgs/registers/{slug}      the registers of the org: entries, finalize, marginal notes, certificate
//
// Registrars (not superusers) finalize and annotate, so the page is theirs.
// Owners and admins can read it. It is a shell, like the other hosted org
// pages: it looks nothing up, it has the same strict Content-Security-Policy
// (the script is /orgs/assets/registers.js), strings from the message catalog
// and accessible mode, and it builds the page with DOM nodes only. All of it
// goes through the register routes (registers.go, registers_list.go), so the
// rules of the server apply: the page only shows them.

var registersPagesTemplates = template.Must(template.Must(orgsPageTemplates.Clone()).Parse(`
{{define "registers"}}{{template "head" .}}
<main class="card wide" id="app" data-page="registers" data-slug="{{.Slug}}" data-collection="{{.Collection}}">
<p id="pstatus" class="msg err" role="alert" tabindex="-1" hidden></p>
<div id="captchaBox" hidden></div>

<div id="denied" hidden>
<h1>{{.Text.T "orgs.org.noAccess"}}</h1>
<p class="muted" id="deniedText">{{.Text.T "orgs.org.notMember"}}</p>
<p><button id="deniedOut" type="button" class="secondary">{{.Text.T "orgs.signOut"}}</button></p>
</div>

{{template "signin" .}}

<div id="main" hidden>
<header class="bar">
<div><h1>{{.Text.T "orgs.registers.page.title"}}</h1><p class="muted" id="orgName"></p></div>
<button id="signout" type="button" class="secondary">{{.Text.T "orgs.signOut"}}</button>
</header>
<p class="sub">{{.Text.T "orgs.registers.page.intro"}}</p>
<p id="loading" class="muted" role="status">{{.Text.T "orgs.registers.page.loading"}}</p>
<p id="none" class="muted" hidden>{{.Text.T "orgs.registers.page.none"}}</p>
<p id="readonly" class="muted" hidden></p>

<div id="listPanel" hidden>
<div class="inline">
<label>{{.Text.T "orgs.registers.page.register"}}<select id="regSelect"></select></label>
<label>{{.Text.T "orgs.registers.page.show"}}<select id="stateSelect">
<option value="">{{.Text.T "orgs.registers.page.stateAll"}}</option>
<option value="draft">{{.Text.T "orgs.registers.page.stateDraft"}}</option>
<option value="pending">{{.Text.T "orgs.registers.page.statePending"}}</option>
<option value="final">{{.Text.T "orgs.registers.page.stateFinal"}}</option>
</select></label>
</div>
<div class="tablewrap"><table id="entries">
<caption class="sr-only">{{.Text.T "orgs.registers.page.title"}}</caption>
<thead><tr id="headRow"></tr></thead>
<tbody></tbody>
</table></div>
<p id="empty" class="muted" hidden>{{.Text.T "orgs.registers.page.empty"}}</p>
<p class="inline">
<button id="prev" type="button" class="secondary">{{.Text.T "orgs.registers.page.prev"}}</button>
<span id="pageInfo" class="muted" role="status"></span>
<button id="next" type="button" class="secondary">{{.Text.T "orgs.registers.page.next"}}</button>
</p>
</div>

<section id="entry" class="hh" aria-labelledby="entryTitle" hidden>
<header class="bar">
<h2 id="entryTitle" tabindex="-1"></h2>
<button id="entryClose" type="button" class="secondary">{{.Text.T "orgs.registers.page.close"}}</button>
</header>
<dl id="facts" class="facts"></dl>
<p id="waiting" class="muted" hidden></p>
<p id="entryActions" class="inline">
<button id="finalize" type="button" hidden></button>
<button id="withdraw" type="button" class="secondary" hidden>{{.Text.T "js.registers.withdraw"}}</button>
<button id="certBtn" type="button" class="secondary" hidden>{{.Text.T "orgs.registers.page.certificate"}}</button>
</p>

<h3 id="marginTitle">{{.Text.T "orgs.registers.page.margin"}}</h3>
<ol id="notes" class="notes" aria-labelledby="marginTitle"></ol>
<p id="noNotes" class="muted" hidden>{{.Text.T "orgs.registers.page.noNotes"}}</p>

<form id="noteForm" class="stack" aria-labelledby="noteTitle" hidden>
<h3 id="noteTitle">{{.Text.T "orgs.registers.page.annotate"}}</h3>
<p class="muted">{{.Text.T "orgs.registers.page.annotateHelp"}}</p>
<label>{{.Text.T "orgs.registers.page.kind"}}<select id="nKind" name="kind"></select></label>
<label>{{.Text.T "orgs.registers.page.text"}}<textarea name="text" rows="3" maxlength="5000" autocomplete="off" required></textarea></label>
<label>{{.Text.T "orgs.registers.page.reason"}}<input name="reason" maxlength="1000" autocomplete="off" required></label>
<label id="supLabel" hidden>{{.Text.T "orgs.registers.page.supersedes"}}<select id="nSup" name="supersedes"></select></label>
<p><button type="submit">{{.Text.T "orgs.registers.page.addNote"}}</button></p>
</form>

<section id="certBox" hidden aria-labelledby="certTitle">
<h3 id="certTitle" tabindex="-1">{{.Text.T "orgs.registers.page.certificate"}}</h3>
<p class="muted">{{.Text.T "orgs.registers.page.certHelp"}}</p>
<p class="inline">
<button id="certPrint" type="button">{{.Text.T "orgs.registers.page.print"}}</button>
<button id="certClose" type="button" class="secondary">{{.Text.T "orgs.registers.page.close"}}</button>
</p>
<div id="certHost"></div>
</section>
</section>
</div>
</main>
<script type="module" src="/orgs/assets/registers.js"></script>
</body></html>{{end}}
`))

func bindRegistersPages(app core.App) {
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id:       "__clvRegistersPages__",
		Priority: 9998,
		Func: func(se *core.ServeEvent) error {
			se.Router.GET(orgsPagesMount+"/registers/{slug}", registersPage)

			return se.Next()
		},
	})
}

func registersPage(e *core.RequestEvent) error {
	slug := e.Request.PathValue("slug")
	if !orgSlugPageRegex.MatchString(slug) {
		return orgsErrorPage(e, http.StatusNotFound, "orgs.err.notFound")
	}

	col, err := core.OrgUserCollection(e.App)
	if err != nil {
		return orgsErrorPage(e, http.StatusNotFound, "orgs.err.orgsUnavailable")
	}

	orgsPageHeaders(e)

	text := NewPageText(e, "")
	e.Response.WriteHeader(http.StatusOK)

	return registersPagesTemplates.ExecuteTemplate(e.Response, "registers", orgsPageData{
		Text: text, Title: text.T("orgs.registers.page.title"), Slug: slug, Collection: col.Name,
	})
}
