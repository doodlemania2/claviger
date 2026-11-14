package apis_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, 88-3 and #86, 86-5): the retention and legal hold
// routes, and expiring prayer requests. Fixture: the pastoral env (org P1 with
// notes written by dave, passkeys enabled).

func (env *pastoralEnv) superFresh() string {
	env.t.Helper()

	su, err := env.app.FindRecordById(core.CollectionNameSuperusers, "sywbhecnh46rhm0")
	orgMust(env.t, err)

	tok, err := su.NewPasskeyAuthToken(time.Now(), true)
	orgMust(env.t, err)

	return tok
}

func (env *pastoralEnv) sysCtx() context.Context {
	return core.WithAuditContext(context.Background(), core.AuditActor{Type: core.AuditActorTypeSystem}, "")
}

func TestRetentionRoutesAreSuperuserOnly(t *testing.T) {
	env := newPastoralEnv(t)

	routes := []struct{ method, url string }{
		{http.MethodGet, "/api/gdpr/retention/policies"},
		{http.MethodPost, "/api/gdpr/retention/policies"},
		{http.MethodGet, "/api/gdpr/retention/policies/abc"},
		{http.MethodPatch, "/api/gdpr/retention/policies/abc"},
		{http.MethodDelete, "/api/gdpr/retention/policies/abc"},
		{http.MethodPost, "/api/gdpr/retention/run"},
		{http.MethodGet, "/api/gdpr/holds"},
		{http.MethodGet, "/api/gdpr/holds/abc"},
		{http.MethodPost, "/api/gdpr/holds"},
		{http.MethodPost, "/api/gdpr/holds/abc/lift"},
	}

	for _, r := range routes {
		for who, tok := range map[string]string{"anonymous": "", "owner of the org": env.fresh("alice"), "member": env.fresh("dave")} {
			s, _ := env.do(r.method, r.url, map[string]any{}, tok)
			if s != http.StatusUnauthorized && s != http.StatusForbidden {
				t.Errorf("%s %s as %s: %d", r.method, r.url, who, s)
			}
		}
	}

	// the records API of both tables is closed to everyone but superusers, and
	// even a superuser cannot write around the routes
	for _, name := range []string{core.CollectionNameRetentionPolicies, core.CollectionNameLegalHolds} {
		if s, _ := env.do(http.MethodGet, "/api/collections/"+name+"/records", nil, env.fresh("alice")); s != http.StatusForbidden {
			t.Errorf("list %s as a member: %d", name, s)
		}

		if s, _ := env.do(http.MethodPost, "/api/collections/"+name+"/records", map[string]any{"scope": "record"}, env.superFresh()); s != http.StatusForbidden {
			t.Errorf("create %s as a superuser through the records API: %d", name, s)
		}
	}
}

func TestRetentionPolicyRoutes(t *testing.T) {
	env := newPastoralEnv(t)
	su := env.superFresh()
	plainSu := auditSuperuserToken

	s, body := env.do(http.MethodPost, "/api/gdpr/retention/policies", map[string]any{
		"collection": pastoralCol, "basis": "created", "after": 30, "unit": "days", "action": "delete",
		"enabled": true, "dryRun": false, // ignored on create
	}, plainSu)
	if s != http.StatusCreated || body["enabled"] != false || body["dryRun"] != true {
		t.Fatalf("a new policy is disabled and dry: %d %v", s, body)
	}

	id, _ := body["id"].(string)

	// a bad policy is a 400 with the field
	if s, b := env.do(http.MethodPost, "/api/gdpr/retention/policies", map[string]any{
		"collection": "users", "basis": "created", "after": 30, "unit": "days", "action": "delete",
	}, plainSu); s != http.StatusBadRequest || !strings.Contains(toJSON(b), "collection") {
		t.Fatalf("accounts are refused: %d %v", s, b)
	}

	// arming it needs a fresh passkey; staying a dry run does not
	if s, _ := env.do(http.MethodPatch, "/api/gdpr/retention/policies/"+id, map[string]any{"enabled": true}, plainSu); s != http.StatusOK {
		t.Fatalf("enabling a dry run: %d", s)
	}

	s, b := env.do(http.MethodPatch, "/api/gdpr/retention/policies/"+id, map[string]any{"dryRun": false}, plainSu)
	if s != http.StatusForbidden || !strings.Contains(toJSON(b), "passkey_step_up_required") {
		t.Fatalf("going live needs a fresh passkey: %d %v", s, b)
	}

	if s, b := env.do(http.MethodPatch, "/api/gdpr/retention/policies/"+id, map[string]any{"dryRun": false}, su); s != http.StatusOK || b["dryRun"] != false {
		t.Fatalf("going live with a passkey: %d %v", s, b)
	}

	// previews change nothing and say what they would do
	if _, err := env.app.DB().NewQuery("UPDATE " + pastoralCol + " SET created='2000-01-01 00:00:00.000Z'").Execute(); err != nil {
		t.Fatal(err)
	}

	s, b = env.do(http.MethodPost, "/api/gdpr/retention/run", map[string]any{"preview": true, "policy": id}, plainSu)
	if s != http.StatusOK || b["preview"] != true {
		t.Fatalf("preview: %d %v", s, b)
	}

	n, _ := env.app.CountRecords(pastoralCol)
	if n != 6 {
		t.Fatalf("a preview deleted notes: %d", n)
	}

	// a live policy cannot be deleted while it is running, and can be deleted after
	if s, _ := env.do(http.MethodDelete, "/api/gdpr/retention/policies/"+id, nil, plainSu); s != http.StatusNoContent {
		t.Fatalf("delete: %d", s)
	}
}

