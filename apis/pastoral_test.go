package apis_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #86): pastoral notes, break-glass and held notes.
//
// Fixture: the org fixture with passkeys enabled. Notes live in P1 and are
// written by dave (a viewer with the clergy tag, so the rules do not lean on
// the role). alice owns P1, bob is an admin of P1, carol is a member with the
// care-team and breakglass tags, frank a member with breakglass and moderator,
// ivan a member with clergy (and owner of P2), gina is admin of the parent
// org, erin belongs to P2, sam is suspended.

type pastoralEnv struct {
	*ceremonyEnv
	notes map[string]string // label -> id
}

const pastoralCol = "pastoral_notes"

func newPastoralEnv(t *testing.T) *pastoralEnv {
	t.Helper()

	setEncKEK(t)

	env := newCeremonyEnv(t)
	seedOrgs(t, env.app)

	roles, err := env.app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
	orgMust(t, err)

	org, err := env.app.FindRecordById(core.CollectionNameOrgs, orgP1)
	orgMust(t, err)

	_, err = core.EnsurePastoralTags(env.app, context.Background(), org)
	orgMust(t, err)

	tagId := func(name string) string {
		r, err := env.app.FindFirstRecordByFilter(roles, "org={:o} && name={:n}", dbx.Params{"o": orgP1, "n": name})
		orgMust(t, err)

		return r.Id
	}

	members, _ := env.app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)

	addTags := func(who string, tags ...string) {
		m, err := core.FindOrgMember(env.app, orgP1, orgUserID(who))
		if err != nil {
			m = core.NewRecord(members)
			m.Set("org", orgP1)
			m.Set("user", orgUserID(who))
			m.Set("role", "member")
			m.Set("status", "active")
		}

		var ids []string
		for _, n := range tags {
			ids = append(ids, tagId(n))
		}

		m.Set("tags", ids)
		orgMust(t, env.app.Save(m))
	}

	addTags("dave", core.PastoralTagClergy)
	addTags("carol", core.PastoralTagCare, core.PastoralTagBreakGlass)
	addTags("frank", core.PastoralTagBreakGlass, core.PastoralTagModerator)
	addTags("ivan", core.PastoralTagClergy)

	col, err := core.CreatePastoralNotes(env.app, core.PastoralNotesOpts{})
	orgMust(t, err)

	pe := &pastoralEnv{ceremonyEnv: env, notes: map[string]string{}}

	mk := func(label, visibility string, sealed, archived bool) {
		r := core.NewRecord(col)
		r.Set("org", orgP1)
		r.Set("author", orgUserID("dave"))
		r.Set("visibility", visibility)
		r.Set("sealed", sealed)
		r.Set("archived", archived)
		r.Set("body", "secret "+label)
		orgMust(t, env.app.Save(r))

		pe.notes[label] = r.Id
	}

	mk("private", "private", false, false)
	mk("clergy", "clergy", false, false)
	mk("care", "care", false, false)
	mk("congregation", "congregation", false, false)
	mk("sealed", "clergy", true, false)
	mk("archived", "congregation", false, true)

	return pe
}

func (env *pastoralEnv) fresh(who string) string {
	env.t.Helper()

	u, err := env.app.FindRecordById("users", orgUserID(who))
	orgMust(env.t, err)

	tok, err := u.NewPasskeyAuthToken(time.Now(), true)
	orgMust(env.t, err)

	return tok
}

func (env *pastoralEnv) plain(who string) string { return orgToken(env.t, who) }

func (env *pastoralEnv) view(who, label string) (int, map[string]any) {
	tok := ""
	switch who {
	case "super":
		tok = auditSuperuserToken
	case "":
	default:
		tok = env.plain(who)
	}

	return env.do(http.MethodGet, "/api/collections/"+pastoralCol+"/records/"+env.notes[label], nil, tok)
}

func (env *pastoralEnv) audit(action string) []*core.Record {
	env.t.Helper()

	rows, err := env.app.FindRecordsByFilter(core.CollectionNameAuditLog, "action={:a}", "created", 0, 0, dbx.Params{"a": action})
	orgMust(env.t, err)

	return rows
}

func (env *pastoralEnv) request(who, label, reason string, hours int) (int, map[string]any) {
	return env.do(http.MethodPost, "/api/breakglass", map[string]any{
		"collection": pastoralCol, "recordId": env.notes[label], "reason": reason, "basis": "safety", "hours": hours,
	}, env.fresh(who))
}

const goodReason = "Parishioner may be at risk, the author cannot be reached tonight"

