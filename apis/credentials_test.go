package apis_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #81): the credential routes, the read rules of the
// system collections and a collection gated by a credential.
//
// Fixed far dates (2099 future, 2020 past) keep every case independent of the
// wall clock.
//
// Fixture: the org fixture plus the type safe_environment at the diocese D
// (12 months) and strict_check at P1 (6 months, needs a second verifier). carol
// (member) and dave (viewer) carry the tag "safeguarding" in P1: carol is a
// coordinator, dave is not (a viewer never is).

const (
	credCarol   = "credcarolsafe01"
	credPending = "credpendingaaa1" // dave, strict_check, recorded by alice, waiting
	credBob     = "credbobpendaaa1" // bob, strict_check, recorded by alice, waiting
	credErin    = "crederinp2aaaa1"
)

func seedCredential(t testing.TB, app core.App, id, user, org, typ, expires string, verified bool, recordedBy string) {
	t.Helper()

	col, err := app.FindCollectionByNameOrId(core.CollectionNameCredentials)
	orgMust(t, err)

	exp, err := types.ParseDateTime(expires)
	orgMust(t, err)

	c := core.NewRecord(col)
	c.Id = id
	c.Set("user", orgUserID(user))
	c.Set("org", org)
	c.Set("type", typ)
	c.Set("issuedAt", "2024-01-01 00:00:00.000Z")
	c.Set("expires", exp)
	c.Set("verified", verified)
	c.Set("revoked", false)
	c.Set("status", map[bool]string{true: "valid", false: "pending"}[verified])

	if recordedBy != "" {
		c.Set("recordedBy", orgUserID(recordedBy))
	}

	if verified {
		c.Set("verifiedBy", orgUserID("bob"))
	}

	orgMust(t, app.Save(c))
}

func credFactory(t testing.TB) *tests.TestApp {
	app := orgsFactory(t)

	roles, err := app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
	orgMust(t, err)

	tag := core.NewRecord(roles)
	tag.Id = "tagsafeguardp1a"
	tag.Set("org", orgP1)
	tag.Set("name", core.CredentialCoordinatorTag)
	orgMust(t, app.Save(tag))

	for _, who := range []string{"carol", "dave"} {
		m, err := core.FindOrgMember(app, orgP1, orgUserID(who))
		orgMust(t, err)
		m.Set("tags", append(m.GetStringSlice("tags"), tag.Id))
		orgMust(t, app.Save(m))
	}

	typesCol, err := app.FindCollectionByNameOrId(core.CollectionNameCredentialTypes)
	orgMust(t, err)

	mkType := func(id, org, key string, months int, verifier bool) {
		r := core.NewRecord(typesCol)
		r.Id = id
		r.Set("org", org)
		r.Set("key", key)
		r.Set("name", key)
		r.Set("validMonths", months)
		r.Set("requiresVerifier", verifier)
		r.Set("status", core.CredentialTypeActive)
		orgMust(t, app.Save(r))
	}
	mkType("ctypesafed00001", orgD, "safe_environment", 12, false)
	mkType("ctypestrictp1a1", orgP1, "strict_check", 6, true)

	seedCredential(t, app, credCarol, "carol", orgP1, "safe_environment", "2099-01-01 00:00:00.000Z", true, "bob")
	seedCredential(t, app, credPending, "dave", orgP1, "strict_check", "2099-01-01 00:00:00.000Z", false, "alice")
	seedCredential(t, app, credBob, "bob", orgP1, "strict_check", "2099-01-01 00:00:00.000Z", false, "alice")
	seedCredential(t, app, credErin, "erin", orgP2, "safe_environment", "2099-01-01 00:00:00.000Z", true, "ivan")

	return app
}

func credCase(name, who, method, url, body string, status int, has ...string) orgCase {
	return orgCase{name: name, who: who, method: method, url: url, body: body, status: status, has: has, factory: credFactory}
}

