package apis_test

import (
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #80): the household routes and the read rules of the
// system collections.
//
// Fixture: the org fixture plus, in P1: erin and ivan (members), frank
// (viewer). carol (member of P1, tagged "registrar") is staff, bob (admin) and
// alice (owner) too; dave (viewer) is not.
//
//	H1 (P1): erin (head, guardian), ivan (spouse, guardian), frank (adult with a
//	         login, not a guardian), kidA (a minor)
//	H2 (P1): dave (guardian) and kidB (a minor)

const (
	hhH1   = "househ1aaaaaaaa"
	hhH2   = "househ2aaaaaaaa"
	hhErin = "hmerinaaaaaaaaa"
	hhIvan = "hmivanaaaaaaaaa"
	hhFrnk = "hmfrankaaaaaaaa"
	hhKidA = "hmkidaaaaaaaaaa"
	hhDave = "hmdaveaaaaaaaaa"
	hhKidB = "hmkidbaaaaaaaaa"
)

func hhYears(n int) string { return time.Now().AddDate(-n, 0, -3).Format("2006-01-02") }

func hhKEK(t testing.TB) {
	t.Helper()

	// 32 bytes of base64 (a fixed test key)
	t.Setenv("CLAVIGER_KEK", "dGVzdC1rZXktZm9yLWhvdXNlaG9sZHMtMTIzNDU2Nzg=")
	t.Setenv("CLAVIGER_KEK_FILE", "")
}

func hhFactory(t testing.TB) *tests.TestApp {
	app := orgsFactory(t)

	// the migration creates plain fields; the server encrypts them at start-up
	orgMust(t, core.UpgradeHouseholdEncryption(app))

	roles, err := app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
	orgMust(t, err)

	tag := core.NewRecord(roles)
	tag.Id = "tagregistrarp1a"
	tag.Set("org", orgP1)
	tag.Set("name", core.HouseholdRegistrarTag)
	orgMust(t, app.Save(tag))

	carol, err := core.FindOrgMember(app, orgP1, orgUserID("carol"))
	orgMust(t, err)
	carol.Set("tags", append(carol.GetStringSlice("tags"), tag.Id))
	orgMust(t, app.Save(carol))

	members, err := app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)
	orgMust(t, err)

	for i, p := range []struct{ who, role string }{{"erin", "member"}, {"ivan", "member"}, {"frank", "viewer"}} {
		m := core.NewRecord(members)
		m.Id = "memhhp1" + strings.Repeat("0", 7) + string(rune('a'+i))
		m.Set("org", orgP1)
		m.Set("user", orgUserID(p.who))
		m.Set("role", p.role)
		m.Set("status", "active")
		orgMust(t, app.Save(m))
	}

	hCol, err := app.FindCollectionByNameOrId(core.CollectionNameHouseholds)
	orgMust(t, err)

	mkH := func(id, name string) {
		h := core.NewRecord(hCol)
		h.Id = id
		h.Set("org", orgP1)
		h.Set("name", name)
		h.Set("status", "active")
		h.Set("address", map[string]any{"street": "9 Elm"})
		h.Set("homePhone", "+1 555 0123")
		orgMust(t, app.Save(h))
	}
	mkH(hhH1, "Erin family")
	mkH(hhH2, "Dave family")

	mCol, err := app.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)
	orgMust(t, err)

	mkM := func(id, h, who, name, role string, guardian bool, birth string) {
		m := core.NewRecord(mCol)
		m.Id = id
		m.Set("household", h)

		if who != "" {
			m.Set("user", orgUserID(who))
		}

		m.Set("name", name)
		m.Set("role", role)
		m.Set("guardian", guardian)
		m.Set("status", "active")
		m.Set("birthDate", birth)
		m.Set("allergies", map[string]any{"food": "peanuts"})
		orgMust(t, app.Save(m))
	}
	mkM(hhErin, hhH1, "erin", "Erin E", "head", true, hhYears(40))
	mkM(hhIvan, hhH1, "ivan", "Ivan E", "spouse", true, hhYears(41))
	mkM(hhFrnk, hhH1, "frank", "Frank E", "adult", false, hhYears(20))
	mkM(hhKidA, hhH1, "", "Kid A", "child", false, hhYears(9))
	mkM(hhDave, hhH2, "dave", "Dave D", "head", true, hhYears(39))
	mkM(hhKidB, hhH2, "", "Kid B", "child", false, hhYears(7))

	return app
}

func hhCase(name, who, method, url, body string, status int, has ...string) orgCase {
	return orgCase{name: name, who: who, method: method, url: url, body: body, status: status, has: has, factory: hhFactory}
}