func TestPastoralRulesMatrix(t *testing.T) {
	env := newPastoralEnv(t)

	want := map[string]map[string]bool{
		// who -> label -> can read
		"dave":  {"private": true, "clergy": true, "care": true, "congregation": true, "sealed": true, "archived": true},
		"alice": {"congregation": true}, // org owner: no automatic right
		"bob":   {"congregation": true}, // org admin: no automatic right
		"carol": {"care": true, "congregation": true},
		"frank": {"congregation": true},
		"ivan":  {"clergy": true, "congregation": true},
		"gina":  {}, // admin of the parent org: no oversight preset
		"erin":  {},
		"sam":   {},
		"":      {},
	}

	for who, allowed := range want {
		for label := range env.notes {
			status, body := env.view(who, label)

			if allowed[label] {
				if status != http.StatusOK || body["body"] != "secret "+label {
					t.Fatalf("%q should read %s: %d %v", who, label, status, body)
				}
			} else if status != http.StatusNotFound && status != http.StatusUnauthorized {
				t.Fatalf("%q must not read %s: %d %v", who, label, status, body)
			}
		}
	}

	// the list rule is the same rule
	status, body := env.do(http.MethodGet, "/api/collections/"+pastoralCol+"/records?perPage=50", nil, env.plain("alice"))
	if status != http.StatusOK || body["totalItems"] != float64(1) {
		t.Fatalf("an owner lists only the congregation note: %d %v", status, body)
	}

	// create: any member, as author; not as someone else; not outside the org
	create := func(who, author, org string) int {
		status, _ := env.do(http.MethodPost, "/api/collections/"+pastoralCol+"/records", map[string]any{
			"org": org, "author": orgUserID(author), "visibility": "private", "body": "mine",
		}, env.plain(who))

		return status
	}

	if s := create("carol", "carol", orgP1); s != http.StatusOK {
		t.Fatalf("a member creates a note: %d", s)
	}

	if s := create("carol", "dave", orgP1); s == http.StatusOK {
		t.Fatal("a note cannot be written in somebody else's name")
	}

	if s := create("erin", "erin", orgP1); s == http.StatusOK {
		t.Fatal("a non-member cannot create a note")
	}

	// update and delete: the author only
	patch := func(who, label string, body map[string]any) int {
		status, _ := env.do(http.MethodPatch, "/api/collections/"+pastoralCol+"/records/"+env.notes[label], body, env.plain(who))

		return status
	}

	if s := patch("dave", "congregation", map[string]any{"body": "edited"}); s != http.StatusOK {
		t.Fatalf("the author edits: %d", s)
	}

	for _, who := range []string{"alice", "bob", "carol", "ivan"} {
		if s := patch(who, "congregation", map[string]any{"body": "hijack"}); s == http.StatusOK {
			t.Fatalf("%s must not edit another author's note", who)
		}

		if s := patch(who, "congregation", map[string]any{"visibility": "private"}); s == http.StatusOK {
			t.Fatalf("%s must not change the visibility", who)
		}
	}

	// the moderator may archive, and nothing else; the response has no body
	if s := patch("frank", "congregation", map[string]any{"body": "x"}); s == http.StatusOK {
		t.Fatal("a moderator must not change the text")
	}

	if s := patch("frank", "congregation", map[string]any{"archived": true, "visibility": "private"}); s == http.StatusOK {
		t.Fatal("a moderator must not change anything but archived")
	}

	status, body = env.do(http.MethodPatch, "/api/collections/"+pastoralCol+"/records/"+env.notes["congregation"],
		map[string]any{"archived": true}, env.plain("frank"))
	if status != http.StatusOK || body["archived"] != true {
		t.Fatalf("the moderator archives: %d %v", status, body)
	}

	if _, has := body["body"]; has {
		t.Fatalf("the moderator's response must not carry the text: %v", body)
	}

	if s := patch("carol", "care", map[string]any{"archived": true}); s == http.StatusOK {
		t.Fatal("only a moderator or the author archives")
	}

	// an archived note leaves everybody but the author
	if s, _ := env.view("alice", "congregation"); s != http.StatusNotFound {
		t.Fatalf("archived: %d", s)
	}

	if s, _ := env.do(http.MethodDelete, "/api/collections/"+pastoralCol+"/records/"+env.notes["care"], nil, env.plain("alice")); s == http.StatusNoContent {
		t.Fatal("an owner cannot delete a note")
	}

	if s, _ := env.do(http.MethodDelete, "/api/collections/"+pastoralCol+"/records/"+env.notes["care"], nil, env.plain("dave")); s != http.StatusNoContent {
		t.Fatalf("the author deletes: %d", s)
	}
}

