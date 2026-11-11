package apis_test

import (
	"bytes"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/filesystem"
)

// Fork-local (Claviger #80, 80-4): merge, split, duplicates and photos.
// Fixture: see households_test.go. The factories below add to it.

const (
	hhH3   = "househ3aaaaaaaa" // archived, in P1
	hhOth  = "househ4aaaaaaaa" // in P2
	hhDup  = "hmdupkidaaaaaaa" // "kid a" born in the same year as Kid A, in H2
	hhNote = "hhnote1aaaaaaaa"
)

func hhMergeFactory(t testing.TB) *tests.TestApp {
	app := hhFactory(t)

	hCol, err := app.FindCollectionByNameOrId(core.CollectionNameHouseholds)
	orgMust(t, err)

	mkH := func(id, org, status string) {
		h := core.NewRecord(hCol)
		h.Id = id
		h.Set("org", org)
		h.Set("name", id)
		h.Set("status", status)
		orgMust(t, app.Save(h))
	}
	mkH(hhH3, orgP1, "archived")
	mkH(hhOth, orgP2, "active")

	mCol, err := app.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)
	orgMust(t, err)

	d := core.NewRecord(mCol)
	d.Id = hhDup
	d.Set("household", hhH2)
	d.Set("name", " kid  A")
	d.Set("role", "child")
	d.Set("status", "active")
	d.Set("birthDate", hhYears(9))
	orgMust(t, app.Save(d))

	// a dependent record about Kid B
	note := core.NewBaseCollection("hh_notes")
	note.Fields.Add(
		&core.TextField{Name: "note"},
		&core.RelationField{Name: "household", CollectionId: hCol.Id, MaxSelect: 1},
		&core.RelationField{Name: "member", CollectionId: mCol.Id, MaxSelect: 1},
	)
	orgMust(t, app.Save(note))

	n := core.NewRecord(note)
	n.Id = hhNote
	n.Set("member", hhKidB)
	n.Set("household", hhH2)
	orgMust(t, app.Save(n))

	return app
}

func hhm(name, who, method, url, body string, status int, has ...string) orgCase {
	return orgCase{name: name, who: who, method: method, url: url, body: body, status: status, has: has, factory: hhMergeFactory}
}

func TestHouseholdsMerge(t *testing.T) {
	hhKEK(t)

	url := "/api/households/" + hhH1 + "/merge"
	body := `{"from":"` + hhH2 + `"}`

	runOrgCases(t, []orgCase{
		{name: "staff merge: the target comes back with every person", who: "bob", method: "POST", url: url, body: body, status: 200, factory: hhMergeFactory,
			has: []string{`"id":"` + hhH1 + `"`, `"id":"` + hhKidB + `"`, `"id":"` + hhDave + `"`, `"id":"` + hhKidA + `"`, `"staff":true`},
			after: func(t testing.TB, app *tests.TestApp) {
				src, err := app.FindRecordById(core.CollectionNameHouseholds, hhH2)
				orgMust(t, err)

				if src.GetString("status") != "archived" {
					t.Fatal("the source must be archived")
				}

				n, err := app.FindRecordById("hh_notes", hhNote)
				orgMust(t, err)

				if n.GetString("household") != hhH1 {
					t.Fatalf("the copy was not rewritten: %s", n.GetString("household"))
				}

				entries, err := app.FindRecordsByFilter(core.CollectionNameAuditLog, "action='household_merge'", "", 0, 0)
				orgMust(t, err)

				if len(entries) != 1 || entries[0].GetString("actorId") != orgUserID("bob") {
					t.Fatalf("unexpected audit entries: %v", entries)
				}
			}},
		hhm("the registrar", "carol", "POST", url, body, 200),
		hhm("the owner", "alice", "POST", url, body, 200),
		hhm("a superuser", "super", "POST", url, body, 200),
		hhm("a guardian of the target is not staff", "erin", "POST", url, body, 403),
		hhm("a person of the target is not staff", "frank", "POST", url, body, 403),
		hhm("an outsider gets a 404", "dave", "POST", url, body, 404),
		hhm("guests", "", "POST", url, body, 401),
		hhm("into itself", "bob", "POST", url, `{"from":"`+hhH1+`"}`, 400),
		hhm("no source", "bob", "POST", url, `{}`, 400),
		hhm("an unknown source", "bob", "POST", url, `{"from":"nope"}`, 404),
		hhm("a source of another org", "bob", "POST", url, `{"from":"`+hhOth+`"}`, 404),
		hhm("an archived source", "bob", "POST", url, `{"from":"`+hhH3+`"}`, 409),
		hhm("an archived target", "bob", "POST", "/api/households/"+hhH3+"/merge", body, 409),
		{name: "a merge that breaks the guard is refused", who: "bob", method: "POST", url: url, body: body, status: 409,
			factory: func(t testing.TB) *tests.TestApp {
				app := hhMergeFactory(t)
				_, err := app.NonconcurrentDB().NewQuery("UPDATE _householdMembers SET guardian=false").Execute()
				orgMust(t, err)

				return app
			}},
		{name: "the same login in both households is refused", who: "bob", method: "POST", url: url, body: body, status: 409,
			factory: func(t testing.TB) *tests.TestApp {
				app := hhMergeFactory(t)
				mCol, _ := app.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)
				d := core.NewRecord(mCol)
				d.Set("household", hhH1)
				d.Set("user", orgUserID("dave"))
				d.Set("name", "Dave twice")
				d.Set("role", "adult")
				d.Set("status", "active")
				orgMust(t, app.Save(d))

				return app
			}},
	})
}