func TestHouseholdsRead(t *testing.T) {
	hhKEK(t)

	base := "/api/households"

	runOrgCases(t, []orgCase{
		hhCase("my households (a guardian sees the health notes of the child)", "erin", "GET", base+"/mine", "", 200,
			`"name":"Erin family"`, `"name":"Kid A"`, `"allergies":{"food":"peanuts"}`, `"guardian":true`, `"me":"`+hhErin+`"`),
		{name: "my households show only mine", who: "erin", url: base + "/mine", status: 200, hasNot: []string{"Dave family", "Kid B"}, factory: hhFactory},
		hhCase("a person who is not a guardian sees the others without birth date and notes", "frank", "GET", base+"/"+hhH1, "", 200,
			`"name":"Kid A"`, `"me":"`+hhFrnk+`"`, `"guardian":false`),
		{name: "no health notes of others for a plain member", who: "frank", url: base + "/" + hhH1, status: 200,
			hasNot: []string{`"birthDate":"` + hhYears(9)}, factory: hhFactory},
		hhCase("guests", "", "GET", base+"/mine", "", 401),
		hhCase("another household is a 404", "dave", "GET", base+"/"+hhH1, "", 404),
		hhCase("org staff (admin)", "bob", "GET", base+"/"+hhH1, "", 200, `"name":"Kid A"`, `"staff":true`, `"allergies"`),
		hhCase("org staff (owner)", "alice", "GET", base+"/"+hhH1, "", 200, `"staff":true`),
		hhCase("org staff (registrar tag)", "carol", "GET", base+"/"+hhH1, "", 200, `"staff":true`),
		hhCase("a viewer of the org is not staff", "dave", "GET", base+"/"+hhH2, "", 200, `"staff":false`, `"guardian":true`),
		hhCase("an admin of the parent org is not staff", "gina", "GET", base+"/"+hhH1, "", 404),
		hhCase("a superuser", "super", "GET", base+"/"+hhH1, "", 200, `"staff":true`),
		hhCase("staff list the org", "bob", "GET", base+"?org="+orgP1, "", 200, `"name":"Erin family"`, `"name":"Dave family"`),
		hhCase("registrar lists the org", "carol", "GET", base+"?org="+orgP1, "", 200, `"name":"Erin family"`),
		hhCase("a guardian is not staff", "erin", "GET", base+"?org="+orgP1, "", 404),
		hhCase("another org's staff", "erin", "GET", base+"?org="+orgP2, "", 404),
	})
}

func TestHouseholdRecordRules(t *testing.T) {
	hhKEK(t)

	list := "/api/collections/" + core.CollectionNameHouseholdMembers + "/records"

	runOrgCases(t, []orgCase{
		hhCase("a member lists the people of the own household", "erin", "GET", list, "", 200,
			`"id":"`+hhKidA+`"`, `"id":"`+hhFrnk+`"`, `"totalItems":4`),
		{name: "and not of others", who: "erin", url: list, status: 200, hasNot: []string{hhKidB, hhDave}, factory: hhFactory},
		hhCase("registrar lists everything of the org", "carol", "GET", list, "", 200, `"totalItems":6`),
		hhCase("an outsider lists nothing", "gina", "GET", list, "", 200, `"totalItems":0`),
		hhCase("a minor row is visible to its household", "frank", "GET", list+"/"+hhKidA, "", 200, `"name":"Kid A"`),
		hhCase("and hidden from another", "dave", "GET", list+"/"+hhKidA, "", 404),
		hhCase("the household record", "erin", "GET", "/api/collections/"+core.CollectionNameHouseholds+"/records/"+hhH1, "", 200, `"name":"Erin family"`),
		hhCase("writes through the records API are closed", "erin", "POST", list,
			`{"household":"`+hhH1+`","name":"Sneaky"}`, 403),
		hhCase("also for an update", "erin", "PATCH", list+"/"+hhErin, `{"guardian":false}`, 403),
		hhCase("and a delete", "bob", "DELETE", list+"/"+hhKidA, "", 403),
	})
}

