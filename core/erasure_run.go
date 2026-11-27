package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/security"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, 88-4): the erasure job.
//
// runErasure works through a list of steps. Each step is idempotent, and the
// progress (steps done, the ids of the person's household rows, the rows
// touched, the partial result) is stored on the request after every step, so a
// run that was cut short continues where it stopped and never does a step
// twice. The steps, in order:
//
//	init      resolve the person's ids (kept in the progress: the login of the
//	          household rows is cleared by the job itself)
//	log       write the erasure log row (a hash, nothing reversible)
//	lock      make sure the account is locked (a ban) and sessions are revoked
//	scim      suspend memberships that came from SCIM (before they are removed)
//	map:<c>   one step per data map collection, in the order of the map
//	pastoral  unlink pastoral notes about the person (UnlinkPastoralSubject)
//	notice    tell the person (Article 12(4)), before the address is gone
//	subject   anonymize the account
//	ban       turn the lock into the permanent "erased" ban
//	audit     scrub the audit log (ScrubAuditForErasure)
//	finish    the gdpr_erase audit entry, the result, the request state
//
// A row under a legal hold is never deleted or anonymized: it goes in the
// retained list with the hold as the reason (LegalHeldAmong, spec 10.6).

// ErasureRetained is a group of rows that were kept, and why.
type ErasureRetained struct {
	Collection string `json:"collection"`
	Count      int    `json:"count"`
	// Code is "legal_hold", "register", "register_draft", "no_fields" or "retain".
	Code   string `json:"code"`
	Reason string `json:"reason"`
	// Ids are the ids of the first rows kept (never content). Hidden from the
	// person in the API.
	Ids []string `json:"ids,omitempty"`
}

// ErasureUnchanged is something that was deliberately left alone.
type ErasureUnchanged struct {
	Collection string `json:"collection"`
	Reason     string `json:"reason"`
}

// ErasureResult is the result of an erasure: what was erased, anonymized,
// pseudonymized (kept as evidence, no longer about the person) and retained.
type ErasureResult struct {
	Subject struct {
		Collection string `json:"collection"`
		Pseudonym  string `json:"pseudonym"`
	} `json:"subject"`
	Erased        map[string]int     `json:"erased"`
	Anonymized    map[string]int     `json:"anonymized"`
	Pseudonymized map[string]int     `json:"pseudonymized"`
	Retained      []ErasureRetained  `json:"retained"`
	Unchanged     []ErasureUnchanged `json:"unchanged"`
	Errors        []string           `json:"errors"`

	// Holds are the ids of the subject holds that blocked part of the erasure.
	Holds []string `json:"holds,omitempty"`

	// Notice is "sent", "no_email", "failed" or "skipped".
	Notice string `json:"notice"`

	AuditScrubbed int `json:"auditScrubbed"`

	// Refused is set when the run was refused before it changed anything.
	Refused string `json:"refused,omitempty"`
}

func newErasureResult() *ErasureResult {
	return &ErasureResult{
		Erased: map[string]int{}, Anonymized: map[string]int{}, Pseudonymized: map[string]int{},
		Retained: []ErasureRetained{}, Unchanged: []ErasureUnchanged{}, Errors: []string{},
	}
}

// Partial reports whether something was kept or failed.
func (r *ErasureResult) Partial() bool { return len(r.Retained) > 0 || len(r.Errors) > 0 }

// Counts returns the totals, for the audit entry and the webhook.
func (r *ErasureResult) Counts() map[string]int {
	sum := func(m map[string]int) int {
		n := 0
		for _, v := range m {
			n += v
		}

		return n
	}

	kept := 0
	for _, k := range r.Retained {
		kept += k.Count
	}

	return map[string]int{
		"erased": sum(r.Erased), "anonymized": sum(r.Anonymized), "pseudonymized": sum(r.Pseudonymized),
		"retained": kept, "errors": len(r.Errors), "auditScrubbed": r.AuditScrubbed,
	}
}

func (r *ErasureResult) retain(k ErasureRetained) {
	for i := range r.Retained {
		if r.Retained[i].Collection == k.Collection && r.Retained[i].Code == k.Code {
			r.Retained[i].Count += k.Count
			r.Retained[i].Ids = capStrings(append(r.Retained[i].Ids, k.Ids...), erasureMaxListedIds)

			return
		}
	}

	k.Ids = capStrings(k.Ids, erasureMaxListedIds)
	r.Retained = append(r.Retained, k)
}

const erasureMaxListedIds = 50

func capStrings(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}

	return s
}

// erasureProgress is stored on the request while it runs.
type erasureProgress struct {
	Attempts int                 `json:"attempts"`
	Done     []string            `json:"done"`
	Ids      []string            `json:"ids"`     // the person's ids: the account, then the household rows
	Members  []string            `json:"members"` // the household member rows of an account
	Touched  map[string][]string `json:"touched"`
	Emails   []string            `json:"emails"`
	Language string              `json:"language"`
	Result   *ErasureResult      `json:"result"`
	Notified bool                `json:"notified"`
	Pseudo   bool                `json:"pseudo"`
}

