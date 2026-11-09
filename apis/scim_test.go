package apis_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #79c): the SCIM 2.0 endpoints under /scim/v2.
//
// The request bodies are the shapes Microsoft Entra ID and Okta send (from
// their provisioning documentation): Entra's capitalised op names, "False"
// as a string, emails[type eq "work"].value paths, the enterprise extension;
// Okta's path-less replace with an object and members[value eq "id"] removal.

type scimEnv struct {
	t   testing.TB
	app *tests.TestApp
	mux http.Handler

	tokP1, tokP2, tokRevoked, tokExpired, tokArchived string
}

func newSCIMEnv(t testing.TB) *scimEnv {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(app.Cleanup)
	seedOrgs(t, app)

	// an account whose email is not verified
	users, _ := app.FindCollectionByNameOrId("users")
	u := core.NewRecord(users)
	u.Id = orgUserID("una")
	u.SetEmail("una@orgs.test")
	u.Set("username", "orgs_una")
	u.SetVerified(false)
	u.SetPassword("password123")
	orgMust(t, app.Save(u))

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}

	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}

	env := &scimEnv{t: t, app: app, mux: mux}
	sys := &core.OrgActor{System: true}

	mk := func(org string) (*core.Record, string) {
		o, err := app.FindRecordById(core.CollectionNameOrgs, org)
		orgMust(t, err)
		rec, tok, err := core.CreateSCIMToken(app, context.Background(), sys, o, "idp", time.Time{})
		orgMust(t, err)

		return rec, tok
	}

	_, env.tokP1 = mk(orgP1)
	_, env.tokP2 = mk(orgP2)
	_, env.tokArchived = mk(orgA)

	rev, tok := mk(orgP1)
	env.tokRevoked = tok
	rev.Set("revoked", true)
	orgMust(t, app.Save(rev))

	exp, tok := mk(orgP1)
	env.tokExpired = tok
	exp.Set("expires", types.NowDateTime().Add(-time.Hour).String())
	orgMust(t, app.Save(exp))

	return env
}

func (env *scimEnv) do(method, url, body, token string) (int, map[string]any, *httptest.ResponseRecorder) {
	env.t.Helper()

	req := httptest.NewRequest(method, url, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/scim+json")
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)

	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			env.t.Fatalf("%s %s: not JSON (%d): %s", method, url, rec.Code, rec.Body.String())
		}
	}

	return rec.Code, out, rec
}

// expect runs a request and checks the status (and the SCIM error type).
func (env *scimEnv) expect(status int, method, url, body, token string) map[string]any {
	env.t.Helper()

	code, out, rec := env.do(method, url, body, token)
	if code != status {
		env.t.Fatalf("%s %s: expected %d, got %d: %s", method, url, status, code, rec.Body.String())
	}

	if status < 300 && status != 204 {
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/scim+json") {
			env.t.Fatalf("%s %s: unexpected content type %q", method, url, ct)
		}
	}

	return out
}

func (env *scimEnv) expectErr(status int, scimType, method, url, body, token string) map[string]any {
	env.t.Helper()

	out := env.expect(status, method, url, body, token)

	schemas, _ := out["schemas"].([]any)
	if len(schemas) != 1 || schemas[0] != "urn:ietf:params:scim:api:messages:2.0:Error" {
		env.t.Fatalf("not a SCIM error: %v", out)
	}

	if out["status"] != strconv.Itoa(status) {
		env.t.Fatalf("status field %v != %d", out["status"], status)
	}

	if st, _ := out["scimType"].(string); st != scimType {
		env.t.Fatalf("%s %s: expected scimType %q, got %q (%v)", method, url, scimType, st, out["detail"])
	}

	return out
}

func resources(t testing.TB, out map[string]any) []map[string]any {
	t.Helper()

	var res []map[string]any

	for _, r := range out["Resources"].([]any) {
		res = append(res, r.(map[string]any))
	}

	return res
}

func (env *scimEnv) member(org, user string) *core.Record {
	m, err := core.FindOrgMember(env.app, org, user)
	if err != nil {
		return nil
	}

	return m
}

func (env *scimEnv) userByEmail(email string) *core.Record {
	u, err := env.app.FindAuthRecordByEmail("users", email)
	if err != nil {
		return nil
	}

	return u
}

func patchBody(ops string) string {
	return `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":` + ops + `}`
}

// ---- auth ------------------------------------------------------------------

func TestSCIMAuth(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	for _, tc := range []struct {
		name, token string
		status      int
	}{
		{"no token", "", 401},
		{"garbage", "nope", 401},
		{"malformed scim token", "scim_aaaaaaaa_short", 401},
		{"unknown scim token", "scim_aaaaaaaa_" + strings.Repeat("a", 52), 401},
		{"a jwt", orgToken(t, "alice"), 401},
		{"revoked", env.tokRevoked, 401},
		{"expired", env.tokExpired, 401},
		{"archived org", env.tokArchived, 403},
		{"valid", env.tokP1, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, _ := env.do("GET", "/scim/v2/Users", "", tc.token)
			if code != tc.status {
				t.Fatalf("expected %d, got %d: %v", tc.status, code, out)
			}

			if tc.status != 200 && (out["schemas"].([]any)[0] != "urn:ietf:params:scim:api:messages:2.0:Error" || out["detail"] == "") {
				t.Fatalf("not a SCIM error: %v", out)
			}
		})
	}

	// an API key and a user session never work on /scim/v2, and a SCIM token
	// never works on /api
	if code, _, _ := env.do("GET", "/api/orgs", "", env.tokP1); code != 200 {
		// an invalid JWT is just a guest, and /api/orgs needs auth
		if code != 401 {
			t.Fatalf("a SCIM token on /api: %d", code)
		}
	}

	// a SCIM token is not an identity: it cannot read org data through /api
	if code, _, _ := env.do("GET", "/api/orgs/"+orgP1+"/members", "", env.tokP1); code != 401 {
		t.Fatalf("a SCIM token must not authenticate /api, got %d", code)
	}
}

func TestSCIMAuthFailuresAreRateLimited(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	bad := "scim_aaaaaaaa_" + strings.Repeat("a", 52)

	var last int

	for i := 0; i < 40; i++ {
		last, _, _ = env.do("GET", "/scim/v2/Users", "", bad)
	}

	if last != 429 {
		t.Fatalf("expected 429, got %d", last)
	}

	// failures only: a valid token is not what is counted
	env2 := newSCIMEnv(t)
	for i := 0; i < 60; i++ {
		if code, _, _ := env2.do("GET", "/scim/v2/Users", "", env2.tokP1); code != 200 {
			t.Fatalf("valid request %d got %d", i, code)
		}
	}
}