func TestPastoralTemplateRoute(t *testing.T) {
	setEncKEK(t)

	env := newCeremonyEnv(t)
	seedOrgs(t, env.app)

	su := auditSuperuserToken

	if s, _ := env.do(http.MethodPost, "/api/pastoral", map[string]any{"name": "pn"}, orgToken(t, "alice")); s != http.StatusForbidden {
		t.Fatalf("only a superuser makes the template: %d", s)
	}

	// a looser rule is refused without the override
	loose := "@request.auth.id != \"\""
	s, body := env.do(http.MethodPost, "/api/pastoral", map[string]any{"name": "pn", "rules": map[string]any{"listRule": loose}}, su)
	if s != http.StatusBadRequest || !strings.Contains(toJSON(body), "allowLooseRules") {
		t.Fatalf("a looser rule needs the override: %d %v", s, body)
	}

	if _, err := env.app.FindCollectionByNameOrId("pn"); err == nil {
		t.Fatal("nothing must be created when the rule is refused")
	}

	s, body = env.do(http.MethodPost, "/api/pastoral", map[string]any{"name": "pn", "orgs": []string{orgP1}}, su)
	if s != http.StatusCreated {
		t.Fatalf("create: %d %v", s, body)
	}

	want := core.PastoralRules("pn")
	if body["listRule"] != *want.List || body["updateRule"] != *want.Update {
		t.Fatalf("the template carries the locked rules: %v", body)
	}

	if !strings.Contains(*want.List, "_breakGlass") || strings.Contains(*want.List, "admin") || strings.Contains(*want.List, "owner") {
		t.Fatalf("the rule has the break-glass clause and no role clause: %s", *want.List)
	}

	for _, tag := range core.PastoralTags {
		if _, err := env.app.FindFirstRecordByFilter(core.CollectionNameOrgRoles, "org={:o} && name={:n}", dbx.Params{"o": orgP1, "n": tag}); err != nil {
			t.Fatalf("tag %s was not created", tag)
		}
	}

	if s, _ := env.do(http.MethodPost, "/api/pastoral", map[string]any{"name": "pn"}, su); s != http.StatusBadRequest {
		t.Fatalf("a second collection of the same name: %d", s)
	}

	// the override lets a looser rule through
	if s, _ := env.do(http.MethodPost, "/api/pastoral", map[string]any{"name": "pn2", "allowLooseRules": true, "rules": map[string]any{"listRule": loose}}, su); s != http.StatusCreated {
		t.Fatalf("override: %d", s)
	}

	// a null rule (superusers only) is stricter and needs no override
	if s, _ := env.do(http.MethodPost, "/api/pastoral", map[string]any{"name": "pn3", "rules": map[string]any{}}, su); s != http.StatusCreated {
		t.Fatalf("null rules: %d", s)
	}

	// the template is in the data map
	if _, err := env.app.FindFirstRecordByData(core.CollectionNameDataMap, "collection", "pn"); err != nil {
		t.Fatalf("the template is not in the data map: %v", err)
	}
}

