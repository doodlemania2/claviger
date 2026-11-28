package apis_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #82): children's check-in. The fixture is the household
// fixture (hhFactory) plus the installed check-in template:
//
//	P1: alice owner, bob admin, carol member tagged "checkin" (staff, not a
//	    supervisor), erin and ivan guardians of H1 (kidA, kidC), dave guardian
//	    of H2 (kidB), frank an adult of H1 who is not a guardian.
//	events: ev1 (open, P1), evClosed (closed, P1), evP2 (open, P2)

const (
	ckEv1    = "ckevent1aaaaaaa"
	ckEvShut = "ckeventcaaaaaaa"
	ckEvP2   = "ckeventpaaaaaaa"
	ckKidC   = "hmkidcaaaaaaaaa"
)

func ckFactory(t testing.TB) *tests.TestApp {
	t.Helper()

	t.Setenv(core.CheckinKeyEnv, "unit-test-checkin-key-0123456789")

	hhKEK(t)

	app := hhFactory(t)

	if _, err := core.CreateCheckinTemplates(app, context.Background()); err != nil {
		t.Fatal(err)
	}

	org, err := app.FindRecordById(core.CollectionNameOrgs, orgP1)
	orgMust(t, err)

	_, err = core.EnsureCheckinTags(app, context.Background(), org)
	orgMust(t, err)

	tag, err := app.FindFirstRecordByFilter(core.CollectionNameOrgRoles, "org={:o} && name={:n}", dbx.Params{"o": orgP1, "n": core.CheckinStaffTag})
	orgMust(t, err)

	carol, err := core.FindOrgMember(app, orgP1, orgUserID("carol"))
	orgMust(t, err)
	carol.Set("tags", append(carol.GetStringSlice("tags"), tag.Id))
	orgMust(t, app.Save(carol))

	mCol, err := app.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)
	orgMust(t, err)

	kid := core.NewRecord(mCol)
	kid.Id = ckKidC
	kid.Set("household", hhH1)
	kid.Set("name", "Kid C")
	kid.Set("role", "child")
	kid.Set("status", "active")
	kid.Set("birthDate", hhYears(5))
	orgMust(t, app.Save(kid))

	evCol, err := app.FindCollectionByNameOrId(core.CheckinEventsCollection)
	orgMust(t, err)

	mkEv := func(id, org, name, status string) {
		r := core.NewRecord(evCol)
		r.Id = id
		r.Set("org", org)
		r.Set("name", name)
		r.Set("status", status)
		r.Set("starts", time.Now().Add(-time.Hour))
		r.Set("ends", time.Now().Add(3*time.Hour))
		orgMust(t, app.Save(r))
	}
	mkEv(ckEv1, orgP1, "Sunday 10:00", "open")
	mkEv(ckEvShut, orgP1, "Last week", "closed")
	mkEv(ckEvP2, orgP2, "Parish Two", "open")

	return app
}

type ckFix struct {
	app *tests.TestApp

	// sessions of kidA (H1) and kidB (H2) when the fixture checked them in
	id1, code1, qr1 string
	id2, code2      string

	kioskId, kioskTok, kioskSess string
	revokedTok                   string
}

func (f *ckFix) user(who string) *core.Record {
	u, err := f.app.FindRecordById("users", orgUserID(who))
	if err != nil {
		panic(err)
	}

	return u
}

func (f *ckFix) fresh(t testing.TB, who string) string {
	tok, err := f.user(who).NewPasskeyAuthToken(time.Now(), true)
	orgMust(t, err)

	return tok
}

func ckSeed(t testing.TB, app *tests.TestApp, checkin bool) *ckFix {
	t.Helper()

	f := &ckFix{app: app}
	ctx := context.Background()

	ev, err := app.FindRecordById(core.CheckinEventsCollection, ckEv1)
	orgMust(t, err)

	org, err := app.FindRecordById(core.CollectionNameOrgs, orgP1)
	orgMust(t, err)

	if checkin {
		res, err := core.CheckIn(app, ctx, core.CheckinInput{
			Event: ev, Household: hhH1, Children: []string{hhKidA}, Guardian: f.user("erin"), Actor: core.CheckinActor{User: f.user("erin")},
		})
		orgMust(t, err)

		f.id1, f.code1, f.qr1 = res.Labels[0].Session, res.Code, res.Labels[0].QR

		res, err = core.CheckIn(app, ctx, core.CheckinInput{
			Event: ev, Household: hhH2, Children: []string{hhKidB}, Guardian: f.user("dave"), Actor: core.CheckinActor{User: f.user("dave")},
		})
		orgMust(t, err)

		f.id2, f.code2 = res.Labels[0].Session, res.Code
	}

	k, tok, err := core.RegisterKiosk(app, ctx, org, f.user("bob"), "Door 1", nil)
	orgMust(t, err)

	f.kioskId, f.kioskTok = k.Id, tok

	h, _, err := core.IssueKioskHandoff(app, f.user("erin"), hhH1, k.Id)
	orgMust(t, err)

	f.kioskSess, _, err = core.ExchangeKioskHandoff(app, k, org, h)
	orgMust(t, err)

	k2, tok2, err := core.RegisterKiosk(app, ctx, org, f.user("bob"), "Lost tablet", nil)
	orgMust(t, err)
	_, err = core.RevokeKiosk(app, ctx, org, f.user("bob"), k2.Id)
	orgMust(t, err)

	f.revokedTok = tok2

	return f
}

// ckCase is one ApiScenario. who is "", "super", a user name, "fresh:<user>",
// "kiosk", "kiosk+session" or "kiosk-revoked".
type ckCase struct {
	name      string
	who       string
	method    string
	url       func(f *ckFix) string
	body      func(f *ckFix) string
	noCheckin bool
	pre       func(t testing.TB, f *ckFix)
	post      func(t testing.TB, f *ckFix)
	status    int
	has       []string
	hasNot    []string
}

func lit(s string) func(*ckFix) string { return func(*ckFix) string { return s } }

func ckSessionURL(suffix string) func(*ckFix) string {
	return func(f *ckFix) string { return "/api/checkin/sessions/" + f.id1 + suffix }
}

func ckBody(format string, pick func(*ckFix) []any) func(*ckFix) string {
	return func(f *ckFix) string { return fmt.Sprintf(format, pick(f)...) }
}

func codeOf1(f *ckFix) []any { return []any{f.code1} }