func TestCredentialTypes(t *testing.T) {
	t.Parallel()

	base := "/api/orgs/" + orgP1 + "/credential-types"

	runOrgCases(t, []orgCase{
		credCase("members list the inherited and own types", "carol", "GET", base, "", 200,
			`"key":"safe_environment"`, `"key":"strict_check"`),
		credCase("a coordinator of the diocese reads them", "gina", "GET", base, "", 200, `"key":"strict_check"`),
		credCase("outsiders get a 404", "frank", "GET", base, "", 404),
		credCase("other org members get a 404", "erin", "GET", base, "", 404),
		credCase("guests", "", "GET", base, "", 401),
		credCase("admin defines a type", "bob", "POST", base, `{"key":"first_aid","name":"First aid","validMonths":24}`, 200,
			`"key":"first_aid"`, `"validMonths":24`, `"status":"active"`),
		credCase("a tagged member is not an admin", "carol", "POST", base, `{"key":"first_aid","name":"First aid"}`, 403),
		credCase("a bad key", "bob", "POST", base, `{"key":"First Aid!","name":"x"}`, 400),
		credCase("a duplicate", "bob", "POST", base, `{"key":"strict_check","name":"x"}`, 409),
		credCase("a document requirement needs the org setting", "bob", "POST", base,
			`{"key":"with_doc","name":"x","requiresDocument":true}`, 400),
		credCase("retire a type", "bob", "PATCH", base+"/ctypestrictp1a1", `{"status":"retired"}`, 200, `"status":"retired"`),
		credCase("a type in use cannot be deleted", "bob", "DELETE", base+"/ctypestrictp1a1", "", 409),
		credCase("a type of an ancestor is not editable here", "bob", "PATCH", base+"/ctypesafed00001", `{"name":"x"}`, 404),
	})
}

func TestCredentialRecording(t *testing.T) {
	t.Parallel()

	base := "/api/orgs/" + orgP1 + "/credentials"
	dave, carol := orgUserID("dave"), orgUserID("carol")

	runOrgCases(t, []orgCase{
		credCase("an admin records one for a member", "bob", "POST", base,
			`{"user":"`+dave+`","type":"safe_environment","expires":"2098-01-31","issuer":"Training Co"}`, 200,
			`"state":"valid"`, `"verified":true`, `"issuer":"Training Co"`, `"expires":"2098-01-31 23:59:59`),
		credCase("an email works for the holder", "bob", "POST", base,
			`{"user":"dave@orgs.test","type":"safe_environment","expires":"2098-01-31"}`, 200, `"user":"`+dave+`"`),
		credCase("a tagged member is a coordinator", "carol", "POST", base,
			`{"user":"`+dave+`","type":"safe_environment","expires":"2098-01-31"}`, 200, `"state":"valid"`),
		credCase("the expiry defaults from the type validity", "bob", "POST", base,
			`{"user":"`+dave+`","type":"safe_environment"}`, 200, `"state":"valid"`),
		credCase("a tagged viewer is not a coordinator", "dave", "POST", base,
			`{"user":"`+carol+`","type":"safe_environment","expires":"2098-01-31"}`, 403),
		credCase("nobody records their own", "bob", "POST", base,
			`{"user":"`+orgUserID("bob")+`","type":"safe_environment","expires":"2098-01-31"}`, 403),
		credCase("a superuser can", "super", "POST", base,
			`{"user":"`+orgUserID("alice")+`","type":"safe_environment","expires":"2098-01-31"}`, 200, `"state":"valid"`),
		credCase("a coordinator of the diocese reads but never writes", "gina", "POST", base,
			`{"user":"`+dave+`","type":"safe_environment","expires":"2098-01-31"}`, 403),
		credCase("an outsider gets a 404", "frank", "POST", base,
			`{"user":"`+dave+`","type":"safe_environment","expires":"2098-01-31"}`, 404),
		credCase("an unknown type", "bob", "POST", base, `{"user":"`+dave+`","type":"nope_nope","expires":"2098-01-31"}`, 400),
		credCase("an expiry in the past", "bob", "POST", base, `{"user":"`+dave+`","type":"safe_environment","expires":"2020-01-31"}`, 400),
		credCase("a holder outside the org", "bob", "POST", base,
			`{"user":"`+orgUserID("erin")+`","type":"safe_environment","expires":"2098-01-31"}`, 400),
		credCase("a type that needs a verifier starts pending", "alice", "POST", base,
			`{"user":"`+carol+`","type":"strict_check","expires":"2098-01-31"}`, 200, `"state":"pending"`, `"verified":false`),
	})
}

