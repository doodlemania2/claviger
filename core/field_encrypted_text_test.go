package core_test

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #75).
//
// These tests set CLAVIGER_KEK through the environment and therefore cannot
// run in parallel.

func setTestKEK(t testing.TB) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	kek := base64.StdEncoding.EncodeToString(b)
	t.Setenv("CLAVIGER_KEK", kek)
	t.Setenv("CLAVIGER_KEK_FILE", "")
	return kek
}

var encColSeq int

// newEncryptedCollection saves a collection with a plain and an encrypted
// text field. The name is unique per call because the collection and field
// ids derive from it, and the data key cache is process-wide.
func newEncryptedCollection(t testing.TB, app core.App, mutate ...func(*core.Collection)) *core.Collection {
	t.Helper()

	encColSeq++
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name := "people" + base64.RawURLEncoding.EncodeToString(suffix)
	name = strings.NewReplacer("-", "x", "_", "y").Replace(name)

	col := core.NewBaseCollection(name)
	col.Fields.Add(
		&core.TextField{Name: "name"},
		&core.EncryptedTextField{Name: "phone", Max: 50},
		&core.TextField{Name: "plainPhone"},
	)
	for _, m := range mutate {
		m(col)
	}

	if err := app.Save(col); err != nil {
		t.Fatalf("failed to save the collection: %v", err)
	}

	return col
}

func rawColumn(t testing.TB, app core.App, col *core.Collection, id, column string) string {
	t.Helper()
	var v string
	err := app.DB().Select(column).From(col.Name).Where(dbx.HashExp{"id": id}).Row(&v)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestEncryptedTextRoundTripStoresCiphertext(t *testing.T) {
	setTestKEK(t)

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	col := newEncryptedCollection(t, app)

	r := core.NewRecord(col)
	r.Set("name", "Ann")
	r.Set("phone", "+1 555 0100")
	r.Set("plainPhone", "+1 555 0100")
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}

	stored := rawColumn(t, app, col, r.Id, "phone")
	if !strings.HasPrefix(stored, "enc:v1:") || strings.Contains(stored, "555") {
		t.Fatalf("expected ciphertext in the database, got %q", stored)
	}
	if got := rawColumn(t, app, col, r.Id, "plainPhone"); got != "+1 555 0100" {
		t.Fatalf("plain column changed: %q", got)
	}

	loaded, err := app.FindRecordById(col, r.Id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.GetString("phone") != "+1 555 0100" {
		t.Fatalf("expected plaintext, got %q", loaded.GetString("phone"))
	}

	// empty stays empty
	empty := core.NewRecord(col)
	empty.Set("name", "Bob")
	if err := app.Save(empty); err != nil {
		t.Fatal(err)
	}
	if got := rawColumn(t, app, col, empty.Id, "phone"); got != "" {
		t.Fatalf("expected an empty column, got %q", got)
	}
}

func newEncApp(t *testing.T) *tests.TestApp {
	t.Helper()
	setTestKEK(t)
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	return app
}

func saveEncRecord(t testing.TB, app core.App, col *core.Collection, name, phone string) *core.Record {
	t.Helper()
	r := core.NewRecord(col)
	r.Set("name", name)
	r.Set("phone", phone)
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestEncryptedTextAADBinding(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app)

	a := saveEncRecord(t, app, col, "A", "111-1111")
	b := saveEncRecord(t, app, col, "B", "222-2222")

	// copy A's ciphertext into B's row with raw SQL (what an attacker with
	// database access, or a leaked backup, could do)
	stolen := rawColumn(t, app, col, a.Id, "phone")
	if _, err := app.DB().NewQuery("UPDATE " + col.Name + " SET phone={:v} WHERE id={:id}").
		Bind(dbx.Params{"v": stolen, "id": b.Id}).Execute(); err != nil {
		t.Fatal(err)
	}

	if _, err := app.FindRecordById(col, b.Id); err == nil {
		t.Fatal("expected the moved ciphertext to be rejected")
	}

	// A is untouched
	if got, err := app.FindRecordById(col, a.Id); err != nil || got.GetString("phone") != "111-1111" {
		t.Fatalf("A should still decrypt: %v", err)
	}
}

func TestEncryptedTextRejectsReservedPrefixOnInput(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app)

	a := saveEncRecord(t, app, col, "A", "111-1111")
	stolen := rawColumn(t, app, col, a.Id, "phone")

	r := core.NewRecord(col)
	r.Set("name", "B")
	r.Set("phone", stolen) // pasted ciphertext
	if err := app.Save(r); err == nil {
		t.Fatal("expected a validation error for pasted ciphertext")
	}
}

