package core_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #74).

func auditCount(t testing.TB, app core.App, filter string) int {
	t.Helper()
	var total int
	q := app.RecordQuery(core.CollectionNameAuditLog).Select("count(*)")
	if filter != "" {
		q = q.AndWhere(dbx.NewExp(filter))
	}
	if err := q.Row(&total); err != nil {
		t.Fatal(err)
	}
	return total
}

// clearAudit drops the system entries written by the schema migrations that
// ran when the fixture app booted (#110).
func clearAudit(t testing.TB, app core.App) {
	t.Helper()
	if _, err := core.DeleteExpiredAuditEntries(app, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func newDemo2(t testing.TB, app core.App, title string) *core.Record {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("demo2")
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(col)
	r.Set("title", title)
	return r
}

func TestAuditEntryForGoSavesUsesSystemActor(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()
	clearAudit(t, app)

	r := newDemo2(t, app, "go-created")
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}

	entries, err := app.FindAllRecords(core.CollectionNameAuditLog)
	if err != nil || len(entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d (%v)", len(entries), err)
	}
	if entries[0].GetString("actorType") != core.AuditActorTypeSystem || entries[0].GetString("targetId") != r.Id {
		t.Fatalf("Unexpected entry %s", entries[0].PublicExport())
	}

	// explicit actor through the context
	ctx := core.WithAuditContext(context.Background(), core.AuditActor{Type: core.AuditActorTypeRecord, Collection: "users", Id: "abc"}, "10.0.0.1")
	r.Set("title", "updated")
	if err := app.SaveWithContext(ctx, r); err != nil {
		t.Fatal(err)
	}
	entries, _ = app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": "update"})
	if len(entries) != 1 || entries[0].GetString("actorId") != "abc" || entries[0].GetString("ip") != "10.0.0.1" {
		t.Fatalf("Unexpected update entry: %v", entries)
	}
}

func TestAuditRollbackLeavesNoEntry(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()
	clearAudit(t, app)

	rollbackErr := errors.New("rollback")
	r := newDemo2(t, app, "rolled-back")

	err := app.RunInTransaction(func(txApp core.App) error {
		if err := txApp.Save(r); err != nil {
			return err
		}
		if n := auditCount(t, txApp, ""); n != 1 {
			t.Fatalf("Expected the entry to be visible inside the transaction, got %d", n)
		}
		return rollbackErr
	})
	if !errors.Is(err, rollbackErr) {
		t.Fatalf("Expected rollback error, got %v", err)
	}
	if n := auditCount(t, app, ""); n != 0 {
		t.Fatalf("Expected no entries after rollback, got %d", n)
	}

	// committed transaction keeps both
	r2 := newDemo2(t, app, "committed")
	if err := app.RunInTransaction(func(txApp core.App) error { return txApp.Save(r2) }); err != nil {
		t.Fatal(err)
	}
	if n := auditCount(t, app, ""); n != 1 {
		t.Fatalf("Expected 1 entry after commit, got %d", n)
	}
}

func TestAuditEntryHookEnrichAndSkip(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()
	clearAudit(t, app)

	app.OnAuditEntry().Bind(&hook.Handler[*core.AuditEntryEvent]{
		Func: func(e *core.AuditEntryEvent) error {
			if e.Record != nil && e.Record.GetString("title") == "skip-me" {
				return nil // skip: no e.Next()
			}
			e.Entry.Meta["enriched"] = true
			return e.Next()
		},
	})

	if err := app.Save(newDemo2(t, app, "skip-me")); err != nil {
		t.Fatal(err)
	}
	if n := auditCount(t, app, ""); n != 0 {
		t.Fatalf("Expected the skipped entry to not be written, got %d", n)
	}

	r := newDemo2(t, app, "keep-me")
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	entries, _ := app.FindAllRecords(core.CollectionNameAuditLog)
	if len(entries) != 1 || !strings.Contains(tests.CompactJSON(entries[0].GetString("meta")), `"enriched":true`) {
		t.Fatalf("Expected 1 enriched entry, got %v", entries)
	}
	if calls := app.EventCallsSnapshot()["OnAuditEntry"]; calls != 2 {
		t.Fatalf("Expected the hook to fire twice, got %d", calls)
	}
}

func TestAuditEntryHookErrorFailsTheChange(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	app.OnAuditEntry().BindFunc(func(e *core.AuditEntryEvent) error { return errors.New("nope") })

	r := newDemo2(t, app, "fail-closed")
	if err := app.Save(r); err == nil {
		t.Fatal("Expected the save to fail when the audit entry cannot be written")
	}
	if _, err := app.FindFirstRecordByData("demo2", "title", "fail-closed"); err == nil {
		t.Fatal("Expected the record change to be rolled back")
	}
}

type forgedKey struct{}

func TestAuditLogIsAppendOnlyForGoCallers(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	if err := app.Save(newDemo2(t, app, "xx")); err != nil {
		t.Fatal(err)
	}
	entry, _ := app.FindFirstRecordByFilter(core.CollectionNameAuditLog, "action='create'")

	// forge
	col, _ := app.FindCachedCollectionByNameOrId(core.CollectionNameAuditLog)
	forged := core.NewRecord(col)
	forged.Set("action", "create")
	if err := app.Save(forged); err == nil {
		t.Fatal("Expected create to be rejected")
	}
	if err := app.SaveNoValidate(forged); err == nil {
		t.Fatal("Expected create (no validate) to be rejected")
	}
	if err := app.SaveWithContext(context.WithValue(context.Background(), forgedKey{}, true), forged); err == nil {
		t.Fatal("Expected create with a forged context to be rejected")
	}

	// update / delete
	entry.Set("action", "delete")
	if err := app.Save(entry); err == nil {
		t.Fatal("Expected update to be rejected")
	}
	if err := app.SaveNoValidate(entry); err == nil {
		t.Fatal("Expected update (no validate) to be rejected")
	}
	if err := app.Delete(entry); err == nil {
		t.Fatal("Expected delete to be rejected")
	}
	if err := app.TruncateCollection(col); err == nil {
		t.Fatal("Expected truncate to be rejected")
	}

	// collection definition
	for name, mutate := range map[string]func(c *core.Collection){
		"rename":    func(c *core.Collection) { c.Name = "renamed" },
		"list rule": func(c *core.Collection) { rule := ""; c.ListRule = &rule },
		"create":    func(c *core.Collection) { rule := ""; c.CreateRule = &rule },
		"field":     func(c *core.Collection) { c.Fields.Add(&core.TextField{Name: "extra"}) },
		"remove":    func(c *core.Collection) { c.Fields.RemoveByName("meta") },
		"indexes":   func(c *core.Collection) { c.Indexes = nil },
	} {
		c, err := app.FindCollectionByNameOrId(core.CollectionNameAuditLog)
		if err != nil {
			t.Fatal(err)
		}
		mutate(c)
		if err := app.Save(c); err == nil {
			t.Fatalf("Expected collection change %q to be rejected", name)
		}
		if err := app.SaveNoValidate(c); err == nil {
			t.Fatalf("Expected collection change %q (no validate) to be rejected", name)
		}
	}
	c, _ := app.FindCollectionByNameOrId(core.CollectionNameAuditLog)
	if err := app.Delete(c); err == nil {
		t.Fatal("Expected collection delete to be rejected")
	}

	// an untouched re-save is still fine (e.g. generic maintenance code)
	c, _ = app.FindCollectionByNameOrId(core.CollectionNameAuditLog)
	if err := app.Save(c); err != nil {
		t.Fatalf("Expected a no-change collection save to pass, got %v", err)
	}
}

func TestAuditRedactionAndExclusions(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	users, _ := app.FindCollectionByNameOrId("users")
	u := core.NewRecord(users)
	u.SetEmail("redact@example.com")
	u.SetPassword("super-secret-password")
	if err := app.Save(u); err != nil {
		t.Fatal(err)
	}

	// a user-defined secret-like and a hidden field
	demo, _ := app.FindCollectionByNameOrId("demo2")
	demo.Fields.Add(&core.TextField{Name: "apiKey"})
	demo.Fields.Add(&core.TextField{Name: "internalNote", Hidden: true})
	if err := app.Save(demo); err != nil {
		t.Fatal(err)
	}
	r := newDemo2(t, app, "tt")
	r.Set("apiKey", "sk-live-123")
	r.Set("internalNote", "private note")
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}

	for _, e := range mustAll(t, app) {
		raw := tests.CompactJSON(e.GetString("before")) + tests.CompactJSON(e.GetString("after"))
		for _, leaked := range []string{"super-secret-password", u.GetString("tokenKey"), u.GetString("password:hash"), "sk-live-123", "private note"} {
			if strings.Contains(raw, leaked) {
				t.Fatalf("Leaked %q in %s", leaked, raw)
			}
		}
	}

	// excluded collections produce no entries
	before := auditCount(t, app, "")
	mfa := core.NewMFA(app)
	mfa.SetCollectionRef(users.Id)
	mfa.SetRecordRef(u.Id)
	mfa.SetMethod(core.MFAMethodPassword)
	otp := core.NewOTP(app)
	otp.SetCollectionRef(users.Id)
	otp.SetRecordRef(u.Id)
	otp.SetPassword("123456")
	origin := core.NewAuthOrigin(app)
	origin.SetCollectionRef(users.Id)
	origin.SetRecordRef(u.Id)
	origin.SetFingerprint("fp")
	for _, m := range []core.Model{mfa, otp, origin} {
		if err := app.Save(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Delete(mfa); err != nil {
		t.Fatal(err)
	}
	if after := auditCount(t, app, ""); after != before {
		t.Fatalf("Expected no entries for the excluded collections, got %d new", after-before)
	}

	// external auths are audited
	ext := core.NewExternalAuth(app)
	ext.SetCollectionRef(users.Id)
	ext.SetRecordRef(u.Id)
	ext.SetProvider("github")
	ext.SetProviderId("123")
	if err := app.Save(ext); err != nil {
		t.Fatal(err)
	}
	if n := auditCount(t, app, "targetCollection='_externalAuths'"); n != 1 {
		t.Fatalf("Expected the external auth to be audited, got %d", n)
	}
}

func TestDeleteExpiredAuditEntries(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()
	clearAudit(t, app)

	for _, title := range []string{"aa", "bb", "cc"} {
		if err := app.Save(newDemo2(t, app, title)); err != nil {
			t.Fatal(err)
		}
	}
	entries := mustAll(t, app)
	if len(entries) != 3 {
		t.Fatalf("Expected 3 entries, got %d", len(entries))
	}

	old := time.Now().AddDate(0, 0, -40).UTC().Format("2006-01-02 15:04:05.000Z")
	if _, err := app.NonconcurrentDB().NewQuery("UPDATE `" + core.CollectionNameAuditLog + "` SET created={:d} WHERE id IN ({:a},{:b})").
		Bind(dbx.Params(map[string]any{"d": old, "a": entries[0].Id, "b": entries[1].Id})).Execute(); err != nil {
		t.Fatal(err)
	}

	n, err := core.DeleteExpiredAuditEntries(app, time.Now().AddDate(0, 0, -30))
	if err != nil || n != 2 {
		t.Fatalf("Expected 2 deleted, got %d (%v)", n, err)
	}
	left := mustAll(t, app)
	if len(left) != 1 || left[0].Id != entries[2].Id {
		t.Fatalf("Expected only the recent entry to remain, got %v", left)
	}
}

func TestAuditCleanupCronRegisteredOffTheHour(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	var found bool
	for _, j := range app.Cron().Jobs() {
		if j.Id() == "__pbAuditCleanup__" {
			found = true
			if strings.HasPrefix(j.Expression(), "0 ") || strings.HasPrefix(j.Expression(), "7 ") || strings.HasPrefix(j.Expression(), "37 ") {
				t.Fatalf("Unexpected cron expression %q", j.Expression())
			}
		}
	}
	if !found {
		t.Fatal("Expected the audit cleanup cron job")
	}
}

func mustAll(t testing.TB, app core.App) []*core.Record {
	t.Helper()
	entries, err := app.FindRecordsByFilter(core.CollectionNameAuditLog, "", "created,id", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// BenchmarkAuditOverhead compares record creation with and without the audit
// hooks, both as individual saves and as one bulk transaction.
//
//	go test ./core -run xxx -bench AuditOverhead -benchtime 2000x
func BenchmarkAuditOverhead(b *testing.B) {
	for _, audited := range []bool{false, true} {
		for _, bulk := range []bool{false, true} {
			name := "unaudited"
			if audited {
				name = "audited"
			}
			if bulk {
				name += "/one-tx"
			} else {
				name += "/single-saves"
			}

			b.Run(name, func(b *testing.B) {
				app, _ := tests.NewTestApp()
				defer app.Cleanup()

				if !audited {
					app.OnRecordCreateExecute().Unbind("__pbAuditCreate__")
				}

				col, _ := app.FindCollectionByNameOrId("demo2")
				var n int
				save := func(a core.App) {
					n++
					r := core.NewRecord(col)
					r.Set("title", "benchmark-"+strconv.Itoa(n))
					if err := a.SaveNoValidate(r); err != nil {
						b.Fatal(err)
					}
				}

				b.ResetTimer()
				if bulk {
					if err := app.RunInTransaction(func(txApp core.App) error {
						for i := 0; i < b.N; i++ {
							save(txApp)
						}
						return nil
					}); err != nil {
						b.Fatal(err)
					}
				} else {
					for i := 0; i < b.N; i++ {
						save(app)
					}
				}
			})
		}
	}
}

func TestAuditIgnoresPasskeyLoginNoise(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	col, err := app.FindCollectionByNameOrId(core.CollectionNameWebAuthnCredentials)
	if err != nil {
		t.Fatal(err)
	}
	// temporary stand-in for the lastUsed field added by #73
	col.Fields.Add(&core.DateField{Name: "lastUsed"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}

	users, _ := app.FindCollectionByNameOrId("users")
	user, _ := app.FindAuthRecordByEmail(users, "test@example.com")

	cred := core.NewRecord(col)
	cred.Set("collectionRef", users.Id)
	cred.Set("recordRef", user.Id)
	cred.Set("credentialId", "cred-1")
	cred.Set("publicKey", "pk")
	cred.Set("name", "first")
	cred.Set("signCount", 1)
	if err := app.Save(cred); err != nil {
		t.Fatal(err)
	}
	count := func() int { return auditCount(t, app, "targetCollection='_webauthnCredentials'") }
	if count() != 1 {
		t.Fatalf("Expected the creation to be audited, got %d", count())
	}

	// login noise only: no entry
	cred.Set("signCount", 2)
	cred.Set("lastUsed", "2026-01-01 00:00:00.000Z")
	if err := app.Save(cred); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatalf("Expected no entry for signCount/lastUsed-only update, got %d", count())
	}

	// a real change alongside the noise is audited, without the noise fields
	cred.Set("signCount", 3)
	cred.Set("name", "renamed")
	if err := app.Save(cred); err != nil {
		t.Fatal(err)
	}
	entries, _ := app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": "update", "targetCollection": core.CollectionNameWebAuthnCredentials})
	if len(entries) != 1 || entries[0].GetString("changedFields") != `["name"]` {
		t.Fatalf("Expected one entry changing only name, got %v", entries)
	}
}

// Fork-local (Claviger #110).

func TestAuditSettingsNoOpAndSystemSave(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	// align the stored settings with the in-memory ones
	if err := app.Save(app.Settings()); err != nil {
		t.Fatal(err)
	}
	before := auditCount(t, app, "action='settings.update'")

	// no-op: no entry
	if err := app.Save(app.Settings()); err != nil {
		t.Fatal(err)
	}
	if n := auditCount(t, app, "action='settings.update'"); n != before {
		t.Fatalf("Expected no new entry for a no-op save, got %d", n-before)
	}

	// a Go-side change is attributed to system, secrets redacted
	app.Settings().SMTP.Password = "go-side-secret"
	app.Settings().Meta.AppName = "renamed"
	if err := app.Save(app.Settings()); err != nil {
		t.Fatal(err)
	}
	entries, err := app.FindRecordsByFilter(core.CollectionNameAuditLog, "action='settings.update'", "-created", 1, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("Expected the new entry: %v", err)
	}
	e := entries[0]
	if e.GetString("actorType") != core.AuditActorTypeSystem || strings.Contains(tests.CompactJSON(e.GetString("after")), "go-side-secret") ||
		!strings.Contains(tests.CompactJSON(e.GetString("after")), `"smtp.password":"[redacted]"`) {
		t.Fatalf("Unexpected entry %s", e.PublicExport())
	}
}

func runAuditCleanup(t testing.TB, app core.App) {
	t.Helper()
	for _, j := range app.Cron().Jobs() {
		if j.Id() == "__pbAuditCleanup__" {
			j.Run()
			return
		}
	}
	t.Fatal("audit cleanup job not registered")
}

// not parallel: uses t.Setenv
func TestAuditRetentionFromSettingsAndEnvOverride(t *testing.T) {
	t.Setenv(core.AuditRetentionDaysEnv, "")

	setup := func(t *testing.T) (*tests.TestApp, string, string) {
		app, _ := tests.NewTestApp()
		t.Cleanup(app.Cleanup)
		clearAudit(t, app)

		for _, title := range []string{"old", "new"} {
			if err := app.Save(newDemo2(t, app, title)); err != nil {
				t.Fatal(err)
			}
		}
		entries := mustAll(t, app)
		old := time.Now().AddDate(0, 0, -40).UTC().Format("2006-01-02 15:04:05.000Z")
		if _, err := app.NonconcurrentDB().NewQuery("UPDATE `" + core.CollectionNameAuditLog + "` SET created={:d} WHERE id={:a}").
			Bind(dbx.Params{"d": old, "a": entries[0].Id}).Execute(); err != nil {
			t.Fatal(err)
		}
		return app, entries[0].Id, entries[1].Id
	}

	// default (0): keep forever
	app, _, _ := setup(t)
	runAuditCleanup(t, app)
	if n := len(mustAll(t, app)); n != 2 {
		t.Fatalf("Expected both entries to be kept by default, got %d", n)
	}

	// from Settings
	app, oldId, newId := setup(t)
	app.Settings().Audit.RetentionDays = 30
	runAuditCleanup(t, app)
	left := mustAll(t, app)
	if len(left) != 1 || left[0].Id != newId {
		t.Fatalf("Expected only the recent entry (not %s) with retention=30, got %v", oldId, left)
	}

	// env wins over the setting (0 = forever)
	t.Setenv(core.AuditRetentionDaysEnv, "0")
	app, _, _ = setup(t)
	app.Settings().Audit.RetentionDays = 30
	runAuditCleanup(t, app)
	if n := len(mustAll(t, app)); n != 2 {
		t.Fatalf("Expected the env override 0 to keep everything, got %d", n)
	}

	// env wins over the setting (shorter)
	t.Setenv(core.AuditRetentionDaysEnv, "1")
	app, _, newId = setup(t)
	app.Settings().Audit.RetentionDays = 0
	runAuditCleanup(t, app)
	if left := mustAll(t, app); len(left) != 1 || left[0].Id != newId {
		t.Fatalf("Expected env retention=1 to apply, got %v", left)
	}

	// an invalid env value is ignored in favor of the setting
	t.Setenv(core.AuditRetentionDaysEnv, "abc")
	app, _, newId = setup(t)
	app.Settings().Audit.RetentionDays = 30
	runAuditCleanup(t, app)
	if left := mustAll(t, app); len(left) != 1 || left[0].Id != newId {
		t.Fatalf("Expected the setting to apply with an invalid env, got %v", left)
	}
}

func TestAuditConfigValidation(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	app.Settings().Audit.RetentionDays = -1
	if err := app.Save(app.Settings()); err == nil {
		t.Fatal("Expected a negative retention to be rejected")
	}
	app.Settings().Audit.RetentionDays = 90
	if err := app.Save(app.Settings()); err != nil {
		t.Fatal(err)
	}
}

func TestAuditSchemaChangeBypass(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	col, err := app.FindCollectionByNameOrId(core.CollectionNameAuditLog)
	if err != nil {
		t.Fatal(err)
	}
	col.Fields.Add(&core.TextField{Name: "extraNote"})

	// the guard blocks a definition change by default, with a plain context too
	if err := app.Save(col); err == nil {
		t.Fatal("Expected the definition change to be rejected")
	}
	if err := app.SaveWithContext(context.Background(), col); err == nil {
		t.Fatal("Expected the definition change to be rejected with a plain context")
	}
	// ... and with the writer context of the entries (a different key)
	if err := app.SaveWithContext(core.WithAuditContext(context.Background(), core.AuditActor{Type: core.AuditActorTypeSystem}, ""), col); err == nil {
		t.Fatal("Expected the definition change to be rejected with an audit actor context")
	}

	// the explicit bypass lets it through
	col, _ = app.FindCollectionByNameOrId(core.CollectionNameAuditLog)
	col.Fields.Add(&core.TextField{Name: "extraNote"})
	if err := app.SaveWithContext(core.WithAuditSchemaChange(context.Background()), col); err != nil {
		t.Fatalf("Expected the bypass to allow the schema change: %v", err)
	}
	stored, _ := app.FindCollectionByNameOrId(core.CollectionNameAuditLog)
	if stored.Fields.GetByName("extraNote") == nil {
		t.Fatal("Expected the new field to be stored")
	}

	// the bypass is not a way to delete the collection ...
	if err := app.DeleteWithContext(core.WithAuditSchemaChange(context.Background()), stored); err == nil {
		t.Fatal("Expected the collection delete to stay blocked")
	}

	// ... nor to touch entries
	clearAudit(t, app)
	if err := core.WriteAuditEntry(app, nil, &core.AuditEntry{Action: "x", Actor: core.AuditActor{Type: core.AuditActorTypeSystem}, CollectionName: "demo2", RecordId: "r1"}, nil); err != nil {
		t.Fatal(err)
	}
	entries := mustAll(t, app)
	if len(entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(entries))
	}
	bypass := core.WithAuditSchemaChange(context.Background())
	if err := app.SaveWithContext(bypass, core.NewRecord(stored)); err == nil {
		t.Fatal("Expected creating an entry with the bypass context to be rejected")
	}
	entries[0].Set("action", "forged")
	if err := app.SaveWithContext(bypass, entries[0]); err == nil {
		t.Fatal("Expected updating an entry with the bypass context to be rejected")
	}
	if err := app.DeleteWithContext(bypass, entries[0]); err == nil {
		t.Fatal("Expected deleting an entry with the bypass context to be rejected")
	}
}