func TestBreakGlassTwoPersonFlow(t *testing.T) {
	env := newPastoralEnv(t)
	sealed := env.notes["sealed"]

	// step-up is required to ask
	status, body := env.do(http.MethodPost, "/api/breakglass", map[string]any{
		"collection": pastoralCol, "recordId": sealed, "reason": goodReason, "basis": "safety",
	}, env.plain("frank"))
	if status != http.StatusForbidden || !strings.Contains(toJSON(body), "passkey_step_up_required") {
		t.Fatalf("the request needs a fresh passkey: %d %v", status, body)
	}

	// who may ask
	if s, _ := env.request("bob", "sealed", goodReason, 0); s != http.StatusForbidden {
		t.Fatalf("an admin without the tag: %d", s)
	}

	if s, _ := env.request("alice", "sealed", goodReason, 0); s != http.StatusForbidden {
		t.Fatalf("an owner without the tag: %d", s)
	}

	if s, _ := env.request("erin", "sealed", goodReason, 0); s != http.StatusNotFound {
		t.Fatalf("an outsider must not learn the note exists: %d", s)
	}

	if s, _ := env.request("frank", "sealed", "too short", 0); s != http.StatusBadRequest {
		t.Fatalf("a short reason: %d", s)
	}

	if s, _ := env.request("frank", "sealed", goodReason, 25); s != http.StatusBadRequest {
		t.Fatalf("more than 24 hours: %d", s)
	}

	status, _ = env.do(http.MethodPost, "/api/breakglass", map[string]any{
		"collection": pastoralCol, "recordId": sealed, "reason": goodReason, "basis": "nonsense",
	}, env.fresh("frank"))
	if status != http.StatusBadRequest {
		t.Fatalf("a bad basis: %d", status)
	}

	status, body = env.request("frank", "sealed", goodReason, 0)
	if status != http.StatusCreated || body["state"] != "pending" {
		t.Fatalf("request: %d %v", status, body)
	}

	grant := body["id"].(string)

	if s, _ := env.request("frank", "sealed", goodReason, 0); s != http.StatusConflict {
		t.Fatalf("a second open request: %d", s)
	}

	// a pending request opens nothing
	if s, _ := env.view("frank", "sealed"); s != http.StatusNotFound {
		t.Fatalf("pending grants nothing: %d", s)
	}

	if n := len(env.audit(core.AuditActionBreakGlassRequest)); n != 1 {
		t.Fatalf("breakglass_request entries: %d", n)
	}

	// approval: not by the requester, not by a non-holder, not without step-up
	approve := func(who, token string) (int, map[string]any) {
		return env.do(http.MethodPost, "/api/breakglass/"+grant+"/approve", nil, token)
	}

	if s, _ := approve("frank", env.fresh("frank")); s != http.StatusForbidden {
		t.Fatalf("self approval: %d", s)
	}

	if s, _ := approve("bob", env.fresh("bob")); s != http.StatusForbidden {
		t.Fatalf("approval by a non-holder: %d", s)
	}

	if s, b := approve("carol", env.plain("carol")); s != http.StatusForbidden || !strings.Contains(toJSON(b), "passkey_step_up_required") {
		t.Fatalf("approval needs a fresh passkey: %d %v", s, b)
	}

	env.app.TestMailer.Reset()

	status, body = approve("carol", env.fresh("carol"))
	if status != http.StatusOK || body["state"] != "active" || body["approvedBy"] != orgUserID("carol") {
		t.Fatalf("approval: %d %v", status, body)
	}

	if _, has := body["reason"]; has {
		t.Fatal("an approver who is not an owner does not get the reason back")
	}

	// the author and the owner were told; the notice carries the reason
	to := map[string]bool{}
	for _, m := range env.app.TestMailer.Messages() {
		to[m.To[0].Address] = true

		if !strings.Contains(m.HTML, "Parishioner may be at risk") {
			t.Fatalf("the notice must carry the reason: %s", m.HTML)
		}
	}

	if !to["dave@orgs.test"] || !to["alice@orgs.test"] || len(to) != 2 {
		t.Fatalf("notified: %v", to)
	}

	exp, _ := time.Parse("2006-01-02 15:04:05.000Z", body["expires"].(string))
	if d := time.Until(exp); d < 23*time.Hour || d > 24*time.Hour+time.Minute {
		t.Fatalf("the default grant is 24 hours: %v", d)
	}

	opens := env.audit(core.AuditActionBreakGlassOpen)
	if len(opens) != 1 {
		t.Fatalf("breakglass_open entries: %d", len(opens))
	}

	var meta map[string]any
	_ = opens[0].UnmarshalJSONField("meta", &meta)

	for _, k := range []string{"orgId", "noteId", "reason", "basis", "expires"} {
		if meta[k] == nil || meta[k] == "" {
			t.Fatalf("breakglass_open meta lacks %s: %v", k, meta)
		}
	}

	// reads: the grantee reads the sealed note; every read is logged with the grant
	status, body = env.view("frank", "sealed")
	if status != http.StatusOK || body["body"] != "secret sealed" {
		t.Fatalf("the grantee reads: %d %v", status, body)
	}

	// grants are per note
	if s, _ := env.view("frank", "private"); s != http.StatusNotFound {
		t.Fatalf("another note: %d", s)
	}

	reads := env.audit(core.AuditActionRead)
	if len(reads) != 1 {
		t.Fatalf("one read entry: %d", len(reads))
	}

	var rm map[string]any
	_ = reads[0].UnmarshalJSONField("meta", &rm)

	if rm["grantId"] != grant || rm["collection"] != pastoralCol || reads[0].GetString("actorId") != orgUserID("frank") {
		t.Fatalf("read entry: %v actor %s", rm, reads[0].GetString("actorId"))
	}

	// a list that holds the sealed note is one entry, and the count rises
	if s, _ := env.do(http.MethodGet, "/api/collections/"+pastoralCol+"/records", nil, env.plain("frank")); s != http.StatusOK {
		t.Fatalf("list: %d", s)
	}

	g, _ := env.app.FindRecordById(core.CollectionNameBreakGlass, grant)
	if g.GetInt("readCount") != 2 || len(env.audit(core.AuditActionRead)) != 2 {
		t.Fatalf("readCount %d, entries %d", g.GetInt("readCount"), len(env.audit(core.AuditActionRead)))
	}

	// the author reading her own note is not logged; a superuser's read is
	if s, _ := env.view("dave", "sealed"); s != http.StatusOK {
		t.Fatalf("author: %d", s)
	}

	if n := len(env.audit(core.AuditActionRead)); n != 2 {
		t.Fatalf("the author's read is not logged: %d", n)
	}

	if s, _ := env.view("super", "sealed"); s != http.StatusOK {
		t.Fatalf("superuser: %d", s)
	}

	reads = env.audit(core.AuditActionRead)
	if len(reads) != 3 {
		t.Fatalf("the superuser's read is logged: %d", len(reads))
	}

	_ = reads[2].UnmarshalJSONField("meta", &rm)

	if rm["superuser"] != true || rm["grantId"] != "" {
		t.Fatalf("superuser read entry: %v", rm)
	}

	// the owner sees the reason, a holder does not
	_, list := env.do(http.MethodGet, "/api/breakglass?org="+orgP1, nil, env.plain("alice"))
	if items, _ := list["items"].([]any); len(items) != 1 || items[0].(map[string]any)["reason"] != goodReason {
		t.Fatalf("owner list: %v", list)
	}

	_, list = env.do(http.MethodGet, "/api/breakglass?org="+orgP1, nil, env.plain("carol"))
	if items, _ := list["items"].([]any); len(items) != 1 || items[0].(map[string]any)["reason"] != nil {
		t.Fatalf("holder list must hide the reason: %v", list)
	}

	if s, _ := env.do(http.MethodGet, "/api/breakglass?org="+orgP1, nil, env.plain("bob")); s != http.StatusForbidden {
		t.Fatalf("an admin without the tag cannot list: %d", s)
	}

	// revoke: not by an admin, by the owner
	if s, _ := env.do(http.MethodPost, "/api/breakglass/"+grant+"/revoke", nil, env.plain("bob")); s != http.StatusNotFound {
		t.Fatalf("an admin cannot revoke: %d", s)
	}

	if s, _ := env.do(http.MethodPost, "/api/breakglass/"+grant+"/revoke", nil, env.plain("alice")); s != http.StatusOK {
		t.Fatalf("the owner revokes: %d", s)
	}

	if s, _ := env.view("frank", "sealed"); s != http.StatusNotFound {
		t.Fatalf("revoked: %d", s)
	}

	if n := len(env.audit(core.AuditActionBreakGlassRevoke)); n != 1 {
		t.Fatalf("breakglass_revoke entries: %d", n)
	}

	if s, _ := env.do(http.MethodPost, "/api/breakglass/"+grant+"/revoke", nil, env.plain("alice")); s != http.StatusConflict {
		t.Fatalf("revoking twice: %d", s)
	}
}