func TestHouseholdsSplit(t *testing.T) {
	hhKEK(t)

	url := "/api/households/" + hhH1 + "/split"
	both := `{"members":["` + hhIvan + `","` + hhKidA + `"],"name":"Ivan family"}`

	runOrgCases(t, []orgCase{
		{name: "the head splits: both households come back", who: "erin", method: "POST", url: url, body: both, status: 200, factory: hhMergeFactory,
			has: []string{`"from":{`, `"to":{`, `"name":"Ivan family"`, `"id":"` + hhKidA + `"`, `"status":"active"`},
			after: func(t testing.TB, app *tests.TestApp) {
				kid, err := app.FindRecordById(core.CollectionNameHouseholdMembers, hhKidA)
				orgMust(t, err)

				if kid.GetString("household") == hhH1 {
					t.Fatal("not moved")
				}

				entries, err := app.FindRecordsByFilter(core.CollectionNameAuditLog, "action='household_split'", "", 0, 0)
				orgMust(t, err)

				if len(entries) != 1 {
					t.Fatalf("%d entries", len(entries))
				}
			}},
		hhm("staff split", "carol", "POST", url, both, 200, `"from":{`),
		hhm("a guardian who is not the head cannot", "ivan", "POST", url, both, 403),
		hhm("a plain person cannot", "frank", "POST", url, both, 403),
		hhm("an outsider gets a 404", "dave", "POST", url, both, 404),
		hhm("guests", "", "POST", url, both, 401),
		hhm("the child alone leaves a minor without a guardian", "erin", "POST", url, `{"members":["`+hhKidA+`"],"name":"X"}`, 409),
		hhm("both guardians away leaves the child without one", "erin", "POST", url, `{"members":["`+hhErin+`","`+hhIvan+`"],"name":"X"}`, 409),
		hhm("a person of another household", "erin", "POST", url, `{"members":["`+hhKidB+`"],"name":"X"}`, 400),
		hhm("nobody named", "erin", "POST", url, `{"members":[],"name":"X"}`, 400),
		hhm("no name", "erin", "POST", url, `{"members":["`+hhFrnk+`"],"name":""}`, 400),
		hhm("everyone", "erin", "POST", url, `{"members":["`+hhErin+`","`+hhIvan+`","`+hhFrnk+`","`+hhKidA+`"],"name":"X"}`, 400),
	})
}

func TestHouseholdsDuplicates(t *testing.T) {
	hhKEK(t)

	url := "/api/households/duplicates?org=" + orgP1

	runOrgCases(t, []orgCase{
		{name: "staff get the candidates, without any birth data", who: "bob", url: url, status: 200, factory: hhMergeFactory,
			has:    []string{`"reason":"same_name_birth_year"`, `"id":"` + hhKidA + `"`, `"id":"` + hhDup + `"`, `"householdName":"Dave family"`, `"truncated":false`},
			hasNot: []string{"birthDate", hhYears(9), hhYears(9)[:5], "allergies", "peanuts"}},
		hhm("the registrar", "carol", "GET", url, "", 200, `"items":[`),
		hhm("a guardian is not staff", "erin", "GET", url, "", 404),
		hhm("another org's staff", "bob", "GET", "/api/households/duplicates?org="+orgP2, "", 404),
		hhm("guests", "", "GET", url, "", 401),
		hhm("no org", "bob", "GET", "/api/households/duplicates", "", 404),
	})
}

// ---- photos ------------------------------------------------------------------

func hhPNG(t testing.TB) []byte {
	t.Helper()

	var buf bytes.Buffer
	orgMust(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))))

	return buf.Bytes()
}

func hhUpload(t testing.TB, name, ctype string, data []byte) (*bytes.Buffer, string) {
	t.Helper()

	body := new(bytes.Buffer)
	mp := multipart.NewWriter(body)

	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="photo"; filename="`+name+`"`)
	h.Set("Content-Type", ctype)

	w, err := mp.CreatePart(h)
	orgMust(t, err)
	_, err = w.Write(data)
	orgMust(t, err)
	orgMust(t, mp.Close())

	return body, mp.FormDataContentType()
}

func hhPhotoCase(t *testing.T, name, who, method, url string, data []byte, fname string, status int, has ...string) {
	t.Helper()

	var (
		body  *bytes.Buffer
		ctype string
	)

	if method == http.MethodPost {
		body, ctype = hhUpload(t, fname, "image/png", data)
	}

	headers := orgAuth(t, who)
	if headers == nil {
		headers = map[string]string{}
	}

	if ctype != "" {
		headers["Content-Type"] = ctype
	}

	s := tests.ApiScenario{
		Name: name, Method: method, URL: url, Headers: headers,
		ExpectedStatus: status, ExpectedContent: has, TestAppFactory: hhMergeFactory,
	}

	if body != nil {
		s.Body = body
	}

	if len(has) == 0 {
		s.ExpectedContent = []string{`"`}
	}

	s.Test(t)
}