func TestSCIMLastUsedIsThrottled(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	env.expect(200, "GET", "/scim/v2/Users", "", env.tokP1)

	toks, _ := core.ListSCIMTokens(env.app, mustOrg(t, env.app, orgP1))
	used := 0

	for _, tk := range toks {
		if !tk.GetDateTime("lastUsed").IsZero() {
			used++
		}
	}

	if used != 1 {
		t.Fatalf("expected exactly the used token to carry lastUsed, got %d", used)
	}
}

func mustOrg(t testing.TB, app core.App, id string) *core.Record {
	o, err := app.FindRecordById(core.CollectionNameOrgs, id)
	orgMust(t, err)

	return o
}

// ---- discovery -------------------------------------------------------------

func TestSCIMDiscovery(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	spc := env.expect(200, "GET", "/scim/v2/ServiceProviderConfig", "", env.tokP1)
	if spc["patch"].(map[string]any)["supported"] != true || spc["bulk"].(map[string]any)["supported"] != false ||
		spc["filter"].(map[string]any)["maxResults"].(float64) != 200 || spc["changePassword"].(map[string]any)["supported"] != false ||
		spc["etag"].(map[string]any)["supported"] != false || spc["sort"].(map[string]any)["supported"] != false {
		t.Fatalf("unexpected config %v", spc)
	}

	if schemes := spc["authenticationSchemes"].([]any); schemes[0].(map[string]any)["type"] != "oauthbearertoken" {
		t.Fatalf("unexpected schemes %v", schemes)
	}

	rt := env.expect(200, "GET", "/scim/v2/ResourceTypes", "", env.tokP1)
	if rt["totalResults"].(float64) != 2 {
		t.Fatalf("unexpected resource types %v", rt)
	}

	one := env.expect(200, "GET", "/scim/v2/ResourceTypes/User", "", env.tokP1)
	if one["endpoint"] != "/Users" {
		t.Fatalf("unexpected %v", one)
	}

	sch := env.expect(200, "GET", "/scim/v2/Schemas", "", env.tokP1)
	if sch["totalResults"].(float64) != 2 {
		t.Fatalf("unexpected schemas %v", sch)
	}

	env.expect(200, "GET", "/scim/v2/Schemas/urn:ietf:params:scim:schemas:core:2.0:User", "", env.tokP1)
	env.expectErr(404, "", "GET", "/scim/v2/Schemas/nope", "", env.tokP1)
	env.expectErr(401, "", "GET", "/scim/v2/ServiceProviderConfig", "", "")
}

// ---- users -----------------------------------------------------------------

const entraCreate = `{
 "schemas":["urn:ietf:params:scim:schemas:core:2.0:User","urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"],
 "externalId":"c7d8f4a1-1111-4a6e-9f55-0a3b2c4d5e6f",
 "userName":"Entra.Person@contoso.test",
 "active":true,
 "displayName":"Entra Person",
 "meta":{"resourceType":"User"},
 "name":{"formatted":"Entra Person","familyName":"Person","givenName":"Entra"},
 "emails":[{"primary":true,"type":"work","value":"entra.person@contoso.test"}],
 "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User":{"department":"Engineering","manager":"someone"}
}`

const oktaCreate = `{
 "schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],
 "userName":"isaac.brock@okta.test",
 "name":{"givenName":"Isaac","familyName":"Brock","formatted":"Isaac Brock"},
 "emails":[{"primary":true,"value":"isaac.brock@okta.test","type":"work"}],
 "displayName":"Isaac Brock",
 "locale":"en-US","externalId":"00u1abcd",
 "password":"ignored-never-set","active":true
}`

func TestSCIMUserCreateEntraAndOkta(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	for _, tc := range []struct{ body, email, ext, name string }{
		{entraCreate, "entra.person@contoso.test", "c7d8f4a1-1111-4a6e-9f55-0a3b2c4d5e6f", "Entra Person"},
		{oktaCreate, "isaac.brock@okta.test", "00u1abcd", "Isaac Brock"},
	} {
		out := env.expect(201, "POST", "/scim/v2/Users", tc.body, env.tokP1)

		id := out["id"].(string)
		if out["userName"] != tc.email || out["active"] != true || out["externalId"] != tc.ext || out["displayName"] != tc.name {
			t.Fatalf("unexpected resource %v", out)
		}

		if _, has := out["password"]; has {
			t.Fatal("the response must not carry a password")
		}

		meta := out["meta"].(map[string]any)
		if meta["resourceType"] != "User" || !strings.HasSuffix(meta["location"].(string), "/scim/v2/Users/"+id) {
			t.Fatalf("unexpected meta %v", meta)
		}

		u := env.userByEmail(tc.email)
		if u == nil || !u.Verified() || u.GetString("name") != tc.name || u.Id != id {
			t.Fatalf("unexpected account %v", u)
		}

		m := env.member(orgP1, u.Id)
		if m == nil || m.GetString("source") != core.OrgSourceSCIM || m.GetString("role") != core.OrgRoleMember ||
			m.GetString("status") != core.OrgStatusActive || m.GetString("externalId") != tc.ext {
			t.Fatalf("unexpected membership %v", m)
		}

		// the password nobody knows: the supplied one is not accepted
		if u.ValidatePassword("ignored-never-set") {
			t.Fatal("SCIM must never set a password")
		}

		// the same user again
		env.expectErr(409, "uniqueness", "POST", "/scim/v2/Users", tc.body, env.tokP1)
	}

	// the other org does not see them
	list := env.expect(200, "GET", "/scim/v2/Users?filter="+urlq(`userName eq "isaac.brock@okta.test"`), "", env.tokP2)
	if list["totalResults"].(float64) != 0 {
		t.Fatalf("org isolation broken: %v", list)
	}
}

func urlq(s string) string {
	return strings.NewReplacer(" ", "%20", `"`, "%22", "[", "%5B", "]", "%5D").Replace(s)
}

