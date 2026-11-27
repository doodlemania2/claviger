package apis

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #87): permanent registers. See docs/registers.md and
// core/register*.go for the rules.
//
//	POST   /api/registers                                 {template: "sacramental", name, signers?, retainReason?}; superuser; creates a register from the template
//	GET    /api/registers, /api/registers/{collection}, /api/registers/_verify   read routes of the screens (registers_list.go)
//	GET    /api/registers/{collection}/{id}               the entry with its annotations, oldest first (registrar, owner, admin)
//	POST   /api/registers/{collection}/{id}/finalize      finalize, or take the next step when the register has two signers; registrar (never a superuser); fresh passkey
//	DELETE /api/registers/{collection}/{id}/finalize      withdraw a pending finalization
//	POST   /api/registers/{collection}/{id}/annotations   {kind, text, reason, supersedes?}; registrar (never a superuser); fresh passkey
//	GET    /api/registers/{collection}/{id}/certificate   HTML page with print CSS; registrar, owner, admin
//	POST   /api/registers/{collection}/{id}/restrict      {reason}; registrar (never a superuser); fresh passkey; restriction of processing (88-7)
//	POST   /api/registers/{collection}/{id}/unrestrict    {reason}; registrar (never a superuser); fresh passkey
//	POST   /api/registers/{collection}/{id}/anonymize     {reason}; registrar; fresh passkey; only when the register has anonymization on and the retention period has passed
//	POST   /api/registers/{collection}/anonymize          {org, reason}; the same for every due entry of the org
//
// A restricted entry is redacted in lists and views, and its certificate is refused, unless a registrar or a
// superuser adds ?reason= to the read (audited as register_restricted_read).
//
// The routes need a signed-in person. API keys are refused: finalizing and
// annotating are acts of a named person.
func bindRegistersApi(rg *router.RouterGroup[*core.RequestEvent]) {
	g := rg.Group("/registers").Bind(RequireAuth())
	g.POST("", registerCreate).Bind(RequireSuperuserAuth())
	g.GET("", registerList)                                        // fork-local (#87, 87-5)
	g.GET("/_verify", registerVerify).Bind(RequireSuperuserAuth()) // a register name starts with a letter, so it cannot clash
	g.GET("/{collection}", registerEntries)
	g.GET("/{collection}/{id}", registerView)
	g.POST("/{collection}/{id}/finalize", registerFinalize).Bind(refuseSuperuserSigning(), RequireFreshPasskey(webauthnStepUpMaxAge))
	g.DELETE("/{collection}/{id}/finalize", registerWithdraw)
	g.POST("/{collection}/{id}/annotations", registerAnnotate).Bind(refuseSuperuserSigning(), RequireFreshPasskey(webauthnStepUpMaxAge))
	g.GET("/{collection}/{id}/certificate", registerCertificate)
	g.POST("/{collection}/{id}/restrict", registerRestrict).Bind(refuseSuperuserSigning(), RequireFreshPasskey(webauthnStepUpMaxAge))
	g.POST("/{collection}/{id}/unrestrict", registerUnrestrict).Bind(refuseSuperuserSigning(), RequireFreshPasskey(webauthnStepUpMaxAge))
	g.POST("/{collection}/{id}/anonymize", registerAnonymize).Bind(refuseSuperuserSigning(), RequireFreshPasskey(webauthnStepUpMaxAge))
	g.POST("/{collection}/anonymize", registerAnonymizeDue).Bind(refuseSuperuserSigning(), RequireFreshPasskey(webauthnStepUpMaxAge))
}

// refuseSuperuserSigning answers a superuser with a clear 403 before the
// passkey step-up: only registrars sign and annotate, a superuser never does.
func refuseSuperuserSigning() *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id: "claviger_register_refuse_superuser",
		Func: func(e *core.RequestEvent) error {
			if e.HasSuperuserAuth() {
				return e.ForbiddenError("Superusers cannot sign or annotate registers. Ask a registrar of the org.", nil)
			}

			return e.Next()
		},
	}
}

