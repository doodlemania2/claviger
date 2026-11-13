package apis

import (
	"html/template"
	"net/http"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #88): the hosted consent purposes page of an org.
//
//	GET /orgs/consents/{slug}      the org's purposes: list, create, edit, retire, delete
//
// Staff (owners, admins and registrars) manage the purposes of their own org
// through GET/POST/PATCH/DELETE /api/orgs/{id}/consent-purposes. The page is a
// shell, like the other hosted org pages: it looks nothing up, it has the same
// strict Content-Security-Policy (the script is /orgs/assets/consents.js),
// strings from the message catalog and accessible mode. It answers 404 when
// consents are not available at all.

var consentPagesTemplates = template.Must(template.Must(orgsPageTemplates.Clone()).Parse(`
{{define "consentpurposes"}}{{template "head" .}}
<main class="card wide" id="app" data-page="consentpurposes" data-slug="{{.Slug}}" data-collection="{{.Collection}}">
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
<div><h1>{{.Text.T "orgs.consentPurposes.title"}}</h1><p class="muted" id="orgName"></p></div>
<button id="signout" type="button" class="secondary">{{.Text.T "orgs.signOut"}}</button>
</header>
<p class="sub">{{.Text.T "orgs.consentPurposes.intro"}}</p>
<p id="loading" class="muted" role="status">{{.Text.T "orgs.consentPurposes.loading"}}</p>

<div class="tablewrap"><table id="purposes">
<caption class="sr-only">{{.Text.T "orgs.consentPurposes.title"}}</caption>
<thead><tr><th scope="col">{{.Text.T "orgs.col.name"}}</th><th scope="col">{{.Text.T "orgs.consentPurposes.key"}}</th><th scope="col">{{.Text.T "orgs.consentPurposes.version"}}</th><th scope="col">{{.Text.T "orgs.consentPurposes.scope"}}</th><th scope="col">{{.Text.T "orgs.col.status"}}</th><th scope="col"><span class="sr-only">{{.Text.T "orgs.col.actions"}}</span></th></tr></thead>
<tbody></tbody>
</table></div>
<p id="empty" class="muted" hidden>{{.Text.T "orgs.consentPurposes.empty"}}</p>

<form id="purposeForm" class="stack" aria-labelledby="formTitle">
<h2 id="formTitle"></h2>
<label>{{.Text.T "orgs.consentPurposes.key"}}<input id="fKey" name="key" autocomplete="off" maxlength="64" required aria-describedby="keyHelp"></label>
<p id="keyHelp" class="muted">{{.Text.T "orgs.consentPurposes.keyHelp"}}</p>
<label>{{.Text.T "orgs.col.name"}}<input name="name" autocomplete="off" maxlength="200" required></label>
<label>{{.Text.T "orgs.consentPurposes.text"}}<textarea name="text" rows="4" autocomplete="off" required aria-describedby="textHelp"></textarea></label>
<p id="textHelp" class="muted">{{.Text.T "orgs.consentPurposes.textHelp"}}</p>
<label>{{.Text.T "orgs.consentPurposes.basis"}}<select id="fBasis" name="lawfulBasis"></select></label>
<label>{{.Text.T "orgs.consentPurposes.minAge"}}<input name="minAge" type="number" min="0" max="21" step="1" inputmode="numeric" aria-describedby="ageHelp"></label>
<p id="ageHelp" class="muted">{{.Text.T "orgs.consentPurposes.minAgeHelp"}}</p>
<label class="check"><input name="requiresGuardian" type="checkbox"> {{.Text.T "orgs.consentPurposes.guardian"}}</label>
<label class="check"><input name="marketing" type="checkbox" aria-describedby="marketingHelp"> {{.Text.T "orgs.consentPurposes.marketing"}}</label>
<p id="marketingHelp" class="muted">{{.Text.T "orgs.consentPurposes.marketingHelp"}}</p>
<p>
<button id="fSave" type="submit"></button>
<button id="fCancel" type="button" class="secondary" hidden>{{.Text.T "orgs.consentPurposes.cancel"}}</button>
</p>
</form>
</div>
</main>
<script type="module" src="/orgs/assets/consents.js"></script>
</body></html>{{end}}
`))

func bindConsentPages(app core.App) {
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id:       "__clvConsentPages__",
		Priority: 9998,
		Func: func(se *core.ServeEvent) error {
			se.Router.GET(orgsPagesMount+"/consents/{slug}", consentPurposesPage)

			return se.Next()
		},
	})
}

func consentPurposesPage(e *core.RequestEvent) error {
	slug := e.Request.PathValue("slug")
	if !orgSlugPageRegex.MatchString(slug) || !core.ConsentsAvailable(e.App) {
		return orgsErrorPage(e, http.StatusNotFound, "orgs.err.notFound")
	}

	col, err := core.OrgUserCollection(e.App)
	if err != nil {
		return orgsErrorPage(e, http.StatusNotFound, "orgs.err.orgsUnavailable")
	}

	orgsPageHeaders(e)

	text := NewPageText(e, "")
	e.Response.WriteHeader(http.StatusOK)

	return consentPagesTemplates.ExecuteTemplate(e.Response, "consentpurposes", orgsPageData{
		Text: text, Title: text.T("orgs.consentPurposes.title"), Slug: slug, Collection: col.Name,
	})
}