func (p *erasureProgress) isDone(step string) bool { return slices.Contains(p.Done, step) }

type erasureRun struct {
	app  App
	ctx  context.Context // audit actor and suppress key
	req  *Record
	prog *erasureProgress
	subj *Record
	meta map[string]any

	rows []dataMapRow
}

func (rc *erasureRun) result() *ErasureResult { return rc.prog.Result }

func (rc *erasureRun) touch(collection string, ids ...string) {
	if rc.prog.Touched == nil {
		rc.prog.Touched = map[string][]string{}
	}

	for _, id := range ids {
		if !slices.Contains(rc.prog.Touched[collection], id) {
			rc.prog.Touched[collection] = append(rc.prog.Touched[collection], id)
		}
	}
}

func (rc *erasureRun) save() error {
	b, err := json.Marshal(rc.prog)
	if err != nil {
		return err
	}

	rc.req.Set("progress", types.JSONRaw(b))

	// without hooks: the row is the job's own bookkeeping
	return rc.app.UnsafeWithoutHooks().SaveNoValidate(rc.req)
}

func loadErasureProgress(req *Record) *erasureProgress {
	p := &erasureProgress{}
	_ = req.UnmarshalJSONField("progress", p)

	if p.Result == nil {
		p.Result = newErasureResult()
	}

	if p.Result.Erased == nil {
		p.Result.Erased = map[string]int{}
	}

	if p.Result.Anonymized == nil {
		p.Result.Anonymized = map[string]int{}
	}

	if p.Result.Pseudonymized == nil {
		p.Result.Pseudonymized = map[string]int{}
	}

	return p
}

func erasureRunContext(app App, req *Record) context.Context {
	actor := AuditActor{Type: AuditActorTypeSystem}

	if who := dataRequestActor(app, req); who != nil && !erasureMetaIsReplay(req) {
		actor = NewAuditActorFromRecord(who)
	}

	ctx := WithAuditContext(context.Background(), actor, "")

	// the job's own writes leave no record-change entries: their before and
	// after values would hold exactly what is being erased
	return context.WithValue(ctx, auditSuppressCtxKey{}, true)
}

func erasureMetaIsReplay(req *Record) bool { return erasureMeta(req)["mode"] == ErasureModeReplay }

// runErasure runs (or resumes) one claimed request. It never panics the loop:
// an unexpected error leaves the request queued for another attempt, up to
// erasureMaxAttempts.
func runErasure(app App, req *Record) bool {
	rc := &erasureRun{app: app, req: req, prog: loadErasureProgress(req), meta: erasureMeta(req)}
	rc.ctx = erasureRunContext(app, req)

	if err := rc.run(); err != nil {
		rc.prog.Attempts++

		app.Logger().Warn("Erasure step failed", "request", req.Id, "attempt", rc.prog.Attempts, "error", err.Error())

		if rc.prog.Attempts >= erasureMaxAttempts {
			rc.fail(err)

			return false
		}

		_ = rc.save()
		// back in the queue: the next pass of the job (or restart) continues
		_, _ = app.NonconcurrentDB().Update(CollectionNameDataRequests,
			dbx.Params{"state": DataRequestStateQueued},
			dbx.HashExp{"id": req.Id, "state": DataRequestStateRunning},
		).Execute()

		return false
	}

	return true
}

func (rc *erasureRun) fail(err error) {
	rc.req.Set("state", DataRequestStateFailed)
	rc.req.Set("error", truncateString(err.Error(), 2000))
	rc.req.Set("completedAt", types.NowDateTime())
	rc.req.Set("result", rc.prog.Result)

	if saveErr := rc.app.UnsafeWithoutHooks().SaveNoValidate(rc.req); saveErr != nil {
		rc.app.Logger().Error("Failed to save the failure of an erasure", "request", rc.req.Id, "error", saveErr)
	}
}

func (rc *erasureRun) run() error {
	app := rc.app

	subj, err := erasureSubjectOf(app, rc.req)
	if err != nil {
		rc.fail(errErasureSubjectGone)

		return nil
	}

	rc.subj = subj

	// the preflight runs once, before anything is changed
	if !rc.prog.isDone("init") {
		if err := erasurePreflight(app, subj); err != nil {
			var ee *ErasureError
			if errors.As(err, &ee) {
				return rc.refuse(ee)
			}

			return err
		}
	}

	step := func(name string, fn func() error) error {
		if rc.prog.isDone(name) {
			return nil
		}

		if erasureStepHook != nil {
			if err := erasureStepHook(rc.req, name); err != nil {
				return err
			}
		}

		if err := fn(); err != nil {
			return err
		}

		rc.prog.Done = append(rc.prog.Done, name)

		return rc.save()
	}

	if err := step("init", rc.stepInit); err != nil {
		return err
	}

	rows, err := loadDataMapRows(app)
	if err != nil {
		return fmt.Errorf("loading the data map: %w", err)
	}

	rc.rows = rows

	// the household rows go last: other entries reach rows through their
	// login (a household row's "user" is cleared when it is anonymized)
	ordered := make([]dataMapRow, 0, len(rows))

	for _, r := range rows {
		if r.Collection != CollectionNameHouseholdMembers {
			ordered = append(ordered, r)
		}
	}

	for _, r := range rows {
		if r.Collection == CollectionNameHouseholdMembers {
			ordered = append(ordered, r)
		}
	}

	for _, st := range []struct {
		name string
		fn   func() error
	}{
		{"log", rc.stepLog},
		{"lock", rc.stepLock},
		{"scim", rc.stepSCIM},
	} {
		if err := step(st.name, st.fn); err != nil {
			return err
		}
	}

	for _, row := range ordered {
		if err := step("map:"+row.Collection, func() error { return rc.stepEntry(row) }); err != nil {
			return err
		}
	}

	for _, st := range []struct {
		name string
		fn   func() error
	}{
		{"pastoral", rc.stepPastoral},
		{"notice", rc.stepNotice},
		{"subject", rc.stepSubject},
		{"ban", rc.stepBan},
		{"audit", rc.stepAudit},
	} {
		if err := step(st.name, st.fn); err != nil {
			return err
		}
	}

	return rc.finish()
}