func runCkCases(t *testing.T, cases []ckCase) {
	t.Helper()

	for _, c := range cases {
		c := c

		var f *ckFix

		s := &tests.ApiScenario{
			Name: c.name, Method: c.method, ExpectedStatus: c.status,
			ExpectedContent: c.has, NotExpectedContent: c.hasNot, TestAppFactory: ckFactory,
		}

		if len(c.has) == 0 && len(c.hasNot) == 0 {
			s.ExpectedContent = []string{`"`}
		}

		if s.Method == "" {
			s.Method = http.MethodPost
		}

		s.BeforeTestFunc = func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
			f = ckSeed(t, app, !c.noCheckin)

			if c.pre != nil {
				c.pre(t, f)
			}

			s.URL = c.url(f)

			if c.body != nil {
				s.Body = strings.NewReader(c.body(f))
			}

			s.Headers = map[string]string{}

			switch {
			case c.who == "":
			case c.who == "super":
				s.Headers["Authorization"] = auditSuperuserToken
			case c.who == "kiosk":
				s.Headers[core.KioskTokenHeader] = f.kioskTok
			case c.who == "kiosk+session":
				s.Headers[core.KioskTokenHeader] = f.kioskTok
				s.Headers[core.KioskSessionHeader] = f.kioskSess
			case c.who == "kiosk-revoked":
				s.Headers[core.KioskTokenHeader] = f.revokedTok
			case strings.HasPrefix(c.who, "fresh:"):
				s.Headers["Authorization"] = f.fresh(t, strings.TrimPrefix(c.who, "fresh:"))
			default:
				s.Headers["Authorization"] = orgToken(t, c.who)
			}
		}

		s.AfterTestFunc = func(t testing.TB, app *tests.TestApp, res *http.Response) {
			if c.post != nil {
				c.post(t, f)
			}
		}

		s.Test(t)
	}
}

func ckSession(t testing.TB, f *ckFix, id string) *core.Record {
	t.Helper()

	r, err := f.app.FindRecordById(core.CheckinSessionCollection, id)
	orgMust(t, err)

	return r
}

func ckLogCount(t testing.TB, f *ckFix, session, action string) int {
	t.Helper()

	n, err := f.app.CountRecords(core.CheckinLogCollection, dbx.HashExp{"session": session, "action": action})
	orgMust(t, err)

	return int(n)
}

func ckAlerts(f *ckFix, kind string) int {
	rows, _ := f.app.FindRecordsByFilter(core.CollectionNameAuditLog, "action={:a}", "", 0, 0, dbx.Params{"a": core.AuditActionCheckinAlert})
	n := 0

	for _, r := range rows {
		var meta map[string]any
		_ = json.Unmarshal([]byte(r.GetString("meta")), &meta)

		if meta["kind"] == kind {
			n++
		}
	}

	return n
}

func ckInBody(household string, children ...string) func(*ckFix) string {
	raw, _ := json.Marshal(map[string]any{"household": household, "children": children})

	return lit(string(raw))
}

// ---- check in ---------------------------------------------------------------

func TestCheckinIn(t *testing.T) {
	in := lit("/api/checkin/" + ckEv1 + "/in")

	runCkCases(t, []ckCase{
		{name: "a guardian checks a child in", who: "erin", url: in, body: ckInBody(hhH1, hhKidA), noCheckin: true, status: 201,
			has:    []string{`"code":"`, `"flags":["allergy"]`, `"childName":"Kid A"`, `"qr":"`},
			hasNot: []string{"peanuts", "codeHash"},
			post: func(t testing.TB, f *ckFix) {
				rows, _ := f.app.FindAllRecords(core.CheckinSessionCollection)
				if len(rows) != 1 || rows[0].GetString("state") != "in" || rows[0].GetString("codeHash") == "" {
					t.Fatalf("expected one session in, got %d", len(rows))
				}

				if ckLogCount(t, f, rows[0].Id, "check_in") != 1 {
					t.Fatal("the check-in must be logged")
				}
			}},
		{name: "the other guardian of the household may too", who: "ivan", url: in, body: ckInBody(hhH1, hhKidA, ckKidC), noCheckin: true, status: 201,
			has: []string{`"childName":"Kid A"`, `"childName":"Kid C"`}},
		{name: "a non-guardian adult of the household is refused", who: "frank", url: in, body: ckInBody(hhH1, hhKidA), noCheckin: true, status: 403},
		{name: "a guardian of another household is refused", who: "dave", url: in, body: ckInBody(hhH1, hhKidA), noCheckin: true, status: 403},
		{name: "a child of another household is refused", who: "erin", url: in, body: ckInBody(hhH1, hhKidB), noCheckin: true, status: 400},
		{name: "a guardian is not a child", who: "erin", url: in, body: ckInBody(hhH1, hhIvan), noCheckin: true, status: 400},
		{name: "a closed event is refused", who: "erin", url: lit("/api/checkin/" + ckEvShut + "/in"), body: ckInBody(hhH1, hhKidA), noCheckin: true, status: 409},
		{name: "an event of another org is refused", who: "erin", url: lit("/api/checkin/" + ckEvP2 + "/in"), body: ckInBody(hhH1, hhKidA), noCheckin: true, status: 403},
		{name: "a child cannot be checked in twice", who: "erin", url: in, body: ckInBody(hhH1, hhKidA), status: 409},
		{name: "guests are refused", who: "", url: in, body: ckInBody(hhH1, hhKidA), noCheckin: true, status: 401},
		{name: "a superuser is not a guardian", who: "super", url: in, body: ckInBody(hhH1, hhKidA), noCheckin: true, status: 403},
		{name: "a kiosk without a kiosk session is refused", who: "kiosk", url: in, body: ckInBody(hhH1, hhKidA), noCheckin: true, status: 403},
		{name: "a kiosk with a guardian's session checks the family in", who: "kiosk+session", url: in, body: ckInBody(hhH1, hhKidA, ckKidC), noCheckin: true, status: 201,
			has: []string{`"childName":"Kid C"`}},
		{name: "a kiosk session is for one household", who: "kiosk+session", url: in, body: ckInBody(hhH2, hhKidB), noCheckin: true, status: 403},
		{name: "a revoked kiosk is refused", who: "kiosk-revoked", url: in, body: ckInBody(hhH1, hhKidA), noCheckin: true, status: 401},
		{name: "an expired kiosk session is refused", who: "kiosk+session", url: in, body: ckInBody(hhH1, hhKidA), noCheckin: true, status: 403,
			pre: func(t testing.TB, f *ckFix) {
				// the session of the fixture was cut for 5 minutes; a session of another kiosk does not match
				org, _ := f.app.FindRecordById(core.CollectionNameOrgs, orgP1)
				k, _, err := core.RegisterKiosk(f.app, context.Background(), org, f.user("bob"), "Other", nil)
				orgMust(t, err)

				h, _, err := core.IssueKioskHandoff(f.app, f.user("erin"), hhH1, k.Id)
				orgMust(t, err)

				f.kioskSess, _, err = core.ExchangeKioskHandoff(f.app, k, org, h)
				orgMust(t, err)
			}},
	})
}

