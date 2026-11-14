package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/replica"
	"github.com/doodlemania2/claviger/tools/router"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, 88-3): the retention engine (spec 10.8).
//
// A policy (_retentionPolicies) says: rows of a collection whose basis date is
// older than "after" days, months or years, and that match a filter, are
// deleted, anonymized or archived. A daily job applies every enabled policy in
// batches. Nothing ships enabled: a new policy is created disabled and as a dry
// run, and a dry run only counts.
//
// Never touched, whatever the policy says:
//   - rows under an active legal hold (legal_hold.go);
//   - pastoral notes with an open _pastoralHolds row (held back from an export);
//   - permanent registers (#87) and their annotation collections: a policy on
//     one is refused when it is saved and again when it runs.
//
// The basis is "created", "updated", the name of a date field, or "ageOut": the
// day a person's isMinor turns false (birthDate plus the org's graduation age,
// see RunHouseholdGraduation) plus the period. The date is computed in Go from
// the (encrypted) birthDate of each row; there is no derived date column, which
// would reveal the birth year.
//
// The job is resumable. A run marks its policies running and persists a cursor
// (the last id scanned) and the counts after every batch, so a run that stops
// part way (the replica restarts on every rollout) continues from the cursor and
// processes each row once. One audit entry (gdpr_retention_run) is written per
// run, with the counts per policy and ids only. Record changes made by the job
// are not audited one by one: a delete entry would keep the deleted data in the
// audit log, which is the opposite of what the policy is for.
//
// See docs/retention.md.

const (
	CollectionNameRetentionPolicies = "_retentionPolicies"
	CollectionNameLegalHolds        = "_legalHolds"

	RetentionActionDelete    = "delete"
	RetentionActionAnonymize = "anonymize"
	RetentionActionArchive   = "archive"

	RetentionBasisCreated = "created"
	RetentionBasisUpdated = "updated"
	RetentionBasisAgeOut  = "ageOut"

	RetentionUnitDays   = "days"
	RetentionUnitMonths = "months"
	RetentionUnitYears  = "years"

	retentionRunning = "running"
	retentionDone    = "done"

	AuditActionRetentionRun = "gdpr_retention_run"

	DefaultRetentionBatchSize = 200

	retentionAfterMax  = 36500
	retentionFilterMax = 2000

	retentionCronId   = "__clvRetention__"
	retentionCronExpr = "27 2 * * *"
)

// RetentionActions, RetentionUnits are the allowed select values.
var (
	RetentionActions = []string{RetentionActionDelete, RetentionActionAnonymize, RetentionActionArchive}
	RetentionUnits   = []string{RetentionUnitDays, RetentionUnitMonths, RetentionUnitYears}
	retentionStates  = []string{retentionRunning, retentionDone}
)

// ErrRetentionRunning is returned when a retention run is already in progress.
var ErrRetentionRunning = errors.New("a retention run is already in progress")

var (
	retentionRunMu    sync.Mutex
	retentionIdentRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	retentionNeverFor = []string{
		CollectionNameAuditLog, CollectionNameRetentionPolicies, CollectionNameLegalHolds, CollectionNameDataMap,
		CollectionNameFieldKeys, CollectionNamePermanentCollections, CollectionNameDataRequests, CollectionNameSuperusers,
	}
)

// retentionBatchHook is a test seam: called after every committed batch with
// the 1-based batch number of the policy; an error aborts the run exactly like
// a crash between two batches would.
var retentionBatchHook func(policyId string, batch int) error

type retentionDeleteCtxKey struct{}

// RetentionAvailable reports whether the policies collection exists.
func RetentionAvailable(app App) bool {
	_, err := app.FindCachedCollectionByNameOrId(CollectionNameRetentionPolicies)

	return err == nil
}

// ---- policies ------------------------------------------------------------------

// RetentionPolicyInput is the writable part of a policy. A nil pointer leaves
// the stored value alone (update) or takes the default (create).
type RetentionPolicyInput struct {
	Name       *string `json:"name" form:"name"`
	Collection *string `json:"collection" form:"collection"`
	Basis      *string `json:"basis" form:"basis"`
	After      *int    `json:"after" form:"after"`
	Unit       *string `json:"unit" form:"unit"`
	Action     *string `json:"action" form:"action"`
	Filter     *string `json:"filter" form:"filter"`
	Enabled    *bool   `json:"enabled" form:"enabled"`
	DryRun     *bool   `json:"dryRun" form:"dryRun"`
}