type registerCreateBody struct {
	Template     string `json:"template" form:"template"`
	Name         string `json:"name" form:"name"`
	Signers      int    `json:"signers" form:"signers"`
	RetainReason string `json:"retainReason" form:"retainReason"`
	// Anonymize turns on the optional anonymization after AnonymizeAfterDays (88-7). Default off.
	Anonymize          bool `json:"anonymize" form:"anonymize"`
	AnonymizeAfterDays int  `json:"anonymizeAfterDays" form:"anonymizeAfterDays"`
}

// registerCreate makes a register from a template. An existing collection is
// registered by creating a _permanentCollections row instead.
func registerCreate(e *core.RequestEvent) error {
	var body registerCreateBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	if body.Template != "sacramental" {
		return e.BadRequestError("Unknown template. Use \"sacramental\", or add a _permanentCollections row for an existing collection.", nil)
	}

	if body.Signers != 0 && body.Signers != 1 && body.Signers != 2 {
		return e.BadRequestError("Signers must be 1 or 2.", nil)
	}

	if core.PermanentConfigFor(e.App, body.Name) != nil {
		return e.Error(http.StatusConflict, "That register already exists.", nil)
	}

	cfg, err := core.CreateSacramentalRegister(e.App, core.SacramentalRegisterOpts{Name: body.Name, Signers: body.Signers, RetainReason: body.RetainReason, Anonymize: body.Anonymize, AnonymizeAfterDays: body.AnonymizeAfterDays})
	if err != nil {
		return e.BadRequestError("Failed to create the register.", err)
	}

	return e.JSON(http.StatusCreated, map[string]any{
		"collection": cfg.Collection, "annotationCollection": cfg.AnnotationCollection,
		"chain": cfg.Chain, "signers": cfg.Signers, "finalizeField": cfg.FinalizeField,
	})
}

func registerGate(e *core.RequestEvent) error {
	if APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}

	return nil
}

// registerEntryJSON is the entry as the routes show it. A restricted entry is
// redacted to its id, org, state and chain position; revealRestricted shows it
// in full (the caller has checked the right and written the audit entry).
func registerEntryJSON(app core.App, cfg *core.PermanentConfig, entry *core.Record) map[string]any {
	return registerEntryJSONAs(app, cfg, entry, false)
}

func registerEntryJSONAs(app core.App, cfg *core.PermanentConfig, entry *core.Record, revealRestricted bool) map[string]any {
	if core.RegisterRestricted(entry) && !revealRestricted {
		full := entry.PublicExport()
		record := map[string]any{}

		for _, k := range core.RegisterRedactedFields(cfg) {
			if v, ok := full[k]; ok {
				record[k] = v
			}
		}

		return map[string]any{
			"collection": cfg.Collection,
			"id":         entry.Id,
			"org":        entry.GetString("org"),
			"state":      core.RegisterState(cfg, entry),
			"signers":    cfg.Signers,
			"chain":      cfg.Chain,
			"restricted": true,
			"redacted":   true,
			"record":     record,
		}
	}

	return map[string]any{
		"restricted": core.RegisterRestricted(entry),
		"anonymized": entry.Collection().Fields.GetByName("anonymizedAt") != nil && !entry.GetDateTime("anonymizedAt").IsZero(),
		"collection": cfg.Collection,
		"id":         entry.Id,
		"org":        entry.GetString("org"),
		"state":      core.RegisterState(cfg, entry),
		"signers":    cfg.Signers,
		"chain":      cfg.Chain,
		"record":     entry.PublicExport(),
		// names for the screens (87-5): the record carries ids only
		"finalizedByName":  core.PersonName(app, entry.GetString("finalizedBy")),
		"secondSignerName": core.PersonName(app, entry.GetString("secondSigner")),
		"pendingByName":    core.PersonName(app, entry.GetString("pendingBy")),
	}
}

func registerAnnotationJSON(app core.App, a *core.Record) map[string]any {
	return map[string]any{
		"id":         a.Id,
		"entry":      a.GetString("entry"),
		"kind":       a.GetString("kind"),
		"text":       a.GetString("text"),
		"author":     a.GetString("author"),
		"authorName": core.PersonName(app, a.GetString("author")),
		"reason":     a.GetString("reason"),
		"supersedes": a.GetString("supersedes"),
		"created":    a.GetDateTime("created"),
	}
}