func TestCheckinFamilyCodeIsOnePerFamilyPerEvent(t *testing.T) {
	app := ckFactory(t)
	defer app.Cleanup()

	ev, _ := app.FindRecordById(core.CheckinEventsCollection, ckEv1)
	f := &ckFix{app: app}
	ctx := context.Background()

	first, err := core.CheckIn(app, ctx, core.CheckinInput{Event: ev, Household: hhH1, Children: []string{hhKidA}, Guardian: f.user("erin"), Actor: core.CheckinActor{User: f.user("erin")}})
	orgMust(t, err)

	second, err := core.CheckIn(app, ctx, core.CheckinInput{Event: ev, Household: hhH1, Children: []string{ckKidC}, Guardian: f.user("ivan"), Actor: core.CheckinActor{User: f.user("ivan")}})
	orgMust(t, err)

	other, err := core.CheckIn(app, ctx, core.CheckinInput{Event: ev, Household: hhH2, Children: []string{hhKidB}, Guardian: f.user("dave"), Actor: core.CheckinActor{User: f.user("dave")}})
	orgMust(t, err)

	if first.Code != second.Code {
		t.Fatalf("one code per family per event: %q vs %q", first.Code, second.Code)
	}

	if first.Code == other.Code {
		t.Fatal("two families of one event share a code")
	}

	if len(first.Code) != 5 || strings.ContainsAny(first.Code, "01OILilo") {
		t.Fatalf("the code is 5 characters of the unambiguous alphabet: %q", first.Code)
	}

	// each child has its own QR token that maps to its own session only
	if first.Labels[0].QR == second.Labels[0].QR {
		t.Fatal("QR tokens must differ")
	}

	// only the HMAC is stored
	rows, _ := app.FindAllRecords(core.CheckinSessionCollection)
	for _, r := range rows {
		if strings.Contains(r.GetString("codeHash"), first.Code) || len(r.GetString("codeHash")) != 64 {
			t.Fatalf("the code must be stored as an HMAC: %q", r.GetString("codeHash"))
		}
	}
}

// ---- release ------------------------------------------------------------------

func TestCheckinRelease(t *testing.T) {
	released := func(method string) func(t testing.TB, f *ckFix) {
		return func(t testing.TB, f *ckFix) {
			s := ckSession(t, f, f.id1)
			if s.GetString("state") != "out" || s.GetString("method") != method || s.GetString("releasedAt") == "" {
				t.Fatalf("expected the child to be released by %s: %s %s", method, s.GetString("state"), s.GetString("method"))
			}

			if ckLogCount(t, f, f.id1, "release") != 1 {
				t.Fatal("the release must be logged")
			}
		}
	}
	untouched := func(t testing.TB, f *ckFix) {
		if s := ckSession(t, f, f.id1); s.GetString("state") != "in" {
			t.Fatalf("the child must stay in, got %s", s.GetString("state"))
		}
	}
	code := ckBody(`{"code":%q,"releasedTo":"Erin E"}`, codeOf1)

	runCkCases(t, []ckCase{
		{name: "staff release by code", who: "carol", url: ckSessionURL("/release"), body: code, status: 200, has: []string{`"state":"out"`, `"method":"code"`}, post: released("code")},
		{name: "a lowercase code with a dash still matches", who: "carol", url: ckSessionURL("/release"),
			body: func(f *ckFix) string { return fmt.Sprintf(`{"code":%q}`, strings.ToLower(f.code1[:2]+"-"+f.code1[2:])) }, status: 200},
		{name: "staff release by QR", who: "carol", url: ckSessionURL("/release"),
			body: func(f *ckFix) string { return fmt.Sprintf(`{"qr":%q,"releasedTo":"Erin E"}`, f.qr1) }, status: 200, has: []string{`"method":"qr"`}, post: released("qr")},
		{name: "an admin releases by code", who: "bob", url: ckSessionURL("/release"), body: code, status: 200},
		{name: "a wrong code is refused with the generic message", who: "carol", url: ckSessionURL("/release"), body: lit(`{"code":"ZZZZZ"}`), status: 403,
			has: []string{"The code could not be verified."}, post: untouched},
		{name: "the code of another family does not work", who: "carol", url: ckSessionURL("/release"),
			body: func(f *ckFix) string { return fmt.Sprintf(`{"code":%q}`, f.code2) }, status: 403, has: []string{"The code could not be verified."}, post: untouched},
		{name: "the QR of another child does not work", who: "carol", url: ckSessionURL("/release"),
			body: func(f *ckFix) string { return fmt.Sprintf(`{"qr":%q}`, f.id1+".aaaaaaaaaaaaaaaaaaaaaaaaaa") }, status: 403, post: untouched},
		{name: "an unknown session gives the same answer", who: "carol", url: lit("/api/checkin/sessions/nosuchsession00/release"), body: lit(`{"code":"ZZZZZ"}`), status: 403,
			has: []string{"The code could not be verified."}},
		{name: "a member without the tag cannot release by code", who: "ivan", url: ckSessionURL("/release"), body: code, status: 403, post: untouched},
		{name: "a viewer cannot release by code", who: "dave", url: ckSessionURL("/release"), body: code, status: 403, post: untouched},
		{name: "guests cannot release", who: "", url: ckSessionURL("/release"), body: code, status: 401},
		{name: "a kiosk releases by code", who: "kiosk", url: ckSessionURL("/release"), body: code, status: 200, post: released("code")},
		{name: "a revoked kiosk cannot release", who: "kiosk-revoked", url: ckSessionURL("/release"), body: code, status: 401, post: untouched},
		{name: "a closed event kills the code", who: "carol", url: ckSessionURL("/release"), body: code, status: 403, post: untouched,
			pre: func(t testing.TB, f *ckFix) {
				ev, _ := f.app.FindRecordById(core.CheckinEventsCollection, ckEv1)
				ev.Set("status", "closed")
				orgMust(t, f.app.Save(ev))
			}},
		{name: "a released child cannot be released again", who: "carol", url: ckSessionURL("/release"), body: code, status: 403,
			pre: func(t testing.TB, f *ckFix) {
				_, err := core.ReleaseSession(f.app, context.Background(), core.ReleaseInput{SessionId: f.id1, Code: f.code1, Actor: core.CheckinActor{User: f.user("carol")}})
				orgMust(t, err)
			}},
	})
}