func TestCredentialVerifyRevoke(t *testing.T) {
	t.Parallel()

	base := "/api/orgs/" + orgP1 + "/credentials/"

	runOrgCases(t, []orgCase{
		credCase("the recorder cannot be the second verifier", "alice", "POST", base+credPending+"/verify", "", 403),
		credCase("a second coordinator verifies", "bob", "POST", base+credPending+"/verify", "", 200,
			`"verified":true`, `"state":"valid"`, `"verifiedBy":"`+orgUserID("bob")+`"`),
		credCase("a tagged member verifies too", "carol", "POST", base+credPending+"/verify", "", 200, `"state":"valid"`),
		credCase("nobody verifies their own", "bob", "POST", base+credBob+"/verify", "", 403),
		credCase("a tagged viewer cannot", "dave", "POST", base+credBob+"/verify", "", 403),
		credCase("an already verified one", "alice", "POST", base+credCarol+"/verify", "", 409),
		credCase("revoke with a reason", "bob", "POST", base+credCarol+"/revoke", `{"reason":"withdrawn"}`, 200,
			`"revoked":true`, `"state":"revoked"`, `"revokedReason":"withdrawn"`),
		credCase("a viewer cannot revoke", "dave", "POST", base+credCarol+"/revoke", "", 403),
		credCase("a credential of another org is a 404", "bob", "POST", base+credErin+"/revoke", "", 404),
		credCase("renew with a new expiry", "bob", "PATCH", base+credCarol, `{"expires":"2098-06-30"}`, 200,
			`"expires":"2098-06-30 23:59:59`, `"state":"valid"`),
		credCase("a renewal of a type with a verifier is pending again", "alice", "PATCH", base+credPending,
			`{"expires":"2098-06-30"}`, 200, `"state":"pending"`),
		{
			name: "revoke is stored and cuts the status", who: "bob", method: "POST", url: "/api/orgs/" + orgP1 + "/credentials/" + credCarol + "/revoke",
			body: `{"reason":"x"}`, status: 200, factory: credFactory,
			after: func(t testing.TB, app *tests.TestApp) {
				c, err := app.FindRecordById(core.CollectionNameCredentials, credCarol)
				orgMust(t, err)

				if !c.GetBool("revoked") || c.GetString("status") != "revoked" || c.GetDateTime("revokedAt").IsZero() {
					t.Fatalf("not revoked: %v", c.PublicExport())
				}
			},
		},
	})
}

func TestCredentialList(t *testing.T) {
	t.Parallel()

	url := "/api/orgs/" + orgP1 + "/credentials"

	runOrgCases(t, []orgCase{
		credCase("a coordinator lists", "alice", "GET", url, "", 200, `"id":"`+credCarol+`"`, `"totalItems":3`),
		credCase("a tagged member lists", "carol", "GET", url, "", 200, `"id":"`+credCarol+`"`),
		credCase("a coordinator of the diocese lists", "gina", "GET", url, "", 200, `"id":"`+credCarol+`"`),
		credCase("a tagged viewer cannot", "dave", "GET", url, "", 403),
		credCase("an outsider gets a 404", "frank", "GET", url, "", 404),
		credCase("a filter by state", "alice", "GET", url+"?state=pending", "", 200, `"totalItems":2`),
		credCase("a filter by type", "alice", "GET", url+"?type=safe_environment", "", 200, `"totalItems":1`),
		credCase("guests", "", "GET", url, "", 401),
	})
}

func TestCredentialAccessStatus(t *testing.T) {
	t.Parallel()

	url := "/api/orgs/" + orgP1 + "/access-status"

	runOrgCases(t, []orgCase{
		// P1 has four active members (sam is suspended): alice, bob, carol, dave
		credCase("counts per type", "alice", "GET", url, "", 200,
			`"key":"safe_environment"`, `"key":"strict_check"`, `"cleared":1`, `"pending":2`, `"missing":3`, `"missing":2`),
		credCase("one type and one standing", "alice", "GET", url+"?type=safe_environment&standing=cleared", "", 200,
			`"user":"`+orgUserID("carol")+`"`, `"standing":"cleared"`, `"totalItems":1`),
		credCase("the diocese coordinator sees the parish", "gina", "GET", url, "", 200, `"key":"safe_environment"`),
		credCase("a tagged viewer is refused", "dave", "GET", url, "", 403),
		credCase("a plain member of another org gets a 404", "erin", "GET", url, "", 404),
		credCase("a bad standing", "alice", "GET", url+"?standing=nope", "", 400),
		credCase("guests", "", "GET", url, "", 401),
		{
			// 10 days from the real clock is inside the 60 day window whatever day this runs
			name: "an expiry inside the window is expiring", who: "alice", url: url + "?type=safe_environment", status: 200,
			has: []string{`"expiring":1`, `"cleared":1`, `"standing":"expiring"`},
			factory: func(t testing.TB) *tests.TestApp {
				app := credFactory(t)
				soon := time.Now().UTC().AddDate(0, 0, 10).Format("2006-01-02 15:04:05.000Z")
				seedCredential(t, app, "credsoonaaaaaa1", "dave", orgP1, "safe_environment", soon, true, "bob")

				return app
			},
		},
	})
}