func registerView(e *core.RequestEvent) error {
	if err := registerGate(e); err != nil {
		return err
	}

	read, err := core.ReadRegisterEntry(e.App, auditContext(e), e.Auth, e.Request.PathValue("collection"), e.Request.PathValue("id"),
		e.Request.URL.Query().Get("reason"))
	if err != nil {
		return orgHTTP(e, err)
	}

	out := registerEntryJSONAs(e.App, read.Config, read.Entry, read.Revealed)
	out["canReveal"] = read.CanReveal

	items := []map[string]any{}

	if read.Revealed {
		notes, err := core.RegisterAnnotations(e.App, read.Config, read.Entry)
		if err != nil {
			return e.InternalServerError("", err)
		}

		for _, a := range notes {
			items = append(items, registerAnnotationJSON(e.App, a))
		}
	}

	out["annotations"] = items

	return e.JSON(http.StatusOK, out)
}

func registerFinalize(e *core.RequestEvent) error {
	if err := registerGate(e); err != nil {
		return err
	}

	res, err := core.FinalizeRegisterEntry(e.App, auditContext(e), e.Auth, e.Request.PathValue("collection"), e.Request.PathValue("id"))
	if err != nil {
		return orgHTTP(e, err)
	}

	cfg := core.PermanentConfigFor(e.App, e.Request.PathValue("collection"))

	out := registerEntryJSON(e.App, cfg, res.Entry)
	out["pending"] = res.Pending
	out["final"] = res.Final

	return e.JSON(http.StatusOK, out)
}

func registerWithdraw(e *core.RequestEvent) error {
	if err := registerGate(e); err != nil {
		return err
	}

	entry, err := core.WithdrawRegisterFinalize(e.App, auditContext(e), e.Auth, e.Request.PathValue("collection"), e.Request.PathValue("id"))
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, registerEntryJSON(e.App, core.PermanentConfigFor(e.App, e.Request.PathValue("collection")), entry))
}

type registerAnnotationBody struct {
	Kind       string `json:"kind" form:"kind"`
	Text       string `json:"text" form:"text"`
	Reason     string `json:"reason" form:"reason"`
	Supersedes string `json:"supersedes" form:"supersedes"`
}

func registerAnnotate(e *core.RequestEvent) error {
	if err := registerGate(e); err != nil {
		return err
	}

	var body registerAnnotationBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	a, err := core.AddRegisterAnnotation(e.App, auditContext(e), e.Auth, e.Request.PathValue("collection"), e.Request.PathValue("id"),
		core.RegisterAnnotationInput{Kind: body.Kind, Text: body.Text, Reason: body.Reason, Supersedes: body.Supersedes})
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusCreated, registerAnnotationJSON(e.App, a))
}

// ---- certificate ----------------------------------------------------------------

// certificateHidden are the fields the certificate does not list as entry
// fields: the bookkeeping of finalizing (shown in their own lines) and the
// organization (shown as the issuer's).
var certificateHidden = []string{
	"id", "org", "finalizedBy", "secondSigner", "pendingBy", "pendingAt", "pendingHash", "seq", "hash", "restricted",
}

type certificateField struct{ Name, Value string }

type certificateNote struct {
	Date, Kind, Text, Reason, Author string
	Retracted                        bool
}

type certificateData struct {
	Text                    *PageText
	Title                   string
	Org, Collection, Entry  string
	Number                  string
	Hash                    string
	Finalized               string
	FinalizedBy, Second     string
	Fields                  []certificateField
	Notes                   []certificateNote
	Issued, Issuer, IssueId string
}