func TestBreakGlassExpiryAndRevokers(t *testing.T) {
	env := newPastoralEnv(t)

	open := func(label string) string {
		status, body := env.request("frank", label, goodReason, 0)
		if status != http.StatusCreated {
			t.Fatalf("request: %d %v", status, body)
		}

		id := body["id"].(string)

		if s, b := env.do(http.MethodPost, "/api/breakglass/"+id+"/approve", nil, env.fresh("carol")); s != http.StatusOK {
			t.Fatalf("approve: %d %v", s, b)
		}

		return id
	}

	id := open("sealed")

	// the clock closes the grant with no job
	g, _ := env.app.FindRecordById(core.CollectionNameBreakGlass, id)
	past, _ := types.ParseDateTime(time.Now().Add(-time.Minute))
	g.Set("expires", past)
	orgMust(t, env.app.Save(g))

	if s, _ := env.view("frank", "sealed"); s != http.StatusNotFound {
		t.Fatalf("expired: %d", s)
	}

	// the grantee and the note's author may revoke
	id = open("private")
	if s, _ := env.do(http.MethodPost, "/api/breakglass/"+id+"/revoke", nil, env.plain("frank")); s != http.StatusOK {
		t.Fatalf("the grantee revokes: %d", s)
	}

	id = open("clergy")
	if s, _ := env.do(http.MethodPost, "/api/breakglass/"+id+"/revoke", nil, env.plain("dave")); s != http.StatusOK {
		t.Fatalf("the author revokes: %d", s)
	}

	// a lapsed request cannot be approved
	_, body := env.request("frank", "care", goodReason, 0)
	lapsed := body["id"].(string)

	g, _ = env.app.FindRecordById(core.CollectionNameBreakGlass, lapsed)
	old, _ := types.ParseDateTime(time.Now().Add(-25 * time.Hour))
	g.Set("requestedAt", old)
	orgMust(t, env.app.Save(g))

	if s, _ := env.do(http.MethodPost, "/api/breakglass/"+lapsed+"/approve", nil, env.fresh("carol")); s != http.StatusConflict {
		t.Fatalf("a lapsed request: %d", s)
	}

	// an empty date never opens a grant (the rule excludes it explicitly)
	_, body = env.request("frank", "congregation", goodReason, 0)
	pend := body["id"].(string)

	if s, _ := env.view("frank", "care"); s != http.StatusNotFound {
		t.Fatalf("a request for another note grants nothing: %d", s)
	}

	_ = pend
}