func (in *RetentionPolicyInput) apply(rec *Record) {
	set := func(key string, v any, isNil bool) {
		if !isNil {
			rec.Set(key, v)
		}
	}

	if in.Name != nil {
		set("name", strings.TrimSpace(*in.Name), false)
	}

	if in.Collection != nil {
		set("collection", strings.TrimSpace(*in.Collection), false)
	}

	if in.Basis != nil {
		set("basis", strings.TrimSpace(*in.Basis), false)
	}

	if in.After != nil {
		set("after", *in.After, false)
	}

	if in.Unit != nil {
		set("unit", *in.Unit, false)
	}

	if in.Action != nil {
		set("action", *in.Action, false)
	}

	if in.Filter != nil {
		set("filter", strings.TrimSpace(*in.Filter), false)
	}

	if in.Enabled != nil {
		set("enabled", *in.Enabled, false)
	}

	if in.DryRun != nil {
		set("dryRun", *in.DryRun, false)
	}
}

// CreateRetentionPolicy creates a policy. It is always created disabled and as
// a dry run, whatever the input says: going live is a second, deliberate step
// (UpdateRetentionPolicy).
func CreateRetentionPolicy(app App, ctx context.Context, in RetentionPolicyInput) (*Record, error) {
	col, err := app.FindCachedCollectionByNameOrId(CollectionNameRetentionPolicies)
	if err != nil {
		return nil, errors.New("retention is not available")
	}

	rec := NewRecord(col)
	rec.Set("after", 0)
	rec.Set("unit", RetentionUnitDays)
	rec.Set("action", RetentionActionDelete)

	in.apply(rec)

	rec.Set("enabled", false)
	rec.Set("dryRun", true)

	if err := app.SaveWithContext(retentionWriteContext(ctx), rec); err != nil {
		return nil, err
	}

	return rec, nil
}

// UpdateRetentionPolicy changes a policy. Changing what a run in progress
// would do is refused (409 from the route): wait for the run to end.
func UpdateRetentionPolicy(app App, ctx context.Context, id string, in RetentionPolicyInput) (*Record, error) {
	rec, err := app.FindRecordById(CollectionNameRetentionPolicies, id)
	if err != nil {
		return nil, err
	}

	if rec.GetString("runStatus") != "" {
		return nil, ErrRetentionRunning
	}

	in.apply(rec)

	if err := app.SaveWithContext(retentionWriteContext(ctx), rec); err != nil {
		return nil, err
	}

	return rec, nil
}

// DeleteRetentionPolicy removes a policy that is not running.
func DeleteRetentionPolicy(app App, ctx context.Context, id string) error {
	rec, err := app.FindRecordById(CollectionNameRetentionPolicies, id)
	if err != nil {
		return err
	}

	if rec.GetString("runStatus") != "" {
		return ErrRetentionRunning
	}

	return app.DeleteWithContext(retentionWriteContext(ctx), rec)
}

type retentionWriteCtxKey struct{}

func retentionWriteContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	return context.WithValue(ctx, retentionWriteCtxKey{}, true)
}

// RetentionBlockedReason says why a collection can never be the target of a
// policy ("" when it can). Registers are refused here, whatever the action.
func RetentionBlockedReason(app App, col *Collection) string {
	switch {
	case col == nil:
		return "No such collection."
	case col.IsView():
		return "A view cannot be changed."
	case col.IsAuth():
		return "Accounts are removed by erasure, not by retention."
	case slices.Contains(retentionNeverFor, col.Name):
		return "Retention does not apply to " + col.Name + "."
	}

	for _, cfg := range PermanentConfigs(app) {
		if cfg.Collection == col.Name || (cfg.AnnotationCollection != "" && cfg.AnnotationCollection == col.Name) {
			return "Permanent registers are never deleted or anonymized by retention."
		}
	}

	return ""
}