func TestHouseholdsCreate(t *testing.T) {
	hhKEK(t)

	base := "/api/households"

	runOrgCases(t, []orgCase{
		hhCase("a member of the org starts a household and becomes its guardian", "erin", "POST", base,
			`{"org":"`+orgP1+`","name":"Carol family","homePhone":"+1 555 0001","address":{"city":"X"}}`, 200,
			`"name":"Carol family"`, `"guardian":true`, `"role":"head"`, `"homePhone":"+1 555 0001"`),
		hhCase("a viewer may too", "frank", "POST", base, `{"org":"`+orgP1+`","name":"Frank family"}`, 200, `"guardian":true`),
		hhCase("staff may create an empty household (a registrar too)", "carol", "POST", base, `{"org":"`+orgP1+`","name":"Empty"}`, 200, `"members":[]`),
		hhCase("or name the first person", "bob", "POST", base,
			`{"org":"`+orgP1+`","name":"Named","first":{"name":"Zed Z","role":"head","guardian":true}}`, 200, `"name":"Zed Z"`),
		hhCase("self service ignores a different first member", "frank", "POST", base,
			`{"org":"`+orgP1+`","name":"F","first":{"name":"Someone","guardian":false,"user":"`+orgUserID("alice")+`"}}`, 200, `"guardian":true`, `"user":"`+orgUserID("frank")+`"`),
		hhCase("a name is required", "erin", "POST", base, `{"org":"`+orgP1+`","name":" "}`, 400),
		hhCase("outsiders get a 404", "hank", "POST", base, `{"org":"`+orgP1+`","name":"X"}`, 404),
		hhCase("a suspended member", "sam", "POST", base, `{"org":"`+orgP1+`","name":"X"}`, 404),
		hhCase("an archived org", "hank", "POST", base, `{"org":"`+orgA+`","name":"X"}`, 404),
		hhCase("guests", "", "POST", base, `{"org":"`+orgP1+`","name":"X"}`, 401),
		hhCase("an unknown org", "erin", "POST", base, `{"org":"nope","name":"X"}`, 404),
	})
}

func TestHouseholdsUpdate(t *testing.T) {
	hhKEK(t)

	url := "/api/households/" + hhH1

	runOrgCases(t, []orgCase{
		hhCase("a guardian renames", "erin", "PATCH", url, `{"name":"Renamed","homePhone":"+1 555 9"}`, 200, `"name":"Renamed"`, `"homePhone":"+1 555 9"`),
		hhCase("a person who is not a guardian cannot", "frank", "PATCH", url, `{"name":"X"}`, 403),
		hhCase("another household gets a 404", "dave", "PATCH", url, `{"name":"X"}`, 404),
		hhCase("staff rename", "carol", "PATCH", url, `{"name":"By staff"}`, 200, `"name":"By staff"`),
		hhCase("a guardian cannot archive", "erin", "PATCH", url, `{"status":"archived"}`, 403),
		hhCase("staff archive", "bob", "PATCH", url, `{"status":"archived"}`, 200, `"status":"archived"`),
		hhCase("a bad status", "bob", "PATCH", url, `{"status":"gone"}`, 400),
		hhCase("an empty name", "erin", "PATCH", url, `{"name":""}`, 400),
	})
}