func TestBreakGlassOrgSettingShortens(t *testing.T) {
	env := newPastoralEnv(t)

	org, _ := env.app.FindRecordById(core.CollectionNameOrgs, orgP1)
	org.Set("settings", map[string]any{"breakglassHours": 2})
	orgMust(t, env.app.Save(org))

	if s, _ := env.request("frank", "sealed", goodReason, 3); s != http.StatusBadRequest {
		t.Fatalf("longer than the org allows: %d", s)
	}

	_, body := env.request("frank", "sealed", goodReason, 0)
	id := body["id"].(string)

	_, body = env.do(http.MethodPost, "/api/breakglass/"+id+"/approve", nil, env.fresh("carol"))

	exp, _ := time.Parse("2006-01-02 15:04:05.000Z", body["expires"].(string))
	if d := time.Until(exp); d > 2*time.Hour+time.Minute || d < time.Hour {
		t.Fatalf("two hours: %v", d)
	}

	// a setting can never lengthen the limit
	org.Set("settings", map[string]any{"breakglassHours": 72})
	orgMust(t, env.app.Save(org))

	if got := core.BreakGlassMaxFor(org); got != 24*time.Hour {
		t.Fatalf("limit %v", got)
	}
}

func TestBreakGlassNoticeCannotBeSkipped(t *testing.T) {
	env := newPastoralEnv(t)

	// the author has no usable address: the other holders and the owners are told
	dave, _ := env.app.FindRecordById("users", orgUserID("dave"))
	dave.SetEmail("erased-dave@invalid")
	orgMust(t, env.app.SaveNoValidate(dave))

	// ivan becomes a third holder (not the grantee, not the approver)
	m, _ := core.FindOrgMember(env.app, orgP1, orgUserID("ivan"))
	roles, _ := env.app.FindFirstRecordByFilter(core.CollectionNameOrgRoles, "org={:o} && name='breakglass'", dbx.Params{"o": orgP1})
	m.Set("tags", append(m.GetStringSlice("tags"), roles.Id))
	orgMust(t, env.app.Save(m))

	_, body := env.request("frank", "sealed", goodReason, 0)
	id := body["id"].(string)

	env.app.TestMailer.Reset()

	if s, b := env.do(http.MethodPost, "/api/breakglass/"+id+"/approve", nil, env.fresh("carol")); s != http.StatusOK {
		t.Fatalf("approve: %d %v", s, b)
	}

	to := map[string]bool{}
	for _, msg := range env.app.TestMailer.Messages() {
		to[msg.To[0].Address] = true
	}

	if !to["alice@orgs.test"] || !to["ivan@orgs.test"] || to["dave@orgs.test"] || to["frank@orgs.test"] {
		t.Fatalf("fallback recipients: %v", to)
	}

	// nobody reachable: nothing is granted
	alice, _ := env.app.FindRecordById("users", orgUserID("alice"))
	alice.SetEmail("erased-alice@invalid")
	orgMust(t, env.app.SaveNoValidate(alice))

	ivan, _ := env.app.FindRecordById("users", orgUserID("ivan"))
	ivan.SetEmail("erased-ivan@invalid")
	orgMust(t, env.app.SaveNoValidate(ivan))

	_, body = env.request("frank", "private", goodReason, 0)
	id = body["id"].(string)

	if s, b := env.do(http.MethodPost, "/api/breakglass/"+id+"/approve", nil, env.fresh("carol")); s != http.StatusServiceUnavailable {
		t.Fatalf("no one to tell: %d %v", s, b)
	}

	if s, _ := env.view("frank", "private"); s != http.StatusNotFound {
		t.Fatalf("not granted: %d", s)
	}
}