func TestCredentialAncestorIssued(t *testing.T) {
	t.Parallel()

	// a credential issued by the diocese counts for the parish in the status
	runOrgCases(t, []orgCase{
		{
			name: "diocese-issued counts", who: "alice", url: "/api/orgs/" + orgP1 + "/access-status?type=safe_environment", status: 200,
			has: []string{`"cleared":2`, `"issuedByOrg":"` + orgD + `"`},
			factory: func(t testing.TB) *tests.TestApp {
				app := credFactory(t)
				seedCredential(t, app, "creddioceseaaa1", "dave", orgD, "safe_environment", "2099-01-01 00:00:00.000Z", true, "gina")

				return app
			},
		},
	})
}

func TestCredentialCollectionRules(t *testing.T) {
	t.Parallel()

	list := "/api/collections/_credentials/records?perPage=50"
	id := func(s string) string { return `"id":"` + s + `"` }

	runOrgCases(t, []orgCase{
		{name: "the holder sees only their own", who: "erin", url: list, status: 200, factory: credFactory,
			has: []string{id(credErin)}, hasNot: []string{id(credPending), id(credCarol), id(credBob)}},
		{name: "a coordinator of two orgs sees both", who: "carol", url: list, status: 200, factory: credFactory,
			has: []string{id(credCarol), id(credPending), id(credErin)}},
		{name: "a tagged viewer sees only their own", who: "dave", url: list, status: 200, factory: credFactory,
			has: []string{id(credPending)}, hasNot: []string{id(credCarol), id(credBob)}},
		{name: "an admin sees the org's, not another's", who: "bob", url: list, status: 200, factory: credFactory,
			has: []string{id(credCarol), id(credPending), id(credBob)}, hasNot: []string{id(credErin)}},
		{name: "a coordinator of the diocese sees the parish's", who: "gina", url: list, status: 200, factory: credFactory,
			has: []string{id(credCarol), id(credPending), id(credBob)}, hasNot: []string{id(credErin)}},
		{name: "an outsider sees none", who: "frank", url: list, status: 200, factory: credFactory, has: []string{`"totalItems":0`}},
		{name: "the revoke reason is hidden", who: "carol", url: list, status: 200, factory: credFactory, hasNot: []string{"revokedReason"}},
		{name: "types: a member of a parish sees the diocese type", who: "carol",
			url: "/api/collections/_credentialTypes/records?perPage=50", status: 200, factory: credFactory,
			has: []string{`"key":"safe_environment"`, `"key":"strict_check"`}},
		{name: "types: another org sees only its own tree", who: "erin",
			url: "/api/collections/_credentialTypes/records?perPage=50", status: 200, factory: credFactory,
			has: []string{`"totalItems":0`}},
		{name: "no writes through the records API", who: "bob", method: http.MethodPost, status: 403, factory: credFactory,
			url:  "/api/collections/_credentials/records",
			body: `{"user":"` + orgUserID("dave") + `","org":"` + orgP1 + `","type":"safe_environment","expires":"2098-01-01 00:00:00.000Z"}`},
	})
}