// RetentionAnonymizeFields returns the fields an anonymize policy blanks: the
// personal fields of the collection's data map entry that exist in the
// collection. An empty list means the policy cannot run.
func RetentionAnonymizeFields(app App, col *Collection) []string {
	row, err := app.FindFirstRecordByData(CollectionNameDataMap, "collection", col.Name)
	if err != nil {
		return nil
	}

	var personal []string

	_ = row.UnmarshalJSONField("personalFields", &personal)

	var out []string

	for _, name := range personal {
		f := col.Fields.GetByName(name)
		if f == nil || f.GetName() == "id" || f.Type() == FieldTypeAutodate || f.Type() == FieldTypePassword {
			continue
		}

		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}

	return out
}

func isDateLike(f Field) bool {
	switch f.(type) {
	case *DateField, *AutodateField:
		return true
	}

	return false
}

func validateRetentionPolicy(app App, rec *Record) error {
	errs := validation.Errors{}

	name := rec.GetString("collection")
	basis := rec.GetString("basis")
	action := rec.GetString("action")
	after := rec.GetInt("after")

	col, err := app.FindCachedCollectionByNameOrId(name)

	switch {
	case name == "":
		errs["collection"] = validation.NewError("validation_required", "A collection is required.")
	case err != nil:
		errs["collection"] = validation.NewError("validation_unknown_collection", "No collection with this name exists.")
	default:
		if why := RetentionBlockedReason(app, col); why != "" {
			errs["collection"] = validation.NewError("validation_retention_refused", why)
		}
	}

	if after < 0 || after > retentionAfterMax {
		errs["after"] = validation.NewError("validation_retention_after", "The period must be from 0 to 36500.")
	}

	if col != nil && errs["collection"] == nil {
		switch {
		case basis == "":
			errs["basis"] = validation.NewError("validation_required", "A basis is required.")
		case basis == RetentionBasisAgeOut:
			f := col.Fields.GetByName("birthDate")
			if f == nil || (f.Type() != FieldTypeText && f.Type() != FieldTypeEncryptedText) {
				errs["basis"] = validation.NewError("validation_retention_basis", "The age-out basis needs a birthDate text field.")
			}
		default:
			f := col.Fields.GetByName(basis)
			if !retentionIdentRe.MatchString(basis) || f == nil || !isDateLike(f) {
				errs["basis"] = validation.NewError("validation_retention_basis", "The basis must be created, updated, ageOut or a date field of the collection.")
			}
		}

		// a period of 0 is for expiry dates only: "older than now" on the
		// creation date would clear the collection
		if errs["basis"] == nil && errs["after"] == nil && after == 0 && (basis == RetentionBasisCreated || basis == RetentionBasisUpdated) {
			errs["after"] = validation.NewError("validation_retention_after", "The period must be at least 1 for the created and updated bases.")
		}

		switch action {
		case RetentionActionArchive:
			if _, ok := col.Fields.GetByName("archived").(*BoolField); !ok {
				errs["action"] = validation.NewError("validation_retention_action", "Archive needs a bool field named archived.")
			}
		case RetentionActionAnonymize:
			if rec.GetBool("enabled") && len(RetentionAnonymizeFields(app, col)) == 0 {
				errs["action"] = validation.NewError("validation_retention_action",
					"The data map has no personal fields for this collection, so an anonymize policy cannot be enabled. Add them to the data map first.")
			}
		}

		if f := strings.TrimSpace(rec.GetString("filter")); f != "" {
			switch {
			case len(f) > retentionFilterMax:
				errs["filter"] = validation.NewError("validation_max_text_constraint", "The filter is too long.")
			case strings.Contains(f, "@request") || strings.Contains(f, "@collection"):
				errs["filter"] = validation.NewError("validation_retention_filter", "The filter can only use the fields of the collection.")
			default:
				if _, err := app.FindRecordsByFilter(col, "("+f+")", "", 1, 0); err != nil {
					errs["filter"] = validation.NewError("validation_retention_filter", "The filter is not valid for this collection.")
				}
			}
		}
	}

	return errs.Filter()
}

// ---- the run -------------------------------------------------------------------