// refuse ends the request before anything changed: the account is unlocked and
// the person is told why.
func (rc *erasureRun) refuse(ee *ErasureError) error {
	res := rc.prog.Result
	res.Refused = ee.Code
	res.Notice = "skipped"

	if rc.subj.Collection().IsAuth() && erasureMetaBool(rc.req, "banCreated") {
		_ = liftErasureBan(rc.app, rc.subj.Collection().Id, rc.subj.Id)
	}

	if !erasureMetaIsReplay(rc.req) && ErasureNotifier != nil {
		if rec, onBehalf := erasureRecipient(rc.app, rc.req, rc.subj); rec != nil && erasureUsableEmail(rec) {
			if err := ErasureNotifier(rc.app, &ErasureNotice{Kind: "result", Recipient: rec, Subject: rc.subj, OnBehalf: onBehalf, Result: res}); err == nil {
				res.Notice = "sent"
			} else {
				res.Notice = "failed"
			}
		}
	}

	rc.fail(ee)

	return nil
}

func (rc *erasureRun) stepInit() error {
	if err := EnsureErasureSecret(rc.app); err != nil {
		return err
	}

	s, err := erasureLoadSubject(rc.app, rc.subj)
	if err != nil {
		return err
	}

	rc.prog.Ids = s.ids

	for _, m := range s.members {
		rc.prog.Members = append(rc.prog.Members, m.Id)
	}

	if rc.subj.Collection().IsAuth() && !IsErasedAccount(rc.subj) {
		if e := rc.subj.Email(); e != "" {
			rc.prog.Emails = append(rc.prog.Emails, e)
		}
	}

	rc.prog.Language = RecordLanguage(rc.app, rc.subj)

	rc.prog.Result.Subject.Collection = rc.subj.Collection().Name
	rc.prog.Result.Subject.Pseudonym = ErasurePseudonym(rc.app, rc.subj.Id)

	return nil
}

func (rc *erasureRun) subjects() []*Record {
	out := []*Record{rc.subj}

	for _, id := range rc.prog.Members {
		if m, err := rc.app.FindRecordById(CollectionNameHouseholdMembers, id); err == nil {
			out = append(out, m)
		}
	}

	return out
}

// ---- log, lock, scim -------------------------------------------------------------

func (rc *erasureRun) stepLog() error {
	return writeErasureLog(rc.app, rc.subj.Collection().Name, rc.subj.Id, nil)
}

func (rc *erasureRun) stepLock() error {
	if !rc.subj.Collection().IsAuth() {
		return nil
	}

	// a request with no grace period (or a replay) has no ban yet
	if FindActiveBan(rc.app, rc.subj.Collection().Id, rc.subj.Id) == nil {
		if _, err := applyErasureBan(rc.app, rc.ctx, rc.req, rc.subj); err != nil {
			return err
		}

		rc.meta["banCreated"] = true
		rc.req.Set("meta", rc.meta)
	}

	_, err := RevokeSessions(rc.app, rc.subj.Collection().Id, rc.subj.Id, SessionRevokedBanned, "")

	return err
}

// stepSCIM suspends the memberships a SCIM client created, before they are
// removed: Claviger is the SCIM target, so the identity provider learns of the
// deactivation from the administrator (docs/erasure.md); the membership itself
// stops granting anything first.
func (rc *erasureRun) stepSCIM() error {
	if !rc.subj.Collection().IsAuth() {
		return nil
	}

	if _, err := rc.app.FindCachedCollectionByNameOrId(CollectionNameOrgMembers); err != nil {
		return nil
	}

	rows, err := rc.app.FindRecordsByFilter(CollectionNameOrgMembers, "user={:u} && source={:s} && status!={:x}", "", 0, 0,
		dbx.Params{"u": rc.subj.Id, "s": OrgSourceSCIM, "x": OrgStatusSuspended})
	if err != nil {
		return err
	}

	for _, m := range rows {
		if m.GetString("status") == OrgStatusSuspended {
			continue
		}

		m.Set("status", OrgStatusSuspended)

		if err := rc.app.SaveNoValidateWithContext(rc.ctx, m); err != nil {
			return err
		}
	}

	return nil
}