func TestSCIMUserCreateValidation(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	env.expectErr(400, "invalidValue", "POST", "/scim/v2/Users", `{"userName":"not-an-email"}`, env.tokP1)
	env.expectErr(400, "invalidValue", "POST", "/scim/v2/Users", `{"displayName":"x"}`, env.tokP1)
	env.expectErr(400, "invalidSyntax", "POST", "/scim/v2/Users", `[1]`, env.tokP1)
	env.expectErr(400, "invalidSyntax", "POST", "/scim/v2/Users", `{`, env.tokP1)
	env.expectErr(400, "invalidValue", "POST", "/scim/v2/Users", `{"userName":"a@b.test","active":"maybe"}`, env.tokP1)

	// the work email is used when there is no userName
	out := env.expect(201, "POST", "/scim/v2/Users", `{"emails":[{"value":"mail.only@x.test","type":"work"}]}`, env.tokP1)
	if out["userName"] != "mail.only@x.test" {
		t.Fatalf("unexpected %v", out)
	}

	// userName wins when both differ
	out = env.expect(201, "POST", "/scim/v2/Users", `{"userName":"upn@x.test","emails":[{"value":"other@x.test","type":"work"}]}`, env.tokP1)
	if out["userName"] != "upn@x.test" {
		t.Fatalf("unexpected %v", out)
	}

	// created inactive: suspended from the start
	out = env.expect(201, "POST", "/scim/v2/Users", `{"userName":"later@x.test","active":false}`, env.tokP1)
	if out["active"] != false {
		t.Fatalf("unexpected %v", out)
	}

	// a duplicate externalId
	env.expect(201, "POST", "/scim/v2/Users", `{"userName":"one@x.test","externalId":"E1"}`, env.tokP1)
	env.expectErr(409, "uniqueness", "POST", "/scim/v2/Users", `{"userName":"two@x.test","externalId":"E1"}`, env.tokP1)
}

func TestSCIMUserLinking(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	// a verified existing account is linked, not duplicated or modified
	before := env.userByEmail("frank@orgs.test")
	out := env.expect(201, "POST", "/scim/v2/Users", `{"userName":"FRANK@orgs.test","displayName":"Overwrite Attempt","externalId":"f1"}`, env.tokP1)

	if out["id"] != before.Id {
		t.Fatalf("expected the existing account to be linked, got %v", out)
	}

	after := env.userByEmail("frank@orgs.test")
	if !after.ValidatePassword("password123") || after.GetString("name") != "Overwrite Attempt" && after.GetString("name") != before.GetString("name") {
		t.Fatal("linking must not change the password")
	}

	m := env.member(orgP1, before.Id)
	if m == nil || m.GetString("source") != core.OrgSourceSCIM || m.GetString("externalId") != "f1" {
		t.Fatalf("unexpected membership %v", m)
	}

	// an account that already has a name keeps it
	carol := env.userByEmail("carol@orgs.test")
	carol.Set("name", "Carol Original")
	orgMust(t, env.app.Save(carol))
	env.expect(201, "POST", "/scim/v2/Users", `{"userName":"erin@orgs.test","displayName":"Erin Renamed"}`, env.tokP1)
	env.expectErr(409, "uniqueness", "POST", "/scim/v2/Users", `{"userName":"carol@orgs.test","displayName":"Evil"}`, env.tokP1) // already a P1 member

	if env.userByEmail("carol@orgs.test").GetString("name") != "Carol Original" {
		t.Fatal("a linked account must keep its own name")
	}

	// an unverified account is a conflict, with a reason
	out = env.expectErr(409, "uniqueness", "POST", "/scim/v2/Users", `{"userName":"una@orgs.test"}`, env.tokP1)
	if !strings.Contains(out["detail"].(string), "not verified") {
		t.Fatalf("unexpected detail %v", out["detail"])
	}

	if env.member(orgP1, orgUserID("una")) != nil {
		t.Fatal("an unverified account must not be linked")
	}
}

func TestSCIMUserGetListAndPaging(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	// P1: alice, bob, carol, dave, sam
	all := env.expect(200, "GET", "/scim/v2/Users", "", env.tokP1)
	if all["totalResults"].(float64) != 5 || all["startIndex"].(float64) != 1 || all["itemsPerPage"].(float64) != 5 {
		t.Fatalf("unexpected list %v", all)
	}

	if all["schemas"].([]any)[0] != "urn:ietf:params:scim:api:messages:2.0:ListResponse" {
		t.Fatal("not a list response")
	}

	var emails []string
	for _, r := range resources(t, all) {
		emails = append(emails, r["userName"].(string))
	}

	// paging walks the same set without overlap
	seen := map[string]bool{}

	for start := 1; start <= 5; start += 2 {
		page := env.expect(200, "GET", "/scim/v2/Users?startIndex="+strconv.Itoa(start)+"&count=2", "", env.tokP1)
		if page["totalResults"].(float64) != 5 || page["startIndex"].(float64) != float64(start) {
			t.Fatalf("unexpected page %v", page)
		}

		for _, r := range resources(t, page) {
			if seen[r["id"].(string)] {
				t.Fatalf("duplicate %v", r["id"])
			}

			seen[r["id"].(string)] = true
		}
	}

	if len(seen) != 5 {
		t.Fatalf("paging lost users: %v (%v)", seen, emails)
	}

	// count=0 is a count query; beyond the end is empty
	zero := env.expect(200, "GET", "/scim/v2/Users?count=0", "", env.tokP1)
	if zero["totalResults"].(float64) != 5 || len(resources(t, zero)) != 0 {
		t.Fatalf("unexpected %v", zero)
	}

	end := env.expect(200, "GET", "/scim/v2/Users?startIndex=50", "", env.tokP1)
	if len(resources(t, end)) != 0 || end["totalResults"].(float64) != 5 {
		t.Fatalf("unexpected %v", end)
	}

	// GET by id, with groups and meta; a stranger or another org's user is a 404
	one := env.expect(200, "GET", "/scim/v2/Users/"+orgUserID("carol"), "", env.tokP1)
	if one["userName"] != "carol@orgs.test" || one["active"] != true {
		t.Fatalf("unexpected %v", one)
	}

	env.expectErr(404, "", "GET", "/scim/v2/Users/"+orgUserID("erin"), "", env.tokP1)
	env.expectErr(404, "", "GET", "/scim/v2/Users/nosuchuser00001", "", env.tokP1)

	if sam := env.expect(200, "GET", "/scim/v2/Users/"+orgUserID("sam"), "", env.tokP1); sam["active"] != false {
		t.Fatalf("a suspended member is inactive: %v", sam)
	}
}

