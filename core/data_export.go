package core

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dialect"
	"github.com/doodlemania2/claviger/tools/filesystem"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, milestone 88-1): the personal data export.
//
// WriteDataExport walks the data map (data_map.go) and writes a ZIP with one
// JSON file per mapped collection, the files of file fields, a manifest and a
// README. What is exported is decided by the data map, never by a list in
// this file:
//
//   - every entry whose treatment is not "none" is exported (export, erase,
//     anonymize and retain all export; erasure is a later milestone);
//   - rows are found through the entry's subject paths;
//   - for the built-in entries the exported fields come from the allowlist in
//     data_export_fields.go (a built-in entry without an allowlist exports
//     only id and dates, so a new system collection can never leak by
//     default; a test insists that every built-in entry has one);
//   - for entries registered by an administrator every field is exported
//     except password fields, the auth token key and fields whose name looks
//     like a secret (auditSensitiveNameRegex).
//
// Encrypted fields (encryptedText, encryptedJSON) are exported DECRYPTED: the
// export is the data subject's own data (GDPR Article 15 and 20), the spec
// says exports decrypt in the app (10.10), and the file is only readable by
// the requester through an authenticated, one-time download.
//
// See docs/data-export.md.

const (
	// DataExportFormatVersion is the version of the ZIP layout and manifest.
	DataExportFormatVersion = 1

	// DataExportMaxFileMBEnv and DataExportMaxTotalMBEnv override the limits
	// for files of file fields (a larger single file, or files beyond the
	// total, are listed in the manifest and left out of the ZIP).
	DataExportMaxFileMBEnv  = "CLAVIGER_EXPORT_MAX_FILE_MB"
	DataExportMaxTotalMBEnv = "CLAVIGER_EXPORT_MAX_TOTAL_MB"

	dataExportDefaultMaxFileMB  = 100
	dataExportDefaultMaxTotalMB = 1024

	dataExportPageSize = 500
)

// DataExportManifest is manifest.json inside the ZIP. It is also the source of
// the summary kept on the request row (without the file list).
type DataExportManifest struct {
	FormatVersion int                    `json:"formatVersion"`
	GeneratedAt   string                 `json:"generatedAt"`
	Subject       DataExportSubject      `json:"subject"`
	DataMap       DataExportDataMap      `json:"dataMap"`
	Collections   []DataExportCollection `json:"collections"`
	Skipped       []DataExportSkipped    `json:"skipped"`
	Errors        []DataExportError      `json:"errors"`
	NotIncluded   []DataExportSkipped    `json:"notIncluded"`
	Files         DataExportFiles        `json:"files"`
}

type DataExportSubject struct {
	Collection string `json:"collection"`
	Id         string `json:"id"`
}

// DataExportDataMap identifies the data map the export was generated from.
type DataExportDataMap struct {
	// Hash is the SHA-256 of the canonical content of the map.
	Hash    string `json:"hash"`
	Entries int    `json:"entries"`
}

type DataExportCollection struct {
	Name      string   `json:"name"`
	File      string   `json:"file"`
	Treatment string   `json:"treatment"`
	Count     int      `json:"count"`
	Fields    []string `json:"fields"`
}

type DataExportSkipped struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type DataExportError struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

type DataExportFiles struct {
	Included int                     `json:"included"`
	Bytes    int64                   `json:"bytes"`
	Skipped  []DataExportFileSkipped `json:"skipped"`
}