func TestCredentialGatedCollection(t *testing.T) {
	t.Parallel()

	factory := func(t testing.TB) *tests.TestApp {
		app := credFactory(t)

		set, err := core.CredentialRuleSet("org", core.CredentialRuleOpts{Type: "safe_environment", Scope: core.CredentialScopeAncestors})
		orgMust(t, err)
		seedDocs(t, app, "docs", "org", false, set)

		return app
	}

	list := "/api/collections/docs/records?perPage=50"

	runOrgCases(t, []orgCase{
		{name: "a member with a current credential reads", who: "carol", url: list, status: 200, factory: factory,
			has: []string{`"id":"` + docP1 + `"`}, hasNot: []string{`"id":"` + docP2 + `"`}},
		{name: "a member without one sees nothing", who: "alice", url: list, status: 200, factory: factory,
			has: []string{`"totalItems":0`}},
		{name: "a credential of another org does not open this one", who: "erin", url: list, status: 200, factory: factory,
			has: []string{`"id":"` + docP2 + `"`}, hasNot: []string{`"id":"` + docP1 + `"`}},
		{name: "view is gated too", who: "alice", url: "/api/collections/docs/records/" + docP1, status: 404, factory: factory},
		{name: "view with a credential", who: "carol", url: "/api/collections/docs/records/" + docP1, status: 200, factory: factory},
		{name: "the rule templates route serves the gated set", who: "super", status: 200,
			url:     "/api/orgs/rule-templates?credentialType=safe_environment&credentialScope=ancestors&credentialMaxDepth=2",
			has:     []string{`credentialGated`, `_credentials:c.expires ?> @now`, `_orgClosure:q.depth ?<= 2`},
			factory: factory},
		{name: "a bad type is refused", who: "super", status: 400, factory: factory,
			url: "/api/orgs/rule-templates?credentialType=" + strings.Repeat("x", 80)},
	})
}

// credentialUpload builds a multipart body with a small PDF.
func credentialUpload(t testing.TB) (*bytes.Buffer, string) {
	t.Helper()

	body := new(bytes.Buffer)
	mp := multipart.NewWriter(body)

	for k, v := range map[string]string{"user": orgUserID("dave"), "type": "safe_environment", "expires": "2098-01-31"} {
		orgMust(t, mp.WriteField(k, v))
	}

	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="document"; filename="certificate.pdf"`)
	h.Set("Content-Type", "application/pdf")

	w, err := mp.CreatePart(h)
	orgMust(t, err)
	_, err = w.Write([]byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n"))
	orgMust(t, err)
	orgMust(t, mp.Close())

	return body, mp.FormDataContentType()
}

func TestCredentialDocuments(t *testing.T) {
	t.Parallel()

	url := "/api/orgs/" + orgP1 + "/credentials"

	run := func(name string, enabled bool, status int, content ...string) {
		body, ctype := credentialUpload(t)

		(&tests.ApiScenario{
			Name: name, Method: http.MethodPost, URL: url, Body: body,
			Headers:         map[string]string{"Authorization": orgToken(t, "bob"), "Content-Type": ctype},
			ExpectedStatus:  status,
			ExpectedContent: content,
			TestAppFactory: func(t testing.TB) *tests.TestApp {
				app := credFactory(t)

				if enabled {
					org, err := app.FindRecordById(core.CollectionNameOrgs, orgP1)
					orgMust(t, err)
					org.Set("settings", `{"credentialDocuments":true}`)
					orgMust(t, app.Save(org))
				}

				return app
			},
		}).Test(t)
	}

	run("a document is refused while the org setting is off (the default)", false, 400, `"message"`)
	run("a document is stored when the org allows it", true, 200, `"document":"certificate`)
}

func TestCredentialBlocksOrgDeleteAPI(t *testing.T) {
	t.Parallel()

	seed := func(t testing.TB, app *tests.TestApp) {
		seedCredential(t, app, "credarchivedaa1", "hank", orgA, "safe_environment", "2099-01-01 00:00:00.000Z", true, "")
	}

	runOrgCases(t, []orgCase{
		{
			name: "an org with credentials cannot be deleted", who: "hank", method: "DELETE", url: "/api/orgs/" + orgA,
			body: `{"confirm":"closed"}`, status: 400, has: []string{"safeguarding credentials"},
			before: seed, factory: credFactory,
		},
		{
			name: "the guard also covers a direct record delete", who: "hank", method: "GET", url: "/api/orgs/" + orgA, status: 200,
			factory: credFactory,
			before: func(t testing.TB, app *tests.TestApp) {
				seed(t, app)

				org, err := app.FindRecordById(core.CollectionNameOrgs, orgA)
				orgMust(t, err)

				if err := app.Delete(org); err == nil {
					t.Fatal("expected the direct delete to be refused")
				}
			},
		},
	})
}
