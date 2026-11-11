package apis

import (
	"html/template"
	"net/http"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #83, 83-2): the hosted recovery pages.
//
//	GET /recovery                  the person types the code (or follows the QR link) and is taken
//	                               through the existing passkey reset enrollment; also the page of
//	                               the "this was not me" link in the notice email
//	GET /orgs/recovery/{slug}      staff: issue a code, confirm a request of a colleague
//
// They follow the conventions of the other hosted pages: no inline script or
// style (same strict Content-Security-Policy, same assets under /orgs/assets),
// strings from the message catalog, everything through the JSON API. Neither
// page looks anything up on the server, so they reveal nothing about who or
// what exists. The code and the report token live in the URL fragment
// (#code=, #report=), which the browser does not send.
//
// Unlike /orgs/{slug}, the staff page is not reachable as /orgs/{slug}/recovery:
// that shape would clash with /orgs/assets/{file} in the router.

var recoveryPageTemplates = template.Must(template.Must(orgsPageTemplates.Clone()).Parse(`
{{define "recovery"}}{{template "head" .}}
<main class="card" id="app" data-page="recovery">
<h1>{{.Text.T "orgs.recovery.title"}}</h1>
<p id="rstatus" class="msg err" role="alert" hidden></p>

<section id="enter">
<p class="sub">{{.Text.T "orgs.recovery.intro"}}</p>
<form id="codeForm">
<label for="code">{{.Text.T "orgs.recovery.codeLabel"}}</label>
<input id="code" name="code" class="codeinput" autocomplete="off" autocapitalize="characters" spellcheck="false" maxlength="40" required>
<p class="muted">{{.Text.T "orgs.recovery.help"}}</p>
<button type="submit">{{.Text.T "orgs.recovery.continue"}}</button>
</form>
</section>

<section id="enroll" hidden>
<p>{{.Text.T "orgs.recovery.verified"}}</p>
<label for="deviceName">{{.Text.T "orgs.recovery.deviceName"}}</label>
<input id="deviceName" name="deviceName" autocomplete="off" maxlength="100">
<button id="enrollBtn" type="button">{{.Text.T "orgs.recovery.createPasskey"}}</button>
</section>

<section id="done" hidden>
<p class="msg ok" id="doneText" role="status"></p>
<p><a href="/passkeys">{{.Text.T "orgs.recovery.signIn"}}</a></p>
</section>

<section id="report" hidden>
<h2>{{.Text.T "orgs.recovery.reportTitle"}}</h2>
<p>{{.Text.T "orgs.recovery.reportIntro"}}</p>
<button id="reportBtn" type="button">{{.Text.T "orgs.recovery.reportButton"}}</button>
</section>
</main>
<script type="module" src="/orgs/assets/recovery.js"></script>
</body></html>{{end}}

{{define "recoverystaff"}}{{template "head" .}}
<main class="card wide" id="app" data-page="recoverystaff" data-slug="{{.Slug}}" data-collection="{{.Collection}}">
<p id="pstatus" class="msg err" role="alert" hidden></p>
<div id="captchaBox" hidden></div>

<div id="denied" hidden>
<h1>{{.Text.T "orgs.org.noAccess"}}</h1>
<p class="muted">{{.Text.T "orgs.org.notMember"}}</p>
<p><button id="deniedOut" type="button" class="secondary">{{.Text.T "orgs.signOut"}}</button></p>
</div>

{{template "signin" .}}

<div id="main" hidden>
<header class="bar">
<div><h1 id="orgName"></h1><p class="muted">{{.Text.T "orgs.recovery.staffTitle"}}</p></div>
<button id="signout" type="button" class="secondary">{{.Text.T "orgs.signOut"}}</button>
</header>
<p class="sub">{{.Text.T "orgs.recovery.staffIntro"}}</p>

<form id="issueForm" class="stack">
<label>{{.Text.T "orgs.recovery.person"}}<select id="person" name="user" required></select></label>
<label>{{.Text.T "orgs.recovery.verification"}}
<select name="verification">
<option value="known_by_sight">{{.Text.T "orgs.recovery.v.sight"}}</option>
<option value="id_checked">{{.Text.T "orgs.recovery.v.idChecked"}}</option>
<option value="other">{{.Text.T "orgs.recovery.v.other"}}</option>
</select></label>
<label>{{.Text.T "orgs.recovery.channel"}}
<select name="channel">
<option value="display">{{.Text.T "orgs.recovery.ch.display"}}</option>
<option value="qr">{{.Text.T "orgs.recovery.ch.qr"}}</option>
<option value="sms">{{.Text.T "orgs.recovery.ch.sms"}}</option>
</select></label>
<label>{{.Text.T "orgs.recovery.note"}}<textarea name="note" rows="2" maxlength="200" autocomplete="off"></textarea></label>
<p class="muted">{{.Text.T "orgs.recovery.noteHelp"}}</p>
<button type="submit">{{.Text.T "orgs.recovery.issue"}}</button>
</form>

<section id="result" hidden aria-live="polite">
<h2 id="resultTitle"></h2>
<p id="codeText" class="codebig"></p>
<p id="resultNote" class="muted"></p>
<p id="resultExtra" class="muted"></p>
</section>

<h2>{{.Text.T "orgs.recovery.open"}}</h2>
<div class="tablewrap"><table id="open">
<thead><tr><th>{{.Text.T "orgs.recovery.person"}}</th><th>{{.Text.T "orgs.col.status"}}</th><th>{{.Text.T "orgs.col.expires"}}</th><th></th></tr></thead>
<tbody></tbody>
</table></div>
</div>
</main>
<script type="module" src="/orgs/assets/recoverystaff.js"></script>
</body></html>{{end}}
`))

func bindRecoveryPages(app core.App) {
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id:       "__clvRecoveryPages__",
		Priority: 9998,
		Func: func(se *core.ServeEvent) error {
			se.Router.GET("/recovery", recoveryPage)
			se.Router.GET(orgsPagesMount+"/recovery/{slug}", recoveryStaffPage)

			return se.Next()
		},
	})
}

func renderRecoveryPage(e *core.RequestEvent, name string, d orgsPageData) error {
	orgsPageHeaders(e)

	if d.Text == nil {
		d.Text = NewPageText(e, "")
	}

	e.Response.WriteHeader(http.StatusOK)

	return recoveryPageTemplates.ExecuteTemplate(e.Response, name, d)
}

// recoveryPage is the page of the person. It is a shell: the recovery codes
// are not looked up here, and the page is the same whether or not recovery is
// set up.
func recoveryPage(e *core.RequestEvent) error {
	if !core.RecoveryAvailable(e.App) {
		return orgsErrorPage(e, http.StatusNotFound, "orgs.err.notFound")
	}

	text := NewPageText(e, "")

	return renderRecoveryPage(e, "recovery", orgsPageData{Text: text, Title: text.T("orgs.recovery.title")})
}

func recoveryStaffPage(e *core.RequestEvent) error {
	slug := e.Request.PathValue("slug")
	if !orgSlugPageRegex.MatchString(slug) || !core.RecoveryAvailable(e.App) {
		return orgsErrorPage(e, http.StatusNotFound, "orgs.err.notFound")
	}

	col, err := core.OrgUserCollection(e.App)
	if err != nil {
		return orgsErrorPage(e, http.StatusNotFound, "orgs.err.orgsUnavailable")
	}

	text := NewPageText(e, "")

	return renderRecoveryPage(e, "recoverystaff", orgsPageData{
		Text: text, Title: text.T("orgs.recovery.staffTitle"), Slug: slug, Collection: col.Name,
	})
}
