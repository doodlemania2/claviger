package core

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, 88-4): scrubbing the audit log on erasure
// (spec 10.6, option A).
//
// The audit log is append-only. This file holds the ONE exception: the
// function ScrubAuditForErasure. For the entries about the erased person it
//
//   - sets before, after and ip to empty;
//   - replaces the person's id (as actor or as target) with a pseudonymous id;
//   - reduces meta to the org ids (so an org still finds its own entries) and
//     a scrubbed marker;
//
// and keeps the action, the time, the collection names and the changed field
// names. It writes nothing else: the caller writes the person's own gdpr_erase
// entry (a normal, append-only entry).
//
// How the guard lets it through: the model guard of _auditLog (audit_model.go)
// refuses every update and delete, except an update that carries auditScrubCtxKey
// in its context and changes nothing but the six scrubbed columns. The key type
// is unexported, so no route, hook or script can build it, and the check on the
// columns means that even code inside this package cannot use it to rewrite an
// action or a time.
//
// Which entries are "about" the person:
//   - the actor or the target is one of the person's ids;
//   - the target is a row the erasure itself deleted or anonymized (the
//     touched rows: a session, a passkey, a household row, ...);
//   - before, after or meta mention one of the person's ids or an email
//     address they had.
//
// The legal question of which entries may be kept for a security reason is
// open (spec question 18); nothing is kept here. See docs/erasure.md.

type auditScrubCtxKey struct{}

// the columns the scrub may change
var auditScrubColumns = []string{"before", "after", "ip", "meta", "actorId", "targetId"}

// AuditScrubTarget says whose entries to scrub.
type AuditScrubTarget struct {
	// Ids are the person's ids (the account and its household rows).
	Ids []string

	// Needles are other strings that identify them: the email addresses they had.
	Needles []string

	// Rows are rows the erasure changed, by collection name.
	Rows map[string][]string
}

const auditScrubBatch = 200

// ScrubAuditForErasure scrubs the audit entries about the person. It is
// idempotent: a scrubbed entry no longer matches. It returns how many entries
// it changed.
func ScrubAuditForErasure(app App, ctx context.Context, t AuditScrubTarget) (int, error) {
	if !auditAvailable(app) {
		return 0, nil
	}

	if ctx == nil {
		ctx = context.Background()
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameAuditLog)
	if err != nil {
		return 0, err
	}

	ids := make([]string, 0, len(t.Ids))

	for _, id := range t.Ids {
		if id != "" {
			ids = append(ids, id)
		}
	}

	if len(ids) == 0 {
		return 0, nil
	}

	d := DialectOf(app)

	var exps []dbx.Expression

	idAny := stringsToAny(ids...)
	exps = append(exps, dbx.In("actorId", idAny...), dbx.In("targetId", idAny...))

	for c, rowIds := range t.Rows {
		if len(rowIds) == 0 {
			continue
		}

		for chunk := range slices.Chunk(rowIds, 200) {
			exps = append(exps, dbx.And(dbx.HashExp{"targetCollection": c}, dbx.In("targetId", stringsToAny(chunk...)...)))
		}
	}

	needles := slices.Clone(ids)

	for _, n := range t.Needles {
		if n = strings.TrimSpace(n); n != "" {
			needles = append(needles, n)
		}
	}

	for i, n := range needles {
		for _, column := range []string{"before", "after", "meta"} {
			param := fmt.Sprintf("__scrub_%s_%d", column, i)
			exps = append(exps, dbx.NewExp(
				d.AsText("[["+column+"]]")+" "+d.LikeOp(false)+" {:"+param+"}",
				dbx.Params{param: "%" + n + "%"},
			))
		}
	}

	where := dbx.Or(exps...)
	scrubCtx := context.WithValue(ctx, auditScrubCtxKey{}, true)
	scrubbed := 0
	last := ""

	for {
		var rows []*Record

		q := app.RecordQuery(col).AndWhere(where).OrderBy("id ASC").Limit(auditScrubBatch)
		if last != "" {
			q = q.AndWhere(dbx.NewExp("[[id]] > {:__last}", dbx.Params{"__last": last}))
		}

		if err := q.All(&rows); err != nil {
			return scrubbed, err
		}

		if len(rows) == 0 {
			return scrubbed, nil
		}

		for _, r := range rows {
			last = r.Id

			changed, err := scrubAuditRow(app, scrubCtx, r, ids)
			if err != nil {
				return scrubbed, err
			}

			if changed {
				scrubbed++
			}
		}

		if len(rows) < auditScrubBatch {
			return scrubbed, nil
		}
	}
}

func scrubAuditRow(app App, ctx context.Context, r *Record, ids []string) (bool, error) {
	changed := false

	set := func(name string, v any) {
		if !auditScrubEqual(r.GetRaw(name), v) {
			r.Set(name, v)

			changed = true
		}
	}

	set("before", nil)
	set("after", nil)
	set("ip", "")

	keep := map[string]any{"scrubbed": true}

	var meta map[string]any
	_ = r.UnmarshalJSONField("meta", &meta)

	for _, k := range []string{"orgId", "orgIds"} {
		if v, ok := meta[k]; ok {
			keep[k] = v
		}
	}

	set("meta", keep)

	for _, f := range []string{"actorId", "targetId"} {
		if v := r.GetString(f); v != "" && slices.Contains(ids, v) {
			set(f, ErasurePseudonym(app, v))
		}
	}

	if !changed {
		return false, nil
	}

	return true, app.SaveNoValidateWithContext(ctx, r)
}

func auditScrubEqual(a, b any) bool {
	empty := func(v any) bool {
		switch t := v.(type) {
		case nil:
			return true
		case string:
			return t == "" || t == "null"
		case types.JSONRaw:
			return len(t) == 0 || string(t) == "null"
		}

		return fmt.Sprint(v) == "null" || fmt.Sprint(v) == "[]byte(null)"
	}

	if empty(a) && empty(b) {
		return true
	}

	return auditEqual(a, b)
}

// auditScrubAllowed is called by the guard of _auditLog for an update that
// carries the scrub key: only the scrubbed columns may differ from the stored
// row.
func auditScrubAllowed(r *Record) bool {
	orig := r.Original()
	if orig == nil {
		return false
	}

	for _, f := range r.Collection().Fields {
		name := f.GetName()
		if name == FieldNameId || slices.Contains(auditScrubColumns, name) {
			continue
		}

		if !auditEqual(orig.GetRaw(name), r.GetRaw(name)) {
			return false
		}
	}

	return true
}