func TestCheckinWrongCodeFiveTimesLocksAndAlerts(t *testing.T) {
	runCkCases(t, []ckCase{
		{name: "the fifth wrong code locks the session", who: "carol", url: ckSessionURL("/release"), body: lit(`{"code":"ZZZZZ"}`), status: 403,
			has: []string{"The code could not be verified."},
			pre: func(t testing.TB, f *ckFix) {
				for i := 0; i < core.CheckinMaxAttempts-1; i++ {
					_, err := core.ReleaseSession(f.app, context.Background(), core.ReleaseInput{SessionId: f.id1, Code: "ZZZZZ", Actor: core.CheckinActor{User: f.user("carol")}})
					if err == nil {
						t.Fatal("a wrong code must fail")
					}
				}
			},
			post: func(t testing.TB, f *ckFix) {
				s := ckSession(t, f, f.id1)
				if s.GetInt("failedAttempts") != core.CheckinMaxAttempts || s.GetDateTime("lockedAt").IsZero() {
					t.Fatalf("the session must be locked after five wrong codes: %d %v", s.GetInt("failedAttempts"), s.GetDateTime("lockedAt"))
				}

				if ckAlerts(f, "locked") != 1 || ckLogCount(t, f, f.id1, "locked") != 1 {
					t.Fatal("locking must raise exactly one alert and one log row")
				}

				// the right code no longer works, and says nothing different
				_, err := core.ReleaseSession(f.app, context.Background(), core.ReleaseInput{SessionId: f.id1, Code: f.code1, Actor: core.CheckinActor{User: f.user("carol")}})
				if err != core.ErrCheckinDenied {
					t.Fatalf("a locked session must give the generic refusal, got %v", err)
				}

				if s := ckSession(t, f, f.id1); s.GetString("state") != "in" {
					t.Fatal("a locked child stays in")
				}
			}},
		{name: "four wrong codes do not lock", who: "carol", url: ckSessionURL("/release"), body: ckBody(`{"code":%q}`, codeOf1), status: 200,
			pre: func(t testing.TB, f *ckFix) {
				for i := 0; i < core.CheckinMaxAttempts-1; i++ {
					_, _ = core.ReleaseSession(f.app, context.Background(), core.ReleaseInput{SessionId: f.id1, Code: "ZZZZZ", Actor: core.CheckinActor{User: f.user("carol")}})
				}
			}},
		{name: "a locked session can still be released by the guardian's passkey", who: "fresh:erin", url: ckSessionURL("/release"), body: lit(`{}`), status: 200,
			has: []string{`"method":"passkey"`},
			pre: func(t testing.TB, f *ckFix) {
				for i := 0; i < core.CheckinMaxAttempts; i++ {
					_, _ = core.ReleaseSession(f.app, context.Background(), core.ReleaseInput{SessionId: f.id1, Code: "ZZZZZ", Actor: core.CheckinActor{User: f.user("carol")}})
				}
			}},
	})
}

func TestCheckinReleaseByPasskeyStepUp(t *testing.T) {
	runCkCases(t, []ckCase{
		{name: "a guardian with a fresh passkey releases their child", who: "fresh:erin", url: ckSessionURL("/release"), body: lit(`{}`), status: 200,
			has: []string{`"method":"passkey"`}, post: func(t testing.TB, f *ckFix) {
				s := ckSession(t, f, f.id1)
				if s.GetString("state") != "out" || s.GetString("releasedTo") != hhErin || s.GetString("releasedBy") != orgUserID("erin") {
					t.Fatalf("unexpected release: %s to %q by %q", s.GetString("state"), s.GetString("releasedTo"), s.GetString("releasedBy"))
				}
			}},
		{name: "the other guardian too", who: "fresh:ivan", url: ckSessionURL("/release"), body: lit(`{}`), status: 200},
		{name: "without the step-up the passkey path asks for it", who: "erin", url: ckSessionURL("/release"), body: lit(`{}`), status: 403,
			has: []string{apis.StepUpRequiredCode}, post: func(t testing.TB, f *ckFix) {
				if ckSession(t, f, f.id1).GetString("state") != "in" {
					t.Fatal("the child must stay in")
				}
			}},
		{name: "a guardian of another household is refused", who: "fresh:dave", url: ckSessionURL("/release"), body: lit(`{}`), status: 403,
			has: []string{"The code could not be verified."}},
		{name: "an adult of the household who is not a guardian is refused", who: "fresh:frank", url: ckSessionURL("/release"), body: lit(`{}`), status: 403},
		{name: "an approved person with an account is released the child", who: "fresh:frank", url: ckSessionURL("/release"), body: lit(`{}`), status: 200,
			pre: func(t testing.TB, f *ckFix) { ckAuthorize(t, f, "frank", "Frank E", time.Now().Add(24*time.Hour)) }},
		{name: "an expired authorization does not count", who: "fresh:frank", url: ckSessionURL("/release"), body: lit(`{}`), status: 403,
			pre: func(t testing.TB, f *ckFix) { ckAuthorize(t, f, "frank", "Frank E", time.Now().Add(-time.Hour)) }},
		{name: "a staff code login cannot use the passkey path to release others", who: "fresh:carol", url: ckSessionURL("/release"), body: lit(`{}`), status: 403},
	})
}

func ckAuthorize(t testing.TB, f *ckFix, who, name string, until time.Time) {
	t.Helper()

	col, err := f.app.FindCollectionByNameOrId(core.PickupAuthCollection)
	orgMust(t, err)

	r := core.NewRecord(col)
	r.Set("household", hhH1)
	r.Set("child", hhKidA)
	r.Set("user", orgUserID(who))
	r.Set("name", name)
	r.Set("relationship", "uncle")
	r.Set("validUntil", until)
	r.Set("approvedBy", hhErin)
	r.Set("status", "active")
	orgMust(t, f.app.Save(r))
}

// ---- override, do-not-release, incident ------------------------------------------------

func TestCheckinOverrideAndDoNotRelease(t *testing.T) {
	flag := func(people ...string) func(t testing.TB, f *ckFix) {
		return func(t testing.TB, f *ckFix) {
			_, err := core.SetCheckinFlag(f.app, context.Background(), f.user("bob"), hhKidA, people, "Court order, see the coordinator.")
			orgMust(t, err)
		}
	}
	stays := func(t testing.TB, f *ckFix) {
		if ckSession(t, f, f.id1).GetString("state") != "in" {
			t.Fatal("the child must stay in")
		}
	}

	runCkCases(t, []ckCase{
		{name: "a coordinator overrides with a typed reason", who: "bob", url: ckSessionURL("/release"),
			body: lit(`{"override":true,"reason":"Parent lost the slip, ID checked at the desk","releasedTo":"Erin E"}`), status: 200,
			has: []string{`"method":"override"`}, post: func(t testing.TB, f *ckFix) {
				if ckAlerts(f, "override") != 1 {
					t.Fatal("an override must raise an alert")
				}

				if ckSession(t, f, f.id1).GetString("method") != "override" {
					t.Fatal("the method must be override")
				}
			}},
		{name: "an override needs a reason", who: "bob", url: ckSessionURL("/release"), body: lit(`{"override":true,"reason":"no","releasedTo":"Erin E"}`), status: 400, post: stays},
		{name: "plain staff cannot override", who: "carol", url: ckSessionURL("/release"),
			body: lit(`{"override":true,"reason":"Parent lost the slip, ID checked","releasedTo":"Erin E"}`), status: 403, post: stays},
		{name: "a do-not-release flag naming the person blocks the code", who: "carol", url: ckSessionURL("/release"),
			body: func(f *ckFix) string { return fmt.Sprintf(`{"code":%q,"releasedTo":"Grandpa Joe"}`, f.code1) }, status: 403,
			has: []string{"cannot be released now"}, pre: flag("grandpa joe"), post: func(t testing.TB, f *ckFix) {
				stays(t, f)

				if ckAlerts(f, "do_not_release") != 1 {
					t.Fatal("a blocked release must raise an alert")
				}
			}},
		{name: "a person the flag does not name is released", who: "carol", url: ckSessionURL("/release"),
			body: func(f *ckFix) string { return fmt.Sprintf(`{"code":%q,"releasedTo":"Erin E"}`, f.code1) }, status: 200, pre: flag("Grandpa Joe")},
		{name: "a flag naming people blocks a release that names nobody", who: "carol", url: ckSessionURL("/release"), body: codeBody, status: 403, pre: flag("Grandpa Joe"), post: stays},
		{name: "a flag with no names blocks everyone, a guardian's passkey too", who: "fresh:erin", url: ckSessionURL("/release"), body: lit(`{}`), status: 403,
			has: []string{"cannot be released now"}, pre: flag(), post: stays},
		{name: "a flag naming the guardian blocks the guardian", who: "fresh:ivan", url: ckSessionURL("/release"), body: lit(`{}`), status: 403, pre: flag("Ivan E"), post: stays},
		{name: "an override cannot beat the flag", who: "bob", url: ckSessionURL("/release"),
			body: lit(`{"override":true,"reason":"Parent lost the slip, ID checked","releasedTo":"Grandpa Joe"}`), status: 403, pre: flag("Grandpa Joe"), post: stays},
		{name: "a lifted flag no longer blocks", who: "carol", url: ckSessionURL("/release"),
			body: func(f *ckFix) string { return fmt.Sprintf(`{"code":%q,"releasedTo":"Grandpa Joe"}`, f.code1) }, status: 200,
			pre: func(t testing.TB, f *ckFix) {
				flag("Grandpa Joe")(t, f)

				rows, _ := f.app.FindAllRecords(core.CheckinFlagsCollection)
				_, err := core.LiftCheckinFlag(f.app, context.Background(), f.user("bob"), rows[0].Id)
				orgMust(t, err)
			}},
	})
}