// RetentionCounts is what a policy did (or, in a dry run, would do).
type RetentionCounts struct {
	// Matched rows are past the period and match the filter.
	Matched int `json:"matched"`
	// Held rows were skipped because of a legal or pastoral hold.
	Held int `json:"held"`
	// Unchanged rows already were in the state the action leaves (anonymized, archived).
	Unchanged int `json:"unchanged"`
	// Failed rows could not be changed (a guard refused, a hook failed).
	Failed int `json:"failed"`
	// Affected rows were deleted, anonymized or archived (a dry run: would be).
	Affected int `json:"affected"`
	// Batches is the number of batches committed.
	Batches int `json:"batches"`
	// Cursor is the last row id scanned; a resumed run continues after it.
	Cursor string `json:"cursor,omitempty"`
	// Done is set when the policy has been scanned to the end.
	Done  bool   `json:"done,omitempty"`
	Error string `json:"error,omitempty"`
}

// RetentionPolicyResult is the outcome for one policy of a run.
type RetentionPolicyResult struct {
	PolicyId   string `json:"policyId"`
	Collection string `json:"collection"`
	Basis      string `json:"basis"`
	After      int    `json:"after"`
	Unit       string `json:"unit"`
	Action     string `json:"action"`
	DryRun     bool   `json:"dryRun"`
	RetentionCounts
}

// RetentionRunResult is the outcome of a run.
type RetentionRunResult struct {
	RunId      string                  `json:"runId"`
	Resumed    bool                    `json:"resumed"`
	Preview    bool                    `json:"preview"`
	StartedAt  string                  `json:"startedAt"`
	FinishedAt string                  `json:"finishedAt"`
	Policies   []RetentionPolicyResult `json:"policies"`
}

// RetentionRunOptions tunes RunRetention.
type RetentionRunOptions struct {
	// PolicyId limits a preview to one policy (enabled or not). It is
	// only valid together with Preview.
	PolicyId string
	// Preview counts what every selected policy would do and changes
	// nothing, whatever the policy's own dryRun flag says. A preview keeps no
	// run state.
	Preview bool
	// BatchSize is the number of rows per batch (default 200).
	BatchSize int
	// Now is the clock (tests).
	Now time.Time
}

func (o *RetentionRunOptions) batchSize() int {
	if o.BatchSize > 0 {
		return o.BatchSize
	}

	return DefaultRetentionBatchSize
}

func lockRetention() (func(), error) {
	if !retentionRunMu.TryLock() {
		return nil, ErrRetentionRunning
	}

	return retentionRunMu.Unlock, nil
}

func lockRetentionReplica(app App, local func()) (func(), error) {
	bus := ReplicaBusOf(app)
	if bus == nil {
		return local, nil
	}

	release, ok, err := replica.TryLock(context.Background(), bus.DB(), "retention")
	if err != nil || !ok {
		local()

		if err != nil {
			return nil, err
		}

		return nil, ErrRetentionRunning
	}

	return func() {
		release()
		local()
	}, nil
}

// RunRetention applies the enabled policies once (or previews them) and writes
// the one audit entry of the run. ctx carries the audit actor (a superuser for
// a manual run, the system for the daily job) and cancels the run between rows.
// A run that was interrupted is continued first: its policies are finished
// with their stored cursor and counts, and the entry says resumed.
func RunRetention(ctx context.Context, app App, opts RetentionRunOptions) (*RetentionRunResult, error) {
	if !RetentionAvailable(app) {
		return nil, errors.New("retention is not available")
	}

	if opts.PolicyId != "" && !opts.Preview {
		return nil, errors.New("a single policy can only be previewed")
	}

	local, err := lockRetention()
	if err != nil {
		return nil, err
	}

	unlock, err := lockRetentionReplica(app, local)
	if err != nil {
		return nil, err
	}
	defer unlock()

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	policies, runId, resumed, err := retentionSelect(app, &opts)
	if err != nil {
		return nil, err
	}

	res := &RetentionRunResult{RunId: runId, Resumed: resumed, Preview: opts.Preview, StartedAt: types.NowDateTime().String()}

	runCtx := context.WithValue(ctx, retentionDeleteCtxKey{}, true)
	runCtx = context.WithValue(runCtx, auditSuppressCtxKey{}, true)

	var runErr error

	for _, p := range policies {
		r, err := runRetentionPolicy(runCtx, app, p, &opts, now)
		res.Policies = append(res.Policies, r)

		if err != nil {
			runErr = err

			break
		}
	}

	if runErr != nil {
		return res, runErr
	}

	res.FinishedAt = types.NowDateTime().String()

	if err := writeRetentionAudit(app, ctx, res); err != nil {
		return res, err
	}

	if !opts.Preview {
		retentionFinish(app, policies, res)
	}

	return res, nil
}