func TestBreakGlassRevokedWhenMemberSuspendedOrRemoved(t *testing.T) {
	env := newPastoralEnv(t)

	for _, how := range []string{"suspend", "remove"} {
		_, body := env.request("frank", "sealed", goodReason, 0)
		id := body["id"].(string)

		if s, b := env.do(http.MethodPost, "/api/breakglass/"+id+"/approve", nil, env.fresh("carol")); s != http.StatusOK {
			t.Fatalf("approve: %d %v", s, b)
		}

		org, _ := env.app.FindRecordById(core.CollectionNameOrgs, orgP1)
		member, _ := core.FindOrgMember(env.app, orgP1, orgUserID("frank"))
		actor := core.NewOrgActor(env.app, org, mustUser(t, env, "alice"))

		if how == "suspend" {
			st := "suspended"
			if _, err := core.UpdateOrgMember(env.app, context.Background(), actor, org, member, core.OrgMemberPatch{Status: &st}); err != nil {
				t.Fatal(err)
			}
		} else if err := core.RemoveOrgMember(env.app, context.Background(), actor, org, member); err != nil {
			t.Fatal(err)
		}

		g, _ := env.app.FindRecordById(core.CollectionNameBreakGlass, id)
		if !g.GetBool("revoked") {
			t.Fatalf("%s: the grant must be revoked", how)
		}

		if s, _ := env.view("frank", "sealed"); s != http.StatusNotFound {
			t.Fatalf("%s: access must be gone: %d", how, s)
		}

		// put frank back for the next round
		if how == "suspend" {
			st := "active"
			fresh, _ := core.FindOrgMember(env.app, orgP1, orgUserID("frank"))
			_, _ = core.UpdateOrgMember(env.app, context.Background(), actor, org, fresh, core.OrgMemberPatch{Status: &st})
		} else {
			t.Log("done")
		}
	}

	if n := len(env.audit(core.AuditActionBreakGlassRevoke)); n != 2 {
		t.Fatalf("revoke entries: %d", n)
	}
}

func mustUser(t testing.TB, env *pastoralEnv, who string) *core.Record {
	t.Helper()

	u, err := env.app.FindRecordById("users", orgUserID(who))
	orgMust(t, err)

	return u
}

func TestBreakGlassRefusesAPIKeysAndSuperusers(t *testing.T) {
	env := newPastoralEnv(t)

	status, _ := env.do(http.MethodPost, "/api/breakglass", map[string]any{
		"collection": pastoralCol, "recordId": env.notes["sealed"], "reason": goodReason, "basis": "safety",
	}, auditSuperuserToken)
	if status == http.StatusCreated {
		t.Fatal("a superuser cannot ask for break-glass access")
	}
}