func codeBody(f *ckFix) string { return fmt.Sprintf(`{"code":%q}`, f.code1) }

func TestCheckinFlagRoutes(t *testing.T) {
	runCkCases(t, []ckCase{
		{name: "a coordinator sets a flag", who: "bob", url: lit("/api/checkin/flags"), body: lit(`{"child":"` + hhKidA + `","people":["X"],"reason":"Custody order"}`), status: 201,
			has: []string{`"active":true`}, hasNot: []string{"Custody"}, post: func(t testing.TB, f *ckFix) {
				rows, _ := f.app.FindAllRecords(core.CheckinFlagsCollection)
				if len(rows) != 1 {
					t.Fatal("one flag expected")
				}

				// the reason is encrypted at rest
				var raw string
				orgMust(t, f.app.DB().NewQuery("SELECT reason FROM {{"+core.CheckinFlagsCollection+"}}").Row(&raw))

				if strings.Contains(raw, "Custody") {
					t.Fatalf("the reason must be encrypted at rest: %q", raw)
				}
			}},
		{name: "plain staff cannot set a flag", who: "carol", url: lit("/api/checkin/flags"), body: lit(`{"child":"` + hhKidA + `","reason":"x"}`), status: 403},
		{name: "a guardian cannot set a flag", who: "erin", url: lit("/api/checkin/flags"), body: lit(`{"child":"` + hhKidA + `","reason":"x"}`), status: 403},
		{name: "a flag needs a reason", who: "bob", url: lit("/api/checkin/flags"), body: lit(`{"child":"` + hhKidA + `","reason":""}`), status: 400},
	})
}

func TestCheckinIncidentHold(t *testing.T) {
	runCkCases(t, []ckCase{
		{name: "a coordinator holds the session and its log", who: "fresh:bob", url: ckSessionURL("/incident"), body: lit(`{"reason":"Child released to the wrong person"}`), status: 200,
			has: []string{`"incident":true`}, post: func(t testing.TB, f *ckFix) {
				s := ckSession(t, f, f.id1)
				if !s.GetBool("incident") {
					t.Fatal("the session must be flagged")
				}

				if held, err := core.IsRecordLegallyHeld(f.app, s); err != nil || !held {
					t.Fatalf("the session must be under a legal hold: %v %v", held, err)
				}

				logs, _ := f.app.FindRecordsByFilter(core.CheckinLogCollection, "session={:s}", "", 0, 0, dbx.Params{"s": f.id1})
				if len(logs) == 0 {
					t.Fatal("expected log rows")
				}

				for _, l := range logs {
					if held, _ := core.IsRecordLegallyHeld(f.app, l); !held {
						t.Fatalf("log row %s must be held", l.Id)
					}
				}

				// nothing deletes a held session
				if err := f.app.Delete(s); err == nil {
					t.Fatal("a held session must not be deletable")
				}
			}},
		{name: "without a fresh passkey", who: "bob", url: ckSessionURL("/incident"), body: lit(`{"reason":"x"}`), status: 403, has: []string{apis.StepUpRequiredCode}},
		{name: "plain staff cannot", who: "fresh:carol", url: ckSessionURL("/incident"), body: lit(`{"reason":"x"}`), status: 404},
	})
}

// ---- kiosks ----------------------------------------------------------------

func TestKioskRegistrationAndRevocation(t *testing.T) {
	runCkCases(t, []ckCase{
		{name: "an org admin registers a kiosk with a fresh passkey; the token is shown once", who: "fresh:bob", url: lit("/api/checkin/kiosks"),
			body: lit(`{"org":"` + orgP1 + `","name":"Nursery door","allowedEvents":["` + ckEv1 + `"]}`), status: 201,
			has: []string{`"token":"clvk_`, `"status":"active"`}, hasNot: []string{"deviceTokenHash"},
			post: func(t testing.TB, f *ckFix) {
				rows, _ := f.app.FindRecordsByFilter(core.CollectionNameKiosks, "name='Nursery door'", "", 0, 0)
				if len(rows) != 1 || len(rows[0].GetString("deviceTokenHash")) != 64 || rows[0].GetString("registeredBy") != orgUserID("bob") {
					t.Fatalf("unexpected kiosk row: %d", len(rows))
				}
			}},
		{name: "without a fresh passkey", who: "bob", url: lit("/api/checkin/kiosks"), body: lit(`{"org":"` + orgP1 + `","name":"x"}`), status: 403, has: []string{apis.StepUpRequiredCode}},
		{name: "staff who are not admins cannot register", who: "fresh:carol", url: lit("/api/checkin/kiosks"), body: lit(`{"org":"` + orgP1 + `","name":"x"}`), status: 404},
		{name: "an admin of another org cannot", who: "fresh:ivan", url: lit("/api/checkin/kiosks"), body: lit(`{"org":"` + orgP1 + `","name":"x"}`), status: 404},
		{name: "an allowed event of another org is refused", who: "fresh:bob", url: lit("/api/checkin/kiosks"),
			body: lit(`{"org":"` + orgP1 + `","name":"x","allowedEvents":["` + ckEvP2 + `"]}`), status: 400},
		{name: "an admin lists the kiosks without any hash", who: "bob", method: http.MethodGet, url: lit("/api/checkin/kiosks?org=" + orgP1), status: 200,
			has: []string{"Door 1", "Lost tablet"}, hasNot: []string{"deviceTokenHash", "clvk_"}},
		{name: "staff cannot list kiosks", who: "carol", method: http.MethodGet, url: lit("/api/checkin/kiosks?org=" + orgP1), status: 404},
		{name: "an admin revokes a kiosk and it stops working at once", who: "bob", url: func(f *ckFix) string { return "/api/checkin/kiosks/" + f.kioskId + "/revoke" }, status: 200,
			has: []string{`"status":"revoked"`}, post: func(t testing.TB, f *ckFix) {
				if _, _, err := core.AuthenticateKiosk(f.app, f.kioskTok); err == nil {
					t.Fatal("a revoked kiosk must not authenticate")
				}
			}},
		{name: "staff cannot revoke", who: "carol", url: func(f *ckFix) string { return "/api/checkin/kiosks/" + f.kioskId + "/revoke" }, status: 404},
	})
}