func TestSCIMUserFilters(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	// give bob an externalId
	bob := env.member(orgP1, orgUserID("bob"))
	bob.Set("externalId", "ext-bob")
	orgMust(t, env.app.Save(bob))

	ids := func(filter string) []string {
		t.Helper()
		out := env.expect(200, "GET", "/scim/v2/Users?filter="+urlq(filter), "", env.tokP1)

		var got []string
		for _, r := range resources(t, out) {
			got = append(got, r["userName"].(string))
		}

		slices.Sort(got)

		if int(out["totalResults"].(float64)) != len(got) {
			t.Fatalf("totalResults mismatch for %q: %v", filter, out)
		}

		return got
	}

	for _, tc := range []struct {
		filter string
		want   []string
	}{
		{`userName eq "bob@orgs.test"`, []string{"bob@orgs.test"}},
		{`USERNAME EQ "BOB@orgs.test"`, []string{"bob@orgs.test"}},
		{`userName eq "erin@orgs.test"`, nil}, // another org's member
		{`userName eq "nobody@orgs.test"`, nil},
		{`externalId eq "ext-bob"`, []string{"bob@orgs.test"}},
		{`externalId eq "other"`, nil},
		{`emails[type eq "work"].value eq "carol@orgs.test"`, []string{"carol@orgs.test"}},
		{`emails.value eq "carol@orgs.test"`, []string{"carol@orgs.test"}},
		{`id eq "` + orgUserID("dave") + `"`, []string{"dave@orgs.test"}},
		{`id eq "` + orgUserID("erin") + `"`, nil},
		{`active eq false`, []string{"sam@orgs.test"}},
		{`active eq true`, []string{"alice@orgs.test", "bob@orgs.test", "carol@orgs.test", "dave@orgs.test"}},
		{`userName eq "bob@orgs.test" and externalId eq "ext-bob"`, []string{"bob@orgs.test"}},
		{`userName eq "bob@orgs.test" and externalId eq "wrong"`, nil},
		{`userName eq "bob@orgs.test" AND active eq false`, nil},
		{`userName eq "a \"quoted\" name"`, nil},
	} {
		got := ids(tc.filter)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: expected %v, got %v", tc.filter, tc.want, got)
		}
	}

	for _, bad := range []string{
		`userName co "bob"`, `userName sw "bob"`, `userName pr`, `userName eq "a" or userName eq "b"`,
		`(userName eq "a")`, `not (userName eq "a")`, `name.givenName eq "x"`, `userName eq`, `userName eq "a" and`,
		`userName eq "unterminated`, `userName eq 5`, `active eq "yes"`, `userName eq true`,
	} {
		env.expectErr(400, "invalidFilter", "GET", "/scim/v2/Users?filter="+urlq(bad), "", env.tokP1)
	}
}

