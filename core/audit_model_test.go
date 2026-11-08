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
	if len(entries) != 1 || !strings.Contains(entries[0].GetString("meta"), `"enriched":true`) {
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
		raw := e.GetString("before") + e.GetString("after")
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