// ---- the data map -----------------------------------------------------------------

// held returns which of the rows are under a legal hold.
func (rc *erasureRun) held(col *Collection, ids []string) (map[string]bool, error) {
	return LegalHeldAmong(rc.app, col, ids)
}

// findRows returns the rows of the collection that are about the person, found
// through the subject paths of the entry for the account and for each of its
// household rows.
func (rc *erasureRun) findRows(col *Collection, entry dataMapRow) ([]*Record, error) {
	seen := map[string]bool{}

	var out []*Record

	for _, subj := range rc.subjects() {
		where, matches, _ := dataExportSubjectFilter(rc.app, col, entry.SubjectPaths, subj)
		if where == nil {
			continue
		}

		for offset := int64(0); ; offset += dataExportPageSize {
			var page []*Record

			err := rc.app.RecordQuery(col).AndWhere(where).OrderBy("id ASC").Limit(dataExportPageSize).Offset(offset).All(&page)
			if err != nil {
				return nil, err
			}

			for _, r := range page {
				if seen[r.Id] || (matches != nil && !matches(r)) {
					continue
				}

				seen[r.Id] = true

				out = append(out, r)
			}

			if len(page) < dataExportPageSize {
				break
			}
		}
	}

	return out, nil
}

func erasureIds(rows []*Record) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.Id)
	}

	return ids
}

func (rc *erasureRun) stepEntry(entry dataMapRow) error {
	res := rc.result()

	if entry.Treatment == DataMapTreatmentNone {
		return nil
	}

	col, err := rc.app.FindCachedCollectionByNameOrId(entry.Collection)
	if err != nil {
		return nil // the collection is gone: nothing to do
	}

	switch col.Name {
	case CollectionNameAuditLog, CollectionNameBans:
		return nil // the audit and ban steps
	case CollectionNameConsents, CollectionNameConsentEvents:
		return rc.stepConsentEvidence()
	}

	// the account itself: the subject step
	if col.Id == rc.subj.Collection().Id && col.IsAuth() {
		return nil
	}

	if col.Name == CollectionNameHouseholdMembers {
		return rc.stepMembers(col, entry)
	}

	if entry.Treatment == DataMapTreatmentExport {
		rows, err := rc.findRows(col, entry)
		if err != nil {
			return err
		}

		if len(rows) > 0 {
			res.Unchanged = append(res.Unchanged, ErasureUnchanged{Collection: col.Name, Reason: "Shared with other people, so it is not erased with one person. " + entry.Note})
		}

		return nil
	}

	rows, err := rc.findRows(col, entry)
	if err != nil {
		return err
	}

	// the current request is not erased with the person it is about
	if col.Name == CollectionNameDataRequests {
		return rc.stepDataRequests(col, rows)
	}

	if len(rows) == 0 {
		return nil
	}

	// pastoral notes: the person's own are other people's records, and the
	// ones about the person are handled by the pastoral step
	if IsPastoralCollection(col) {
		res.Unchanged = append(res.Unchanged, ErasureUnchanged{Collection: col.Name, Reason: "Pastoral notes are kept for their author; the link to the person is removed."})

		return nil
	}

	held, err := rc.held(col, erasureIds(rows))
	if err != nil {
		return err
	}

	var free, kept []*Record

	for _, r := range rows {
		if held[r.Id] {
			kept = append(kept, r)
		} else {
			free = append(free, r)
		}
	}

	if len(kept) > 0 {
		res.retain(ErasureRetained{
			Collection: col.Name, Count: len(kept), Code: "legal_hold", Ids: erasureIds(kept),
			Reason: "Kept under a legal hold.",
		})
	}

	if len(free) == 0 {
		return nil
	}

	// permanent registers: kept, with a note and restricted
	if cfg := PermanentConfigFor(rc.app, col.Name); cfg != nil {
		return rc.stepRegister(col, cfg, free, entry)
	}

	if isRegisterAnnotationCollection(rc.app, col.Name) {
		res.retain(ErasureRetained{Collection: col.Name, Count: len(free), Code: "register",
			Reason: "Notes in a permanent register can never be changed or deleted.", Ids: erasureIds(free)})

		return nil
	}

	switch col.Name {
	case CollectionNameBreakGlass:
		return rc.stepBreakGlass(col, free)
	case CollectionNameLegalHolds:
		return rc.stepLegalHolds(col, free)
	}

	switch entry.Treatment {
	case DataMapTreatmentErase:
		return rc.eraseRows(col, free)
	case DataMapTreatmentAnonymize:
		return rc.anonymizeRows(col, free)
	default: // retain
		reason := entry.RetainReason
		if reason == "" {
			reason = "Kept for the reason stated in the data map."
		}

		res.retain(ErasureRetained{Collection: col.Name, Count: len(free), Code: "retain", Reason: reason, Ids: erasureIds(free)})
	}

	return nil
}

