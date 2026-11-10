package core_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/filesystem"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, 88-1): the personal data export.

type exportZip struct {
	files    map[string][]byte
	manifest core.DataExportManifest
}

func (z *exportZip) all() string {
	var b strings.Builder
	for _, c := range z.files {
		b.Write(c)
		b.WriteByte('\n')
	}
	return b.String()
}

func (z *exportZip) rows(t testing.TB, collection string) []map[string]any {
	t.Helper()
	raw, ok := z.files["data/"+collection+".json"]
	if !ok {
		return nil
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("%s: %v", collection, err)
	}
	return rows
}

func buildExport(t testing.TB, app core.App, subject *core.Record) *exportZip {
	t.Helper()
	var buf bytes.Buffer
	m, err := core.WriteDataExport(app, subject, &buf)
	if err != nil {
		t.Fatal(err)
	}
	return readExportZip(t, buf.Bytes(), m)
}

func readExportZip(t testing.TB, data []byte, m *core.DataExportManifest) *exportZip {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	z := &exportZip{files: map[string][]byte{}}
	for _, f := range zr.File {
		r, _ := f.Open()
		b, _ := io.ReadAll(r)
		r.Close()
		z.files[f.Name] = b
	}
	if raw, ok := z.files["manifest.json"]; ok {
		if err := json.Unmarshal(raw, &z.manifest); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal("no manifest.json")
	}
	if _, ok := z.files["README.txt"]; !ok {
		t.Fatal("no README.txt")
	}
	_ = m
	return z
}

func newExportApp(t *testing.T) *tests.TestApp {
	t.Helper()
	setTestKEK(t)
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	return app
}

func newExportSubject(t testing.TB, app core.App, email string) *core.Record {
	t.Helper()
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	u := core.NewRecord(users)
	u.SetEmail(email)
	u.SetPassword("1234567890")
	u.Set("name", "Export Person")
	u.SetVerified(true)
	if err := app.Save(u); err != nil {
		t.Fatal(err)
	}
	return u
}