func TestEncryptedTextUnchangedSaveKeepsCiphertext(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app)

	r := saveEncRecord(t, app, col, "A", "111-1111")
	before := rawColumn(t, app, col, r.Id, "phone")

	for _, ignoreUnchanged := range []bool{false, true} {
		loaded, err := app.FindRecordById(col, r.Id)
		if err != nil {
			t.Fatal(err)
		}
		loaded.IgnoreUnchangedFields(ignoreUnchanged)
		loaded.Set("name", "A "+strings.Repeat("x", 1+btoi(ignoreUnchanged)))
		if err := app.Save(loaded); err != nil {
			t.Fatal(err)
		}
		if after := rawColumn(t, app, col, r.Id, "phone"); after != before {
			t.Fatalf("ignoreUnchanged=%v: the ciphertext was rewritten:\n%s\n%s", ignoreUnchanged, before, after)
		}
	}

	// a real change seals a new value
	loaded, _ := app.FindRecordById(col, r.Id)
	loaded.Set("phone", "333-3333")
	if err := app.Save(loaded); err != nil {
		t.Fatal(err)
	}
	if after := rawColumn(t, app, col, r.Id, "phone"); after == before {
		t.Fatal("expected a new ciphertext")
	}
	if got, _ := app.FindRecordById(col, r.Id); got.GetString("phone") != "333-3333" {
		t.Fatal("changed value did not round trip")
	}

	// the same plaintext sealed twice differs (random nonce)
	r2 := saveEncRecord(t, app, col, "A2", "111-1111")
	if rawColumn(t, app, col, r2.Id, "phone") == before {
		t.Fatal("ciphertexts must not repeat")
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestEncryptedTextValidation(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app, func(c *core.Collection) {
		f := c.Fields.GetByName("phone").(*core.EncryptedTextField)
		f.Required = true
		f.Min = 3
		f.Max = 6
	})

	scenarios := map[string]struct {
		value string
		ok    bool
	}{
		"empty (required)":             {"", false},
		"too short":                    {"ab", false},
		"too long":                     {"1234567", false},
		"ok":                           {"123456", true},
		"multibyte counts as one char": {"日本語", true},
	}
	for name, s := range scenarios {
		r := core.NewRecord(col)
		r.Set("phone", s.value)
		err := app.Save(r)
		if (err == nil) != s.ok {
			t.Errorf("%s: ok=%v, err=%v", name, s.ok, err)
		}
	}
}

func TestEncryptedTextSchemaRejections(t *testing.T) {
	app := newEncApp(t)

	scenarios := map[string]func(*core.Collection){
		"unique index": func(c *core.Collection) {
			c.AddIndex("idx_u_"+c.Name, true, "phone", "")
		},
		"plain index": func(c *core.Collection) {
			c.AddIndex("idx_p_"+c.Name, false, "name, phone", "")
		},
		"expression index": func(c *core.Collection) {
			c.Indexes = append(c.Indexes, "CREATE INDEX `idx_e_"+c.Name+"` ON `"+c.Name+"` (lower(phone))")
		},
		"list rule": func(c *core.Collection) {
			rule := `phone = "x"`
			c.ListRule = &rule
		},
		"view rule": func(c *core.Collection) {
			rule := `phone != ""`
			c.ViewRule = &rule
		},
		"before id": func(c *core.Collection) {
			c.Fields = append(c.Fields[1:], c.Fields[0]) // the id field last
		},
	}

	for name, mutate := range scenarios {
		t.Run(name, func(t *testing.T) {
			col := core.NewBaseCollection("rej" + strings.ReplaceAll(name, " ", ""))
			col.Fields.Add(&core.TextField{Name: "name"}, &core.EncryptedTextField{Name: "phone"})
			mutate(col)
			if err := app.Save(col); err == nil {
				t.Fatalf("expected %q to be rejected", name)
			} else {
				t.Log(err)
			}
		})
	}

	// a rule on a plain field of a collection that has an encrypted one is fine
	col := core.NewBaseCollection("rejokay")
	col.Fields.Add(&core.TextField{Name: "name"}, &core.EncryptedTextField{Name: "phone"})
	rule := `name != "" && @request.body.phone != ""`
	col.ListRule = &rule
	if err := app.Save(col); err != nil {
		t.Fatalf("rule on plain fields and @request.body should be allowed: %v", err)
	}
}

func TestEncryptedTextRejectsOtherCollectionRuleViaRelation(t *testing.T) {
	app := newEncApp(t)
	people := newEncryptedCollection(t, app)

	rel := core.NewBaseCollection("relrule")
	rel.Fields.Add(&core.RelationField{Name: "person", CollectionId: people.Id, MaxSelect: 1})
	rule := `person.phone = "x"`
	rel.ListRule = &rule
	if err := app.Save(rel); err == nil {
		t.Fatal("expected a rule that traverses to an encrypted field to be rejected")
	}
}