func isRegisterAnnotationCollection(app App, name string) bool {
	for _, c := range PermanentConfigs(app) {
		if c.AnnotationCollection == name {
			return true
		}
	}

	return false
}

func (rc *erasureRun) eraseRows(col *Collection, rows []*Record) error {
	res := rc.result()

	for _, r := range rows {
		// the stored row: it may be gone already (a cascade, or a first run)
		fresh, err := rc.app.FindRecordById(col, r.Id)
		if err != nil {
			continue
		}

		rc.touch(col.Name, r.Id)

		if err := rc.app.DeleteWithContext(rc.ctx, fresh); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s %s: %s", col.Name, r.Id, errText(err)))

			continue
		}

		res.Erased[col.Name]++
	}

	return nil
}

func errText(err error) string {
	return truncateString(err.Error(), 300)
}

func (rc *erasureRun) anonymizeRows(col *Collection, rows []*Record) error {
	res := rc.result()

	fields := RetentionAnonymizeFields(rc.app, col)
	if len(fields) == 0 {
		res.retain(ErasureRetained{Collection: col.Name, Count: len(rows), Code: "no_fields",
			Reason: "The data map lists no personal fields for this collection, so nothing could be anonymized. It is kept until an administrator defines them.",
			Ids:    erasureIds(rows)})

		return nil
	}

	for _, r := range rows {
		if retentionAnonymized(r, fields) {
			continue
		}

		rc.touch(col.Name, r.Id)

		if err := retentionApply(rc.ctx, rc.app, r, RetentionActionAnonymize, fields); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s %s: %s", col.Name, r.Id, errText(err)))

			continue
		}

		res.Anonymized[col.Name]++
	}

	return nil
}

// stepDataRequests deletes the person's export requests (with their files) and
// pseudonymizes their earlier erasure requests. The running request is not
// touched until it finishes.
func (rc *erasureRun) stepDataRequests(col *Collection, rows []*Record) error {
	res := rc.result()
	p := ErasurePseudonym(rc.app, rc.subj.Id)

	for _, r := range rows {
		if r.Id == rc.req.Id {
			continue
		}

		if r.GetString("type") == DataRequestTypeExport {
			rc.touch(col.Name, r.Id)

			if err := rc.app.DeleteWithContext(rc.ctx, r); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s %s: %s", col.Name, r.Id, errText(err)))

				continue
			}

			res.Erased[col.Name]++

			continue
		}

		// an earlier erase request (cancelled, failed): keep the record that
		// there was one, without the person
		if r.GetString("recordRef") == p {
			continue
		}

		r.Set("recordRef", p)

		if containsString(rc.prog.Ids, r.GetString("requestedBy")) {
			r.Set("requestedBy", ErasurePseudonym(rc.app, r.GetString("requestedBy")))
		}

		if err := rc.app.UnsafeWithoutHooks().SaveNoValidate(r); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s %s: %s", col.Name, r.Id, errText(err)))

			continue
		}

		res.Pseudonymized[col.Name]++
	}

	return nil
}

func (rc *erasureRun) stepMembers(col *Collection, entry dataMapRow) error {
	res := rc.result()

	var rows []*Record

	for _, id := range append([]string{}, append(rc.prog.Members, rc.memberSubjectIds()...)...) {
		if m, err := rc.app.FindRecordById(col, id); err == nil && !containsRecord(rows, m.Id) {
			rows = append(rows, m)
		}
	}

	if len(rows) == 0 {
		return nil
	}

	held, err := rc.held(col, erasureIds(rows))
	if err != nil {
		return err
	}

	fields := []string{"name", "birthDate", "photo", "allergies", "medical"}

	for _, m := range rows {
		if held[m.Id] {
			res.retain(ErasureRetained{Collection: col.Name, Count: 1, Code: "legal_hold", Ids: []string{m.Id}, Reason: "Kept under a legal hold."})

			continue
		}

		if erasedMember(m) {
			continue
		}

		rc.touch(col.Name, m.Id)

		for _, name := range fields {
			switch col.Fields.GetByName(name).(type) {
			case nil:
				continue
			case *FileField:
				m.Set(name, []string{})
			default:
				m.Set(name, "")
			}
		}

		m.Set("user", "")
		m.Set("guardian", false)
		m.Set("status", HouseholdMemberLeft)

		// the guards (a minor needs an adult guardian, ...) were checked by the
		// preflight; a validated save would run them on a half-empty row
		if err := rc.app.SaveNoValidateWithContext(rc.ctx, m); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s %s: %s", col.Name, m.Id, errText(err)))

			continue
		}

		res.Anonymized[col.Name]++
	}

	_ = entry

	return nil
}

// memberSubjectIds is the household row of a minor who is the subject.
func (rc *erasureRun) memberSubjectIds() []string {
	if rc.subj.Collection().Name == CollectionNameHouseholdMembers {
		return []string{rc.subj.Id}
	}

	return nil
}

func containsRecord(rows []*Record, id string) bool {
	for _, r := range rows {
		if r.Id == id {
			return true
		}
	}

	return false
}