func storedColumn(t testing.TB, app core.App, table, id, column string) string {
	t.Helper()
	var v string
	if err := app.DB().Select("[[" + column + "]]").From(table).Where(dbx.HashExp{"id": id}).Row(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// hardSecrets are the columns that must never leave the system, named here
// independently of the export allowlist so that a mistake in the allowlist
// cannot hide itself. They are seeded with "HARDSECRET-..." markers.
var hardSecrets = map[string][]string{
	core.CollectionNameTOTPs:               {"secret", "backupCodes"},
	core.CollectionNameWebAuthnCredentials: {"credentialId", "publicKey", "attestationType", "aaguid", "transport", "flags"},
	core.CollectionNameAPIKeys:             {"secretHash"},
	core.CollectionNameOAuthRefreshTokens:  {"tokenHash"},
	core.CollectionNameOAuthRequests:       {"requestId", "data", "codeHash", "bindingHash"},
	core.CollectionNameOAuthSso:            {"ssoHash"},
	core.CollectionNameOAuthDeviceCodes:    {"deviceHash", "userCodeHash", "bindingHash"},
	core.CollectionNameInvitations:         {"tokenHash"},
	core.CollectionNameOTPs:                {"sentTo"},
	core.CollectionNameFieldKeys:           {"wrapped", "keyId"},
	core.CollectionNameSCIMTokens:          {"tokenHash"},
	core.CollectionNameSigningKeys:         {"privateKey"},
	core.CollectionNameOAuthClients:        {"secretHash"},
	core.CollectionNameWebhooks:            {"secret", "prevSecret"},
}

func isHardSecret(collection, field string) bool {
	return slices.Contains(hardSecrets[collection], field)
}

// seedBuiltinRows writes one row about the subject into every built-in
// collection that the data map exports. Columns outside the export allowlist
// hold "SECRET-..." markers, allowlisted ones "OK-...", so a leak of any
// non-allowlisted column fails the scan.
func seedBuiltinRows(t testing.TB, app core.App, subject *core.Record) []string {
	t.Helper()

	maps, err := app.FindAllRecords(core.CollectionNameDataMap)
	if err != nil {
		t.Fatal(err)
	}

	var seeded []string
	for _, m := range maps {
		name := m.GetString("collection")
		if !m.GetBool("builtin") || name == "users" || name == "_superusers" {
			continue
		}
		none := m.GetString("treatment") == core.DataMapTreatmentNone
		col, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		allow := map[string]bool{}
		for _, f := range core.DataExportFieldsForTest(col) {
			allow[f] = true
		}

		row := core.NewRecord(col)
		row.Id = core.GenerateDefaultRandomId()
		for _, f := range col.Fields {
			n := f.GetName()
			if n == "id" || f.Type() == core.FieldTypeAutodate || f.Type() == core.FieldTypeFile || f.Type() == core.FieldTypePassword {
				continue
			}
			marker := "SECRET-" + name + "-" + n
			if allow[n] {
				marker = "OK-" + name + "-" + n
			}
			if isHardSecret(name, n) {
				marker = "HARDSECRET-" + name + "-" + n
			}
			switch f.Type() {
			case core.FieldTypeText, core.FieldTypeEmail, core.FieldTypeURL, core.FieldTypeEditor, core.FieldTypeRelation:
				row.Set(n, marker)
			case core.FieldTypeJSON:
				row.Set(n, map[string]any{"v": marker})
			case core.FieldTypeDate:
				row.Set(n, types.NowDateTime())
			case core.FieldTypeBool:
				row.Set(n, true)
			case core.FieldTypeNumber:
				row.Set(n, 1)
			case core.FieldTypeSelect:
				row.Set(n, f.(*core.SelectField).Values[0])
			}
		}

		// the link to the subject
		var paths []string
		_ = m.UnmarshalJSONField("subjectPaths", &paths)
		for _, p := range paths {
			if f := col.Fields.GetByName(p); f != nil && f.Type() != core.FieldTypeJSON {
				v := subject.Id
				if p == "email" {
					v = subject.Email()
				}
				row.Set(p, v)
			}
		}
		if col.Fields.GetByName("collectionRef") != nil {
			row.Set("collectionRef", subject.Collection().Id)
		}
		switch name {
		case core.CollectionNameAuditLog:
			row.Set("actorCollection", "users")
			row.Set("targetCollection", "users")
		case core.CollectionNameWebhookDeliveries:
			row.Set("payload", map[string]any{"type": "record.updated", "data": map[string]any{
				"collection": "users", "recordId": subject.Id, "record": map[string]any{"v": "OK-payload"},
			}})
		}

		for _, n := range []string{"created", "updated"} {
			if col.Fields.GetByName(n) != nil {
				row.SetRaw(n, types.NowDateTime())
			}
		}
		if err := app.UnsafeWithoutHooks().SaveNoValidate(row); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !none {
			seeded = append(seeded, name)
		}
	}

	return seeded
}

func TestDataExportBuiltinAllowlistIsComplete(t *testing.T) {
	app := newExportApp(t)

	maps, _ := app.FindAllRecords(core.CollectionNameDataMap)
	for _, m := range maps {
		if !m.GetBool("builtin") || m.GetString("treatment") == core.DataMapTreatmentNone {
			continue
		}
		name := m.GetString("collection")
		spec, ok := core.BuiltinDataExportForTest[name]
		if !ok {
			t.Errorf("built-in data map entry %q is exported but has no allowlist: add it to builtinDataExport (core/data_export_fields.go)", name)
			continue
		}
		col, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			continue
		}
		for _, f := range spec.Fields {
			if col.Fields.GetByName(f) == nil {
				t.Errorf("%s: allowlisted field %q does not exist", name, f)
			}
		}
	}
}

func TestDataExportEveryBuiltinCollectionAppearsAndNoSecretsLeak(t *testing.T) {
	app := newExportApp(t)
	subject := newExportSubject(t, app, "subject@example.com")
	seeded := seedBuiltinRows(t, app, subject)
	if len(seeded) < 15 {
		t.Fatalf("expected most built-in collections to be seeded, got %v", seeded)
	}

	z := buildExport(t, app, subject)

	counts := z.manifest.Categories()
	for _, name := range seeded {
		if counts[name] < 1 {
			t.Errorf("%s has a row about the subject but is not in the export (counts %v)", name, counts)
		}
		if len(z.rows(t, name)) < 1 {
			t.Errorf("data/%s.json is empty", name)
		}
	}
	if counts["users"] != 1 {
		t.Errorf("expected the subject's own record, got %d", counts["users"])
	}

	// the profile
	users := z.rows(t, "users")
	if users[0]["email"] != "subject@example.com" || users[0]["name"] != "Export Person" {
		t.Errorf("unexpected profile %v", users[0])
	}
	for _, k := range []string{"password", "tokenKey"} {
		if _, ok := users[0][k]; ok {
			t.Errorf("%s must not be exported", k)
		}
	}

	all := z.all()

	// 1. no non-allowlisted column of any built-in collection
	if strings.Contains(all, "HARDSECRET") {
		i := strings.Index(all, "HARDSECRET")
		t.Fatalf("a secret column was exported: %s", all[max(0, i-60):min(len(all), i+80)])
	}
	if i := strings.Index(all, "SECRET-"); i >= 0 {
		t.Fatalf("a column outside the allowlist was exported: %s", all[max(0, i-60):min(len(all), i+80)])
	}

	// 2. the real secrets of the subject's own record
	hash := storedColumn(t, app, "users", subject.Id, "password")
	for what, v := range map[string]string{
		"password hash":  hash,
		"tokenKey":       subject.TokenKey(),
		"plain password": "1234567890",
	} {
		if v == "" {
			t.Fatalf("no %s to scan for", what)
		}
		if strings.Contains(all, v) {
			t.Errorf("the %s was exported", what)
		}
	}

	// 3. the allowlisted columns are there (so the scan is not vacuous)
	for _, want := range []string{"OK-_sessions-userAgent", "OK-_apiKeys-prefix", "OK-_webauthnCredentials-name", "OK-payload", "OK-_externalAuths-provider"} {
		if !strings.Contains(all, want) {
			t.Errorf("expected %q in the export", want)
		}
	}

	// the manifest
	if z.manifest.FormatVersion != core.DataExportFormatVersion || z.manifest.DataMap.Hash == "" || z.manifest.GeneratedAt == "" {
		t.Errorf("incomplete manifest: %+v", z.manifest)
	}
	skipped := map[string]string{}
	for _, s := range z.manifest.Skipped {
		skipped[s.Name] = s.Reason
	}
	if skipped[core.CollectionNameOrgs] == "" || skipped[core.CollectionNameFieldKeys] == "" {
		t.Errorf("collections without personal data must be listed as skipped: %v", skipped)
	}
	if len(z.manifest.Errors) != 0 {
		t.Errorf("unexpected errors %v", z.manifest.Errors)
	}
}

func TestDataExportLeavesOtherPeopleOut(t *testing.T) {
	app := newExportApp(t)
	subject := newExportSubject(t, app, "subject@example.com")
	other := newExportSubject(t, app, "other@example.com")

	sessions, _ := app.FindCollectionByNameOrId(core.CollectionNameSessions)
	mk := func(who *core.Record, ua string) {
		r := core.NewRecord(sessions)
		r.Id = core.GenerateDefaultRandomId()
		r.Set("collectionRef", who.Collection().Id)
		r.Set("recordRef", who.Id)
		r.Set("userAgent", ua)
		if err := app.UnsafeWithoutHooks().SaveNoValidate(r); err != nil {
			t.Fatal(err)
		}
	}
	mk(subject, "mine")
	mk(other, "theirs")

	// the same id in another collection must not match either
	r := core.NewRecord(sessions)
	r.Id = core.GenerateDefaultRandomId()
	r.Set("collectionRef", "pbc_somethingelse")
	r.Set("recordRef", subject.Id)
	r.Set("userAgent", "wrong-collection")
	_ = app.UnsafeWithoutHooks().SaveNoValidate(r)

	// audit: the subject acted on somebody else's record
	audit, _ := app.FindCollectionByNameOrId(core.CollectionNameAuditLog)
	e := core.NewRecord(audit)
	e.Id = core.GenerateDefaultRandomId()
	e.Set("action", "update")
	e.Set("actorCollection", "users")
	e.Set("actorId", subject.Id)
	e.Set("targetCollection", "users")
	e.Set("targetId", other.Id)
	e.Set("before", map[string]any{"email": "other@example.com", "name": "OTHERNAME"})
	e.Set("after", map[string]any{"name": "OTHERNAME2"})
	e.SetRaw("created", types.NowDateTime())
	if err := app.UnsafeWithoutHooks().SaveNoValidate(e); err != nil {
		t.Fatal(err)
	}

	// an invitation the subject sent to somebody else
	inv, _ := app.FindCollectionByNameOrId(core.CollectionNameInvitations)
	i := core.NewRecord(inv)
	i.Id = core.GenerateDefaultRandomId()
	i.Set("email", "invitee@example.com")
	i.Set("invitedBy", subject.Id)
	i.Set("status", "pending")
	i.SetRaw("created", types.NowDateTime())
	if err := app.UnsafeWithoutHooks().SaveNoValidate(i); err != nil {
		t.Fatal(err)
	}

	z := buildExport(t, app, subject)
	all := z.all()

	for _, bad := range []string{"theirs", "wrong-collection", "OTHERNAME", "other@example.com", "invitee@example.com"} {
		if strings.Contains(all, bad) {
			t.Errorf("another person's data (%q) was exported", bad)
		}
	}
	if !strings.Contains(all, "mine") {
		t.Error("the subject's own session is missing")
	}
	var actorOnly map[string]any
	for _, r := range z.rows(t, core.CollectionNameAuditLog) {
		if r["targetId"] == other.Id {
			actorOnly = r
		}
	}
	if actorOnly == nil {
		t.Fatal("the entry where the subject is the actor must be listed")
	}
	if _, ok := actorOnly["before"]; ok {
		t.Error("before/after of somebody else's record must be removed")
	}
	if rows := z.rows(t, core.CollectionNameInvitations); len(rows) != 1 {
		t.Errorf("the sent invitation is listed without the invited address: %v", rows)
	} else if _, ok := rows[0]["email"]; ok {
		t.Error("invited address exported")
	}
}

func TestDataExportCustomCollectionDecryptsAndSkipsSecrets(t *testing.T) {
	app := newExportApp(t)
	subject := newExportSubject(t, app, "subject@example.com")
	other := newExportSubject(t, app, "other@example.com")

	users, _ := app.FindCollectionByNameOrId("users")
	col := core.NewBaseCollection("prayers")
	col.Fields.Add(
		&core.TextField{Name: "title"},
		&core.RelationField{Name: "author", CollectionId: users.Id, MaxSelect: 1},
		&core.RelationField{Name: "watchers", CollectionId: users.Id, MaxSelect: 10},
		&core.EncryptedTextField{Name: "note", Max: 200},
		&core.TextField{Name: "apiToken"},
		&core.FileField{Name: "attachment", MaxSelect: 3, MaxSize: 1 << 20},
	)
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}

	mkFile := func(name, content string) *filesystem.File {
		f, err := filesystem.NewFileFromBytes([]byte(content), name)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	mine := core.NewRecord(col)
	mine.Set("title", "mine")
	mine.Set("author", subject.Id)
	mine.Set("note", "sealed-note-text")
	mine.Set("apiToken", "TOKENVALUE")
	mine.Set("attachment", []*filesystem.File{mkFile("a.txt", "small file"), mkFile("big.bin", strings.Repeat("x", 4096))})
	if err := app.Save(mine); err != nil {
		t.Fatal(err)
	}
	watched := core.NewRecord(col)
	watched.Set("title", "watched")
	watched.Set("author", other.Id)
	watched.Set("watchers", []string{other.Id, subject.Id})
	if err := app.Save(watched); err != nil {
		t.Fatal(err)
	}
	theirs := core.NewRecord(col)
	theirs.Set("title", "theirs")
	theirs.Set("author", other.Id)
	if err := app.Save(theirs); err != nil {
		t.Fatal(err)
	}

	// not registered yet: nothing about the collection
	if got := buildExport(t, app, subject).manifest.Categories()["prayers"]; got != 0 {
		t.Fatalf("an unmapped collection must not be exported, got %d", got)
	}

	entry := core.NewRecord(mustCollection(t, app, core.CollectionNameDataMap))
	entry.Set("collection", "prayers")
	entry.Set("treatment", core.DataMapTreatmentErase)
	entry.Set("subjectPaths", []string{"author", "watchers"})
	if err := app.Save(entry); err != nil {
		t.Fatal(err)
	}

	t.Setenv(core.DataExportMaxFileMBEnv, "") // default limits
	z := buildExport(t, app, subject)

	rows := z.rows(t, "prayers")
	if len(rows) != 2 {
		t.Fatalf("expected the authored and the watched prayer, got %d: %v", len(rows), rows)
	}
	all := z.all()
	if !strings.Contains(all, "sealed-note-text") {
		t.Error("encrypted fields are exported decrypted")
	}
	if strings.Contains(all, "TOKENVALUE") || strings.Contains(all, "apiToken") {
		t.Error("a field that looks like a secret was exported")
	}
	if strings.Contains(all, "\"theirs\"") {
		t.Error("a prayer of somebody else was exported")
	}
	if len(z.manifest.Files.Skipped) != 0 || z.manifest.Files.Included != 2 {
		t.Errorf("files: %+v", z.manifest.Files)
	}
	var found bool
	for name, b := range z.files {
		if strings.HasPrefix(name, "files/prayers/"+mine.Id+"/") && strings.Contains(string(b), "small file") {
			found = true
		}
	}
	if !found {
		t.Errorf("the uploaded file is missing from the ZIP: %v", fileNames(z.files))
	}
}

func TestDataExportFileLimitsAreReported(t *testing.T) {
	app := newExportApp(t)
	subject := newExportSubject(t, app, "subject@example.com")

	// users.avatar is the file field of the default collection
	users, _ := app.FindCollectionByNameOrId("users")
	f, err := filesystem.NewFileFromBytes(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 2<<20)...), "big.png")
	if err != nil {
		t.Fatal(err)
	}
	subject.Set("avatar", f)
	if err := app.Save(subject); err != nil {
		t.Fatal(err)
	}
	_ = users

	t.Setenv(core.DataExportMaxFileMBEnv, "1")
	z := buildExport(t, app, subject)
	if z.manifest.Files.Included != 0 || len(z.manifest.Files.Skipped) != 1 || !strings.Contains(z.manifest.Files.Skipped[0].Reason, "1 MB") {
		t.Fatalf("expected the 2 MB file to be skipped: %+v", z.manifest.Files)
	}
	if !z.manifest.Partial() {
		t.Error("a skipped file makes the export partial")
	}

	t.Setenv(core.DataExportMaxFileMBEnv, "")
	z = buildExport(t, app, subject)
	if z.manifest.Files.Included != 1 || z.manifest.Partial() {
		t.Fatalf("expected the file to be included: %+v", z.manifest.Files)
	}
}