func TestHouseholdMembers(t *testing.T) {
	hhKEK(t)

	m := "/api/households/" + hhH1 + "/members"

	runOrgCases(t, []orgCase{
		hhCase("a guardian adds a child: the birth date makes a minor", "erin", "POST", m,
			`{"name":"Baby","role":"child","birthDate":"`+hhYears(1)+`","allergies":{"milk":true}}`, 200,
			`"name":"Baby"`, `"isMinor":true`, `"source":"manual"`, `"milk":true`),
		hhCase("an adult without a birth date", "erin", "POST", m, `{"name":"Gran"}`, 200, `"isMinor":false`, `"role":"adult"`),
		hhCase("a minor with a login is refused", "bob", "POST", m,
			`{"name":"Teen","birthDate":"`+hhYears(12)+`","user":"`+orgUserID("alice")+`"}`, 400),
		hhCase("an explicit minor with a login is refused", "bob", "POST", m, `{"name":"Teen","isMinor":true,"user":"`+orgUserID("alice")+`"}`, 400),
		hhCase("a minor as guardian is refused", "erin", "POST", m, `{"name":"Teen","birthDate":"`+hhYears(12)+`","guardian":true}`, 400),
		hhCase("a guardian cannot give someone else a login", "erin", "POST", m, `{"name":"Alice","user":"`+orgUserID("alice")+`"}`, 403),
		hhCase("staff can", "carol", "POST", m, `{"name":"Alice","user":"`+orgUserID("alice")+`"}`, 200, `"user":"`+orgUserID("alice")+`"`),
		hhCase("a login that does not exist", "carol", "POST", m, `{"name":"Nobody","user":"nobodyxxxxxxxxx"}`, 400),
		hhCase("the same login twice", "carol", "POST", m, `{"name":"Ivan again","user":"`+orgUserID("ivan")+`"}`, 400),
		hhCase("a person who is not a guardian cannot add", "frank", "POST", m, `{"name":"X"}`, 403),
		hhCase("another household gets a 404", "dave", "POST", m, `{"name":"X"}`, 404),
		hhCase("a name is required", "erin", "POST", m, `{"role":"adult"}`, 400),
		hhCase("a bad birth date", "erin", "POST", m, `{"name":"X","birthDate":"yesterday"}`, 400),
		hhCase("a bad role", "erin", "POST", m, `{"name":"X","role":"boss"}`, 400),

		hhCase("a guardian edits the child", "erin", "PATCH", m+"/"+hhKidA, `{"name":"Kid Alpha","medical":{"asthma":true}}`, 200, `"name":"Kid Alpha"`, `"asthma":true`),
		hhCase("a guardian cannot attach a login", "erin", "PATCH", m+"/"+hhFrnk, `{"user":"`+orgUserID("alice")+`"}`, 403),
		hhCase("a minor never gets a login, not even from staff", "carol", "PATCH", m+"/"+hhKidA, `{"user":"`+orgUserID("alice")+`"}`, 400),
		hhCase("a person edits their own name", "frank", "PATCH", m+"/"+hhFrnk, `{"name":"Frank F"}`, 200, `"name":"Frank F"`),
		hhCase("but not their own role", "frank", "PATCH", m+"/"+hhFrnk, `{"role":"head"}`, 403),
		hhCase("or someone else", "frank", "PATCH", m+"/"+hhKidA, `{"name":"X"}`, 403),
		hhCase("a minor row cannot be made a guardian", "erin", "PATCH", m+"/"+hhKidA, `{"guardian":true}`, 400),
		hhCase("the birth date wins over isMinor set by hand", "carol", "PATCH", m+"/"+hhFrnk, `{"isMinor":true}`, 200, `"isMinor":false`),
		hhCase("a member of another household gets a 404", "dave", "PATCH", m+"/"+hhKidA, `{"name":"X"}`, 404),
		hhCase("a row of another household in the path", "erin", "PATCH", m+"/"+hhKidB, `{"name":"X"}`, 404),
	})
}

func TestHouseholdGuardians(t *testing.T) {
	hhKEK(t)

	m := "/api/households/" + hhH1 + "/members"

	runOrgCases(t, []orgCase{
		hhCase("a guardian names another", "erin", "POST", m+"/"+hhFrnk+"/guardian", "", 200, `"guardian":true`),
		hhCase("a person who is not a guardian cannot", "frank", "POST", m+"/"+hhFrnk+"/guardian", "", 403),
		hhCase("a minor cannot be one", "erin", "POST", m+"/"+hhKidA+"/guardian", "", 400),
		hhCase("a guardian can step down while another remains", "ivan", "DELETE", m+"/"+hhIvan+"/guardian", "", 200, `"guardian":false`),
		hhCase("a guardian cannot demote another guardian", "erin", "DELETE", m+"/"+hhIvan+"/guardian", "", 403),
		hhCase("staff can: the registrar removes a guardian quickly", "carol", "DELETE", m+"/"+hhIvan+"/guardian", "", 200, `"guardian":false`),
		hhCase("owner too", "alice", "DELETE", m+"/"+hhIvan+"/guardian", "", 200),
		hhCase("admin too", "bob", "DELETE", m+"/"+hhIvan+"/guardian", "", 200),
		hhCase("a superuser too", "super", "DELETE", m+"/"+hhIvan+"/guardian", "", 200),

		{name: "the last guardian cannot step down while there is a minor", who: "ivan", method: "DELETE", url: m + "/" + hhIvan + "/guardian",
			status: 409, factory: func(t testing.TB) *tests.TestApp {
				app := hhFactory(t)
				e, err := app.FindRecordById(core.CollectionNameHouseholdMembers, hhErin)
				orgMust(t, err)
				e.Set("guardian", false)
				orgMust(t, app.Save(e))

				return app
			}},
		{name: "nor be demoted by the registrar", who: "carol", method: "DELETE", url: "/api/households/" + hhH2 + "/members/" + hhDave + "/guardian",
			status: 409, factory: hhFactory},
	})
}

