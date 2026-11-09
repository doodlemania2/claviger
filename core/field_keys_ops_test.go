package core_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #75, M3): rotation, rewrap, convert, strict, prune.
// Not parallel: the keys come from the environment and the data key cache is
// process-wide.

type keyRow struct {
	Id, KeyId, KekId, Wrapped, Retired, Strict string
}

func fieldKeyRowsOf(t testing.TB, app core.App, col *core.Collection, fieldName string) []keyRow {
	t.Helper()
	f := col.Fields.GetByName(fieldName)
	var maps []dbx.NullStringMap
	err := app.DB().Select("id", "keyId", "kekId", "wrapped", "retired", "strictMode").From(core.CollectionNameFieldKeys).
		Where(dbx.HashExp{"collectionRef": col.Id, "fieldRef": f.GetId()}).OrderBy("created ASC").All(&maps)
	if err != nil {
		t.Fatal(err)
	}
	var rows []keyRow
	for _, m := range maps {
		rows = append(rows, keyRow{m["id"].String, m["keyId"].String, m["kekId"].String, m["wrapped"].String, m["retired"].String, m["strictMode"].String})
	}
	return rows
}

func activeKey(t testing.TB, app core.App, col *core.Collection, fieldName string) keyRow {
	t.Helper()
	var active []keyRow
	for _, r := range fieldKeyRowsOf(t, app, col, fieldName) {
		if r.Retired == "" {
			active = append(active, r)
		}
	}
	if len(active) != 1 {
		t.Fatalf("expected exactly one active key, got %d", len(active))
	}
	return active[0]
}