// retentionSelect returns the policies of this run, in order, with the run id.
func retentionSelect(app App, opts *RetentionRunOptions) ([]*Record, string, bool, error) {
	if opts.Preview {
		var (
			ps  []*Record
			err error
		)

		if opts.PolicyId != "" {
			var p *Record

			p, err = app.FindRecordById(CollectionNameRetentionPolicies, opts.PolicyId)
			if err == nil {
				ps = []*Record{p}
			}
		} else {
			ps, err = app.FindRecordsByFilter(CollectionNameRetentionPolicies, "enabled = true", "created,id", 0, 0)
		}

		return ps, "preview-" + GenerateDefaultRandomId(), false, err
	}

	// an interrupted run first
	running, err := app.FindRecordsByFilter(CollectionNameRetentionPolicies, `runStatus != ""`, "created,id", 0, 0)
	if err != nil {
		return nil, "", false, err
	}

	if len(running) > 0 {
		runId := running[0].GetString("runId")

		var ps []*Record

		for _, p := range running {
			if p.GetString("runId") == runId {
				ps = append(ps, p)
			}
		}

		return ps, runId, true, nil
	}

	ps, err := app.FindRecordsByFilter(CollectionNameRetentionPolicies, "enabled = true", "created,id", 0, 0)
	if err != nil {
		return nil, "", false, err
	}

	runId := GenerateDefaultRandomId()

	for _, p := range ps {
		p.Set("runId", runId)
		p.Set("runStatus", retentionRunning)
		p.Set("runCounts", retentionJSON(RetentionCounts{}))

		if err := app.UnsafeWithoutHooks().SaveNoValidate(p); err != nil {
			return nil, "", false, err
		}
	}

	return ps, runId, false, nil
}

func retentionJSON(v any) types.JSONRaw {
	b, _ := json.Marshal(v)

	return types.JSONRaw(b)
}

// retentionFinish clears the run state and stores the result of each policy.
func retentionFinish(app App, policies []*Record, res *RetentionRunResult) {
	byId := map[string]RetentionPolicyResult{}
	for _, r := range res.Policies {
		byId[r.PolicyId] = r
	}

	for _, p := range policies {
		fresh, err := app.FindRecordById(CollectionNameRetentionPolicies, p.Id)
		if err != nil {
			continue
		}

		fresh.Set("runStatus", "")
		fresh.Set("runId", "")
		fresh.Set("runCounts", retentionJSON(RetentionCounts{}))
		fresh.Set("lastRunAt", types.NowDateTime())
		fresh.Set("lastResult", retentionJSON(byId[p.Id]))

		if err := app.UnsafeWithoutHooks().SaveNoValidate(fresh); err != nil {
			app.Logger().Warn("Failed to store the retention result", "policy", p.Id, "error", err.Error())
		}
	}
}

func retentionPeriod(t time.Time, n int, unit string, sign int) time.Time {
	switch unit {
	case RetentionUnitMonths:
		return t.AddDate(0, sign*n, 0)
	case RetentionUnitYears:
		return t.AddDate(sign*n, 0, 0)
	}

	return t.AddDate(0, 0, sign*n)
}