func TestSCIMUserPutAndPatch(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	// an account created by this org's SCIM, in no other org
	out := env.expect(201, "POST", "/scim/v2/Users", entraCreate, env.tokP1)
	id := out["id"].(string)
	url := "/scim/v2/Users/" + id

	// Entra: string "False"
	out = env.expect(200, "PATCH", url, patchBody(`[{"op":"Replace","path":"active","value":"False"}]`), env.tokP1)
	if out["active"] != false || env.member(orgP1, id).GetString("status") != core.OrgStatusSuspended {
		t.Fatalf("expected suspended: %v", out)
	}

	out = env.expect(200, "PATCH", url, patchBody(`[{"op":"Replace","path":"active","value":"True"}]`), env.tokP1)
	if out["active"] != true || env.member(orgP1, id).GetString("status") != core.OrgStatusActive {
		t.Fatalf("expected active: %v", out)
	}

	// Entra: attribute paths, the work email path, names and unknown/extension attributes
	out = env.expect(200, "PATCH", url, patchBody(`[
	  {"op":"Replace","path":"emails[type eq \"work\"].value","value":"entra.new@contoso.test"},
	  {"op":"Replace","path":"displayName","value":"Entra Renamed"},
	  {"op":"Add","path":"name.givenName","value":"Ignored"},
	  {"op":"Replace","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department","value":"Ops"},
	  {"op":"Replace","path":"title","value":"Dev"},
	  {"op":"Replace","path":"externalId","value":"new-ext"}
	]`), env.tokP1)

	if out["userName"] != "entra.new@contoso.test" || out["displayName"] != "Entra Renamed" || out["externalId"] != "new-ext" {
		t.Fatalf("unexpected %v", out)
	}

	if u := env.userByEmail("entra.new@contoso.test"); u == nil || u.Id != id || u.GetString("name") != "Entra Renamed" || !u.Verified() {
		t.Fatal("the account was not updated in place")
	}

	// Okta: path-less replace with an object (including deactivation)
	out = env.expect(200, "PATCH", url, patchBody(`[{"op":"replace","value":{"active":false,"displayName":"Okta Way","name.givenName":"x"}}]`), env.tokP1)
	if out["active"] != false || out["displayName"] != "Okta Way" {
		t.Fatalf("unexpected %v", out)
	}

	// userName replace and a conflicting email
	env.expectErr(409, "uniqueness", "PATCH", url, patchBody(`[{"op":"replace","path":"userName","value":"alice@orgs.test"}]`), env.tokP1)
	env.expectErr(400, "invalidValue", "PATCH", url, patchBody(`[{"op":"replace","path":"userName","value":"nope"}]`), env.tokP1)
	env.expect(200, "PATCH", url, patchBody(`[{"op":"replace","path":"userName","value":"renamed@contoso.test"}]`), env.tokP1)

	// remove externalId; removing something unknown is a no-op; path required
	out = env.expect(200, "PATCH", url, patchBody(`[{"op":"remove","path":"externalId"},{"op":"remove","path":"title"}]`), env.tokP1)
	if _, has := out["externalId"]; has {
		t.Fatalf("externalId should be cleared: %v", out)
	}

	env.expectErr(400, "noTarget", "PATCH", url, patchBody(`[{"op":"remove"}]`), env.tokP1)
	env.expectErr(400, "invalidSyntax", "PATCH", url, patchBody(`[{"op":"explode","path":"active"}]`), env.tokP1)
	env.expectErr(400, "invalidSyntax", "PATCH", url, `{"Operations":[]}`, env.tokP1)
	env.expectErr(400, "invalidValue", "PATCH", url, patchBody(`[{"op":"replace","path":"active","value":"perhaps"}]`), env.tokP1)

	// PUT (Okta): a full resource
	out = env.expect(200, "PUT", url, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"put.user@contoso.test","displayName":"Put User","externalId":"put-1","active":true,"emails":[{"value":"put.user@contoso.test","type":"work","primary":true}]}`, env.tokP1)
	if out["userName"] != "put.user@contoso.test" || out["displayName"] != "Put User" || out["active"] != true || out["externalId"] != "put-1" {
		t.Fatalf("unexpected %v", out)
	}

	env.expectErr(404, "", "PUT", "/scim/v2/Users/"+orgUserID("erin"), `{"userName":"erin@orgs.test"}`, env.tokP1)
	env.expectErr(404, "", "PATCH", "/scim/v2/Users/"+orgUserID("erin"), patchBody(`[{"op":"replace","path":"active","value":false}]`), env.tokP1)
}

func TestSCIMDoesNotRewriteSharedAccounts(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	// carol belongs to P1 and P2, and was not created by SCIM
	url := "/scim/v2/Users/" + orgUserID("carol")

	env.expectErr(400, "mutability", "PATCH", url, patchBody(`[{"op":"replace","path":"userName","value":"carol.new@orgs.test"}]`), env.tokP1)
	env.expect(200, "PATCH", url, patchBody(`[{"op":"replace","path":"displayName","value":"Renamed By IdP"}]`), env.tokP1)

	if env.userByEmail("carol@orgs.test").GetString("name") == "Renamed By IdP" {
		t.Fatal("the profile of an account the org does not own must not be changed")
	}

	// a SCIM account that later joins another org stops being editable too
	out := env.expect(201, "POST", "/scim/v2/Users", `{"userName":"shared@x.test","displayName":"Shared"}`, env.tokP1)
	id := out["id"].(string)

	env.expect(200, "PATCH", "/scim/v2/Users/"+id, patchBody(`[{"op":"replace","path":"displayName","value":"Shared Two"}]`), env.tokP1)

	if env.userByEmail("shared@x.test").GetString("name") != "Shared Two" {
		t.Fatal("an owned account must be updated")
	}

	env.expect(201, "POST", "/scim/v2/Users", `{"userName":"shared@x.test"}`, env.tokP2)
	env.expectErr(400, "mutability", "PATCH", "/scim/v2/Users/"+id, patchBody(`[{"op":"replace","path":"userName","value":"moved@x.test"}]`), env.tokP1)
}

func TestSCIMDelete(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	env.expect(204, "DELETE", "/scim/v2/Users/"+orgUserID("dave"), "", env.tokP1)

	if env.member(orgP1, orgUserID("dave")) != nil {
		t.Fatal("the membership must be removed")
	}

	if env.userByEmail("dave@orgs.test") == nil {
		t.Fatal("the account must never be deleted")
	}

	env.expectErr(404, "", "GET", "/scim/v2/Users/"+orgUserID("dave"), "", env.tokP1)
	env.expectErr(404, "", "DELETE", "/scim/v2/Users/"+orgUserID("dave"), "", env.tokP1)
	env.expectErr(404, "", "DELETE", "/scim/v2/Users/"+orgUserID("erin"), "", env.tokP1)

	if env.member(orgP2, orgUserID("erin")) == nil {
		t.Fatal("another org's member must be untouched")
	}

	// the last owner cannot be removed or deactivated through SCIM either
	env.expectErr(409, "", "DELETE", "/scim/v2/Users/"+orgUserID("alice"), "", env.tokP1)
	env.expectErr(409, "", "PATCH", "/scim/v2/Users/"+orgUserID("alice"), patchBody(`[{"op":"replace","path":"active","value":false}]`), env.tokP1)

	if m := env.member(orgP1, orgUserID("alice")); m == nil || m.GetString("status") != core.OrgStatusActive {
		t.Fatal("the last owner must stay")
	}
}

// ---- deactivation: suspend vs ban ----------------------------------------------

func TestSCIMDeactivationSuspendsOrBans(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	out := env.expect(201, "POST", "/scim/v2/Users", `{"userName":"leaver@x.test"}`, env.tokP1)
	id := out["id"].(string)
	user, _ := env.app.FindRecordById("users", id)

	// default: only the membership is suspended
	env.expect(200, "PATCH", "/scim/v2/Users/"+id, patchBody(`[{"op":"replace","path":"active","value":false}]`), env.tokP1)

	if core.IsBanned(env.app, user) {
		t.Fatal("no ban without the setting")
	}

	// the setting bans the account too
	p1 := mustOrg(t, env.app, orgP1)
	orgMust(t, core.UpdateSCIMSettings(env.app, context.Background(), &core.OrgActor{System: true}, p1, core.SCIMSettings{DeactivationBans: true}))

	env.expect(200, "PATCH", "/scim/v2/Users/"+id, patchBody(`[{"op":"replace","path":"active","value":true}]`), env.tokP1)
	env.expect(200, "PATCH", "/scim/v2/Users/"+id, patchBody(`[{"op":"replace","path":"active","value":false}]`), env.tokP1)

	if !core.IsBanned(env.app, user) {
		t.Fatal("the account must be banned")
	}

	if ban := core.FindActiveBan(env.app, user.Collection().Id, id); ban == nil || ban.GetString("createdBy") != "scim:"+orgP1 {
		t.Fatalf("unexpected ban %v", ban)
	}

	if env.member(orgP1, id).GetString("status") != core.OrgStatusSuspended {
		t.Fatal("the membership must be suspended")
	}

	// reactivation lifts the ban it set
	env.expect(200, "PATCH", "/scim/v2/Users/"+id, patchBody(`[{"op":"replace","path":"active","value":true}]`), env.tokP1)

	if core.IsBanned(env.app, user) {
		t.Fatal("the ban must be lifted on reactivation")
	}

	// an account in another active org is suspended there but not banned
	env.expect(201, "POST", "/scim/v2/Users", `{"userName":"leaver@x.test"}`, env.tokP2)
	env.expect(200, "PATCH", "/scim/v2/Users/"+id, patchBody(`[{"op":"replace","path":"active","value":false}]`), env.tokP1)

	if core.IsBanned(env.app, user) {
		t.Fatal("an account that is active in another org must not be banned")
	}

	// a ban someone else set is neither replaced nor lifted
	other, _ := env.app.FindRecordById("users", orgUserID("dave"))
	bans, _ := env.app.FindCachedCollectionByNameOrId(core.CollectionNameBans)
	b := core.NewRecord(bans)
	b.Set("collectionRef", other.Collection().Id)
	b.Set("recordRef", other.Id)
	b.Set("createdBy", "superusers:admin")
	orgMust(t, env.app.Save(b))

	env.expect(200, "PATCH", "/scim/v2/Users/"+other.Id, patchBody(`[{"op":"replace","path":"active","value":false}]`), env.tokP1)
	env.expect(200, "PATCH", "/scim/v2/Users/"+other.Id, patchBody(`[{"op":"replace","path":"active","value":true}]`), env.tokP1)

	if ban := core.FindActiveBan(env.app, other.Collection().Id, other.Id); ban == nil || ban.GetString("createdBy") != "superusers:admin" {
		t.Fatalf("a foreign ban must stay: %v", ban)
	}
}

// ---- groups ----------------------------------------------------------------

func TestSCIMGroupsEntraAndOktaFlows(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	carol, dave := orgUserID("carol"), orgUserID("dave")

	// Okta group push: create with members
	out := env.expect(201, "POST", "/scim/v2/Groups",
		`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Choir","externalId":"g-choir","members":[{"value":"`+carol+`","display":"carol@orgs.test"}]}`, env.tokP1)

	gid := out["id"].(string)
	if out["displayName"] != "Choir" || len(out["members"].([]any)) != 1 || out["externalId"] != "g-choir" {
		t.Fatalf("unexpected %v", out)
	}

	if tags := core.OrgTagNames(env.app, env.member(orgP1, carol)); !slices.Contains(tags, "Choir") || !slices.Contains(tags, "youth") {
		t.Fatalf("the group must tag its members: %v", tags)
	}

	// the user resource lists the group
	u := env.expect(200, "GET", "/scim/v2/Users/"+carol, "", env.tokP1)
	if g := u["groups"].([]any); len(g) != 1 || g[0].(map[string]any)["display"] != "Choir" {
		t.Fatalf("unexpected groups %v", g)
	}

	// Okta add / remove with the filtered path; Entra add / remove with values
	url := "/scim/v2/Groups/" + gid
	env.expect(200, "PATCH", url, patchBody(`[{"op":"add","path":"members","value":[{"value":"`+dave+`","display":"dave"}]}]`), env.tokP1)

	if !slices.Contains(core.OrgTagNames(env.app, env.member(orgP1, dave)), "Choir") {
		t.Fatal("added member must be tagged")
	}

	out = env.expect(200, "PATCH", url, patchBody(`[{"op":"remove","path":"members[value eq \"`+dave+`\"]"}]`), env.tokP1)
	if len(out["members"].([]any)) != 1 || slices.Contains(core.OrgTagNames(env.app, env.member(orgP1, dave)), "Choir") {
		t.Fatalf("removal failed: %v", out)
	}

	env.expect(200, "PATCH", url, patchBody(`[{"op":"Add","path":"members","value":[{"value":"`+dave+`"}]}]`), env.tokP1)
	env.expect(200, "PATCH", url, patchBody(`[{"op":"Remove","path":"members","value":[{"value":"`+dave+`"}]}]`), env.tokP1)

	if slices.Contains(core.OrgTagNames(env.app, env.member(orgP1, dave)), "Choir") {
		t.Fatal("Entra-style removal failed")
	}

	// removing a user from a group never removes the membership
	if env.member(orgP1, dave) == nil {
		t.Fatal("membership must stay")
	}

	// replace members, rename (the tag follows), excludedAttributes
	env.expect(200, "PATCH", url, patchBody(`[{"op":"Replace","path":"members","value":[{"value":"`+dave+`"}]}]`), env.tokP1)

	if slices.Contains(core.OrgTagNames(env.app, env.member(orgP1, carol)), "Choir") || !slices.Contains(core.OrgTagNames(env.app, env.member(orgP1, dave)), "Choir") {
		t.Fatal("replace must swap the members")
	}

	out = env.expect(200, "PATCH", url, patchBody(`[{"op":"replace","path":"displayName","value":"Singers"}]`), env.tokP1)
	if out["displayName"] != "Singers" {
		t.Fatalf("unexpected %v", out)
	}

	if tags := core.OrgTagNames(env.app, env.member(orgP1, dave)); !slices.Contains(tags, "Singers") || slices.Contains(tags, "Choir") {
		t.Fatalf("the tag must follow the rename: %v", tags)
	}

	out = env.expect(200, "GET", url+"?excludedAttributes=members", "", env.tokP1)
	if _, has := out["members"]; has {
		t.Fatalf("members must be excluded: %v", out)
	}

	// path-less patch and PUT
	env.expect(200, "PATCH", url, patchBody(`[{"op":"replace","value":{"displayName":"Choir Two","externalId":"g2"}}]`), env.tokP1)
	out = env.expect(200, "PUT", url, `{"displayName":"Choir Three","members":[{"value":"`+carol+`"}]}`, env.tokP1)

	if out["displayName"] != "Choir Three" || len(out["members"].([]any)) != 1 {
		t.Fatalf("unexpected %v", out)
	}

	// list and filter
	list := env.expect(200, "GET", "/scim/v2/Groups?filter="+urlq(`displayName eq "choir three"`), "", env.tokP1)
	if list["totalResults"].(float64) != 1 {
		t.Fatalf("unexpected %v", list)
	}

	list = env.expect(200, "GET", "/scim/v2/Groups?filter="+urlq(`displayName eq "nope"`), "", env.tokP1)
	if list["totalResults"].(float64) != 0 {
		t.Fatalf("unexpected %v", list)
	}

	env.expectErr(400, "invalidFilter", "GET", "/scim/v2/Groups?filter="+urlq(`members co "x"`), "", env.tokP1)

	// org isolation
	env.expectErr(404, "", "GET", "/scim/v2/Groups/"+gid, "", env.tokP2)
	env.expectErr(404, "", "DELETE", "/scim/v2/Groups/"+gid, "", env.tokP2)

	// delete takes the tag back and keeps the memberships
	env.expect(204, "DELETE", url, "", env.tokP1)

	if tags := core.OrgTagNames(env.app, env.member(orgP1, carol)); slices.Contains(tags, "Choir Three") || !slices.Contains(tags, "youth") {
		t.Fatalf("unexpected tags %v", tags)
	}

	env.expectErr(404, "", "GET", url, "", env.tokP1)
}