func TestHouseholdMemberPhotoRoutes(t *testing.T) {
	hhKEK(t)

	kid := "/api/households/" + hhH1 + "/members/" + hhKidA + "/photo"
	own := "/api/households/" + hhH1 + "/members/" + hhFrnk + "/photo"
	img := hhPNG(t)

	hhPhotoCase(t, "staff upload", "carol", "POST", kid, img, "a.png", 200, `.png"`)
	hhPhotoCase(t, "a guardian uploads for the minor", "erin", "POST", kid, img, "a.png", 200, `.png"`)
	hhPhotoCase(t, "a guardian uploads for an adult too (the PATCH rule)", "erin", "POST", own, img, "a.png", 200)
	hhPhotoCase(t, "the person themselves", "frank", "POST", own, img, "a.png", 200)
	hhPhotoCase(t, "a person cannot change the minor's photo", "frank", "POST", kid, img, "a.png", 403)
	hhPhotoCase(t, "an outsider gets a 404", "dave", "POST", kid, img, "a.png", 404)
	hhPhotoCase(t, "guests", "", "POST", kid, img, "a.png", 401)
	hhPhotoCase(t, "a person of another household in the path", "erin", "POST", "/api/households/"+hhH1+"/members/"+hhKidB+"/photo", img, "a.png", 404)
	hhPhotoCase(t, "text named like an image", "erin", "POST", kid, []byte("not an image at all"), "a.png", 400)
	hhPhotoCase(t, "a gif", "erin", "POST", kid, []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), "a.gif", 400)
	hhPhotoCase(t, "too large", "erin", "POST", kid, append(append([]byte{}, img...), make([]byte, 5<<20)...), "big.png", 400)

	// no file
	(&tests.ApiScenario{
		Name: "no file", Method: http.MethodPost, URL: kid, Body: bytes.NewReader(nil),
		Headers:        map[string]string{"Authorization": orgToken(t, "erin")},
		ExpectedStatus: 400, ExpectedContent: []string{`"message"`}, TestAppFactory: hhMergeFactory,
	}).Test(t)

	hhPhotoCase(t, "a guardian removes it", "erin", "DELETE", kid, nil, "", 200, `"photo":""`)
	hhPhotoCase(t, "a person cannot remove the minor's", "frank", "DELETE", kid, nil, "", 403)
}

// The protected file route follows the view rule of _householdMembers: exactly
// the people who can see the member can read the photo (with a file token).
func TestHouseholdPhotoFileAccess(t *testing.T) {
	hhKEK(t)

	factory := func(t testing.TB) *tests.TestApp {
		app := hhMergeFactory(t)

		m, err := app.FindRecordById(core.CollectionNameHouseholdMembers, hhKidA)
		orgMust(t, err)

		f, err := filesystem.NewFileFromBytes(hhPNG(t), "kid.png")
		orgMust(t, err)
		f.Name = "kid.png"
		m.Set("photo", f)
		orgMust(t, app.Save(m))

		return app
	}

	url := "/api/files/" + core.CollectionNameHouseholdMembers + "/" + hhKidA + "/kid.png"

	for who, status := range map[string]int{
		"erin": 200, "frank": 200, // the household
		"carol": 200, "bob": 200, // staff
		"dave": 404, // another household
		"gina": 404, // not even in the org
	} {
		(&tests.ApiScenario{
			Name: "file token of " + who, Method: http.MethodGet, URL: url + "?token=" + orgFileToken(t, who),
			ExpectedStatus: status, TestAppFactory: factory,
			ExpectedContent: func() []string {
				if status == 200 {
					return []string{"PNG"}
				}

				return []string{`"`}
			}(),
		}).Test(t)
	}

	(&tests.ApiScenario{
		Name: "no token", Method: http.MethodGet, URL: url, ExpectedStatus: 404, ExpectedContent: []string{`"`}, TestAppFactory: factory,
	}).Test(t)
}

func orgFileToken(t testing.TB, name string) string {
	t.Helper()

	app, err := tests.NewTestApp()
	orgMust(t, err)

	defer app.Cleanup()

	users, _ := app.FindCollectionByNameOrId("users")
	r := core.NewRecord(users)
	r.Id = orgUserID(name)
	r.Set("tokenKey", orgTokenKey(name))

	tok, err := r.NewFileToken()
	orgMust(t, err)

	return tok
}