func countOnKey(t testing.TB, app core.App, col *core.Collection, column, keyId string) int {
	t.Helper()
	var n int
	err := app.DB().NewQuery("SELECT COUNT(*) FROM {{" + col.Name + "}} WHERE [[" + column + "]] LIKE {:p}").
		Bind(dbx.Params{"p": "enc:v1:" + keyId + ":%"}).Row(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func countEncrypted(t testing.TB, app core.App, col *core.Collection, column string) int {
	t.Helper()
	var n int
	if err := app.DB().NewQuery("SELECT COUNT(*) FROM {{" + col.Name + "}} WHERE [[" + column + "]] LIKE 'enc:v1:%'").Row(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func seedPeople(t testing.TB, app core.App, col *core.Collection, n int) map[string]string {
	t.Helper()
	want := map[string]string{}
	for i := 0; i < n; i++ {
		phone := fmt.Sprintf("+1 555 %04d", i)
		if i%10 == 9 {
			phone = "" // empty values stay empty through everything
		}
		r := saveEncRecord(t, app, col, fmt.Sprintf("p%d", i), phone)
		want[r.Id] = phone
	}
	return want
}

func assertAllReadable(t testing.TB, app core.App, col *core.Collection, want map[string]string) {
	t.Helper()
	for id, phone := range want {
		r, err := app.FindRecordById(col, id)
		if err != nil {
			t.Fatalf("record %s unreadable: %v", id, err)
		}
		if got := r.GetString("phone"); got != phone {
			t.Fatalf("record %s: expected %q, got %q", id, phone, got)
		}
	}
}

func auditEntriesFor(t testing.TB, app core.App, action string) []*core.Record {
	t.Helper()
	entries, err := app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": action})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestRotateFieldKeysReencryptsEveryRow(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app)
	want := seedPeople(t, app, col, 230)

	oldKey := activeKey(t, app, col, "phone")
	auditBefore := len(mustAllRecords(t, app, core.CollectionNameAuditLog))

	var progress int
	res, err := core.RotateFieldKeys(context.Background(), app, core.FieldKeyOptions{
		Collection: col.Name, Field: "phone", BatchSize: 50,
		Progress: func(core.FieldKeyProgress) { progress++ },
	})
	if err != nil {
		t.Fatal(err)
	}

	newKey := activeKey(t, app, col, "phone")
	if newKey.KeyId == oldKey.KeyId {
		t.Fatal("the key id must advance")
	}

	f := res.Fields[0]
	if f.OldKeyId != oldKey.KeyId || f.NewKeyId != newKey.KeyId || f.Resumed || f.Reencrypted != 207 || f.Total != 207 {
		t.Fatalf("unexpected result: %+v", f)
	}
	if progress < 5 {
		t.Fatalf("expected progress reports per batch, got %d", progress)
	}

	// every non-empty row is on the new key, none on the old one
	if n := countOnKey(t, app, col, "phone", newKey.KeyId); n != 207 {
		t.Fatalf("expected 207 rows on the new key, got %d", n)
	}
	if n := countOnKey(t, app, col, "phone", oldKey.KeyId); n != 0 {
		t.Fatalf("%d rows are still on the old key", n)
	}
	assertAllReadable(t, app, col, want)

	// the old key is retired, not deleted
	rows := fieldKeyRowsOf(t, app, col, "phone")
	if len(rows) != 2 || rows[0].KeyId != oldKey.KeyId || rows[0].Retired == "" {
		t.Fatalf("expected the old key to be retired: %+v", rows)
	}

	// a fresh process (cache emptied) reads everything from the database
	core.ResetFieldKeyring()
	app2, err := restartApp(t, app)
	if err != nil {
		t.Fatal(err)
	}
	defer app2.Cleanup()
	assertAllReadable(t, app2, col, want)
	r := saveEncRecord(t, app2, col, "new", "after restart")
	if !strings.HasPrefix(rawColumn(t, app2, col, r.Id, "phone"), "enc:v1:"+newKey.KeyId+":") {
		t.Fatal("new writes must use the new key")
	}

	// exactly one audit entry for the run, none per row, and no plaintext in it
	entries := auditEntriesFor(t, app2, core.AuditActionFieldKeyRotate)
	if len(entries) != 1 {
		t.Fatalf("expected one rotation audit entry, got %d", len(entries))
	}
	meta := tests.CompactJSON(entries[0].GetString("meta"))
	if strings.Contains(meta, "555") || !strings.Contains(meta, newKey.KeyId) || !strings.Contains(meta, `"status":"ok"`) {
		t.Fatalf("unexpected audit meta: %s", meta)
	}
	if entries[0].GetString("targetCollection") != core.CollectionNameFieldKeys {
		t.Fatalf("unexpected target %q", entries[0].GetString("targetCollection"))
	}
	// +1 for the record created after the restart (audited as a normal create), +1 for the rotation
	if got := len(mustAllRecords(t, app2, core.CollectionNameAuditLog)) - auditBefore; got != 2 {
		t.Fatalf("expected 2 new audit entries (rotation + the new record), got %d", got)
	}
}

func mustAllRecords(t testing.TB, app core.App, collection string) []*core.Record {
	t.Helper()
	rs, err := app.FindAllRecords(collection)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestRotateFieldKeysRotatesEncryptedJSONAndEveryField(t *testing.T) {
	app := newEncApp(t)
	text := newEncryptedCollection(t, app)
	js := newEncryptedJSONCollection(t, app)
	wantText := seedPeople(t, app, text, 15)
	jr := saveJSONRecord(t, app, js, "A", map[string]any{"a": []int{1, 2, 3}})

	oldText := activeKey(t, app, text, "phone")
	oldJSON := activeKey(t, app, js, "profile")

	// no filter: every encrypted field of the app (other tests' collections
	// are in other apps)
	res, err := core.RotateFieldKeys(context.Background(), app, core.FieldKeyOptions{BatchSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %+v", res.Fields)
	}

	if activeKey(t, app, text, "phone").KeyId == oldText.KeyId || activeKey(t, app, js, "profile").KeyId == oldJSON.KeyId {
		t.Fatal("both keys must advance")
	}
	assertAllReadable(t, app, text, wantText)
	got, err := app.FindRecordById(js, jr.Id)
	if err != nil || string(got.GetRaw("profile").(types.JSONRaw)) != `{"a":[1,2,3]}` {
		t.Fatalf("json value after rotation: %v %v", err, got)
	}
	if n := countOnKey(t, app, js, "profile", oldJSON.KeyId); n != 0 {
		t.Fatal("json row still on the old key")
	}
}

func TestRotateFieldKeysIsResumableAfterACrash(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app)
	want := seedPeople(t, app, col, 100)

	oldKey := activeKey(t, app, col, "phone")

	crash := errors.New("simulated crash")
	core.SetFieldKeyBatchHook(func(batch int) error {
		if batch == 2 {
			return crash
		}
		return nil
	})
	t.Cleanup(func() { core.SetFieldKeyBatchHook(nil) })

	res, err := core.RotateFieldKeys(context.Background(), app, core.FieldKeyOptions{Collection: col.Name, BatchSize: 20})
	if !errors.Is(err, crash) {
		t.Fatalf("expected the crash, got %v", err)
	}
	newKeyId := res.Fields[0].NewKeyId

	// half way: two batches (40 rows) moved, the rest still on the old key, all readable
	onNew, onOld := countOnKey(t, app, col, "phone", newKeyId), countOnKey(t, app, col, "phone", oldKey.KeyId)
	if onNew != 40 || onOld != 50 {
		t.Fatalf("expected 40 on the new key and 50 on the old, got %d / %d", onNew, onOld)
	}
	assertAllReadable(t, app, col, want)

	// the process is gone: a new one starts from the database only
	core.SetFieldKeyBatchHook(nil)
	core.ResetFieldKeyring()
	app2, err := restartApp(t, app)
	if err != nil {
		t.Fatal(err)
	}
	defer app2.Cleanup()
	assertAllReadable(t, app2, col, want)

	// a prune now is refused for the key in use
	pr, err := core.PruneFieldKeys(context.Background(), app2, core.FieldKeyOptions{Collection: col.Name})
	if err != nil || len(pr.Pruned) != 0 || pr.InUse[oldKey.KeyId] != 50 {
		t.Fatalf("prune must keep a key that is in use: %+v %v", pr, err)
	}

	res, err = core.RotateFieldKeys(context.Background(), app2, core.FieldKeyOptions{Collection: col.Name, BatchSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	f := res.Fields[0]
	if !f.Resumed || f.NewKeyId != newKeyId || f.Total != 50 || f.Reencrypted != 50 {
		t.Fatalf("expected a resumed run on the same new key: %+v", f)
	}
	if rows := fieldKeyRowsOf(t, app2, col, "phone"); len(rows) != 2 {
		t.Fatalf("resuming must not create another key, got %d", len(rows))
	}
	if n := countOnKey(t, app2, col, "phone", newKeyId); n != 90 {
		t.Fatalf("expected 90 rows on the new key, got %d", n)
	}
	assertAllReadable(t, app2, col, want)

	// idempotent: the rows already on the new key are skipped (a rerun is a new rotation, which
	// moves everything again, but never loses a row)
	pr, err = core.PruneFieldKeys(context.Background(), app2, core.FieldKeyOptions{Collection: col.Name})
	if err != nil || len(pr.Pruned) != 1 || pr.Pruned[0] != oldKey.KeyId || len(pr.InUse) != 0 {
		t.Fatalf("expected the unused old key to be pruned: %+v %v", pr, err)
	}
	if rows := fieldKeyRowsOf(t, app2, col, "phone"); len(rows) != 1 || rows[0].KeyId != newKeyId {
		t.Fatalf("unexpected keys after prune: %+v", rows)
	}
	assertAllReadable(t, app2, col, want)

	// and after another restart, with the pruned key gone for good
	core.ResetFieldKeyring()
	app3, err := restartApp(t, app2)
	if err != nil {
		t.Fatal(err)
	}
	defer app3.Cleanup()
	assertAllReadable(t, app3, col, want)
}

func TestRotateFieldKeysRowsSavedDuringRotationEndOnTheNewKey(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app)
	want := seedPeople(t, app, col, 60)

	// a record loaded BEFORE the rotation (it carries the old ciphertext in its stash).
	// Take the lowest id: the hook below rewrites the highest one, and saving a stale
	// copy of that same row afterwards would (correctly) write its old value back.
	var staleId string
	for id, p := range want {
		if p != "" && (staleId == "" || id < staleId) {
			staleId = id
		}
	}
	stale, err := app.FindRecordById(col, staleId)
	if err != nil {
		t.Fatal(err)
	}

	var lastId string
	core.SetFieldKeyBatchHook(func(batch int) error {
		if batch != 1 {
			return nil
		}
		// between two batches the app writes: an existing row that has not been
		// reached yet (the highest id), and a new row
		rows, _ := app.FindAllRecords(col, dbx.NewExp("phone != ''"))
		var maxId string
		for _, r := range rows {
			if r.Id > maxId {
				maxId = r.Id
			}
		}
		lastId = maxId
		r, _ := app.FindRecordById(col, maxId)
		r.Set("phone", "written during rotation")
		if err := app.Save(r); err != nil {
			return err
		}
		want[maxId] = "written during rotation"

		n := saveEncRecord(t, app, col, "created during", "created during rotation")
		want[n.Id] = "created during rotation"
		return nil
	})
	t.Cleanup(func() { core.SetFieldKeyBatchHook(nil) })

	if _, err := core.RotateFieldKeys(context.Background(), app, core.FieldKeyOptions{Collection: col.Name, BatchSize: 10}); err != nil {
		t.Fatal(err)
	}
	core.SetFieldKeyBatchHook(nil)

	newKey := activeKey(t, app, col, "phone")
	assertAllReadable(t, app, col, want)
	if n := countEncrypted(t, app, col, "phone"); n != countOnKey(t, app, col, "phone", newKey.KeyId) {
		t.Fatalf("rows are left on the old key: %d encrypted, %d on the new one", n, countOnKey(t, app, col, "phone", newKey.KeyId))
	}
	if !strings.HasPrefix(rawColumn(t, app, col, lastId, "phone"), "enc:v1:"+newKey.KeyId+":") {
		t.Fatal("the row saved during the rotation must be on the new key")
	}

	// the stale record (loaded before) saved after the rotation is sealed with the new key
	stale.Set("name", "renamed")
	if err := app.Save(stale); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rawColumn(t, app, col, staleId, "phone"), "enc:v1:"+newKey.KeyId+":") {
		t.Fatal("a record loaded before the rotation must be re-sealed with the new key on save")
	}
	assertAllReadable(t, app, col, want)
}

func TestRotateFieldKeysUnderConcurrentWrites(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app)
	want := seedPeople(t, app, col, 120)

	ids := make([]string, 0, len(want))
	for id, p := range want {
		if p != "" {
			ids = append(ids, id)
		}
	}

	var mu sync.Mutex
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				id := ids[(i*7+w*13)%len(ids)]
				mu.Lock()
				// serialize per id by holding the lock across the save, so the expected value is exact
				r, err := app.FindRecordById(col, id)
				if err == nil {
					v := fmt.Sprintf("w%d-%d", w, i)
					r.Set("phone", v)
					if err = app.Save(r); err == nil {
						want[id] = v
					}
				}
				mu.Unlock()
				if err != nil {
					t.Errorf("writer: %v", err)
					return
				}
			}
		}(w)
	}

	for round := 0; round < 3; round++ {
		if _, err := core.RotateFieldKeys(context.Background(), app, core.FieldKeyOptions{Collection: col.Name, BatchSize: 15}); err != nil {
			// a write that sealed with the previous key right at the switch can be left behind;
			// the documented answer is to run it again
			if _, err2 := core.RotateFieldKeys(context.Background(), app, core.FieldKeyOptions{Collection: col.Name, BatchSize: 15}); err2 != nil {
				t.Fatalf("rotation failed twice: %v / %v", err, err2)
			}
		}
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	assertAllReadable(t, app, col, want)
	if rows := fieldKeyRowsOf(t, app, col, "phone"); len(rows) < 4 {
		t.Fatalf("expected at least 3 rotations, got %d keys", len(rows))
	}
}

func TestRotateFieldKeysFailsOnUndecryptableRowWithoutLosingTheOthers(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app)
	want := seedPeople(t, app, col, 20)

	// a ciphertext moved to another record (what a tampered database looks like)
	var a, b string
	for id, p := range want {
		if p == "" {
			continue
		}
		if a == "" {
			a = id
		} else if b == "" {
			b = id
			break
		}
	}
	if _, err := app.DB().NewQuery("UPDATE {{" + col.Name + "}} SET phone=(SELECT phone FROM {{" + col.Name + "}} WHERE id={:a}) WHERE id={:b}").
		Bind(dbx.Params{"a": a, "b": b}).Execute(); err != nil {
		t.Fatal(err)
	}
	delete(want, b)

	res, err := core.RotateFieldKeys(context.Background(), app, core.FieldKeyOptions{Collection: col.Name})
	if err == nil || !strings.Contains(err.Error(), "still on an old key") {
		t.Fatalf("expected an error about the row left behind, got %v", err)
	}
	if res.Fields[0].Failed != 1 {
		t.Fatalf("expected 1 failed row, got %+v", res.Fields[0])
	}
	assertAllReadable(t, app, col, want)

	// the failed run is audited as failed
	entries := auditEntriesFor(t, app, core.AuditActionFieldKeyRotate)
	if len(entries) != 1 || !strings.Contains(tests.CompactJSON(entries[0].GetString("meta")), `"status":"failed"`) {
		t.Fatalf("expected one failed audit entry: %v", entries)
	}

	// and its old key cannot be pruned
	pr, _ := core.PruneFieldKeys(context.Background(), app, core.FieldKeyOptions{Collection: col.Name})
	if len(pr.Pruned) != 0 || len(pr.InUse) != 1 {
		t.Fatalf("prune must keep the key: %+v", pr)
	}
}

func TestRotateFieldKeysRejectsBadTargets(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app)

	for name, o := range map[string]core.FieldKeyOptions{
		"unknown collection": {Collection: "nope"},
		"unknown field":      {Collection: col.Name, Field: "nope"},
		"field only":         {Field: "phone"},
		"plain field":        {Collection: col.Name, Field: "name"},
	} {
		if _, err := core.RotateFieldKeys(context.Background(), app, o); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestFieldKeyOperationsDoNotOverlap(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app)
	seedPeople(t, app, col, 10)

	entered := make(chan struct{})
	release := make(chan struct{})
	core.SetFieldKeyBatchHook(func(batch int) error {
		if batch == 1 {
			close(entered)
			<-release
		}
		return nil
	})
	t.Cleanup(func() { core.SetFieldKeyBatchHook(nil) })

	done := make(chan error, 1)
	go func() {
		_, err := core.RotateFieldKeys(context.Background(), app, core.FieldKeyOptions{Collection: col.Name, BatchSize: 3})
		done <- err
	}()
	<-entered

	if _, err := core.RewrapFieldKeys(context.Background(), app); !errors.Is(err, core.ErrFieldKeyOpRunning) {
		t.Fatalf("expected ErrFieldKeyOpRunning, got %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// KEK rotation
// ---------------------------------------------------------------------------

func newKEK(t testing.TB) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestRewrapFieldKeysWithoutTouchingData(t *testing.T) {
	kekA := setTestKEK(t)
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	col := newEncryptedCollection(t, app)
	want := seedPeople(t, app, col, 30)
	colJSON := newEncryptedJSONCollection(t, app)
	saveJSONRecord(t, app, colJSON, "j", map[string]any{"a": 1})

	// retire a key too: rewrap must cover retired keys as well
	if _, err := core.RotateFieldKeys(context.Background(), app, core.FieldKeyOptions{Collection: col.Name}); err != nil {
		t.Fatal(err)
	}

	dump := func() string {
		var sb strings.Builder
		rows, _ := app.FindAllRecords(col)
		for _, r := range rows {
			sb.WriteString(rawColumn(t, app, col, r.Id, "phone") + "|")
		}
		return sb.String()
	}
	dataBefore := dump()
	keysBefore := fieldKeyRowsOf(t, app, col, "phone")

	// a new KEK is made primary; the old one stays as the previous KEK
	kekB := newKEK(t)
	t.Setenv("CLAVIGER_KEK", kekB)
	t.Setenv("CLAVIGER_KEK_OLD", kekA)
	core.ResetFieldKeyring()
	app2, err := restartApp(t, app)
	if err != nil {
		t.Fatalf("the app must start with the new KEK + the previous one: %v", err)
	}
	defer app2.Cleanup()
	assertAllReadable(t, app2, col, want)

	res, err := core.RewrapFieldKeys(context.Background(), app2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rewrapped != 3 || res.Unchanged != 0 || len(res.From) != 1 { // 2 keys of col + 1 of colJSON
		t.Fatalf("unexpected result: %+v", res)
	}

	// the row data did not change at all; only the wrapping did
	if dump() != dataBefore {
		t.Fatal("rewrap must not touch the rows")
	}
	keysAfter := fieldKeyRowsOf(t, app2, col, "phone")
	for i := range keysAfter {
		if keysAfter[i].KeyId != keysBefore[i].KeyId || keysAfter[i].Wrapped == keysBefore[i].Wrapped || keysAfter[i].KekId == keysBefore[i].KekId {
			t.Fatalf("key %d: ids must stay, wrapping and kek id must change: %+v / %+v", i, keysBefore[i], keysAfter[i])
		}
		if keysAfter[i].KekId != res.KekId {
			t.Fatalf("not wrapped under the primary KEK")
		}
	}

	// second run is a no-op
	again, err := core.RewrapFieldKeys(context.Background(), app2)
	if err != nil || again.Rewrapped != 0 || again.Unchanged != 3 {
		t.Fatalf("expected a no-op: %+v %v", again, err)
	}

	// one audit entry per effective run, no key material in it
	entries := auditEntriesFor(t, app2, core.AuditActionFieldKeyRewrap)
	if len(entries) != 2 {
		t.Fatalf("expected 2 rewrap entries, got %d", len(entries))
	}
	for _, e := range entries {
		if s := tests.CompactJSON(e.GetString("meta")); strings.Contains(s, kekA) || strings.Contains(s, kekB) || strings.Contains(s, keysAfter[0].Wrapped) {
			t.Fatalf("audit meta leaks key material: %s", s)
		}
	}

	// the new KEK alone is enough now
	t.Setenv("CLAVIGER_KEK_OLD", "")
	core.ResetFieldKeyring()
	app3, err := restartApp(t, app2)
	if err != nil {
		t.Fatalf("the new KEK alone must open the keys: %v", err)
	}
	app3.Cleanup()

	// and the old one alone does not
	t.Setenv("CLAVIGER_KEK", kekA)
	core.ResetFieldKeyring()
	if _, err := restartApp(t, app2); err == nil {
		t.Fatal("the old KEK must no longer open the keys")
	}
}

func TestRewrapFieldKeysFailsClosedWithoutThePreviousKEK(t *testing.T) {
	setTestKEK(t)
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	col := newEncryptedCollection(t, app)
	want := seedPeople(t, app, col, 5)
	before := fieldKeyRowsOf(t, app, col, "phone")

	// a new primary KEK but no previous one: the app cannot even start, and a
	// rewrap on an app that is already running with only the new KEK finds
	// keys it cannot open
	t.Setenv("CLAVIGER_KEK", newKEK(t))
	core.ResetFieldKeyring()
	if _, err := restartApp(t, app); err == nil {
		t.Fatal("expected the start to fail without the previous KEK")
	}

	// simulate that running app: its source is the new KEK, the keys are in memory
	app.Store().Remove("__clavigerFieldKeySource__")
	_, err = core.RewrapFieldKeys(context.Background(), app)
	if err == nil || !strings.Contains(err.Error(), "CLAVIGER_KEK_OLD") {
		t.Fatalf("expected an error naming the previous KEK variables, got %v", err)
	}
	after := fieldKeyRowsOf(t, app, col, "phone")
	if after[0].Wrapped != before[0].Wrapped || after[0].KekId != before[0].KekId {
		t.Fatal("nothing may change when a KEK is missing")
	}
	entries := auditEntriesFor(t, app, core.AuditActionFieldKeyRewrap)
	if len(entries) != 1 || !strings.Contains(tests.CompactJSON(entries[0].GetString("meta")), `"status":"failed"`) {
		t.Fatal("the refused run must be audited as failed")
	}
	_ = want
}

func TestRewrapFromTheAppKeyDerivedKEKToADedicatedOne(t *testing.T) {
	t.Setenv("CLAVIGER_KEK", "")
	t.Setenv("CLAVIGER_KEK_FILE", "")
	t.Setenv("pb_test_env", strings.Repeat("k", 32))

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	col := newEncryptedCollection(t, app)
	want := seedPeople(t, app, col, 12)

	t.Setenv("CLAVIGER_KEK", newKEK(t))
	t.Setenv("CLAVIGER_KEK_OLD_FROM_APP_KEY", "1")
	core.ResetFieldKeyring()
	app2, err := restartApp(t, app)
	if err != nil {
		t.Fatal(err)
	}
	defer app2.Cleanup()
	assertAllReadable(t, app2, col, want)

	res, err := core.RewrapFieldKeys(context.Background(), app2)
	if err != nil || res.Rewrapped != 1 {
		t.Fatalf("%+v %v", res, err)
	}

	// the dedicated KEK alone (app key still set, but not as a fallback) now works
	t.Setenv("CLAVIGER_KEK_OLD_FROM_APP_KEY", "")
	core.ResetFieldKeyring()
	app3, err := restartApp(t, app2)
	if err != nil {
		t.Fatal(err)
	}
	defer app3.Cleanup()
	assertAllReadable(t, app3, col, want)
}

// ---------------------------------------------------------------------------
// convert + strict
// ---------------------------------------------------------------------------

func TestConvertFieldValuesAndStrictMode(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app)
	want := seedPeople(t, app, col, 25)

	// legacy plaintext: what a text -> encryptedText conversion leaves behind
	legacy := map[string]string{}
	for id, p := range want {
		if p != "" && len(legacy) < 12 {
			if _, err := app.DB().NewQuery("UPDATE {{" + col.Name + "}} SET phone={:v} WHERE id={:id}").
				Bind(dbx.Params{"v": p, "id": id}).Execute(); err != nil {
				t.Fatal(err)
			}
			legacy[id] = p
		}
	}
	assertAllReadable(t, app, col, want) // legacy values are read as plaintext

	// strict cannot be turned on while legacy values exist
	if err := core.SetFieldKeysStrict(context.Background(), app, col.Name, "phone", true); err == nil || !strings.Contains(err.Error(), "keys convert") {
		t.Fatalf("expected a refusal that points at convert, got %v", err)
	}

	var batches int
	core.SetFieldKeyBatchHook(func(int) error { batches++; return nil })
	res, err := core.ConvertFieldValues(context.Background(), app, core.FieldKeyOptions{Collection: col.Name, Field: "phone", BatchSize: 5})
	core.SetFieldKeyBatchHook(nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fields[0].Converted != 12 || res.Fields[0].Total != 12 || batches < 3 {
		t.Fatalf("unexpected result %+v (batches %d)", res.Fields[0], batches)
	}
	for id := range legacy {
		if !strings.HasPrefix(rawColumn(t, app, col, id, "phone"), "enc:v1:") {
			t.Fatalf("record %s was not converted", id)
		}
	}
	assertAllReadable(t, app, col, want)

	// idempotent
	res, err = core.ConvertFieldValues(context.Background(), app, core.FieldKeyOptions{Collection: col.Name, Field: "phone"})
	if err != nil || res.Fields[0].Converted != 0 {
		t.Fatalf("expected a no-op, got %+v %v", res, err)
	}

	// strict on: allowed now
	if err := core.SetFieldKeysStrict(context.Background(), app, col.Name, "phone", true); err != nil {
		t.Fatal(err)
	}
	if activeKey(t, app, col, "phone").Strict != "1" {
		t.Fatal("the flag must be stored with the field key")
	}
	assertAllReadable(t, app, col, want)

	// an unencrypted value written behind the app's back is now a read error
	var victim string
	for id, p := range want {
		if p != "" {
			victim = id
			break
		}
	}
	if _, err := app.DB().NewQuery("UPDATE {{" + col.Name + "}} SET phone='sneaky plaintext' WHERE id={:id}").Bind(dbx.Params{"id": victim}).Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.FindRecordById(col, victim); err == nil || !strings.Contains(err.Error(), "strict mode") {
		t.Fatalf("expected a strict mode read error, got %v", err)
	}
	if _, err := app.FindAllRecords(col); err == nil {
		t.Fatal("a list containing the row must fail too")
	}

	// the flag survives a DEK rotation and a restart
	if _, err := app.DB().NewQuery("UPDATE {{" + col.Name + "}} SET phone='' WHERE id={:id}").Bind(dbx.Params{"id": victim}).Execute(); err != nil {
		t.Fatal(err)
	}
	want[victim] = ""
	if _, err := core.RotateFieldKeys(context.Background(), app, core.FieldKeyOptions{Collection: col.Name}); err != nil {
		t.Fatal(err)
	}
	if activeKey(t, app, col, "phone").Strict != "1" {
		t.Fatal("rotation must carry the strict flag to the new key")
	}
	core.ResetFieldKeyring()
	app2, err := restartApp(t, app)
	if err != nil {
		t.Fatal(err)
	}
	defer app2.Cleanup()
	if _, err := app2.DB().NewQuery("UPDATE {{" + col.Name + "}} SET phone='again' WHERE id={:id}").Bind(dbx.Params{"id": victim}).Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := app2.FindRecordById(col, victim); err == nil || !strings.Contains(err.Error(), "strict mode") {
		t.Fatalf("strict mode must survive a restart, got %v", err)
	}

	// off again: legacy values are readable again
	if err := core.SetFieldKeysStrict(context.Background(), app2, col.Name, "phone", false); err != nil {
		t.Fatal(err)
	}
	if r, err := app2.FindRecordById(col, victim); err != nil || r.GetString("phone") != "again" {
		t.Fatalf("expected the legacy value back, got %v", err)
	}

	// strict needs both names
	if err := core.SetFieldKeysStrict(context.Background(), app2, col.Name, "", true); err == nil {
		t.Fatal("expected an error without a field")
	}
	if n := len(auditEntriesFor(t, app2, core.AuditActionFieldKeyStrict)); n != 4 {
		t.Fatalf("expected 4 strict audit entries (refused, on, off, refused), got %d", n)
	}
}

func TestConvertEncryptedJSONLegacyValues(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedJSONCollection(t, app)
	r := saveJSONRecord(t, app, col, "A", map[string]any{"a": 1})
	r2 := saveJSONRecord(t, app, col, "B", "x")

	for id, v := range map[string]string{r.Id: `{"legacy":true}`, r2.Id: `not json at all`} {
		if _, err := app.DB().NewQuery("UPDATE {{" + col.Name + "}} SET profile={:v} WHERE id={:id}").Bind(dbx.Params{"v": v, "id": id}).Execute(); err != nil {
			t.Fatal(err)
		}
	}

	res, err := core.ConvertFieldValues(context.Background(), app, core.FieldKeyOptions{Collection: col.Name})
	if err != nil || res.Fields[0].Converted != 2 {
		t.Fatalf("%+v %v", res, err)
	}

	got, err := app.FindRecordById(col, r.Id)
	if err != nil || string(got.GetRaw("profile").(types.JSONRaw)) != `{"legacy":true}` {
		t.Fatalf("%v %v", err, got.GetRaw("profile"))
	}
	got, err = app.FindRecordById(col, r2.Id)
	if err != nil || string(got.GetRaw("profile").(types.JSONRaw)) != `"not json at all"` {
		t.Fatalf("a non-JSON legacy string must come back as a JSON string: %v %s", err, got.GetRaw("profile"))
	}
}

func TestFieldKeysStatus(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app)
	seedPeople(t, app, col, 20)

	st, err := core.FieldKeysStatus(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Fields) != 1 || st.Fields[0].RowsOnCurrentKey != 18 || st.Fields[0].RowsOnOldKeys != 0 || st.Fields[0].NeedsRewrap || st.HasPreviousKek {
		t.Fatalf("%+v", st)
	}

	crash := errors.New("crash")
	core.SetFieldKeyBatchHook(func(b int) error {
		if b == 1 {
			return crash
		}
		return nil
	})
	_, _ = core.RotateFieldKeys(context.Background(), app, core.FieldKeyOptions{Collection: col.Name, BatchSize: 5})
	core.SetFieldKeyBatchHook(nil)

	st, _ = core.FieldKeysStatus(context.Background(), app)
	f := st.Fields[0]
	if f.RowsOnCurrentKey != 5 || f.RowsOnOldKeys != 13 || len(f.Retired) != 1 || f.Retired[0].Rows != 13 {
		t.Fatalf("a half finished rotation must be visible: %+v", f)
	}
}