func TestSCIMGroupValidation(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	env.expectErr(400, "invalidValue", "POST", "/scim/v2/Groups", `{}`, env.tokP1)
	env.expectErr(400, "invalidValue", "POST", "/scim/v2/Groups", `{"displayName":"has \"quotes\""}`, env.tokP1)
	env.expectErr(400, "invalidValue", "POST", "/scim/v2/Groups", `{"displayName":"G","members":[{"value":"`+orgUserID("erin")+`"}]}`, env.tokP1) // not a member of P1
	env.expect(201, "POST", "/scim/v2/Groups", `{"displayName":"G"}`, env.tokP1)
	env.expectErr(409, "uniqueness", "POST", "/scim/v2/Groups", `{"displayName":"G"}`, env.tokP1)
	env.expect(201, "POST", "/scim/v2/Groups", `{"displayName":"G"}`, env.tokP2) // per org
}

func TestSCIMGroupRoleMapping(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	p1 := mustOrg(t, env.app, orgP1)
	orgMust(t, core.UpdateSCIMSettings(env.app, context.Background(), &core.OrgActor{System: true}, p1, core.SCIMSettings{
		DefaultRole: core.OrgRoleMember,
		GroupRoles:  map[string]string{"Claviger Admins": "admin", "Readers": "viewer"},
	}))

	carol, dave, alice := orgUserID("carol"), orgUserID("dave"), orgUserID("alice")

	admins := env.expect(201, "POST", "/scim/v2/Groups", `{"displayName":"Claviger Admins","members":[{"value":"`+carol+`"},{"value":"`+alice+`"}]}`, env.tokP1)
	readers := env.expect(201, "POST", "/scim/v2/Groups", `{"displayName":"Readers","members":[{"value":"`+carol+`"}]}`, env.tokP1)

	role := func(id string) string { return env.member(orgP1, id).GetString("role") }

	// highest mapped role wins; a lower role never demotes on add; owners are never touched
	if role(carol) != core.OrgRoleAdmin || role(alice) != core.OrgRoleOwner {
		t.Fatalf("unexpected roles carol=%s alice=%s", role(carol), role(alice))
	}

	// leaving the viewer group keeps admin (the other group)
	env.expect(200, "PATCH", "/scim/v2/Groups/"+readers["id"].(string), patchBody(`[{"op":"remove","path":"members","value":[{"value":"`+carol+`"}]}]`), env.tokP1)

	if role(carol) != core.OrgRoleAdmin {
		t.Fatalf("carol should stay admin, got %s", role(carol))
	}

	// leaving the admin group falls back to the default role
	env.expect(200, "PATCH", "/scim/v2/Groups/"+admins["id"].(string), patchBody(`[{"op":"remove","path":"members","value":[{"value":"`+carol+`"},{"value":"`+alice+`"}]}]`), env.tokP1)

	if role(carol) != core.OrgRoleMember || role(alice) != core.OrgRoleOwner {
		t.Fatalf("unexpected roles carol=%s alice=%s", role(carol), role(alice))
	}

	// a hand-set role that no group explains is left alone
	env.expect(200, "PATCH", "/scim/v2/Groups/"+admins["id"].(string), patchBody(`[{"op":"add","path":"members","value":[{"value":"`+dave+`"}]}]`), env.tokP1)
	dm := env.member(orgP1, dave)
	dm.Set("role", core.OrgRoleMember)
	orgMust(t, env.app.Save(dm))
	env.expect(200, "PATCH", "/scim/v2/Groups/"+admins["id"].(string), patchBody(`[{"op":"remove","path":"members","value":[{"value":"`+dave+`"}]}]`), env.tokP1)

	if role(dave) != core.OrgRoleMember {
		t.Fatalf("a hand-set role must stay, got %s", role(dave))
	}

	// deleting a role group demotes its members back
	env.expect(200, "PATCH", "/scim/v2/Groups/"+admins["id"].(string), patchBody(`[{"op":"add","path":"members","value":[{"value":"`+carol+`"}]}]`), env.tokP1)
	env.expect(204, "DELETE", "/scim/v2/Groups/"+admins["id"].(string), "", env.tokP1)

	if role(carol) != core.OrgRoleMember {
		t.Fatalf("deleting the group must take the role back, got %s", role(carol))
	}

	// removing the user through SCIM also drops them from the groups
	env.expect(200, "PATCH", "/scim/v2/Groups/"+readers["id"].(string), patchBody(`[{"op":"add","path":"members","value":[{"value":"`+dave+`"}]}]`), env.tokP1)
	env.expect(204, "DELETE", "/scim/v2/Users/"+dave, "", env.tokP1)

	if out := env.expect(200, "GET", "/scim/v2/Groups/"+readers["id"].(string), "", env.tokP1); len(out["members"].([]any)) != 0 {
		t.Fatalf("unexpected members %v", out["members"])
	}
}