// runRetentionPolicy scans one policy from its stored cursor to the end.
func runRetentionPolicy(ctx context.Context, app App, p *Record, opts *RetentionRunOptions, now time.Time) (RetentionPolicyResult, error) {
	res := RetentionPolicyResult{
		PolicyId: p.Id, Collection: p.GetString("collection"), Basis: p.GetString("basis"),
		After: p.GetInt("after"), Unit: p.GetString("unit"), Action: p.GetString("action"),
		DryRun: p.GetBool("dryRun") || opts.Preview,
	}

	var counts RetentionCounts

	if !opts.Preview {
		_ = p.UnmarshalJSONField("runCounts", &counts)
	}

	finish := func(msg string) (RetentionPolicyResult, error) {
		if msg != "" {
			counts.Error = msg
		}

		counts.Done = true
		res.RetentionCounts = counts

		if !opts.Preview {
			retentionSaveProgress(app, p.Id, counts, retentionDone)
		}

		return res, nil
	}

	// a policy that was finished before the interruption, or disabled since
	if counts.Done {
		res.RetentionCounts = counts

		return res, nil
	}

	if !opts.Preview && !p.GetBool("enabled") {
		return finish("The policy was disabled during the run.")
	}

	col, err := app.FindCachedCollectionByNameOrId(res.Collection)
	if err != nil {
		return finish("The collection does not exist.")
	}

	if why := RetentionBlockedReason(app, col); why != "" {
		return finish(why)
	}

	var anonFields []string

	if res.Action == RetentionActionAnonymize {
		if anonFields = RetentionAnonymizeFields(app, col); len(anonFields) == 0 {
			return finish("The data map has no personal fields for this collection.")
		}
	}

	pastoral := IsPastoralCollection(col) && retentionHasCollection(app, CollectionNamePastoralHolds)
	cutoff := retentionPeriod(now.UTC(), res.After, res.Unit, -1)
	ageOut := res.Basis == RetentionBasisAgeOut
	userFilter := strings.TrimSpace(p.GetString("filter"))
	grad := newRetentionGraduation(app)

	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		filter := `id > {:__after}`

		params := dbx.Params{"__after": counts.Cursor}

		// the age-out basis cannot be filtered in SQL: birthDate is encrypted,
		// so every row is read and its date computed in Go
		if !ageOut {
			filter += " && " + res.Basis + ` != "" && ` + res.Basis + ` <= {:__cutoff}`
			params["__cutoff"] = cutoffString(cutoff)
		}

		if userFilter != "" {
			filter += " && (" + userFilter + ")"
		}

		rows, err := app.FindRecordsByFilter(col, filter, "+id", opts.batchSize(), 0, params)
		if err != nil {
			return finish("The scan failed: " + err.Error())
		}

		if len(rows) == 0 {
			return finish("")
		}

		var eligible []*Record

		for _, r := range rows {
			counts.Cursor = r.Id

			if ageOut && !grad.eligible(r, res.After, res.Unit, now) {
				continue
			}

			eligible = append(eligible, r)
		}

		held, err := retentionHeld(app, col, eligible, pastoral)
		if err != nil {
			return res, err
		}

		for _, r := range eligible {
			switch {
			case res.Action == RetentionActionArchive && r.GetBool("archived"),
				res.Action == RetentionActionAnonymize && retentionAnonymized(r, anonFields):
				counts.Matched++
				counts.Unchanged++
			case held[r.Id]:
				counts.Matched++
				counts.Held++
			default:
				counts.Matched++

				if res.DryRun {
					counts.Affected++

					continue
				}

				if err := retentionApply(ctx, app, r, res.Action, anonFields); err != nil {
					counts.Failed++

					app.Logger().Warn("Retention could not change a row", "policy", p.Id, "collection", res.Collection, "record", r.Id, "error", err.Error())

					continue
				}

				counts.Affected++
			}
		}

		counts.Batches++

		if !opts.Preview {
			retentionSaveProgress(app, p.Id, counts, retentionRunning)
		}

		if retentionBatchHook != nil {
			if err := retentionBatchHook(p.Id, counts.Batches); err != nil {
				return res, err
			}
		}
	}
}

func retentionHasCollection(app App, name string) bool {
	_, err := app.FindCachedCollectionByNameOrId(name)

	return err == nil
}

func cutoffString(t time.Time) string {
	dt, _ := types.ParseDateTime(t)

	return dt.String()
}

func retentionSaveProgress(app App, id string, counts RetentionCounts, state string) {
	fresh, err := app.FindRecordById(CollectionNameRetentionPolicies, id)
	if err != nil {
		return
	}

	fresh.Set("runStatus", state)
	fresh.Set("runCounts", retentionJSON(counts))

	if err := app.UnsafeWithoutHooks().SaveNoValidate(fresh); err != nil {
		app.Logger().Warn("Failed to store the retention progress", "policy", id, "error", err.Error())
	}
}