func TestKioskTokenIsARestrictedPrincipal(t *testing.T) {
	runCkCases(t, []ckCase{
		{name: "the records API does not know the kiosk: no sessions listed", who: "kiosk", method: http.MethodGet, url: lit("/api/collections/" + core.CheckinSessionCollection + "/records"),
			status: 200, has: []string{`"totalItems":0`}, hasNot: []string{"codeHash", "Kid"}},
		{name: "as a bearer token it is no session either", who: "", method: http.MethodGet, url: lit("/api/collections/" + core.CheckinSessionCollection + "/records"),
			status: 200, has: []string{`"totalItems":0`}},
		{name: "kiosk rows are not readable through the records API", who: "kiosk", method: http.MethodGet, url: lit("/api/collections/" + core.CollectionNameKiosks + "/records"), status: 403},
		{name: "the kiosk cannot register kiosks", who: "kiosk", url: lit("/api/checkin/kiosks"), body: lit(`{"org":"` + orgP1 + `","name":"x"}`), status: 401},
		{name: "the kiosk cannot set flags", who: "kiosk", url: lit("/api/checkin/flags"), body: lit(`{"child":"` + hhKidA + `","reason":"x"}`), status: 401},
		{name: "the kiosk cannot list households", who: "kiosk", method: http.MethodGet, url: lit("/api/households"), status: 401},
		{name: "the kiosk cannot use another org's event", who: "kiosk+session", url: lit("/api/checkin/" + ckEvP2 + "/in"), body: ckInBody(hhH1, hhKidA), noCheckin: true, status: 403},
		{name: "a kiosk limited to one event cannot serve another", who: "kiosk+session", url: lit("/api/checkin/" + ckEv1 + "/in"), body: ckInBody(hhH1, hhKidA), noCheckin: true, status: 403,
			pre: func(t testing.TB, f *ckFix) {
				k, _ := f.app.FindRecordById(core.CollectionNameKiosks, f.kioskId)
				k.Set("allowedEvents", []string{ckEvShut})
				orgMust(t, f.app.Save(k))
			}},
		{name: "a kiosk of another org cannot release a session of this one", who: "kiosk", url: ckSessionURL("/release"), body: ckBody(`{"code":%q}`, codeOf1), status: 403,
			pre: func(t testing.TB, f *ckFix) {
				k, _ := f.app.FindRecordById(core.CollectionNameKiosks, f.kioskId)
				k.Set("org", orgP2)
				orgMust(t, f.app.Save(k))
			}},
		{name: "a garbage kiosk token is a 401", who: "", url: ckSessionURL("/release"), body: codeBodyLit, status: 401},
	})
}

func codeBodyLit(f *ckFix) string { return `{"code":"ABCDE"}` }

func TestKioskHandoffAndSession(t *testing.T) {
	app := ckFactory(t)
	defer app.Cleanup()

	f := ckSeed(t, app, false)
	org, _ := app.FindRecordById(core.CollectionNameOrgs, orgP1)
	kiosk, _ := app.FindRecordById(core.CollectionNameKiosks, f.kioskId)

	// a non-guardian cannot ask for a handoff
	if _, _, err := core.IssueKioskHandoff(app, f.user("frank"), hhH1, kiosk.Id); err == nil {
		t.Fatal("a non-guardian must not get a handoff")
	}

	h, _, err := core.IssueKioskHandoff(app, f.user("erin"), hhH1, kiosk.Id)
	orgMust(t, err)

	if _, _, err := core.ExchangeKioskHandoff(app, kiosk, org, h); err != nil {
		t.Fatal(err)
	}

	// a handoff works once
	if _, _, err := core.ExchangeKioskHandoff(app, kiosk, org, h); err == nil {
		t.Fatal("a handoff must be single use")
	}

	// a handoff is for one kiosk
	other, _, err := core.RegisterKiosk(app, context.Background(), org, f.user("bob"), "Other", nil)
	orgMust(t, err)

	h2, _, err := core.IssueKioskHandoff(app, f.user("erin"), hhH1, kiosk.Id)
	orgMust(t, err)

	if _, _, err := core.ExchangeKioskHandoff(app, other, org, h2); err == nil {
		t.Fatal("a handoff must not work on another kiosk")
	}

	// a tampered token fails
	if _, err := core.VerifyKioskSession(app, kiosk, f.kioskSess+"x"); err == nil {
		t.Fatal("a tampered session must fail")
	}

	// a session is short: the org's setting (5 minutes by default) bounds it
	if ttl := core.KioskSessionTTL(org); ttl != 5*time.Minute {
		t.Fatalf("default kiosk session is 5 minutes, got %v", ttl)
	}

	org.Set("settings", map[string]any{core.KioskSessionSetting: 2})
	if ttl := core.KioskSessionTTL(org); ttl != 2*time.Minute {
		t.Fatalf("the org setting must apply, got %v", ttl)
	}
}

func TestKioskSessionRoute(t *testing.T) {
	var handoff string

	s := &tests.ApiScenario{
		Name: "a kiosk exchanges a guardian's handoff for a session", Method: http.MethodPost, URL: "/api/checkin/kiosk/sessions",
		ExpectedStatus: 200, ExpectedContent: []string{`"session":"`, `"expires":"`}, TestAppFactory: ckFactory,
	}
	s.BeforeTestFunc = func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
		f := ckSeed(t, app, false)

		var err error

		handoff, _, err = core.IssueKioskHandoff(app, f.user("erin"), hhH1, f.kioskId)
		orgMust(t, err)

		s.Body = strings.NewReader(fmt.Sprintf(`{"handoff":%q}`, handoff))
		s.Headers = map[string]string{core.KioskTokenHeader: f.kioskTok}
	}
	s.Test(t)

	s2 := &tests.ApiScenario{
		Name: "a user cannot open a kiosk session", Method: http.MethodPost, URL: "/api/checkin/kiosk/sessions",
		ExpectedStatus: 401, ExpectedContent: []string{`"`}, TestAppFactory: ckFactory,
		Headers: map[string]string{"Authorization": ""}, Body: strings.NewReader(`{"handoff":"x"}`),
	}
	s2.Test(t)

	runCkCases(t, []ckCase{
		{name: "the handoff route needs a fresh passkey", who: "erin", url: lit("/api/checkin/handoffs"), body: func(f *ckFix) string {
			return fmt.Sprintf(`{"household":%q,"kiosk":%q}`, hhH1, f.kioskId)
		}, status: 403, has: []string{apis.StepUpRequiredCode}},
		{name: "a guardian with a fresh passkey gets a handoff", who: "fresh:erin", url: lit("/api/checkin/handoffs"), body: func(f *ckFix) string {
			return fmt.Sprintf(`{"household":%q,"kiosk":%q}`, hhH1, f.kioskId)
		}, status: 200, has: []string{`"handoff":"`}},
		{name: "a non-guardian does not", who: "fresh:frank", url: lit("/api/checkin/handoffs"), body: func(f *ckFix) string {
			return fmt.Sprintf(`{"household":%q,"kiosk":%q}`, hhH1, f.kioskId)
		}, status: 403},
	})
}