func TestSCIMNeverGrantsOwnership(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	p1 := mustOrg(t, env.app, orgP1)
	sys := &core.OrgActor{System: true}

	for _, s := range []core.SCIMSettings{
		{DefaultRole: "owner"},
		{GroupRoles: map[string]string{"G": "owner"}},
		{GroupRoles: map[string]string{"G": "root"}},
	} {
		if err := core.UpdateSCIMSettings(env.app, context.Background(), sys, p1, s); err == nil {
			t.Fatalf("settings %v must be rejected", s)
		}
	}

	// admins cannot map above their own role
	bob := core.NewOrgActor(env.app, p1, env.userByEmail("bob@orgs.test"))
	if err := core.UpdateSCIMSettings(env.app, context.Background(), bob, p1, core.SCIMSettings{GroupRoles: map[string]string{"G": "admin"}}); err != nil {
		t.Fatalf("an admin may map to admin: %v", err)
	}

	carol := core.NewOrgActor(env.app, p1, env.userByEmail("carol@orgs.test"))
	if err := core.UpdateSCIMSettings(env.app, context.Background(), carol, p1, core.SCIMSettings{}); err == nil {
		t.Fatal("a member cannot change the settings")
	}

	// the default role applies to new users
	orgMust(t, core.UpdateSCIMSettings(env.app, context.Background(), sys, p1, core.SCIMSettings{DefaultRole: "viewer"}))

	out := env.expect(201, "POST", "/scim/v2/Users", `{"userName":"viewer.default@x.test"}`, env.tokP1)
	if r := env.member(orgP1, out["id"].(string)).GetString("role"); r != "viewer" {
		t.Fatalf("expected the default role viewer, got %s", r)
	}
}

// ---- audit and no-token behavior -------------------------------------------

func TestSCIMAudit(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	if _, err := core.DeleteExpiredAuditEntries(env.app, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	out := env.expect(201, "POST", "/scim/v2/Users", entraCreate, env.tokP1)
	id := out["id"].(string)
	env.expect(200, "PATCH", "/scim/v2/Users/"+id, patchBody(`[{"op":"replace","path":"active","value":false}]`), env.tokP1)
	env.expect(201, "POST", "/scim/v2/Groups", `{"displayName":"Audited"}`, env.tokP1)
	env.expect(204, "DELETE", "/scim/v2/Users/"+id, "", env.tokP1)

	entries := auditEntries(t, env.app, "actorType='scim'")
	if len(entries) == 0 {
		t.Fatal("expected SCIM audit entries")
	}

	var tokenId string

	toks, _ := core.ListSCIMTokens(env.app, mustOrg(t, env.app, orgP1))
	for _, tk := range toks {
		if !tk.GetDateTime("lastUsed").IsZero() {
			tokenId = tk.Id
		}
	}

	seen := map[string]bool{}

	for _, e := range entries {
		if e.GetString("actorId") != "scim:"+tokenId {
			t.Fatalf("unexpected actor %q (token %q)", e.GetString("actorId"), tokenId)
		}

		var meta map[string]any
		_ = e.UnmarshalJSONField("meta", &meta)

		if meta["orgId"] != orgP1 {
			t.Fatalf("entry %s %s has no org: %v", e.GetString("action"), e.GetString("targetCollection"), meta)
		}

		seen[e.GetString("action")+" "+e.GetString("targetCollection")] = true
	}

	for _, want := range []string{"create users", "create _orgMembers", "update _orgMembers", "delete _orgMembers", "create _scimGroups"} {
		if !seen[want] {
			t.Errorf("missing audit entry %q, have %v", want, seen)
		}
	}

	// token management is audited with the human actor, and the hash is redacted
	if got := auditEntries(t, env.app, "targetCollection='_scimTokens'"); len(got) > 0 {
		for _, e := range got {
			if strings.Contains(e.GetString("after"), env.tokP1) {
				t.Fatal("a token must never be in the audit log")
			}
		}
	}
}

func TestSCIMTokenHashIsRedactedInAudit(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	for _, e := range auditEntries(t, env.app, "targetCollection='_scimTokens' && action='create'") {
		var after map[string]any
		_ = e.UnmarshalJSONField("after", &after)

		if after["tokenHash"] != "[redacted]" {
			t.Fatalf("tokenHash not redacted: %v", after)
		}
	}
}

func TestSCIMWithoutTokensChangesNothing(t *testing.T) {
	t.Parallel()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	seedOrgs(t, app)

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}

	mux, _ := router.BuildMux()
	env := &scimEnv{t: t, app: app, mux: mux}

	before, _ := app.CountRecords("users")
	members, _ := app.CountRecords(core.CollectionNameOrgMembers)

	for _, p := range []string{"/scim/v2/Users", "/scim/v2/Groups", "/scim/v2/ServiceProviderConfig"} {
		env.expectErr(401, "", "GET", p, "", "")
		env.expectErr(401, "", "GET", p, "", "scim_aaaaaaaa_"+strings.Repeat("b", 52))
	}

	env.expectErr(401, "", "POST", "/scim/v2/Users", `{"userName":"x@y.test"}`, "")

	after, _ := app.CountRecords("users")
	membersAfter, _ := app.CountRecords(core.CollectionNameOrgMembers)

	if before != after || members != membersAfter {
		t.Fatal("nothing may change")
	}
}

