package core_test

import (
	"crypto/rand"
	"encoding/base64"
	stdjson "encoding/json"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #75, M2): the encryptedJSON field type.

func newEncryptedJSONCollection(t testing.TB, app core.App, mutate ...func(*core.Collection)) *core.Collection {
	t.Helper()

	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name := "notes" + strings.NewReplacer("-", "x", "_", "y").Replace(base64.RawURLEncoding.EncodeToString(suffix))

	col := core.NewBaseCollection(name)
	col.Fields.Add(
		&core.TextField{Name: "name"},
		&core.EncryptedJSONField{Name: "profile", MaxSize: 200},
	)
	for _, m := range mutate {
		m(col)
	}
	if err := app.Save(col); err != nil {
		t.Fatalf("failed to save the collection: %v", err)
	}
	return col
}

func saveJSONRecord(t testing.TB, app core.App, col *core.Collection, name string, profile any) *core.Record {
	t.Helper()
	r := core.NewRecord(col)
	r.Set("name", name)
	r.Set("profile", profile)
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestEncryptedJSONRoundTripStoresCiphertext(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedJSONCollection(t, app)

	r := saveJSONRecord(t, app, col, "Ann", map[string]any{"ssn": "123-45-6789", "kids": []int{1, 2}})

	stored := rawColumn(t, app, col, r.Id, "profile")
	if !strings.HasPrefix(stored, "enc:v1:") || strings.Contains(stored, "123") {
		t.Fatalf("expected ciphertext in the database, got %q", stored)
	}

	loaded, err := app.FindRecordById(col, r.Id)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := loaded.GetRaw("profile").(types.JSONRaw)
	if !ok || !strings.Contains(string(raw), `"ssn":"123-45-6789"`) {
		t.Fatalf("expected the plaintext JSON back, got %T %s", loaded.GetRaw("profile"), raw)
	}

	// the API representation is real JSON, not a quoted string
	exported, _ := loaded.MarshalJSON()
	if !strings.Contains(string(exported), `"ssn":"123-45-6789"`) {
		t.Fatalf("unexpected export: %s", exported)
	}

	// other JSON shapes
	for name, v := range map[string]any{"string": "hello", "number": 42, "array": []string{"a"}, "bool": true} {
		r := saveJSONRecord(t, app, col, name, v)
		if got, _ := app.FindRecordById(col, r.Id); string(got.GetRaw("profile").(types.JSONRaw)) == "" {
			t.Fatalf("%s: lost the value", name)
		}
	}
}

func TestEncryptedJSONNullAndEmptyAreStoredEmpty(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedJSONCollection(t, app)

	for name, v := range map[string]any{"unset": nil, "null": "null"} {
		r := core.NewRecord(col)
		r.Set("name", name)
		if v != nil {
			r.Set("profile", v)
		}
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
		if got := rawColumn(t, app, col, r.Id, "profile"); got != "" {
			t.Fatalf("%s: expected an empty column, got %q", name, got)
		}
		loaded, err := app.FindRecordById(col, r.Id)
		if err != nil {
			t.Fatal(err)
		}
		if s := loaded.GetRaw("profile").(types.JSONRaw).String(); s != "null" {
			t.Fatalf("%s: expected null, got %s", name, s)
		}
	}
}

func TestEncryptedJSONAADBindingAndPastedCiphertext(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedJSONCollection(t, app)

	a := saveJSONRecord(t, app, col, "A", map[string]any{"k": "a"})
	b := saveJSONRecord(t, app, col, "B", map[string]any{"k": "b"})

	stolen := rawColumn(t, app, col, a.Id, "profile")
	if _, err := app.DB().NewQuery("UPDATE {{" + col.Name + "}} SET profile={:v} WHERE id={:id}").
		Bind(dbx.Params{"v": stolen, "id": b.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.FindRecordById(col, b.Id); err == nil {
		t.Fatal("expected the moved ciphertext to be rejected")
	}

	// pasted ciphertext through the setter is just a JSON string, never decrypted
	r := saveJSONRecord(t, app, col, "C", stolen)
	got, err := app.FindRecordById(col, r.Id)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.GetRaw("profile").(types.JSONRaw)) != `"`+stolen+`"` {
		t.Fatalf("pasted ciphertext should round trip as a string, got %s", got.GetRaw("profile"))
	}
	if stored := rawColumn(t, app, col, r.Id, "profile"); strings.Contains(stored, stolen) {
		t.Fatal("pasted ciphertext was stored as is")
	}
}

func TestEncryptedJSONUnchangedSaveKeepsCiphertext(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedJSONCollection(t, app)

	r := saveJSONRecord(t, app, col, "A", map[string]any{"k": "v"})
	before := rawColumn(t, app, col, r.Id, "profile")

	for _, ignoreUnchanged := range []bool{false, true} {
		loaded, _ := app.FindRecordById(col, r.Id)
		loaded.IgnoreUnchangedFields(ignoreUnchanged)
		loaded.Set("name", "A"+strings.Repeat("x", 1+btoi(ignoreUnchanged)))
		if err := app.Save(loaded); err != nil {
			t.Fatal(err)
		}
		if after := rawColumn(t, app, col, r.Id, "profile"); after != before {
			t.Fatalf("ignoreUnchanged=%v: the ciphertext was rewritten", ignoreUnchanged)
		}
	}

	loaded, _ := app.FindRecordById(col, r.Id)
	loaded.Set("profile", map[string]any{"k": "changed"})
	if err := app.Save(loaded); err != nil {
		t.Fatal(err)
	}
	if rawColumn(t, app, col, r.Id, "profile") == before {
		t.Fatal("expected a new ciphertext")
	}
}

func TestEncryptedJSONValidation(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedJSONCollection(t, app, func(c *core.Collection) {
		f := c.Fields.GetByName("profile").(*core.EncryptedJSONField)
		f.Required = true
		f.MaxSize = 20
	})

	scenarios := map[string]struct {
		value any
		ok    bool
	}{
		"empty (required)":  {"", false},
		"null (required)":   {"null", false},
		"empty object":      {"{}", false},
		"too large":         {map[string]string{"a": strings.Repeat("x", 30)}, false},
		"ok":                {map[string]string{"a": "b"}, true},
		"ok string literal": {"abc", true},
	}
	for name, s := range scenarios {
		r := core.NewRecord(col)
		r.Set("profile", s.value)
		if err := app.Save(r); (err == nil) != s.ok {
			t.Errorf("%s: ok=%v, err=%v", name, s.ok, err)
		}
	}
}

func TestEncryptedJSONSchemaRejections(t *testing.T) {
	app := newEncApp(t)

	scenarios := map[string]func(*core.Collection){
		"unique index": func(c *core.Collection) { c.AddIndex("idx_u_"+c.Name, true, "profile", "") },
		"list rule": func(c *core.Collection) {
			rule := `profile != ""`
			c.ListRule = &rule
		},
		"json path rule": func(c *core.Collection) {
			rule := `profile.a = "x"`
			c.ViewRule = &rule
		},
		"before id": func(c *core.Collection) { c.Fields = append(c.Fields[1:], c.Fields[0]) },
	}
	for name, mutate := range scenarios {
		t.Run(name, func(t *testing.T) {
			col := core.NewBaseCollection("jrej" + strings.ReplaceAll(name, " ", ""))
			col.Fields.Add(&core.TextField{Name: "name"}, &core.EncryptedJSONField{Name: "profile"})
			mutate(col)
			if err := app.Save(col); err == nil {
				t.Fatalf("expected %q to be rejected", name)
			}
		})
	}

	col := newEncryptedJSONCollection(t, app)
	saveJSONRecord(t, app, col, "A", map[string]any{"a": 1})
	if _, err := app.FindRecordsByFilter(col, `profile != ""`, "", 10, 0); err == nil || !strings.Contains(err.Error(), "ncrypted") {
		t.Fatalf("expected the filter to be rejected, got %v", err)
	}
	// a JSON path into the ciphertext is rejected too (as an invalid path)
	if _, err := app.FindRecordsByFilter(col, `profile.a = 1`, "", 10, 0); err == nil {
		t.Fatal("expected the json path filter to be rejected")
	}
	if _, err := app.FindRecordsByFilter(col, `name != ""`, "profile", 10, 0); err == nil {
		t.Fatal("expected the sort to be rejected")
	}
}

func TestEncryptedJSONLegacyPlaintextAndColumnType(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedJSONCollection(t, app)
	r := saveJSONRecord(t, app, col, "A", map[string]any{"a": 1})

	// a value written before the column became encrypted
	if _, err := app.DB().NewQuery("UPDATE {{" + col.Name + "}}" + ` SET profile='{"legacy":true}' WHERE id={:id}`).
		Bind(dbx.Params{"id": r.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	loaded, err := app.FindRecordById(col, r.Id)
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded.GetRaw("profile").(types.JSONRaw)) != `{"legacy":true}` {
		t.Fatalf("legacy plaintext should be readable: %s", loaded.GetRaw("profile"))
	}
	loaded.Set("name", "A2")
	if err := app.Save(loaded); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rawColumn(t, app, col, r.Id, "profile"), "enc:v1:") {
		t.Fatal("expected the legacy value to be encrypted on the next save")
	}

	var colType string
	info, _ := app.TableInfo(col.Name)
	for _, c := range info {
		if c.Name == "profile" {
			colType = c.Type
		}
	}
	if strings.Contains(strings.ToUpper(colType), "JSON") {
		t.Fatalf("the column must be plain TEXT, got %q", colType)
	}
}

func TestEncryptedJSONAuditRedactionByType(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedJSONCollection(t, app)

	r := saveJSONRecord(t, app, col, "Ann", map[string]any{"diagnosis": "secretvalue"})
	r.Set("profile", map[string]any{"diagnosis": "othersecret"})
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}

	entries, err := app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"targetCollection": col.Name})
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected 2 audit entries, got %d (%v)", len(entries), err)
	}
	for _, e := range entries {
		for _, f := range []string{"before", "after", "meta"} {
			if s := e.GetString(f); strings.Contains(s, "secret") {
				t.Fatalf("audit entry leaks the plaintext in %s: %s", f, s)
			}
		}
	}
}

func TestEncryptedJSONHiddenAndRestart(t *testing.T) {
	app := newEncApp(t)
	col := newEncryptedJSONCollection(t, app, func(c *core.Collection) {
		c.Fields.GetByName("profile").SetHidden(true)
	})
	r := saveJSONRecord(t, app, col, "A", map[string]any{"a": 1})

	loaded, _ := app.FindRecordById(col, r.Id)
	if strings.Contains(publicJSON(t, loaded), "profile") {
		t.Fatal("hidden field leaked in the public export")
	}

	app2, err := restartApp(t, app)
	if err != nil {
		t.Fatal(err)
	}
	defer app2.Cleanup()
	got, err := app2.FindRecordById(col.Id, r.Id)
	if err != nil || string(got.GetRaw("profile").(types.JSONRaw)) != `{"a":1}` {
		t.Fatalf("after restart: %v %v", err, got)
	}
}

func publicJSON(t testing.TB, r *core.Record) string {
	t.Helper()
	b, err := stdjson.Marshal(r.PublicExport())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