func TestDataExportOwnRecordWithoutMapEntryAndNoneSkipped(t *testing.T) {
	app := newExportApp(t)

	// an auth collection nobody mapped
	nologin, _ := app.FindCollectionByNameOrId("nologin")
	u := core.NewRecord(nologin)
	u.SetEmail("nl@example.com")
	u.SetPassword("1234567890")
	if err := app.Save(u); err != nil {
		t.Fatal(err)
	}

	z := buildExport(t, app, u)
	rows := z.rows(t, "nologin")
	if len(rows) != 1 || rows[0]["email"] != "nl@example.com" {
		t.Fatalf("the subject's own record must always be exported: %v", rows)
	}
	if _, ok := rows[0]["tokenKey"]; ok {
		t.Error("tokenKey exported")
	}
	if z.manifest.Skipped == nil || len(z.manifest.NotIncluded) == 0 {
		t.Error("manifest must list skipped collections and the logs notice")
	}
	// "users" has an id path, but the subject is in another collection
	for _, s := range z.manifest.Skipped {
		if s.Name == "users" && !strings.Contains(s.Reason, "not in this collection") {
			t.Errorf("unexpected reason %q", s.Reason)
		}
	}
}

func mustCollection(t testing.TB, app core.App, name string) *core.Collection {
	t.Helper()
	c, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func fileNames(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// requests and the job
// ---------------------------------------------------------------------------

func TestDataExportRequestJobAuditWebhookDownloadAndExpiry(t *testing.T) {
	setTestKEK(t)
	app, _ := newWebhookTestApp(t)
	admin, _ := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	subject := newExportSubject(t, app, "subject@example.com")
	seedBuiltinRows(t, app, subject)

	addEndpoint(t, app, "http://127.0.0.1:9/hook", []string{"*"}, true)

	// one at a time per person: a running request blocks a new one
	busy := core.NewRecord(mustCollection(t, app, core.CollectionNameDataRequests))
	busy.Set("type", core.DataRequestTypeExport)
	busy.Set("state", core.DataRequestStateRunning)
	busy.Set("collectionRef", subject.Collection().Id)
	busy.Set("recordRef", subject.Id)
	if err := app.Save(busy); err != nil {
		t.Fatal(err)
	}
	if _, err := core.RequestDataExport(app, subject, admin, false, ""); err != core.ErrDataExportActive {
		t.Fatalf("a second request while one is active must be refused, got %v", err)
	}
	if err := app.Delete(busy); err != nil {
		t.Fatal(err)
	}

	req, err := core.RequestDataExport(app, subject, admin, false, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		core.ProcessDataExports(app)
		req, _ = app.FindRecordById(core.CollectionNameDataRequests, req.Id)
		if s := req.GetString("state"); s == core.DataRequestStateDone || s == core.DataRequestStatePartial || s == core.DataRequestStateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("export did not finish, state %s", req.GetString("state"))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if req.GetString("state") != core.DataRequestStateDone {
		t.Fatalf("state %s error %s", req.GetString("state"), req.GetString("error"))
	}
	if req.GetDateTime("dueAt").IsZero() || req.GetDateTime("expires").IsZero() || req.GetString("file") == "" {
		t.Fatalf("incomplete request row")
	}
	if d := req.GetDateTime("expires").Time().Sub(time.Now()); d < 6*24*time.Hour || d > 8*24*time.Hour {
		t.Errorf("default retention is 7 days, got %v", d)
	}

	// audit: request and completion, no content
	var actions []string
	entries, _ := app.FindAllRecords(core.CollectionNameAuditLog, dbx.NewExp("[[action]] LIKE 'gdpr_export%'"))
	for _, e := range entries {
		actions = append(actions, e.GetString("action"))
		if strings.Contains(e.GetString("meta"), "OK-") {
			t.Errorf("audit entry holds content: %s", e.GetString("meta"))
		}
	}
	if len(entries) != 2 {
		t.Fatalf("expected request + completion entries, got %v", actions)
	}

	// webhook: one gdpr.export delivery (ids and counts only)
	var got *core.Record
	for _, d := range deliveries(t, app) {
		if d.GetString("eventType") == "gdpr.export" {
			got = d
		}
	}
	if got == nil {
		t.Fatal("no gdpr.export webhook delivery")
	}
	if p := got.GetString("payload"); strings.Contains(p, "OK-") || !strings.Contains(p, "categories") {
		t.Errorf("unexpected webhook payload %s", p)
	}

	// the file is the export and is not part of a backup (storage exclusion is tested separately)
	r, size, err := core.OpenDataExportFile(app, req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r)
	r.Close()
	if int64(len(data)) != size {
		t.Fatalf("size mismatch")
	}
	z := readExportZip(t, data, nil)
	if z.manifest.Subject.Id != subject.Id {
		t.Errorf("wrong subject in the manifest")
	}

	// download: the file is gone afterwards
	if err := core.FinishDataExportDownload(app, req, admin, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	req, _ = app.FindRecordById(core.CollectionNameDataRequests, req.Id)
	if req.GetString("file") != "" || req.GetDateTime("downloadedAt").IsZero() {
		t.Error("the file must be deleted after the download")
	}
	if _, _, err := core.OpenDataExportFile(app, req); err != core.ErrDataExportUnavailable {
		t.Errorf("expected unavailable, got %v", err)
	}
	if n, _ := app.CountRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": core.AuditActionDataExportDownload}); n != 1 {
		t.Errorf("expected a download audit entry")
	}

	// expiry
	req2, err := core.RequestDataExport(app, subject, admin, false, "")
	if err != nil {
		t.Fatal(err)
	}
	for range 400 {
		core.ProcessDataExports(app)
		req2, _ = app.FindRecordById(core.CollectionNameDataRequests, req2.Id)
		if req2.GetString("state") == core.DataRequestStateDone {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if req2.GetString("file") == "" {
		t.Fatal("second export did not finish")
	}
	req2.Set("expires", types.NowDateTime().Add(-time.Hour))
	if err := app.Save(req2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.OpenDataExportFile(app, req2); err != core.ErrDataExportUnavailable {
		t.Errorf("an expired export must not open, got %v", err)
	}
	n, err := core.ExpireDataExports(app)
	if err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	req2, _ = app.FindRecordById(core.CollectionNameDataRequests, req2.Id)
	if req2.GetString("file") != "" || req2.GetString("state") != core.DataRequestStateExpired {
		t.Errorf("expected expired without file, got %s %q", req2.GetString("state"), req2.GetString("file"))
	}
}

func TestDataExportExcludedFromBackups(t *testing.T) {
	app := newExportApp(t)
	admin, _ := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	subject := newExportSubject(t, app, "subject@example.com")

	req, err := core.RequestDataExport(app, subject, admin, false, "")
	if err != nil {
		t.Fatal(err)
	}
	for range 400 {
		core.ProcessDataExports(app)
		req, _ = app.FindRecordById(core.CollectionNameDataRequests, req.Id)
		if req.GetString("file") != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if req.GetString("file") == "" {
		t.Fatal("export did not finish")
	}

	if err := app.CreateBackup(t.Context(), "b.zip"); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(app.DataDir() + "/backups/b.zip")
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.Contains(f.Name, req.Collection().Id) {
			t.Errorf("the export file is in the backup: %s", f.Name)
		}
	}
}