// retentionHeld returns the rows the engine must leave alone.
func retentionHeld(app App, col *Collection, rows []*Record, pastoral bool) (map[string]bool, error) {
	if len(rows) == 0 {
		return map[string]bool{}, nil
	}

	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.Id)
	}

	held, err := LegalHeldAmong(app, col, ids)
	if err != nil {
		return nil, err
	}

	if pastoral {
		holds, err := app.FindRecordsByFilter(CollectionNamePastoralHolds, "collection = {:c} && state = {:s}", "", 0, 0,
			dbx.Params{"c": col.Name, "s": PastoralHoldHeld})
		if err != nil {
			return nil, err
		}

		for _, h := range holds {
			held[h.GetString("recordId")] = true
		}
	}

	return held, nil
}

func retentionBlank(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == "" || t == "null" || t == "[]" || t == "{}"
	case bool:
		return !t
	case int:
		return t == 0
	case float64:
		return t == 0
	case []string:
		return len(t) == 0
	case types.DateTime:
		return t.IsZero()
	case types.JSONRaw:
		return len(t) == 0 || string(t) == "null" || string(t) == "[]" || string(t) == "{}"
	case []any:
		return len(t) == 0
	}

	return false
}

func retentionAnonymized(r *Record, fields []string) bool {
	for _, f := range fields {
		if !retentionBlank(r.GetRaw(f)) {
			return false
		}
	}

	return true
}

func retentionApply(ctx context.Context, app App, r *Record, action string, anonFields []string) error {
	switch action {
	case RetentionActionDelete:
		return app.DeleteWithContext(ctx, r)
	case RetentionActionArchive:
		r.Set("archived", true)

		return app.SaveNoValidateWithContext(ctx, r)
	case RetentionActionAnonymize:
		for _, name := range anonFields {
			switch r.Collection().Fields.GetByName(name).(type) {
			case *BoolField:
				r.Set(name, false)
			case *NumberField:
				r.Set(name, 0)
			case *SelectField, *RelationField, *FileField:
				r.Set(name, []string{})
			default:
				r.Set(name, "")
			}
		}

		return app.SaveNoValidateWithContext(ctx, r)
	}

	return fmt.Errorf("unknown action %q", action)
}

// ---- age out -------------------------------------------------------------------

type retentionGraduation struct {
	app  App
	ages map[string]int // org id or household id -> graduation age
}

func newRetentionGraduation(app App) *retentionGraduation {
	return &retentionGraduation{app: app, ages: map[string]int{}}
}

// age returns the graduation age that applies to the row: its org's setting,
// found directly or through its household, or the default.
func (g *retentionGraduation) age(r *Record) int {
	orgId := ""

	if r.Collection().Fields.GetByName("org") != nil {
		orgId = r.GetString("org")
	} else if r.Collection().Fields.GetByName("household") != nil {
		if hid := r.GetString("household"); hid != "" {
			if v, ok := g.ages["h:"+hid]; ok {
				return v
			}

			age := DefaultHouseholdGraduationAge

			if h, err := g.app.FindRecordById(CollectionNameHouseholds, hid); err == nil {
				if org, err := g.app.FindRecordById(CollectionNameOrgs, h.GetString("org")); err == nil {
					age = HouseholdGraduationAge(org)
				}
			}

			g.ages["h:"+hid] = age

			return age
		}
	}

	if orgId == "" {
		return DefaultHouseholdGraduationAge
	}

	if v, ok := g.ages["o:"+orgId]; ok {
		return v
	}

	age := DefaultHouseholdGraduationAge

	if org, err := g.app.FindRecordById(CollectionNameOrgs, orgId); err == nil {
		age = HouseholdGraduationAge(org)
	}

	g.ages["o:"+orgId] = age

	return age
}

// eligible reports whether the person's isMinor has turned false at least the
// period ago. A row without a usable birth date never qualifies.
func (g *retentionGraduation) eligible(r *Record, after int, unit string, now time.Time) bool {
	birth, err := ParseBirthDate(r.GetString("birthDate"))
	if err != nil {
		return false
	}

	adult := birth.AddDate(g.age(r), 0, 0)

	return !retentionPeriod(adult, after, unit, 1).After(now.UTC())
}

// ---- audit ---------------------------------------------------------------------