var certificateTemplate = template.Must(template.New("certificate").Parse(`<!doctype html>
<html lang="{{.Text.Lang}}"{{.Text.Attrs}}>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
<link rel="icon" href="data:,">
<link rel="stylesheet" href="/orgs/assets/certificate.css">
</head>
<body>
<main class="cert" id="app">
<header>
<p class="org">{{.Org}}</p>
<h1>{{.Text.T "orgs.registers.cert.title"}}</h1>
<p class="reg">{{.Text.T "orgs.registers.cert.register"}}: {{.Collection}}{{if .Number}} · {{.Text.T "orgs.registers.cert.number"}} {{.Number}}{{end}}</p>
</header>

<section>
<table class="fields">
{{range .Fields}}<tr><th scope="row">{{.Name}}</th><td>{{.Value}}</td></tr>
{{end}}</table>
</section>

<section>
<table class="fields">
<tr><th scope="row">{{.Text.T "orgs.registers.cert.finalized"}}</th><td>{{.Finalized}}</td></tr>
{{if .FinalizedBy}}<tr><th scope="row">{{.Text.T "orgs.registers.cert.finalizedBy"}}</th><td>{{.FinalizedBy}}</td></tr>{{end}}
{{if .Second}}<tr><th scope="row">{{.Text.T "orgs.registers.cert.secondSigner"}}</th><td>{{.Second}}</td></tr>{{end}}
{{if .Hash}}<tr><th scope="row">{{.Text.T "orgs.registers.cert.chainHash"}}</th><td><code>{{.Hash}}</code></td></tr>{{end}}
</table>
</section>

<section>
<h2>{{.Text.T "orgs.registers.cert.annotations"}}</h2>
{{if .Notes}}<ol class="notes">
{{range .Notes}}<li{{if .Retracted}} class="retracted"{{end}}>
<p class="meta">{{.Date}} · {{.Kind}}{{if .Author}} · {{.Author}}{{end}}{{if .Retracted}} · {{$.Text.T "orgs.registers.cert.retracted"}}{{end}}</p>
<p>{{.Text}}</p>
<p class="reason">{{$.Text.T "orgs.registers.cert.reason"}}: {{.Reason}}</p>
</li>
{{end}}</ol>{{else}}<p class="none">{{.Text.T "orgs.registers.cert.none"}}</p>{{end}}
</section>

<footer>
<p>{{.Text.T "orgs.registers.cert.issued"}}: {{.Issued}}{{if .Issuer}} · {{.Text.T "orgs.registers.cert.issuedBy"}}: {{.Issuer}}, {{.Org}}{{end}}</p>
<p class="small">{{.Text.T "orgs.registers.cert.notice"}}</p>
</footer>
</main>
</body>
</html>
`))

func registerCertificate(e *core.RequestEvent) error {
	if err := registerGate(e); err != nil {
		return err
	}

	read, err := core.ReadRegisterEntry(e.App, auditContext(e), e.Auth, e.Request.PathValue("collection"), e.Request.PathValue("id"),
		e.Request.URL.Query().Get("reason"))
	if err != nil {
		return orgHTTP(e, err)
	}

	cfg, entry, org := read.Config, read.Entry, read.Org

	if !read.Revealed {
		return e.ForbiddenError("This entry is restricted. A registrar can issue its certificate with ?reason=, which is audited.", nil)
	}

	if !cfg.IsFinalized(entry) {
		return e.Error(http.StatusConflict, "Only a final entry has a certificate.", nil)
	}

	notes, err := core.RegisterAnnotations(e.App, cfg, entry)
	if err != nil {
		return e.InternalServerError("", err)
	}

	text := NewPageTextDefault(e, "", core.OrgDefaultLanguage(e.App, org))
	now := time.Now().UTC()

	d := certificateData{
		Text:        text,
		Title:       text.T("orgs.registers.cert.title") + " " + entry.Id,
		Org:         org.GetString("name"),
		Collection:  cfg.Collection,
		Entry:       entry.Id,
		Hash:        entry.GetString("hash"),
		Finalized:   entry.GetDateTime(cfg.FinalizeField).Time().UTC().Format("2006-01-02 15:04 UTC"),
		FinalizedBy: core.PersonName(e.App, entry.GetString("finalizedBy")),
		Second:      core.PersonName(e.App, entry.GetString("secondSigner")),
		Issued:      now.Format("2006-01-02 15:04 UTC"),
		Issuer:      core.PersonName(e.App, e.Auth.Id),
	}

	if n := entry.GetInt("seq"); n > 0 {
		d.Number = strconv.Itoa(n)
	}

	for _, f := range entry.Collection().Fields {
		name := f.GetName()

		if f.GetHidden() || slices.Contains(certificateHidden, name) || name == cfg.FinalizeField {
			continue
		}

		if _, ok := f.(*core.AutodateField); ok {
			continue
		}

		if v := certificateValue(entry, f); v != "" {
			d.Fields = append(d.Fields, certificateField{Name: name, Value: v})
		}
	}

	retracted := map[string]bool{}

	for _, a := range notes {
		if a.GetString("kind") == core.RegisterKindRetraction {
			retracted[a.GetString("supersedes")] = true
		}
	}

	for _, a := range notes {
		d.Notes = append(d.Notes, certificateNote{
			Date:      a.GetDateTime("created").Time().UTC().Format("2006-01-02 15:04 UTC"),
			Kind:      text.T("orgs.registers.kind." + a.GetString("kind")),
			Text:      a.GetString("text"),
			Reason:    a.GetString("reason"),
			Author:    core.PersonName(e.App, a.GetString("author")),
			Retracted: retracted[a.Id],
		})
	}

	// one audit entry per issued certificate
	if err := core.WriteRegisterCertificateAudit(e.App, auditContext(e), cfg, entry, map[string]any{
		"issuer": e.Auth.Id, "annotations": len(notes), "issuedAt": now.Format(time.RFC3339),
	}); err != nil {
		return e.InternalServerError("", err)
	}

	h := e.Response.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	e.Response.WriteHeader(http.StatusOK)

	return certificateTemplate.Execute(e.Response, d)
}