// stepConsentEvidence pseudonymizes the person's consents and consent events:
// the evidence stays (spec 10.4) and is no longer about them. One call does
// both collections. When any of the rows is under a legal hold nothing is
// changed and the rows are listed as retained.
func (rc *erasureRun) stepConsentEvidence() error {
	res := rc.result()

	if rc.prog.Pseudo {
		return nil
	}

	type group struct {
		col  *Collection
		rows []*Record
	}

	var groups []group

	heldAny := false

	for _, name := range []string{CollectionNameConsents, CollectionNameConsentEvents} {
		col, err := rc.app.FindCachedCollectionByNameOrId(name)
		if err != nil {
			continue
		}

		var entry dataMapRow

		for _, r := range rc.rows {
			if r.Collection == name {
				entry = r
			}
		}

		rows, err := rc.findRows(col, entry)
		if err != nil {
			return err
		}

		held, err := rc.held(col, erasureIds(rows))
		if err != nil {
			return err
		}

		if len(held) > 0 {
			heldAny = true
		}

		groups = append(groups, group{col, rows})
	}

	if heldAny {
		for _, g := range groups {
			if len(g.rows) > 0 {
				res.retain(ErasureRetained{Collection: g.col.Name, Count: len(g.rows), Code: "legal_hold",
					Reason: "Kept under a legal hold.", Ids: erasureIds(g.rows)})
			}
		}

		rc.prog.Pseudo = true

		return nil
	}

	events, states, err := pseudonymizeConsents(rc.app, rc.ctx, rc.prog.Ids...)
	if err != nil {
		return err
	}

	if events > 0 {
		res.Pseudonymized[CollectionNameConsentEvents] += events
	}

	if states > 0 {
		res.Pseudonymized[CollectionNameConsents] += states
	}

	for _, g := range groups {
		for _, r := range g.rows {
			rc.touch(g.col.Name, r.Id)
		}
	}

	rc.prog.Pseudo = true

	return nil
}

func (rc *erasureRun) stepBreakGlass(col *Collection, rows []*Record) error {
	res := rc.result()

	for _, r := range rows {
		changed := false

		for _, f := range []string{"grantee", "approvedBy", "revokedBy"} {
			if v := r.GetString(f); v != "" && containsString(rc.prog.Ids, v) {
				r.Set(f, ErasurePseudonym(rc.app, v))

				changed = true
			}
		}

		if !changed {
			continue
		}

		rc.touch(col.Name, r.Id)

		if err := rc.app.SaveNoValidateWithContext(rc.ctx, r); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s %s: %s", col.Name, r.Id, errText(err)))

			continue
		}

		res.Pseudonymized[col.Name]++
	}

	return nil
}

func (rc *erasureRun) stepLegalHolds(col *Collection, rows []*Record) error {
	res := rc.result()

	for _, h := range rows {
		// an active hold stops the erasure of what it covers; the hold itself stays
		if h.GetDateTime("liftedAt").IsZero() {
			res.retain(ErasureRetained{Collection: col.Name, Count: 1, Code: "retain", Ids: []string{h.Id},
				Reason: "A legal hold is in force."})

			continue
		}

		if v := h.GetString("subject"); v != "" && containsString(rc.prog.Ids, v) {
			h.Set("subject", ErasurePseudonym(rc.app, v))
			rc.touch(col.Name, h.Id)

			if err := rc.app.SaveNoValidateWithContext(legalHoldWriteContext(rc.ctx), h); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s %s: %s", col.Name, h.Id, errText(err)))

				continue
			}

			res.Pseudonymized[col.Name]++
		}
	}

	return nil
}

func (rc *erasureRun) stepRegister(col *Collection, cfg *PermanentConfig, rows []*Record, entry dataMapRow) error {
	res := rc.result()
	reason := "Data subject erasure request " + rc.req.Id

	for _, r := range rows {
		rc.touch(col.Name, r.Id)

		if !cfg.IsFinalized(r) {
			res.retain(ErasureRetained{Collection: col.Name, Count: 1, Code: "register_draft", Ids: []string{r.Id},
				Reason: "A draft register entry. A registrar decides whether to delete it; it is not final."})

			continue
		}

		reasonText := cfg.RetainReason
		if reasonText == "" {
			reasonText = entry.RetainReason
		}

		res.retain(ErasureRetained{Collection: col.Name, Count: 1, Code: "register", Ids: []string{r.Id},
			Reason: "Entries in a church register are permanent. The entry is restricted instead: it stays unchanged and is hidden from normal reads (GDPR Article 18). " + reasonText})

		done, err := hasErasureAnnotation(rc.app, cfg, r, reason)
		if err != nil {
			return err
		}

		if done {
			continue
		}

		if _, err := AnnotateErasureRequest(rc.app, rc.ctx, r, reason); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s %s: %s", col.Name, r.Id, errText(err)))
		}
	}

	return nil
}