func TestEncryptedTextFilterAndSortAreRejected(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app)
	saveEncRecord(t, app, col, "A", "111-1111")

	if _, err := app.FindRecordsByFilter(col, `phone = "111-1111"`, "", 10, 0); err == nil {
		t.Fatal("expected the filter to be rejected")
	} else if !strings.Contains(err.Error(), "encrypted") {
		t.Fatalf("expected a clear message, got %v", err)
	}

	if _, err := app.FindRecordsByFilter(col, `name != ""`, "-phone", 10, 0); err == nil {
		t.Fatal("expected the sort to be rejected")
	}

	if rs, err := app.FindRecordsByFilter(col, `name = "A"`, "", 10, 0); err != nil || len(rs) != 1 {
		t.Fatalf("plain filters keep working: %d, %v", len(rs), err)
	}
}

func TestEncryptedTextRename(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app)
	r := saveEncRecord(t, app, col, "A", "111-1111")

	col.Fields.GetByName("phone").SetName("mobile")
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}

	got, err := app.FindRecordById(col.Id, r.Id)
	if err != nil {
		t.Fatalf("a renamed field must stay readable: %v", err)
	}
	if got.GetString("mobile") != "111-1111" {
		t.Fatalf("got %q", got.GetString("mobile"))
	}
}

func TestEncryptedTextRequiresKEKWhenDefiningField(t *testing.T) {
	t.Setenv("CLAVIGER_KEK", "")
	t.Setenv("CLAVIGER_KEK_FILE", "")
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	col := core.NewBaseCollection("nokek")
	col.Fields.Add(&core.EncryptedTextField{Name: "secret"})
	err = app.Save(col)
	if err == nil || !strings.Contains(err.Error(), "CLAVIGER_KEK") {
		t.Fatalf("expected a clear key-encryption key error, got %v", err)
	}
}

// restartApp copies the data dir of app (after a checkpoint) and bootstraps a
// new app on the copy, as a process restart would.
func restartApp(t testing.TB, app core.App) (*tests.TestApp, error) {
	t.Helper()
	if _, err := app.DB().NewQuery("PRAGMA wal_checkpoint(TRUNCATE)").Execute(); err != nil {
		t.Fatal(err)
	}
	return tests.NewTestApp(app.DataDir())
}

func TestEncryptedTextSurvivesRestartAndFailsClosed(t *testing.T) {
	kek := setTestKEK(t)
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	col := newEncryptedCollection(t, app)
	r := saveEncRecord(t, app, col, "A", "111-1111")

	// same KEK: reads back
	app2, err := restartApp(t, app)
	if err != nil {
		t.Fatalf("restart with the right key failed: %v", err)
	}
	if got, err := app2.FindRecordById(col.Id, r.Id); err != nil || got.GetString("phone") != "111-1111" {
		t.Fatalf("after restart: %v", err)
	}
	app2.Cleanup()

	// missing KEK: refuses to start
	t.Setenv("CLAVIGER_KEK", "")
	if _, err := restartApp(t, app); err == nil {
		t.Fatal("expected the app to refuse to start without a key")
	} else if !strings.Contains(err.Error(), "key-encryption key") {
		t.Fatalf("unclear error: %v", err)
	}

	// wrong KEK: refuses to start and names the field
	other := make([]byte, 32)
	_, _ = rand.Read(other)
	t.Setenv("CLAVIGER_KEK", base64.StdEncoding.EncodeToString(other))
	if _, err := restartApp(t, app); err == nil {
		t.Fatal("expected the app to refuse to start with the wrong key")
	} else if !strings.Contains(err.Error(), col.Name+".phone") || !strings.Contains(err.Error(), "different key-encryption key") {
		t.Fatalf("unclear error: %v", err)
	}

	// back to the right one
	t.Setenv("CLAVIGER_KEK", kek)
	app3, err := restartApp(t, app)
	if err != nil {
		t.Fatal(err)
	}
	app3.Cleanup()
}

func TestEncryptedTextDerivedFromAppEncryptionKey(t *testing.T) {
	t.Setenv("CLAVIGER_KEK", "")
	t.Setenv("CLAVIGER_KEK_FILE", "")
	t.Setenv("pb_test_env", strings.Repeat("k", 32)) // tests.NewTestApp uses this encryption env

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	col := newEncryptedCollection(t, app)
	r := saveEncRecord(t, app, col, "A", "111-1111")

	app2, err := restartApp(t, app)
	if err != nil {
		t.Fatal(err)
	}
	defer app2.Cleanup()
	if got, err := app2.FindRecordById(col.Id, r.Id); err != nil || got.GetString("phone") != "111-1111" {
		t.Fatalf("after restart: %v", err)
	}
}