func TestLegalHoldRoutesNeedFreshPasskey(t *testing.T) {
	env := newPastoralEnv(t)
	plainSu, su := auditSuperuserToken, env.superFresh()

	note := env.notes["private"]
	in := map[string]any{"scope": "record", "collection": pastoralCol, "recordId": note, "reason": "Pending claim, matter 12", "reference": "M-12"}

	s, b := env.do(http.MethodPost, "/api/gdpr/holds", in, plainSu)
	if s != http.StatusForbidden || !strings.Contains(toJSON(b), "passkey_step_up_required") {
		t.Fatalf("placing needs a fresh passkey: %d %v", s, b)
	}

	if n, _ := env.app.CountRecords(core.CollectionNameLegalHolds); n != 0 {
		t.Fatal("nothing was placed")
	}

	s, b = env.do(http.MethodPost, "/api/gdpr/holds", in, su)
	if s != http.StatusCreated {
		t.Fatalf("place: %d %v", s, b)
	}

	id, _ := b["id"].(string)

	for _, e := range env.audit(core.AuditActionLegalHold) {
		raw := e.GetString("meta")
		if strings.Contains(raw, "claim") || strings.Contains(raw, "M-12") || !strings.Contains(raw, note) {
			t.Fatalf("ids only, no reason or reference: %s", raw)
		}
	}

	if len(env.audit(core.AuditActionLegalHold)) != 1 {
		t.Fatal("one gdpr_hold entry")
	}

	// the author cannot delete a held note, whatever the rules say
	if s, _ := env.do(http.MethodDelete, "/api/collections/"+pastoralCol+"/records/"+note, nil, env.fresh("dave")); s == http.StatusNoContent {
		t.Fatal("a held note was deleted")
	}

	if s, b := env.do(http.MethodGet, "/api/gdpr/holds?active=1", nil, plainSu); s != http.StatusOK || !strings.Contains(toJSON(b), id) {
		t.Fatalf("list: %d %v", s, b)
	}

	// lifting needs one too
	s, b = env.do(http.MethodPost, "/api/gdpr/holds/"+id+"/lift", nil, plainSu)
	if s != http.StatusForbidden || !strings.Contains(toJSON(b), "passkey_step_up_required") {
		t.Fatalf("lifting needs a fresh passkey: %d %v", s, b)
	}

	if s, _ := env.do(http.MethodPost, "/api/gdpr/holds/"+id+"/lift", nil, su); s != http.StatusOK {
		t.Fatalf("lift: %d", s)
	}

	if s, _ := env.do(http.MethodPost, "/api/gdpr/holds/"+id+"/lift", nil, su); s != http.StatusConflict {
		t.Fatalf("a second lift: %d", s)
	}

	if len(env.audit(core.AuditActionLegalHoldLift)) != 1 {
		t.Fatal("one gdpr_hold_lift entry")
	}

	if s, _ := env.do(http.MethodDelete, "/api/collections/"+pastoralCol+"/records/"+note, nil, env.fresh("dave")); s != http.StatusNoContent {
		t.Fatalf("after the lift the author can delete: %d", s)
	}

	// bad input
	if s, _ := env.do(http.MethodPost, "/api/gdpr/holds", map[string]any{"scope": "subject", "subject": "nobody", "reason": "x"}, su); s != http.StatusBadRequest {
		t.Fatalf("unknown subject: %d", s)
	}

	if s, _ := env.do(http.MethodPost, "/api/gdpr/holds", map[string]any{"scope": "subject", "subject": orgUserID("carol")}, su); s != http.StatusBadRequest {
		t.Fatalf("a reason is required: %d", s)
	}
}