// ---- the log is append-only -----------------------------------------------------

func TestCheckinLogIsAppendOnly(t *testing.T) {
	app := ckFactory(t)
	defer app.Cleanup()

	f := ckSeed(t, app, true)
	rows, err := app.FindRecordsByFilter(core.CheckinLogCollection, "session={:s}", "", 0, 0, dbx.Params{"s": f.id1})
	orgMust(t, err)

	if len(rows) == 0 {
		t.Fatal("expected a log row")
	}

	row := rows[0]
	row.Set("action", "edited")

	if err := app.Save(row); err == nil {
		t.Fatal("updating the log must be refused for code")
	}

	if err := app.Delete(row); err == nil {
		t.Fatal("deleting from the log must be refused for code")
	}

	mux := ckMux(t, app)

	// the records API: the log is read-only for staff, and refused for writes even for a superuser
	for _, c := range []struct {
		who, method, body string
	}{
		{"super", http.MethodPatch, `{"action":"edited"}`},
		{"super", http.MethodDelete, ``},
		{"bob", http.MethodPatch, `{"action":"edited"}`},
		{"bob", http.MethodDelete, ``},
		{"carol", http.MethodPatch, `{"action":"edited"}`},
	} {
		url := "/api/collections/" + core.CheckinLogCollection + "/records/" + row.Id

		runOnMux(t, mux, c.method, url, c.body, c.who, func(code int) {
			if code < 400 || code >= 500 {
				t.Errorf("%s %s: expected a 4xx, got %d", c.who, c.method, code)
			}
		})
	}

	if got, err := app.FindRecordById(core.CheckinLogCollection, row.Id); err != nil || got.GetString("action") == "edited" {
		t.Fatal("the log row must be unchanged")
	}

	// raw creates through the records API are refused to everyone but superusers (null rules)
	runOnMux(t, mux, http.MethodPost, "/api/collections/"+core.CheckinLogCollection+"/records", `{"org":"`+orgP1+`","action":"forged"}`, "bob", func(code int) {
		if code < 400 || code >= 500 {
			t.Errorf("an admin must not create log rows, got %d", code)
		}
	})
}

// ckMux builds the router of an existing app once.
func ckMux(t testing.TB, app *tests.TestApp) http.Handler {
	t.Helper()

	router, err := apis.NewRouter(app)
	orgMust(t, err)

	var mux http.Handler

	orgMust(t, app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(e *core.ServeEvent) error {
		var err error

		mux, err = e.Router.BuildMux()

		return err
	}))

	return mux
}