func hasErasureAnnotation(app App, cfg *PermanentConfig, entry *Record, reason string) (bool, error) {
	if cfg.AnnotationCollection == "" {
		return false, nil
	}

	rows, err := app.FindRecordsByFilter(cfg.AnnotationCollection, "entry={:e} && kind={:k} && reason={:r}", "", 1, 0,
		dbx.Params{"e": entry.Id, "k": RegisterKindDataSubject, "r": reason})
	if err != nil {
		return false, err
	}

	return len(rows) > 0, nil
}

// ---- pastoral, notice, subject, ban, audit ------------------------------------------

func (rc *erasureRun) stepPastoral() error {
	res := rc.result()

	// a subject hold, or a hold on one note, keeps that note linked
	holds, err := ActiveLegalHolds(rc.app)
	if err != nil {
		return err
	}

	subjectHeld := false

	for _, h := range holds {
		if h.GetString("scope") == LegalHoldScopeSubject && containsString(rc.prog.Ids, h.GetString("subject")) {
			subjectHeld = true

			res.Holds = append(res.Holds, h.Id)
		}
	}

	notes, holdRows, err := unlinkPastoralSubject(rc.app, rc.ctx, func(col *Collection, r *Record) bool {
		if subjectHeld {
			return true
		}

		held, _ := LegalHeldAmong(rc.app, col, []string{r.Id})

		return held[r.Id]
	}, rc.prog.Ids...)
	if err != nil {
		return err
	}

	if notes > 0 {
		res.Anonymized["pastoral notes (link removed)"] += notes
	}

	if holdRows > 0 {
		res.Erased[CollectionNamePastoralHolds] += holdRows
	}

	if subjectHeld {
		res.retain(ErasureRetained{Collection: "pastoral notes", Count: 1, Code: "legal_hold", Reason: "Kept under a legal hold."})
	}

	return nil
}

// stepNotice tells the person what happened (Article 12(4)). It runs while the
// address is still there. A failure to send is recorded and does not stop the
// erasure.
func (rc *erasureRun) stepNotice() error {
	res := rc.result()

	if rc.prog.Notified {
		return nil
	}

	if erasureMetaIsReplay(rc.req) || ErasureNotifier == nil {
		res.Notice = "skipped"

		return nil
	}

	recipient, onBehalf := erasureRecipient(rc.app, rc.req, rc.subj)
	if recipient == nil || !erasureUsableEmail(recipient) {
		res.Notice = "no_email"

		return nil
	}

	// what the next steps will do to the account is part of what we tell them
	preview := *res
	if rc.subj.Collection().IsAuth() {
		held, _ := rc.held(rc.subj.Collection(), []string{rc.subj.Id})
		if !held[rc.subj.Id] {
			preview.Anonymized = copyCounts(res.Anonymized)
			preview.Anonymized[rc.subj.Collection().Name]++
		}
	}

	err := ErasureNotifier(rc.app, &ErasureNotice{Kind: "result", Recipient: recipient, Subject: rc.subj, OnBehalf: onBehalf, Result: &preview})
	if err != nil {
		res.Notice = "failed"
		rc.app.Logger().Warn("Failed to send the erasure result notice", "request", rc.req.Id)
	} else {
		res.Notice = "sent"
	}

	rc.prog.Notified = true

	return nil
}

func copyCounts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m)+1)
	for k, v := range m {
		out[k] = v
	}

	return out
}

// stepSubject anonymizes the account: the address, the name and the other
// personal fields of the data map, the password and the tokens.
func (rc *erasureRun) stepSubject() error {
	res := rc.result()
	subj := rc.subj

	if !subj.Collection().IsAuth() {
		return nil
	}

	// the row as it is stored now
	fresh, err := rc.app.FindRecordById(subj.Collection(), subj.Id)
	if err != nil {
		return err
	}

	if IsErasedAccount(fresh) {
		return nil
	}

	held, err := rc.held(fresh.Collection(), []string{fresh.Id})
	if err != nil {
		return err
	}

	if held[fresh.Id] {
		res.retain(ErasureRetained{Collection: fresh.Collection().Name, Count: 1, Code: "legal_hold", Ids: []string{fresh.Id},
			Reason: "The account is kept under a legal hold."})

		return nil
	}

	var personal []string

	for _, row := range rc.rows {
		if row.Collection == fresh.Collection().Name {
			personal = row.PersonalFields
		}
	}

	for _, name := range personal {
		f := fresh.Collection().Fields.GetByName(name)
		if f == nil || name == FieldNameId || name == "email" || f.Type() == FieldTypePassword || f.Type() == FieldTypeAutodate {
			continue
		}

		switch f.(type) {
		case *FileField, *RelationField, *SelectField:
			fresh.Set(name, []string{})
		case *BoolField:
			fresh.Set(name, false)
		case *NumberField:
			fresh.Set(name, 0)
		default:
			fresh.Set(name, "")
		}
	}

	// the username is a name too
	if f, ok := fresh.Collection().Fields.GetByName("username").(*TextField); ok && f != nil {
		fresh.Set("username", "erased-"+fresh.Id)
	}

	fresh.SetEmail("erased-" + fresh.Id + "@invalid")
	fresh.SetEmailVisibility(false)
	fresh.SetVerified(false)
	fresh.SetPassword(security.RandomString(48))
	fresh.RefreshTokenKey()

	rc.touch(fresh.Collection().Name, fresh.Id)

	if err := rc.app.SaveNoValidateWithContext(rc.ctx, fresh); err != nil {
		return fmt.Errorf("anonymizing the account: %w", err)
	}

	res.Anonymized[fresh.Collection().Name]++

	return nil
}