func TestPastoralHeldForReviewInExport(t *testing.T) {
	env := newPastoralEnv(t)

	// a note about frank
	col, _ := env.app.FindCollectionByNameOrId(pastoralCol)
	r := core.NewRecord(col)
	r.Set("org", orgP1)
	r.Set("author", orgUserID("dave"))
	r.Set("subject", orgUserID("frank"))
	r.Set("visibility", "clergy")
	r.Set("body", "Frank's family is in difficulty; his sister said more")
	orgMust(t, env.app.Save(r))

	export := func() (map[string]any, string) {
		var buf bytes.Buffer

		if _, err := core.WriteDataExport(env.app, mustUser(t, env, "frank"), &buf); err != nil {
			t.Fatal(err)
		}

		zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
		orgMust(t, err)

		var manifest map[string]any
		all := ""

		for _, f := range zr.File {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			_ = rc.Close()

			all += string(b)

			if f.Name == "manifest.json" {
				manifest = decodeJSONMap(t, b)
			}
		}

		return manifest, all
	}

	manifest, all := export()

	if strings.Contains(all, "sister said") {
		t.Fatal("a note about the person must not be in the automatic export")
	}

	held, _ := manifest["held"].([]any)
	if len(held) != 1 || held[0].(map[string]any)["count"] != float64(1) || held[0].(map[string]any)["name"] != pastoralCol {
		t.Fatalf("held in the manifest: %v", manifest["held"])
	}

	// a second export does not create a second hold
	export()

	holds, _ := env.app.FindAllRecords(core.CollectionNamePastoralHolds)
	if len(holds) != 1 || holds[0].GetString("state") != "held" {
		t.Fatalf("holds: %d", len(holds))
	}

	hold := holds[0].Id

	if n := len(env.audit(core.AuditActionHoldCreate)); n != 1 {
		t.Fatalf("pastoral_hold entries: %d", n)
	}

	// only an owner of the note's org reviews: not an admin, not a superuser, not the person
	for _, who := range []string{"bob", "carol", "frank", "erin"} {
		if s, _ := env.do(http.MethodGet, "/api/gdpr/pastoral-holds?org="+orgP1, nil, env.plain(who)); s == http.StatusOK {
			t.Fatalf("%s must not list the holds", who)
		}

		if s, _ := env.do(http.MethodPost, "/api/gdpr/pastoral-holds/"+hold+"/release", nil, env.plain(who)); s == http.StatusOK {
			t.Fatalf("%s must not release", who)
		}
	}

	if s, _ := env.do(http.MethodPost, "/api/gdpr/pastoral-holds/"+hold+"/release", nil, auditSuperuserToken); s != http.StatusForbidden {
		t.Fatalf("a superuser does not decide: %d", s)
	}

	status, body := env.do(http.MethodGet, "/api/gdpr/pastoral-holds?org="+orgP1, nil, env.plain("alice"))
	if items, _ := body["items"].([]any); status != http.StatusOK || len(items) != 1 {
		t.Fatalf("the owner lists: %d %v", status, body)
	}

	// the review is a logged read
	status, body = env.do(http.MethodGet, "/api/gdpr/pastoral-holds/"+hold, nil, env.plain("alice"))
	if status != http.StatusOK || !strings.Contains(toJSON(body), "sister said") {
		t.Fatalf("review: %d %v", status, body)
	}

	reads := env.audit(core.AuditActionRead)
	if len(reads) != 1 || reads[0].GetString("actorId") != orgUserID("alice") {
		t.Fatalf("the review is logged: %d", len(reads))
	}

	// withholding needs a reason; still not exported
	if s, _ := env.do(http.MethodPost, "/api/gdpr/pastoral-holds/"+hold+"/withhold", nil, env.plain("alice")); s != http.StatusBadRequest {
		t.Fatalf("withhold without a reason: %d", s)
	}

	if s, _ := env.do(http.MethodPost, "/api/gdpr/pastoral-holds/"+hold+"/withhold", map[string]any{"note": "names a third party"}, env.plain("alice")); s != http.StatusOK {
		t.Fatalf("withhold: %d", s)
	}

	if _, all := export(); strings.Contains(all, "sister said") {
		t.Fatal("a withheld note is not exported")
	}

	// release: the next export has it
	if s, _ := env.do(http.MethodPost, "/api/gdpr/pastoral-holds/"+hold+"/release", map[string]any{"note": "reviewed, third party removed"}, env.plain("alice")); s != http.StatusOK {
		t.Fatalf("release: %d", s)
	}

	manifest, all = export()
	if !strings.Contains(all, "sister said") {
		t.Fatal("a released note is in the next export")
	}

	if held, _ := manifest["held"].([]any); len(held) != 0 {
		t.Fatalf("nothing is held any more: %v", held)
	}

	if len(env.audit(core.AuditActionHoldFree)) != 1 || len(env.audit(core.AuditActionHoldKeep)) != 1 {
		t.Fatal("release and withhold are audited")
	}
}

func decodeJSONMap(t testing.TB, b []byte) map[string]any {
	t.Helper()

	out := map[string]any{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}

	return out
}

func TestPastoralTemplateNeedsAKey(t *testing.T) {
	t.Setenv("CLAVIGER_KEK", "")
	t.Setenv("CLAVIGER_KEK_FILE", "")

	env := newCeremonyEnv(t)
	if core.FieldEncryptionAvailable(env.app) {
		t.Skip("the test app has a key from another source")
	}

	s, body := env.do(http.MethodPost, "/api/pastoral", map[string]any{"name": "pn"}, auditSuperuserToken)
	if s != http.StatusBadRequest || !strings.Contains(toJSON(body), "key-encryption key") {
		t.Fatalf("expected a refusal without a key: %d %v", s, body)
	}

	if _, err := env.app.FindCollectionByNameOrId("pn"); err == nil {
		t.Fatal("nothing must be created without a key")
	}
}