func TestPrayerRequestExpiryArchiveDeleteAndRenew(t *testing.T) {
	env := newPastoralEnv(t)

	col, err := env.app.FindCollectionByNameOrId(pastoralCol)
	orgMust(t, err)

	mk := func(kind string) *core.Record {
		r := core.NewRecord(col)
		r.Set("org", orgP1)
		r.Set("author", orgUserID("dave"))
		r.Set("visibility", "congregation")
		r.Set("kind", kind)
		r.Set("body", "pray for "+kind)
		orgMust(t, env.app.Save(r))

		return r
	}

	days := func(r *core.Record) int {
		return int(time.Until(r.GetDateTime("expiresAt").Time()).Hours()/24 + 0.5)
	}

	// default: 90 days for a prayer request, nothing for a care note
	prayer, care := mk("prayer"), mk("care")
	if d := days(prayer); d != 90 {
		t.Fatalf("a prayer request defaults to 90 days, got %d", d)
	}

	if !care.GetDateTime("expiresAt").IsZero() {
		t.Fatal("a care note does not expire by default")
	}

	// the org can set another default (7 to 730); a bad value is refused
	org, _ := env.app.FindRecordById(core.CollectionNameOrgs, orgP1)
	org.Set("settings", map[string]any{core.PastoralExpiryDaysSetting: 5})

	if err := env.app.Save(org); err == nil {
		t.Fatal("5 days is below the minimum")
	}

	org.Set("settings", map[string]any{core.PastoralExpiryDaysSetting: 30})
	orgMust(t, env.app.Save(org))

	if d := days(mk("prayer")); d != 30 {
		t.Fatalf("the org default is 30 days, got %d", d)
	}

	// the two policies of docs/retention.md: archive at expiry, delete 30 days later
	str := func(s string) *string { return &s }
	zero, thirty := 0, 30

	archive, err := core.CreateRetentionPolicy(env.app, env.sysCtx(), core.RetentionPolicyInput{
		Collection: str(pastoralCol), Basis: str("expiresAt"), After: &zero, Unit: str("days"), Action: str("archive"),
	})
	orgMust(t, err)

	del, err := core.CreateRetentionPolicy(env.app, env.sysCtx(), core.RetentionPolicyInput{
		Collection: str(pastoralCol), Basis: str("archivedAt"), After: &thirty, Unit: str("days"), Action: str("delete"),
		Filter: str("archived = true"),
	})
	orgMust(t, err)

	on, off := true, false
	for _, p := range []*core.Record{archive, del} {
		_, err := core.UpdateRetentionPolicy(env.app, env.sysCtx(), p.Id, core.RetentionPolicyInput{Enabled: &on, DryRun: &off})
		orgMust(t, err)
	}

	runJob := func() *core.RetentionRunResult {
		res, err := core.RunRetention(env.sysCtx(), env.app, core.RetentionRunOptions{})
		orgMust(t, err)

		return res
	}

	// nothing has expired yet
	if res := runJob(); res.Policies[0].Affected != 0 {
		t.Fatalf("nothing expires early: %+v", res.Policies)
	}

	// expire the prayer request
	prayer.Set("expiresAt", types.NowDateTime().Add(-time.Hour))
	orgMust(t, env.app.Save(prayer))

	if s, _ := env.do(http.MethodGet, "/api/collections/"+pastoralCol+"/records/"+prayer.Id, nil, env.plain("carol")); s != http.StatusOK {
		t.Fatalf("before the archive a member reads a congregation note: %d", s)
	}

	res := runJob()
	if res.Policies[0].Affected != 1 {
		t.Fatalf("archived at expiry: %+v", res.Policies)
	}

	prayer, _ = env.app.FindRecordById(col, prayer.Id)
	if !prayer.GetBool("archived") || prayer.GetDateTime("archivedAt").IsZero() {
		t.Fatal("archived and stamped")
	}

	// hidden from everyone but the author
	if s, _ := env.do(http.MethodGet, "/api/collections/"+pastoralCol+"/records/"+prayer.Id, nil, env.plain("carol")); s != http.StatusNotFound {
		t.Fatalf("an archived request is hidden from members: %d", s)
	}

	if s, _ := env.do(http.MethodGet, "/api/collections/"+pastoralCol+"/records/"+prayer.Id, nil, env.plain("dave")); s != http.StatusOK {
		t.Fatalf("the author still sees it: %d", s)
	}

	// the rules stay locked
	if p := core.PastoralRuleProblems(pastoralCol, core.OrgRuleSet{
		List: col.ListRule, View: col.ViewRule, Create: col.CreateRule, Update: col.UpdateRule, Delete: col.DeleteRule,
	}); len(p) != 0 {
		t.Fatalf("the rules differ from the locked ones: %v", p)
	}

	// not deleted before the archive period is over
	if res := runJob(); res.Policies[1].Affected != 0 {
		t.Fatalf("%+v", res.Policies[1])
	}

	// renew: only the author; it comes back from the archive with a new date
	for _, who := range []string{"carol", "alice", "frank"} {
		if s, _ := env.do(http.MethodPost, "/api/pastoral/"+pastoralCol+"/"+prayer.Id+"/renew", map[string]any{}, env.plain(who)); s != http.StatusNotFound {
			t.Fatalf("%s must not renew another's request: %d", who, s)
		}
	}

	if s, _ := env.do(http.MethodPost, "/api/pastoral/"+pastoralCol+"/"+prayer.Id+"/renew", map[string]any{}, auditSuperuserToken); s != http.StatusUnauthorized && s != http.StatusNotFound && s != http.StatusForbidden {
		t.Fatalf("a superuser must not renew: %d", s)
	}

	s, b := env.do(http.MethodPost, "/api/pastoral/"+pastoralCol+"/"+prayer.Id+"/renew", map[string]any{"days": 14}, env.plain("dave"))
	if s != http.StatusOK || b["archived"] != false {
		t.Fatalf("renew: %d %v", s, b)
	}

	prayer, _ = env.app.FindRecordById(col, prayer.Id)
	if prayer.GetBool("archived") || !prayer.GetDateTime("archivedAt").IsZero() || days(prayer) != 14 {
		t.Fatalf("renewed: archived=%v days=%d", prayer.GetBool("archived"), days(prayer))
	}

	if len(env.audit(core.AuditActionPastoralRenew)) != 1 {
		t.Fatal("renewal is audited")
	}

	if s, _ := env.do(http.MethodGet, "/api/collections/"+pastoralCol+"/records/"+prayer.Id, nil, env.plain("carol")); s != http.StatusOK {
		t.Fatalf("visible again: %d", s)
	}

	// expire, archive, and let the archive period pass: the note is deleted
	prayer.Set("expiresAt", types.NowDateTime().Add(-time.Hour))
	orgMust(t, env.app.Save(prayer))
	runJob()

	old := types.NowDateTime().Add(-31 * 24 * time.Hour).String()
	if _, err := env.app.DB().NewQuery("UPDATE " + pastoralCol + " SET archivedAt={:a} WHERE id={:id}").Bind(dbx.Params{"a": old, "id": prayer.Id}).Execute(); err != nil {
		t.Fatal(err)
	}

	res = runJob()
	if res.Policies[1].Affected != 1 {
		t.Fatalf("deleted after the archive period: %+v", res.Policies[1])
	}

	if _, err := env.app.FindRecordById(col, prayer.Id); err == nil {
		t.Fatal("the request is gone")
	}

	// the care note and the older fixture notes were not touched
	if _, err := env.app.FindRecordById(col, care.Id); err != nil {
		t.Fatal("the care note stays")
	}
}