func certificateValue(entry *core.Record, f core.Field) string {
	raw := entry.GetRaw(f.GetName())

	switch v := raw.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		if v {
			return "yes"
		}

		return ""
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}

	if f.Type() == core.FieldTypeDate {
		if dt := entry.GetDateTime(f.GetName()); !dt.IsZero() {
			return dt.Time().UTC().Format("2006-01-02")
		}

		return ""
	}

	b, err := json.Marshal(raw)
	if err != nil {
		return fmt.Sprint(raw)
	}

	if s := string(b); s != "null" && s != "[]" && s != "{}" && s != `""` {
		return s
	}

	return ""
}

// ---- restriction and anonymization (88-7) ------------------------------------------

type registerReasonBody struct {
	Org    string `json:"org" form:"org"`
	Reason string `json:"reason" form:"reason"`
}

func registerReasonRoute(e *core.RequestEvent, do func(reason string) (*core.Record, error)) error {
	if err := registerGate(e); err != nil {
		return err
	}

	var body registerReasonBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	entry, err := do(body.Reason)
	if err != nil {
		return orgHTTP(e, err)
	}

	cfg := core.PermanentConfigFor(e.App, e.Request.PathValue("collection"))

	return e.JSON(http.StatusOK, registerEntryJSONAs(e.App, cfg, entry, false))
}

func registerRestrict(e *core.RequestEvent) error {
	return registerReasonRoute(e, func(reason string) (*core.Record, error) {
		return core.RestrictRegisterEntry(e.App, auditContext(e), e.Auth, e.Request.PathValue("collection"), e.Request.PathValue("id"), reason)
	})
}

func registerUnrestrict(e *core.RequestEvent) error {
	return registerReasonRoute(e, func(reason string) (*core.Record, error) {
		return core.LiftRegisterRestriction(e.App, auditContext(e), e.Auth, e.Request.PathValue("collection"), e.Request.PathValue("id"), reason)
	})
}

func registerAnonymize(e *core.RequestEvent) error {
	return registerReasonRoute(e, func(reason string) (*core.Record, error) {
		return core.AnonymizeRegisterEntry(e.App, auditContext(e), e.Auth, e.Request.PathValue("collection"), e.Request.PathValue("id"), reason)
	})
}

func registerAnonymizeDue(e *core.RequestEvent) error {
	if err := registerGate(e); err != nil {
		return err
	}

	var body registerReasonBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	if body.Org == "" {
		return e.BadRequestError("Say which organization with org.", nil)
	}

	if strings.TrimSpace(body.Reason) == "" {
		return e.BadRequestError("A reason is required.", nil)
	}

	done, err := core.AnonymizeDueRegisterEntries(e.App, auditContext(e), e.Auth, e.Request.PathValue("collection"), body.Org, body.Reason)
	if err != nil {
		return orgHTTP(e, err)
	}

	if done == nil {
		done = []string{}
	}

	return e.JSON(http.StatusOK, map[string]any{"anonymized": done, "count": len(done)})
}