func writeRetentionAudit(app App, ctx context.Context, res *RetentionRunResult) error {
	if !auditAvailable(app) {
		return nil
	}

	totals := RetentionCounts{}

	for _, p := range res.Policies {
		totals.Matched += p.Matched
		totals.Held += p.Held
		totals.Unchanged += p.Unchanged
		totals.Failed += p.Failed
		totals.Affected += p.Affected
	}

	totals.Cursor = ""

	// ids and counts only: no policy name or filter text, no row content
	policies := make([]map[string]any, 0, len(res.Policies))

	for _, p := range res.Policies {
		m := map[string]any{
			"policyId": p.PolicyId, "collection": p.Collection, "basis": p.Basis, "after": p.After,
			"unit": p.Unit, "action": p.Action, "dryRun": p.DryRun,
			"matched": p.Matched, "held": p.Held, "unchanged": p.Unchanged, "failed": p.Failed, "affected": p.Affected,
		}

		if p.Error != "" {
			m["error"] = p.Error
		}

		policies = append(policies, m)
	}

	entry := &AuditEntry{
		Action:         AuditActionRetentionRun,
		CollectionName: CollectionNameRetentionPolicies,
		ChangedFields:  []string{},
		Meta: map[string]any{
			"runId": res.RunId, "resumed": res.Resumed, "preview": res.Preview,
			"startedAt": res.StartedAt, "finishedAt": res.FinishedAt,
			"policies": policies, "totals": totals,
		},
	}

	if c, err := app.FindCachedCollectionByNameOrId(CollectionNameRetentionPolicies); err == nil {
		entry.CollectionId = c.Id
	}

	applyAuditContext(ctx, entry)

	return WriteAuditEntry(app, ctx, entry, nil)
}

// ---- hooks ---------------------------------------------------------------------

// startRetentionRun starts a background run (used by the daily job and at
// start-up). It does nothing when a run is already in progress.
func startRetentionRun(app App) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				app.Logger().Error("Retention job panicked", "panic", r)
			}
		}()

		ctx := WithAuditContext(context.Background(), AuditActor{Type: AuditActorTypeSystem}, "")

		if _, err := RunRetention(ctx, app, RetentionRunOptions{}); err != nil && !errors.Is(err, ErrRetentionRunning) && !errors.Is(err, context.Canceled) {
			app.Logger().Warn("Failed to run the retention job", "error", err.Error())
		}
	}()
}

func (app *BaseApp) registerRetentionHooks() {
	app.registerLegalHoldHooks()

	app.OnRecordValidate(CollectionNameRetentionPolicies).Bind(&hook.Handler[*RecordEvent]{
		Id: "__clvRetentionValidate__",
		Func: func(e *RecordEvent) error {
			if e.Context == nil || e.Context.Value(retentionWriteCtxKey{}) == nil {
				return validation.Errors{"collection": validation.NewError("validation_forbidden", "Policies are managed through /api/gdpr/retention/policies.")}
			}

			if err := validateRetentionPolicy(e.App, e.Record); err != nil {
				return err
			}

			return e.Next()
		},
	})

	for _, bindFn := range []func(tags ...string) *hook.TaggedHook[*RecordRequestEvent]{
		app.OnRecordCreateRequest, app.OnRecordUpdateRequest, app.OnRecordDeleteRequest,
	} {
		bindFn(CollectionNameRetentionPolicies).Bind(&hook.Handler[*RecordRequestEvent]{
			Id:       "__clvRetentionGuard__",
			Priority: -100000,
			Func: func(e *RecordRequestEvent) error {
				return router.NewForbiddenError("Retention policies are managed through /api/gdpr/retention/policies.", nil)
			},
		})
	}

	app.OnServe().Bind(&hook.Handler[*ServeEvent]{
		Id: "__clvRetentionResume__",
		Func: func(e *ServeEvent) error {
			// a run cut short by a restart continues from its cursor
			if RetentionAvailable(e.App) {
				n, err := e.App.CountRecords(CollectionNameRetentionPolicies, dbx.NewExp("[[runStatus]] != ''"))
				if err == nil && n > 0 {
					startRetentionRun(e.App)
				}
			}

			return e.Next()
		},
	})

	app.Cron().Add(retentionCronId, retentionCronExpr, func() {
		if RetentionAvailable(app) {
			startRetentionRun(app)
		}
	})
}