func TestHouseholdRemoveMember(t *testing.T) {
	hhKEK(t)

	m := "/api/households/" + hhH1 + "/members"

	runOrgCases(t, []orgCase{
		hhCase("a guardian removes the child", "erin", "DELETE", m+"/"+hhKidA, "", 204),
		hhCase("a guardian removes an adult who is not a guardian", "erin", "DELETE", m+"/"+hhFrnk, "", 204),
		hhCase("a guardian cannot remove another guardian", "erin", "DELETE", m+"/"+hhIvan, "", 403),
		hhCase("a guardian can leave when another remains", "ivan", "DELETE", m+"/"+hhIvan, "", 204),
		hhCase("the registrar removes a guardian at once", "carol", "DELETE", m+"/"+hhIvan, "", 204),
		hhCase("a person leaves", "frank", "DELETE", m+"/"+hhFrnk, "", 204),
		hhCase("a person cannot remove others", "frank", "DELETE", m+"/"+hhKidA, "", 403),
		hhCase("another household gets a 404", "dave", "DELETE", m+"/"+hhKidA, "", 404),
		hhCase("guests", "", "DELETE", m+"/"+hhKidA, "", 401),
		{name: "removing the last guardian is refused", who: "carol", method: "DELETE", url: m + "/" + hhIvan, status: 409,
			factory: func(t testing.TB) *tests.TestApp {
				app := hhFactory(t)
				// erin is already gone
				e, err := app.FindRecordById(core.CollectionNameHouseholdMembers, hhErin)
				orgMust(t, err)
				orgMust(t, app.Delete(e))

				return app
			}},
		{name: "a removed guardian loses access at once", who: "ivan", url: "/api/households/" + hhH1, status: 404,
			factory: func(t testing.TB) *tests.TestApp {
				app := hhFactory(t)
				e, err := app.FindRecordById(core.CollectionNameHouseholdMembers, hhIvan)
				orgMust(t, err)
				orgMust(t, app.Delete(e))

				return app
			}},
		{name: "the removal is audited with the org", who: "carol", method: "DELETE", url: m + "/" + hhIvan, status: 204, factory: hhFactory,
			after: func(t testing.TB, app *tests.TestApp) {
				entries, err := app.FindRecordsByFilter(core.CollectionNameAuditLog,
					"action='delete' && targetCollection={:c}", "", 0, 0, map[string]any{"c": core.CollectionNameHouseholdMembers})
				orgMust(t, err)

				if len(entries) != 1 || !strings.Contains(entries[0].GetString("meta"), orgP1) || !strings.Contains(entries[0].GetString("meta"), hhH1) ||
					!strings.Contains(entries[0].GetString("actorId"), orgUserID("carol")) {
					t.Fatalf("unexpected audit entries: %v", entries)
				}
			}},
	})
}

func TestHouseholdsWithoutAKey(t *testing.T) {
	// the migration ran without a key: plain fields, and no sensitive data accepted
	t.Setenv("CLAVIGER_KEK", "")
	t.Setenv("CLAVIGER_KEK_FILE", "")

	noKey := func(name, who, method, url, body string, status int, has ...string) orgCase {
		return orgCase{name: name, who: who, method: method, url: url, body: body, status: status, has: has,
			factory: func(t testing.TB) *tests.TestApp {
				app := orgsFactory(t)
				h, err := app.FindCollectionByNameOrId(core.CollectionNameHouseholds)
				orgMust(t, err)
				rec := core.NewRecord(h)
				rec.Id = hhH1
				rec.Set("org", orgP1)
				rec.Set("name", "Erin family")
				rec.Set("status", "active")
				orgMust(t, app.Save(rec))

				m, err := app.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)
				orgMust(t, err)
				e := core.NewRecord(m)
				e.Id = hhErin
				e.Set("household", hhH1)
				e.Set("user", orgUserID("alice"))
				e.Set("name", "Erin E")
				e.Set("guardian", true)
				e.Set("status", "active")
				orgMust(t, app.Save(e))

				return app
			}}
	}

	runOrgCases(t, []orgCase{
		noKey("a household without sensitive data works", "bob", "POST", "/api/households", `{"org":"`+orgP1+`","name":"Plain"}`, 200, `"name":"Plain"`),
		noKey("an address is refused", "bob", "POST", "/api/households", `{"org":"`+orgP1+`","name":"Plain","address":{"a":"b"}}`, 409, "not encrypted"),
		noKey("a phone is refused", "bob", "POST", "/api/households", `{"org":"`+orgP1+`","name":"Plain","homePhone":"1"}`, 409),
		noKey("a birth date is refused", "alice", "POST", "/api/households/"+hhH1+"/members", `{"name":"Kid","birthDate":"2020-01-01"}`, 409),
		noKey("health notes are refused", "alice", "POST", "/api/households/"+hhH1+"/members", `{"name":"Kid","allergies":{"a":1}}`, 409),
		noKey("a person without them is fine", "alice", "POST", "/api/households/"+hhH1+"/members", `{"name":"Gran","role":"adult"}`, 200),
	})
}