func TestEncryptedTextNoEncryptedFieldsNeedsNoKey(t *testing.T) {
	t.Setenv("CLAVIGER_KEK", "")
	t.Setenv("CLAVIGER_KEK_FILE", "")
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("an app without encrypted fields must not need a key: %v", err)
	}
	app.Cleanup()
}

func TestFieldKeysCollectionIsServerManaged(t *testing.T) {
	app := newEncApp(t)
	newEncryptedCollection(t, app)

	col, err := app.FindCollectionByNameOrId(core.CollectionNameFieldKeys)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := app.FindAllRecords(col)
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected one data key, got %d (%v)", len(rows), err)
	}
	if strings.Contains(rows[0].GetString("wrapped"), "=") && len(rows[0].GetString("wrapped")) < 40 {
		t.Fatal("suspicious wrapped value")
	}

	rows[0].Set("wrapped", "tampered")
	if err := app.Save(rows[0]); err == nil {
		t.Fatal("updating a data key through the app must be refused")
	}
	if err := app.Delete(rows[0]); err == nil {
		t.Fatal("deleting a data key through the app must be refused")
	}
	nr := core.NewRecord(col)
	nr.Set("collectionRef", "x")
	if err := app.Save(nr); err == nil {
		t.Fatal("creating a data key through the app must be refused")
	}
}

func TestEncryptedTextAuditAndWebhookRedactionByType(t *testing.T) {
	t.Setenv(core.WebhooksAllowLocalhostEnv, "true")
	app := newEncApp(t)
	// a name that the audit's name-based rules would NOT redact
	col := newEncryptedCollection(t, app)

	addEndpoint(t, app, "https://a.example.com/h", []string{col.Name + ":*"}, true)

	r := saveEncRecord(t, app, col, "Ann", "+1 555 0100")

	loaded, _ := app.FindRecordById(col, r.Id)
	loaded.Set("phone", "+1 555 0199")
	loaded.Set("name", "Ann B")
	if err := app.Save(loaded); err != nil {
		t.Fatal(err)
	}
	if err := app.Delete(loaded); err != nil {
		t.Fatal(err)
	}

	entries, err := app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"targetCollection": col.Name})
	if err != nil || len(entries) != 3 {
		t.Fatalf("expected 3 audit entries, got %d (%v)", len(entries), err)
	}
	for _, e := range entries {
		for _, f := range []string{"before", "after", "meta"} {
			s := e.GetString(f)
			if strings.Contains(s, "555") {
				t.Fatalf("audit entry leaks the plaintext in %s: %s", f, s)
			}
		}
		if e.GetString("action") == core.AuditActionCreate && !strings.Contains(e.GetString("after"), "[redacted]") {
			t.Fatalf("expected a redaction marker: %s", e.GetString("after"))
		}
	}

	ds := deliveries(t, app)
	if len(ds) != 3 {
		t.Fatalf("expected 3 webhook deliveries, got %d", len(ds))
	}
	for _, d := range ds {
		if p := d.GetString("payload"); strings.Contains(p, "555") {
			t.Fatalf("webhook payload leaks the plaintext: %s", p)
		}
	}
}

func TestEncryptedTextCollectionExportImport(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedCollection(t, app)

	exported, err := app.FindCollectionByNameOrId(col.Id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := exported.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"type":"encryptedText"`) {
		t.Fatalf("the exported collection must carry the field type: %s", raw)
	}
	// no key material in the export
	if strings.Contains(string(raw), "wrapped") || strings.Contains(string(raw), "keyId") {
		t.Fatalf("the export must not contain key material: %s", raw)
	}

	// import it into a fresh app (new data keys are created there)
	app2 := newEncApp(t)
	if err := app2.ImportCollectionsByMarshaledJSON([]byte("["+string(raw)+"]"), false); err != nil {
		t.Fatalf("import failed: %v", err)
	}

	imported, err := app2.FindCollectionByNameOrId(col.Name)
	if err != nil {
		t.Fatal(err)
	}
	r := saveEncRecord(t, app2, imported, "Z", "999")
	if got, _ := app2.FindRecordById(imported, r.Id); got.GetString("phone") != "999" {
		t.Fatal("imported collection does not round trip")
	}
	if rawColumn(t, app2, imported, r.Id, "phone") == "999" {
		t.Fatal("imported collection stores plaintext")
	}
}