// stepBan turns the grace-period lock into the permanent ban of an erased
// account (a stub row stays: other rows point at it).
func (rc *erasureRun) stepBan() error {
	if !rc.subj.Collection().IsAuth() {
		return nil
	}

	fresh, err := rc.app.FindRecordById(rc.subj.Collection(), rc.subj.Id)
	if err != nil || !IsErasedAccount(fresh) {
		// held, or not erased: the account is not a stub, so unlock it
		return liftErasureBan(rc.app, rc.subj.Collection().Id, rc.subj.Id)
	}

	rows, err := rc.app.FindAllRecords(CollectionNameBans, dbx.HashExp{
		"collectionRef": rc.subj.Collection().Id, "recordRef": rc.subj.Id,
	})
	if err != nil {
		return err
	}

	if len(rows) == 0 {
		_, err = applyErasureBan(rc.app, rc.ctx, rc.req, rc.subj)
		if err != nil {
			return err
		}

		rows, err = rc.app.FindAllRecords(CollectionNameBans, dbx.HashExp{
			"collectionRef": rc.subj.Collection().Id, "recordRef": rc.subj.Id,
		})
		if err != nil || len(rows) == 0 {
			return err
		}
	}

	b := rows[0]
	b.Set("reason", BanReasonErased)
	b.Set("until", nil)
	b.Set("createdBy", "erasure")

	return rc.app.SaveNoValidateWithContext(rc.ctx, b)
}

func (rc *erasureRun) stepAudit() error {
	n, err := ScrubAuditForErasure(rc.app, rc.ctx, AuditScrubTarget{
		Ids: rc.prog.Ids, Needles: rc.prog.Emails, Rows: rc.prog.Touched,
	})
	if err != nil {
		return err
	}

	rc.result().AuditScrubbed += n

	return nil
}

// ---- finish ---------------------------------------------------------------------------

func (rc *erasureRun) finish() error {
	res := rc.result()

	state := DataRequestStateDone
	if res.Partial() {
		state = DataRequestStatePartial
	}

	requester := dataRequestActor(rc.app, rc.req)
	pseudo := ErasurePseudonym(rc.app, rc.subj.Id)

	// the actor of an entry about the person's own request is the person
	actorId := ""
	if requester != nil && containsString(rc.prog.Ids, requester.Id) {
		actorId = pseudo
	}

	meta := map[string]any{
		"request":           rc.req.Id,
		"subjectCollection": rc.subj.Collection().Name,
		"subject":           pseudo,
		"status":            state,
		"counts":            res.Counts(),
		"mode":              rc.meta["mode"],
		"notice":            res.Notice,
	}

	err := rc.app.RunInTransaction(func(txApp App) error {
		if auditAvailable(txApp) {
			entry := &AuditEntry{
				Action:         AuditActionErase,
				CollectionId:   rc.req.Collection().Id,
				CollectionName: CollectionNameDataRequests,
				RecordId:       rc.req.Id,
				ChangedFields:  []string{},
				Meta:           meta,
			}

			switch {
			case erasureMetaIsReplay(rc.req) || requester == nil:
				entry.Actor = AuditActor{Type: AuditActorTypeSystem}
			case actorId != "":
				entry.Actor = AuditActor{Type: AuditActorTypeRecord, Collection: requester.Collection().Name, Id: actorId}
			default:
				entry.Actor = NewAuditActorFromRecord(requester)
			}

			if err := WriteAuditEntry(txApp, context.Background(), entry, nil); err != nil {
				return err
			}
		}

		rc.req.Set("state", state)
		rc.req.Set("recordRef", pseudo)

		if actorId != "" {
			rc.req.Set("requestedBy", actorId)
		}

		rc.req.Set("completedAt", types.NowDateTime())
		rc.req.Set("error", "")
		rc.req.Set("tokenHash", "")
		rc.req.Set("cancelHash", "")
		rc.req.Set("progress", nil) // the ids the job needed are no longer kept
		rc.req.Set("result", res)

		return txApp.UnsafeWithoutHooks().SaveNoValidate(rc.req)
	})
	if err != nil {
		return err
	}

	// the log row now lists what was touched
	cols := make([]string, 0, len(rc.prog.Touched))
	for c := range rc.prog.Touched {
		cols = append(cols, c)
	}

	slices.Sort(cols)

	if err := writeErasureLog(rc.app, rc.subj.Collection().Name, rc.subj.Id, cols); err != nil {
		rc.app.Logger().Warn("Failed to update the erasure log", "request", rc.req.Id, "error", err.Error())
	}

	if erasureMetaIsReplay(rc.req) {
		_ = markErasureReplayed(rc.app, rc.subj.Collection().Name, rc.subj.Id)
	}

	return nil
}
