package core_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// storedSecret reads the secret straight from the database.
func storedSecret(t *testing.T, app core.App) string {
	t.Helper()

	var v string
	if err := app.DB().NewQuery("SELECT [[value]] FROM {{_params}} WHERE [[id]]='erasureSecret'").Row(&v); err != nil {
		t.Fatal(err)
	}

	return strings.Trim(v, `"`)
}

func TestErasurePseudonymIsKeyedPerInstall(t *testing.T) {
	a, _ := tests.NewTestApp()
	defer a.Cleanup()
	b, _ := tests.NewTestApp()
	defer b.Cleanup()

	id := "abc123def456ghi"
	p := core.ErasurePseudonym(a, id)

	if !regexp.MustCompile(`^erased-[0-9a-f]{24}$`).MatchString(p) {
		t.Fatalf("form %q", p)
	}

	// not the old, unkeyed values
	for _, label := range []string{"claviger-consent:", "claviger-pseudonym:", ""} {
		sum := sha256.Sum256([]byte(label + id))
		if p == "erased-"+hex.EncodeToString(sum[:])[:24] {
			t.Errorf("the pseudonym equals an unkeyed hash (%q)", label)
		}
	}

	if sum := sha256.Sum256([]byte("claviger-erasure-log:users:" + id)); core.ErasureSubjectHash(a, "users", id) == hex.EncodeToString(sum[:]) {
		t.Error("the log hash equals sha256")
	}

	// stable in one install, different in another
	if p != core.ErasurePseudonym(a, id) || p == core.ErasurePseudonym(b, id) {
		t.Error("keyed per install")
	}

	if core.ConsentPseudonym(a, id) != p {
		t.Error("consents use the same pseudonym")
	}

	if core.ErasureSubjectHash(a, "users", id) == core.ErasureSubjectHash(b, "users", id) {
		t.Error("the log hash is keyed per install")
	}

	// the secret is made once, kept in the database, and survives a cold cache
	s := storedSecret(t, a)
	core.ForgetErasureSecret(a)

	if core.ErasurePseudonym(a, id) != p || storedSecret(t, a) != s {
		t.Error("the secret must be read back, not remade")
	}
}

func TestErasureReplaySkipsWhenTheSecretIsMissing(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	gwen := w.users["gwen"]
	email := gwen.Email()

	w.erase(t, gwen)

	back := w.fresh(t, gwen)
	back.SetEmail(email)
	mustSave(t, app, back)

	// the secret is lost (a database restored without it)
	if _, err := app.DB().NewQuery("DELETE FROM {{_params}} WHERE [[id]]='erasureSecret'").Execute(); err != nil {
		t.Fatal(err)
	}

	core.ForgetErasureSecret(app)

	n, err := core.ReplayErasureLog(app)
	if err != nil || n != 0 {
		t.Fatalf("replay with no secret: %d %v", n, err)
	}

	if core.IsErasedAccount(w.fresh(t, gwen)) {
		t.Fatal("nothing may be erased")
	}

	var cnt int
	_ = app.DB().NewQuery("SELECT COUNT(*) FROM {{_params}} WHERE [[id]]='erasureSecret'").Row(&cnt)

	if cnt != 0 {
		t.Fatal("a replay must not make a new secret")
	}

	// a new erasure makes one
	if _, res := w.erase(t, w.users["yan"]); res.Subject.Pseudonym == "" {
		t.Fatal("no pseudonym")
	}

	if storedSecret(t, app) == "" {
		t.Fatal("erasure creates the secret")
	}
}

func TestErasureSecretIsNeverInAnExportOrTheAuditLog(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	ned := w.users["ned"]

	w.erase(t, w.users["gwen"])

	secret := storedSecret(t, app)
	if len(secret) < 40 {
		t.Fatalf("secret %q", secret)
	}

	var buf bytes.Buffer
	if _, err := core.WriteDataExport(app, ned, &buf); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(buf.String(), secret) {
		t.Error("the export holds the secret")
	}

	for _, c := range []string{core.CollectionNameAuditLog, core.CollectionNameErasureLog, core.CollectionNameDataRequests} {
		rows, _ := app.FindAllRecords(c)
		for _, r := range rows {
			if strings.Contains(string(erJSON(t, r.FieldsData())), secret) {
				t.Errorf("%s holds the secret", c)
			}
		}
	}

	// the settings (what the settings API returns) do not carry it
	if strings.Contains(string(erJSON(t, app.Settings())), secret) {
		t.Error("settings hold the secret")
	}
}