type DataExportFileSkipped struct {
	Collection string `json:"collection"`
	Record     string `json:"record"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	Reason     string `json:"reason"`
}

// Partial reports whether something could not be exported (an error, or a
// file left out). Skipped collections are not a problem: they are reviewed.
func (m *DataExportManifest) Partial() bool {
	return len(m.Errors) > 0 || len(m.Files.Skipped) > 0
}

// Categories returns the row count of every exported collection.
func (m *DataExportManifest) Categories() map[string]int {
	out := make(map[string]int, len(m.Collections))
	for _, c := range m.Collections {
		out[c.Name] = c.Count
	}
	return out
}

// dataMapRow is a stored _dataMap row.
type dataMapRow struct {
	DataMapEntry
	Builtin bool
}

func loadDataMapRows(app App) ([]dataMapRow, error) {
	records, err := app.FindAllRecords(CollectionNameDataMap)
	if err != nil {
		return nil, err
	}

	rows := make([]dataMapRow, 0, len(records))
	for _, r := range records {
		var paths, personal []string
		_ = r.UnmarshalJSONField("subjectPaths", &paths)
		_ = r.UnmarshalJSONField("personalFields", &personal)
		rows = append(rows, dataMapRow{
			DataMapEntry: DataMapEntry{
				Collection:     r.GetString("collection"),
				Treatment:      r.GetString("treatment"),
				SubjectPaths:   paths,
				PersonalFields: personal,
				RetainReason:   r.GetString("retainReason"),
				AgeOutBasis:    r.GetString("ageOutBasis"),
				Note:           r.GetString("note"),
			},
			Builtin: r.GetBool("builtin"),
		})
	}

	slices.SortFunc(rows, func(a, b dataMapRow) int { return strings.Compare(a.Collection, b.Collection) })

	return rows, nil
}

func dataMapHash(rows []dataMapRow) string {
	h := sha256.New()
	for _, r := range rows {
		b, _ := json.Marshal([]any{
			r.Collection, r.Treatment, r.SubjectPaths, r.PersonalFields,
			r.RetainReason, r.AgeOutBasis, r.Builtin,
		})
		h.Write(b)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func dataExportLimit(env string, defaultMB int64) int64 {
	if v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(env)), 10, 64); err == nil && v > 0 {
		return v << 20
	}
	return defaultMB << 20
}

type dataExporter struct {
	app     App
	subject *Record
	zw      *zip.Writer
	m       *DataExportManifest

	fsys     *filesystem.System
	maxFile  int64
	maxTotal int64
}

// WriteDataExport writes the export of the subject (an auth record) to dst as
// a ZIP. A failure to write to dst is returned as an error. Anything else
// that goes wrong (a collection that cannot be read, a file that is missing)
// is recorded in the manifest, so the result is a complete ZIP that says what
// it lacks.
func WriteDataExport(app App, subject *Record, dst io.Writer) (*DataExportManifest, error) {
	if subject == nil || !subject.Collection().IsAuth() {
		return nil, errors.New("the export subject must be an auth record")
	}

	rows, err := loadDataMapRows(app)
	if err != nil {
		return nil, fmt.Errorf("loading the data map: %w", err)
	}

	m := &DataExportManifest{
		FormatVersion: DataExportFormatVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Subject:       DataExportSubject{Collection: subject.Collection().Name, Id: subject.Id},
		DataMap:       DataExportDataMap{Hash: dataMapHash(rows), Entries: len(rows)},
		Collections:   []DataExportCollection{},
		Skipped:       []DataExportSkipped{},
		Errors:        []DataExportError{},
		NotIncluded: []DataExportSkipped{{
			Name:   LogsTableName,
			Reason: "Request logs are not part of the export. They are removed by the log retention setting (logs.maxDays).",
		}},
		Files: DataExportFiles{Skipped: []DataExportFileSkipped{}},
	}

	// the subject's own record is always exported, even when the app has not
	// mapped the collection it lives in
	hasOwn := false
	for _, r := range rows {
		if r.Collection == subject.Collection().Name {
			hasOwn = true
			break
		}
	}
	if !hasOwn {
		rows = append(rows, dataMapRow{DataMapEntry: DataMapEntry{
			Collection:   subject.Collection().Name,
			Treatment:    DataMapTreatmentExport,
			SubjectPaths: []string{"id"},
			Note:         "The subject's own record (the collection is not in the data map).",
		}})
	}

	ex := &dataExporter{
		app:      app,
		subject:  subject,
		zw:       zip.NewWriter(dst),
		m:        m,
		maxFile:  dataExportLimit(DataExportMaxFileMBEnv, dataExportDefaultMaxFileMB),
		maxTotal: dataExportLimit(DataExportMaxTotalMBEnv, dataExportDefaultMaxTotalMB),
	}
	defer func() {
		if ex.fsys != nil {
			_ = ex.fsys.Close()
		}
	}()

	for _, row := range rows {
		if err := ex.exportEntry(row); err != nil {
			return nil, err
		}
	}

	if err := ex.writeJSON("manifest.json", m); err != nil {
		return nil, err
	}
	if err := ex.writeText("README.txt", dataExportReadme(m)); err != nil {
		return nil, err
	}
	if err := ex.zw.Close(); err != nil {
		return nil, err
	}

	return m, nil
}

func (ex *dataExporter) writeText(name, content string) error {
	w, err := ex.zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, content)
	return err
}

func (ex *dataExporter) writeJSON(name string, v any) error {
	w, err := ex.zw.Create(name)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (ex *dataExporter) skip(name, reason string) {
	ex.m.Skipped = append(ex.m.Skipped, DataExportSkipped{Name: name, Reason: reason})
}

func (ex *dataExporter) fail(name string, err error) {
	ex.m.Errors = append(ex.m.Errors, DataExportError{Name: name, Error: err.Error()})
}

func (ex *dataExporter) exportEntry(entry dataMapRow) error {
	if entry.Treatment == DataMapTreatmentNone {
		ex.skip(entry.Collection, "No personal data (reviewed). "+entry.Note)
		return nil
	}

	col, err := ex.app.FindCachedCollectionByNameOrId(entry.Collection)
	if err != nil {
		ex.skip(entry.Collection, "The collection does not exist.")
		return nil
	}

	where, matches, reason := dataExportSubjectFilter(ex.app, col, entry.SubjectPaths, ex.subject)
	if where == nil {
		ex.skip(entry.Collection, reason)
		return nil
	}

	fields, spec := dataExportFieldsFor(col, entry)
	fileName := "data/" + col.Name + ".json"

	// collect the rows first (the JSON file is written in one entry, and the
	// files of file fields must not be interleaved with it in the ZIP)
	type exported struct {
		rec *Record
		row map[string]any
	}
	var collected []exported
	var queryErr error

	for offset := int64(0); ; offset += dataExportPageSize {
		var records []*Record
		err := ex.app.RecordQuery(col).
			AndWhere(where).
			OrderBy("id ASC").
			Limit(dataExportPageSize).
			Offset(offset).
			All(&records)
		if err != nil {
			queryErr = err
			break
		}

		for _, rec := range records {
			if matches != nil && !matches(rec) {
				continue
			}
			row := make(map[string]any, len(fields))
			for _, name := range fields {
				row[name] = rec.Get(name)
			}
			if spec.Redact != nil {
				spec.Redact(&dataExportCtx{subject: ex.subject}, rec, row)
			}
			collected = append(collected, exported{rec, row})
		}

		if len(records) < dataExportPageSize {
			break
		}
	}

	if queryErr != nil {
		ex.fail(entry.Collection, queryErr)
		return nil
	}

	w, err := ex.zw.Create(fileName)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "[\n"); err != nil {
		return err
	}
	for i, c := range collected {
		b, err := json.Marshal(c.row)
		if err != nil {
			ex.fail(entry.Collection, fmt.Errorf("record %s: %w", c.rec.Id, err))
			b = []byte(`{"id":` + strconv.Quote(c.rec.Id) + `}`)
		}
		sep := ",\n"
		if i == len(collected)-1 {
			sep = "\n"
		}
		if _, err := w.Write(append(b, sep...)); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "]\n"); err != nil {
		return err
	}

	ex.m.Collections = append(ex.m.Collections, DataExportCollection{
		Name:      col.Name,
		File:      fileName,
		Treatment: entry.Treatment,
		Count:     len(collected),
		Fields:    fields,
	})

	// files of file fields
	for _, c := range collected {
		for _, name := range fields {
			f := col.Fields.GetByName(name)
			if f == nil || f.Type() != FieldTypeFile {
				continue
			}
			for _, fn := range c.rec.GetStringSlice(name) {
				if err := ex.exportFile(col, c.rec, fn); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (ex *dataExporter) exportFile(col *Collection, rec *Record, name string) error {
	skipped := DataExportFileSkipped{Collection: col.Name, Record: rec.Id, Name: name}

	if ex.fsys == nil {
		fsys, err := ex.app.NewFilesystem()
		if err != nil {
			skipped.Reason = "the file storage is not available: " + err.Error()
			ex.m.Files.Skipped = append(ex.m.Files.Skipped, skipped)
			return nil
		}
		ex.fsys = fsys
	}

	key := rec.BaseFilesPath() + "/" + name

	attrs, err := ex.fsys.Attributes(key)
	if err != nil {
		skipped.Reason = "the file was not found in the storage"
		ex.m.Files.Skipped = append(ex.m.Files.Skipped, skipped)
		return nil
	}
	skipped.Size = attrs.Size

	switch {
	case attrs.Size > ex.maxFile:
		skipped.Reason = fmt.Sprintf("larger than the %d MB limit for one file", ex.maxFile>>20)
	case ex.m.Files.Bytes+attrs.Size > ex.maxTotal:
		skipped.Reason = fmt.Sprintf("the %d MB limit for all files was reached", ex.maxTotal>>20)
	}
	if skipped.Reason != "" {
		ex.m.Files.Skipped = append(ex.m.Files.Skipped, skipped)
		return nil
	}

	r, err := ex.fsys.GetReader(key)
	if err != nil {
		skipped.Reason = "the file could not be read"
		ex.m.Files.Skipped = append(ex.m.Files.Skipped, skipped)
		return nil
	}
	defer r.Close()

	w, err := ex.zw.Create(path.Join("files", col.Name, rec.Id, name))
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		return err
	}

	ex.m.Files.Included++
	ex.m.Files.Bytes += attrs.Size

	return nil
}

// ---------------------------------------------------------------------------
// finding the subject's rows
// ---------------------------------------------------------------------------

// dataExportSubjectFilter builds the WHERE expression that selects the rows
// of col that belong to the subject, from the entry's subject paths. matches,
// when not nil, must be applied to every loaded row too (the SQL part is a
// coarse text match for JSON and multi value fields). The expression is nil
// when no path is usable, and reason says why.
func dataExportSubjectFilter(app App, col *Collection, paths []string, subject *Record) (dbx.Expression, func(*Record) bool, string) {
	d := DialectOf(app)
	subCol := subject.Collection()

	var exps []dbx.Expression
	var postChecks []func(*Record) bool
	var problems []string

	for _, p := range paths {
		if p == "id" {
			if col.Id != subCol.Id {
				problems = append(problems, "the subject is not in this collection")
				continue
			}
			exps = append(exps, dbx.HashExp{"id": subject.Id})
			continue
		}

		f := col.Fields.GetByName(p)
		if f == nil {
			problems = append(problems, "no field "+p)
			continue
		}

		var exp dbx.Expression
		switch ft := f.(type) {
		case *RelationField:
			if ft.CollectionId != subCol.Id {
				problems = append(problems, "field "+p+" does not point to the subject's collection")
				continue
			}
			if ft.IsMultiple() {
				exp, postChecks = dataExportContainsExp(d, p, subject.Id, postChecks)
			} else {
				exp = dbx.HashExp{p: subject.Id}
			}
		case *JSONField:
			exp, postChecks = dataExportContainsExp(d, p, subject.Id, postChecks)
		default:
			if p == "email" {
				email := subject.Email()
				if email == "" {
					problems = append(problems, "the subject has no email address")
					continue
				}
				exp = dbx.NewExp(d.NoCaseEqual("[[email]]", "{:__email}"), dbx.Params{"__email": email})
				break
			}

			exp = dbx.HashExp{p: subject.Id}

			// a bare id needs the collection it belongs to
			switch {
			case p == "actorId" && col.Fields.GetByName("actorCollection") != nil:
				exp = dbx.And(exp, dbx.HashExp{"actorCollection": subCol.Name})
			case p == "targetId" && col.Fields.GetByName("targetCollection") != nil:
				exp = dbx.And(exp, dbx.HashExp{"targetCollection": subCol.Name})
			case col.Fields.GetByName("collectionRef") != nil && f.Type() == FieldTypeText:
				exp = dbx.And(exp, dbx.HashExp{"collectionRef": subCol.Id})
			}
		}
		exps = append(exps, exp)
	}

	if len(exps) == 0 {
		reason := "No usable subject path."
		if len(problems) > 0 {
			reason = "Not about this subject: " + strings.Join(problems, "; ") + "."
		}
		return nil, nil, reason
	}

	var matches func(*Record) bool
	if len(postChecks) > 0 {
		// the post checks only narrow the JSON and multi value paths; a row
		// found through another (exact) path must stay, so the check is only
		// applied when every path is of the coarse kind
		if len(postChecks) == len(exps) {
			matches = func(r *Record) bool {
				for _, c := range postChecks {
					if c(r) {
						return true
					}
				}
				return false
			}
		}
	}

	if len(exps) == 1 {
		return exps[0], matches, ""
	}

	return dbx.Or(exps...), matches, ""
}

// dataExportContainsExp matches a JSON or multi value column that holds the
// id as a quoted string. The record check decodes the value to make sure the
// id is an element or a value, not a substring of something else.
func dataExportContainsExp(d dialect.Dialect, field, id string, checks []func(*Record) bool) (dbx.Expression, []func(*Record) bool) {
	exp := dbx.NewExp(
		d.AsText("[["+field+"]]")+" "+d.LikeOp(false)+" {:__like_"+field+"}",
		dbx.Params{"__like_" + field: `%"` + id + `"%`},
	)

	checks = append(checks, func(r *Record) bool {
		return jsonContainsString(r.GetRaw(field), id)
	})

	return exp, checks
}

// jsonContainsString reports whether the (decoded) value is, or holds at any
// depth, the string s.
func jsonContainsString(v any, s string) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		if t == s {
			return true
		}
		var decoded any
		if (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Unmarshal([]byte(t), &decoded) == nil {
			return jsonContainsString(decoded, s)
		}
		return false
	case []string:
		return slices.Contains(t, s)
	case []any:
		for _, e := range t {
			if jsonContainsString(e, s) {
				return true
			}
		}
		return false
	case map[string]any:
		for _, e := range t {
			if jsonContainsString(e, s) {
				return true
			}
		}
		return false
	case types.JSONRaw:
		var decoded any
		if json.Unmarshal(t, &decoded) != nil {
			return false
		}
		return jsonContainsString(decoded, s)
	default:
		return false
	}
}

func dataExportReadme(m *DataExportManifest) string {
	var b strings.Builder
	b.WriteString("Your data export\n================\n\n")
	b.WriteString("Generated: " + m.GeneratedAt + "\n\n")
	b.WriteString("manifest.json   what is in this archive, row counts, what was left out and why\n")
	b.WriteString("data/           one JSON file per collection, holding the rows about you\n")
	b.WriteString("files/          files you uploaded (name: files/<collection>/<record>/<file>)\n\n")
	b.WriteString("Passwords, password hashes, tokens, API key secrets, passkey keys and other\n")
	b.WriteString("secrets are never included. Other people's data is left out.\n")
	b.WriteString("Values stored encrypted in the system are decrypted here.\n")
	if m.Partial() {
		b.WriteString("\nSome parts could not be exported. See \"errors\" and \"files.skipped\" in manifest.json.\n")
	}
	return b.String()
}