// runOnMux sends one request and reports the status.
func runOnMux(t testing.TB, mux http.Handler, method, url, body, who string, check func(code int)) {
	t.Helper()

	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")

	for k, v := range orgAuth(t, who) {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	check(rec.Code)
}

// ---- template, rules, retention --------------------------------------------------

func TestCheckinInstall(t *testing.T) {
	probe := func(name, who string, status int, has ...string) tests.ApiScenario {
		return tests.ApiScenario{
			Name: name, Method: http.MethodPost, URL: "/api/checkin/install", Body: strings.NewReader(`{"orgs":["` + orgP1 + `"]}`),
			Headers: orgAuth(t, who), ExpectedStatus: status, ExpectedContent: append(has, `"`),
			TestAppFactory: func(tb testing.TB) *tests.TestApp {
				tb.Setenv(core.CheckinKeyEnv, "unit-test-checkin-key-0123456789")
				hhKEK(tb)

				return hhFactory(tb)
			},
		}
	}

	for _, s := range []tests.ApiScenario{
		probe("a superuser installs the template", "super", 201, `"checkin_sessions"`, `"checkin"`, `"checkin-supervisor"`),
		probe("an org admin cannot", "bob", 403),
		probe("guests cannot", "", 401),
	} {
		s := s
		s.Test(t)
	}

	// installing twice is refused
	app := ckFactory(t)
	defer app.Cleanup()

	if _, err := core.CreateCheckinTemplates(app, context.Background()); err == nil {
		t.Fatal("a second install must be refused")
	}

	// every new collection is classified in the data map (#88)
	unmapped, err := core.UnmappedCollections(app)
	orgMust(t, err)

	for _, u := range unmapped {
		if strings.HasPrefix(u.Name, "checkin_") || u.Name == core.PickupAuthCollection || u.Name == core.CollectionNameKiosks {
			t.Fatalf("%s is not in the data map", u.Name)
		}
	}

	// the retention policies exist, disabled and as dry runs, on the purgeAfter date
	pols, _ := app.FindAllRecords(core.CollectionNameRetentionPolicies)
	if len(pols) != 2 {
		t.Fatalf("expected two retention policies, got %d", len(pols))
	}

	for _, p := range pols {
		if p.GetBool("enabled") || !p.GetBool("dryRun") || p.GetString("basis") != "purgeAfter" || p.GetString("action") != "delete" {
			t.Fatalf("unexpected policy: %v", p.FieldsData())
		}
	}
}

func TestCheckinRecordRules(t *testing.T) {
	sessions := lit("/api/collections/" + core.CheckinSessionCollection + "/records")

	runCkCases(t, []ckCase{
		{name: "a guardian sees the sessions of their household, without the hashes", who: "erin", method: http.MethodGet, url: sessions, status: 200,
			has: []string{`"totalItems":1`, `"state":"in"`, `"flags":["allergy"]`}, hasNot: []string{"codeHash", "qrHash", "peanuts"}},
		{name: "another household sees only its own", who: "dave", method: http.MethodGet, url: sessions, status: 200,
			has: []string{`"totalItems":1`}},
		{name: "staff see every session of the org", who: "carol", method: http.MethodGet, url: sessions, status: 200, has: []string{`"totalItems":2`}},
		{name: "a stranger sees none", who: "gina", method: http.MethodGet, url: sessions, status: 200, has: []string{`"totalItems":0`}},
		{name: "a guardian cannot create a session", who: "erin", url: sessions, status: 403, noCheckin: true,
			body: lit(`{"event":"` + ckEv1 + `","org":"` + orgP1 + `","household":"` + hhH1 + `","child":"` + hhKidA + `","state":"out","codeHash":"x"}`)},
		{name: "a guardian cannot release by editing the session", who: "erin", method: http.MethodPatch, status: 403,
			url: func(f *ckFix) string {
				return "/api/collections/" + core.CheckinSessionCollection + "/records/" + f.id1
			},
			body: lit(`{"state":"out"}`)},
		{name: "staff read the log", who: "carol", method: http.MethodGet, url: lit("/api/collections/" + core.CheckinLogCollection + "/records"), status: 200, has: []string{`"totalItems":2`}},
		{name: "a guardian does not read the log", who: "erin", method: http.MethodGet, url: lit("/api/collections/" + core.CheckinLogCollection + "/records"), status: 200, has: []string{`"totalItems":0`}},
		{name: "plain staff do not read the flags", who: "carol", method: http.MethodGet, url: lit("/api/collections/" + core.CheckinFlagsCollection + "/records"), status: 200, has: []string{`"totalItems":0`}},
		{name: "a guardian approves a pickup person for their own child", who: "erin", url: lit("/api/collections/" + core.PickupAuthCollection + "/records"), status: 200, noCheckin: true,
			body: lit(`{"household":"` + hhH1 + `","child":"` + hhKidA + `","name":"Aunt May","relationship":"aunt","approvedBy":"` + hhErin + `","status":"active","org":"` + orgP2 + `"}`),
			post: func(t testing.TB, f *ckFix) {
				rows, _ := f.app.FindAllRecords(core.PickupAuthCollection)
				if len(rows) != 1 || rows[0].GetString("org") != orgP1 {
					t.Fatalf("the org of a pickup authorization is the household's, got %d rows", len(rows))
				}
			}},
		{name: "not for a child of another household", who: "erin", url: lit("/api/collections/" + core.PickupAuthCollection + "/records"), status: 400, noCheckin: true,
			body: lit(`{"household":"` + hhH1 + `","child":"` + hhKidB + `","name":"Aunt May","status":"active"}`)},
		{name: "not by an adult who is not a guardian", who: "frank", url: lit("/api/collections/" + core.PickupAuthCollection + "/records"), status: 400, noCheckin: true,
			body: lit(`{"household":"` + hhH1 + `","child":"` + hhKidA + `","name":"Aunt May","status":"active"}`)},
	})
}

func TestCheckinRetention(t *testing.T) {
	app := ckFactory(t)
	defer app.Cleanup()

	f := ckSeed(t, app, true)
	s1 := ckSession(t, f, f.id1)

	// 12 months by default
	want := time.Now().AddDate(1, 0, 0)
	if d := s1.GetDateTime("purgeAfter").Time().Sub(want); d > time.Hour || d < -time.Hour {
		t.Fatalf("purgeAfter must be 12 months out, got %v", s1.GetDateTime("purgeAfter"))
	}

	// a per-org setting overrides it
	org, _ := app.FindRecordById(core.CollectionNameOrgs, orgP1)
	org.Set("settings", map[string]any{core.CheckinRetentionSetting: 3})
	orgMust(t, app.Save(org))

	ev, _ := app.FindRecordById(core.CheckinEventsCollection, ckEv1)
	res, err := core.CheckIn(app, context.Background(), core.CheckinInput{Event: ev, Household: hhH1, Children: []string{ckKidC}, Guardian: f.user("erin"), Actor: core.CheckinActor{User: f.user("erin")}})
	orgMust(t, err)

	s3 := ckSession(t, f, res.Labels[0].Session)
	if d := s3.GetDateTime("purgeAfter").Time().Sub(time.Now().AddDate(0, 3, 0)); d > time.Hour || d < -time.Hour {
		t.Fatalf("the org setting must give 3 months, got %v", s3.GetDateTime("purgeAfter"))
	}

	// the engine: a past purgeAfter deletes the session and its log, a held one stays
	past := time.Now().AddDate(-1, 0, 0)

	for _, id := range []string{f.id1, f.id2} {
		ckExec(t, app, "UPDATE {{"+core.CheckinSessionCollection+"}} SET purgeAfter={:p} WHERE id={:id}", dbx.Params{"p": past.UTC().Format("2006-01-02 15:04:05.000Z"), "id": id})
		ckExec(t, app, "UPDATE {{"+core.CheckinLogCollection+"}} SET purgeAfter={:p} WHERE session={:id}", dbx.Params{"p": past.UTC().Format("2006-01-02 15:04:05.000Z"), "id": id})
	}

	// session 1 is an incident
	_, err = core.PlaceCheckinIncidentHold(app, context.Background(), f.user("bob"), f.id1, "Wrong person took the child")
	orgMust(t, err)

	pols, _ := app.FindAllRecords(core.CollectionNameRetentionPolicies)
	for _, p := range pols {
		on, off := true, false
		_, err := core.UpdateRetentionPolicy(app, context.Background(), p.Id, core.RetentionPolicyInput{Enabled: &on, DryRun: &off})
		orgMust(t, err)
	}

	if _, err := core.RunRetention(context.Background(), app, core.RetentionRunOptions{}); err != nil {
		t.Fatal(err)
	}

	if _, err := app.FindRecordById(core.CheckinSessionCollection, f.id2); err == nil {
		t.Fatal("the expired session must be deleted")
	}

	if n, _ := app.CountRecords(core.CheckinLogCollection, dbx.HashExp{"session": f.id2}); n != 0 {
		t.Fatal("the log of the expired session must be deleted")
	}

	if _, err := app.FindRecordById(core.CheckinSessionCollection, f.id1); err != nil {
		t.Fatal("an incident session must survive retention")
	}

	if n, _ := app.CountRecords(core.CheckinLogCollection, dbx.HashExp{"session": f.id1}); n == 0 {
		t.Fatal("the log of an incident session must survive retention")
	}

	if _, err := app.FindRecordById(core.CheckinSessionCollection, res.Labels[0].Session); err != nil {
		t.Fatal("a session that has not expired must stay")
	}
}

func ckExec(t testing.TB, app *tests.TestApp, sql string, params dbx.Params) {
	t.Helper()

	if _, err := app.DB().NewQuery(sql).Bind(params).Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckinPersonalDataExportCoversTheTemplate(t *testing.T) {
	app := ckFactory(t)
	defer app.Cleanup()

	f := ckSeed(t, app, true)

	var buf bytes.Buffer

	manifest, err := core.WriteDataExport(app, f.user("erin"), &buf)
	orgMust(t, err)

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	orgMust(t, err)

	var all strings.Builder

	for _, zf := range zr.File {
		rc, err := zf.Open()
		orgMust(t, err)

		b, _ := io.ReadAll(rc)
		_ = rc.Close()

		all.WriteString(zf.Name + "\n" + string(b) + "\n")
	}

	raw := all.String()
	if manifest == nil || !strings.Contains(raw, core.CheckinSessionCollection+".json") {
		t.Fatalf("the guardian's export must include the check-in sessions they made: %v %v", manifest.Skipped, manifest.Errors)
	}

	for _, secret := range []string{"codeHash", f.code1, f.qr1} {
		if strings.Contains(raw, secret) {
			t.Fatalf("the export must not carry %q", secret)
		}
	}
}