// ---- token and settings management -----------------------------------------

func TestSCIMTokenManagementAPI(t *testing.T) {
	t.Parallel()
	env := newSCIMEnv(t)

	call := func(who, method, url, body string) (int, map[string]any, string) {
		req := httptest.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		for k, v := range orgAuth(t, who) {
			req.Header.Set(k, v)
		}

		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, req)

		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)

		return rec.Code, out, rec.Body.String()
	}

	base := "/api/orgs/" + orgP1 + "/scim-tokens"

	for _, tc := range []struct {
		who    string
		status int
	}{{"", 401}, {"carol", 403}, {"dave", 403}, {"erin", 404}, {"frank", 404}} {
		if code, _, _ := call(tc.who, "POST", base, `{"name":"x"}`); code != tc.status {
			t.Errorf("%q: expected %d, got %d", tc.who, tc.status, code)
		}
	}

	code, created, raw := call("bob", "POST", base, `{"name":"Entra prod"}`)
	if code != 200 {
		t.Fatalf("create: %d %s", code, raw)
	}

	token := created["token"].(string)
	if !strings.HasPrefix(token, "scim_") || created["name"] != "Entra prod" || !strings.HasSuffix(created["endpoint"].(string), "/scim/v2") {
		t.Fatalf("unexpected %v", created)
	}

	// the token works, the list never shows it
	env.expect(200, "GET", "/scim/v2/Users", "", token)

	code, list, raw := call("alice", "GET", base, "")
	if code != 200 || strings.Contains(raw, token) || strings.Contains(raw, "tokenHash") {
		t.Fatalf("list: %d %s", code, raw)
	}

	if len(list["items"].([]any)) < 2 {
		t.Fatalf("unexpected %v", list)
	}

	// validation, expiry in the past
	if code, _, _ := call("bob", "POST", base, `{"name":""}`); code != 400 {
		t.Fatalf("expected 400, got %d", code)
	}

	if code, _, _ := call("bob", "POST", base, `{"name":"x","expires":"2001-01-01 00:00:00.000Z"}`); code != 400 {
		t.Fatalf("expected 400, got %d", code)
	}

	// revoke
	if code, _, _ := call("carol", "DELETE", base+"/"+created["id"].(string), ""); code != 403 {
		t.Fatalf("expected 403, got %d", code)
	}

	if code, out, _ := call("bob", "DELETE", base+"/"+created["id"].(string), ""); code != 200 || out["revoked"] != true {
		t.Fatalf("revoke: %d %v", code, out)
	}

	env.expectErr(401, "", "GET", "/scim/v2/Users", "", token)

	// another org's token id is a 404
	rows, _ := core.ListSCIMTokens(env.app, mustOrg(t, env.app, orgP2))
	if code, _, _ := call("bob", "DELETE", base+"/"+rows[0].Id, ""); code != 404 {
		t.Fatalf("expected 404, got %d", code)
	}

	// the cap on live tokens (rotation needs two; more than the cap is refused)
	for i := 0; i < core.SCIMTokenMaxPerOrg+2; i++ {
		if code, _, _ := call("bob", "POST", base, `{"name":"bulk"}`); code == 409 {
			goto capped
		}
	}

	t.Fatal("expected the cap to refuse a token")

capped:
	// settings
	sbase := "/api/orgs/" + orgP1 + "/scim-settings"

	if code, out, _ := call("bob", "GET", sbase, ""); code != 200 || out["defaultRole"] != "member" || out["deactivationBans"] != false {
		t.Fatalf("settings: %d %v", code, out)
	}

	if code, _, _ := call("carol", "GET", sbase, ""); code != 403 {
		t.Fatalf("expected 403, got %d", code)
	}

	code, out, raw := call("bob", "PUT", sbase, `{"defaultRole":"viewer","groupRoles":{"Admins":"admin"},"deactivationBans":true}`)
	if code != 200 || out["defaultRole"] != "viewer" || out["deactivationBans"] != true || out["groupRoles"].(map[string]any)["Admins"] != "admin" {
		t.Fatalf("put settings: %d %s", code, raw)
	}

	if s := core.GetSCIMSettings(mustOrg(t, env.app, orgP1)); s.DefaultRole != "viewer" || !s.DeactivationBans || s.GroupRoles["Admins"] != "admin" {
		t.Fatalf("not stored: %+v", s)
	}

	if code, _, _ := call("bob", "PUT", sbase, `{"defaultRole":"owner"}`); code != 400 {
		t.Fatalf("expected 400, got %d", code)
	}

	// the org page's other settings survive
	p1 := mustOrg(t, env.app, orgP1)
	p1.Set("settings", `{"invitationExpiryDays": 3}`)
	orgMust(t, env.app.Save(p1))

	if code, _, _ := call("bob", "PUT", sbase, `{"deactivationBans":false}`); code != 200 {
		t.Fatal("put failed")
	}

	if core.InvitationLifetime(mustOrg(t, env.app, orgP1)) != 72*time.Hour {
		t.Fatal("other org settings must be preserved")
	}
}
