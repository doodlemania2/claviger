package core

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #84): hierarchical organizations.
//
// `_orgs.parent` stays the source of truth. The hidden system collection
// `_orgClosure` is derived from it: one row per (ancestor, descendant) pair,
// including every org with itself at depth 0. Rules reach any ancestor with
// the multi-valued back-relation `org.orgClosure_via_descendant` (see
// OrgAncestorRule), so no resolver change is needed.
//
// The table is kept consistent by model hooks on _orgs (create, re-parent and
// delete) in the same transaction as the org write. It holds no personal data
// and is never edited by hand: its write rules are null and the rows are
// written with plain SQL, so no record hooks, realtime events or per-row audit
// entries are produced. A re-parent writes one audit entry (org_reparent)
// with counts. RepairOrgClosure rebuilds the table from `parent`.

const (
	// CollectionNameOrgClosure is the hidden closure table of the org tree.
	CollectionNameOrgClosure = "_orgClosure"

	// OrgMaxDepthEnv sets how many levels the org tree may have (a root is
	// level 1). Default 4, hard cap 8.
	OrgMaxDepthEnv = "CLAVIGER_ORGS_MAX_DEPTH"

	OrgDefaultMaxDepth = 4
	OrgHardMaxDepth    = 8

	// OrgKindMaxLength is the longest `kind` label of an org.
	OrgKindMaxLength = 64

	// AuditActionOrgReparent is the audit action of a re-parent (counts only).
	AuditActionOrgReparent = "org_reparent"
)

// OrgMaxDepth returns the configured maximum number of levels of the org
// tree: CLAVIGER_ORGS_MAX_DEPTH, default 4, never above the hard cap of 8.
// An unparsable or non-positive value falls back to the default.
func OrgMaxDepth() int {
	v := strings.TrimSpace(os.Getenv(OrgMaxDepthEnv))
	if v == "" {
		return OrgDefaultMaxDepth
	}

	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return OrgDefaultMaxDepth
	}

	return min(n, OrgHardMaxDepth)
}

type orgClosureRow struct {
	Ancestor   string `db:"ancestor"`
	Descendant string `db:"descendant"`
	Depth      int    `db:"depth"`
}

func orgClosureAvailable(app App) bool {
	_, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgClosure)

	return err == nil
}

func insertOrgClosureRows(app App, rows []orgClosureRow) error {
	db := app.NonconcurrentDB()

	for _, r := range rows {
		_, err := db.Insert(CollectionNameOrgClosure, dbx.Params{
			"id":         GenerateDefaultRandomId(),
			"ancestor":   r.Ancestor,
			"descendant": r.Descendant,
			"depth":      r.Depth,
		}).Execute()
		if err != nil {
			return err
		}
	}

	return nil
}

// orgSubtree returns (descendant, depth) of every org below org, itself at 0.
func orgSubtreeClosure(app App, orgId string) ([]orgClosureRow, error) {
	rows := []orgClosureRow{}

	err := app.DB().NewQuery("SELECT [[ancestor]], [[descendant]], [[depth]] FROM {{" + CollectionNameOrgClosure +
		"}} WHERE [[ancestor]]={:id}").Bind(dbx.Params{"id": orgId}).All(&rows)

	return rows, err
}

// orgAncestorClosure returns every ancestor row (ancestor, depth) of orgId,
// itself at 0.
func orgAncestorClosure(app App, orgId string) ([]orgClosureRow, error) {
	rows := []orgClosureRow{}

	err := app.DB().NewQuery("SELECT [[ancestor]], [[descendant]], [[depth]] FROM {{" + CollectionNameOrgClosure +
		"}} WHERE [[descendant]]={:id}").Bind(dbx.Params{"id": orgId}).All(&rows)

	return rows, err
}

// ---- validation -------------------------------------------------------------

// orgParentChain returns the ids from parentId up to the root (parentId
// first). It fails on a cycle through stopId or on a loop in the data.
func orgParentChain(app App, parentId, stopId string) ([]string, error) {
	chain := []string{}
	seen := map[string]bool{}

	for cur := parentId; cur != ""; {
		if cur == stopId {
			return nil, orgErr(400, "The parent would create a cycle in the organization tree.")
		}

		if seen[cur] || len(chain) > OrgHardMaxDepth {
			return nil, orgErr(400, "The organization tree is too deep or contains a cycle.")
		}

		seen[cur] = true

		rec, err := app.FindRecordById(CollectionNameOrgs, cur)
		if err != nil {
			return nil, orgErr(400, "Unknown parent organization.")
		}

		chain = append(chain, cur)
		cur = rec.GetString("parent")
	}

	return chain, nil
}

// orgSubtreeHeight returns how many levels sit below the org (0 for a leaf),
// walking `parent` so it does not depend on the closure table.
func orgSubtreeHeight(app App, orgId string) (int, error) {
	height := 0
	level := []string{orgId}

	for len(level) > 0 {
		exprs := make([]any, len(level))
		for i, id := range level {
			exprs[i] = id
		}

		kids, err := app.FindAllRecords(CollectionNameOrgs, dbx.In("parent", exprs...))
		if err != nil {
			return 0, err
		}

		if len(kids) == 0 {
			break
		}

		height++
		if height > OrgHardMaxDepth {
			return 0, orgErr(400, "The organization tree is too deep or contains a cycle.")
		}

		level = level[:0]
		for _, k := range kids {
			level = append(level, k.Id)
		}
	}

	return height, nil
}

func validateOrg(app App, org *Record) error {
	parentId := org.GetString("parent")
	if parentId == "" {
		return nil
	}

	if parentId == org.Id {
		return orgErr(400, "An organization cannot be its own parent.")
	}

	if _, err := app.FindRecordById(CollectionNameOrgs, parentId); err != nil {
		return orgErr(400, "Unknown parent organization.")
	}

	// the shape of the tree only matters when it changes, so lowering the
	// configured depth later does not block unrelated edits of a deep org
	if !org.IsNew() && org.Original().GetString("parent") == parentId {
		return nil
	}

	stop := org.Id
	if org.IsNew() {
		stop = ""
	}

	chain, err := orgParentChain(app, parentId, stop)
	if err != nil {
		return err
	}

	below := 0
	if !org.IsNew() {
		if below, err = orgSubtreeHeight(app, org.Id); err != nil {
			return err
		}
	}

	levels := len(chain) + 1 + below
	if limit := OrgMaxDepth(); levels > limit {
		return orgErr(400, fmt.Sprintf("The organization tree can have at most %d levels (%s); this change would make it %d.",
			limit, OrgMaxDepthEnv, levels))
	}

	return nil
}

// ---- hooks ------------------------------------------------------------------

func (app *BaseApp) registerOrgClosureHooks() {
	app.OnRecordCreateExecute(CollectionNameOrgs).Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbOrgClosureCreate__",
		Func: func(e *RecordEvent) error {
			if !orgClosureAvailable(e.App) {
				return e.Next()
			}

			return orgClosureInTx(e, func(txApp App) error {
				if err := e.Next(); err != nil {
					return err
				}

				rows := []orgClosureRow{{Ancestor: e.Record.Id, Descendant: e.Record.Id}}

				if parent := e.Record.GetString("parent"); parent != "" {
					anc, err := orgAncestorClosure(txApp, parent)
					if err != nil {
						return err
					}

					for _, a := range anc {
						rows = append(rows, orgClosureRow{Ancestor: a.Ancestor, Descendant: e.Record.Id, Depth: a.Depth + 1})
					}
				}

				return insertOrgClosureRows(txApp, rows)
			})
		},
		Priority: 90,
	})

	app.OnRecordUpdateExecute(CollectionNameOrgs).Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbOrgClosureUpdate__",
		Func: func(e *RecordEvent) error {
			if !orgClosureAvailable(e.App) {
				return e.Next()
			}

			// the stored parent, read before the write
			oldParent := e.Record.Original().GetString("parent")
			if stored, err := e.App.FindRecordById(CollectionNameOrgs, e.Record.Id); err == nil {
				oldParent = stored.GetString("parent")
			}

			newParent := e.Record.GetString("parent")
			if oldParent == newParent {
				return e.Next()
			}

			return orgClosureInTx(e, func(txApp App) error {
				if err := e.Next(); err != nil {
					return err
				}

				removed, added, err := reparentOrgClosure(txApp, e.Record.Id, newParent)
				if err != nil {
					return err
				}

				return writeOrgReparentAudit(txApp, e.Context, e.Record, oldParent, newParent, removed, added)
			})
		},
		Priority: 90,
	})

	app.OnRecordDeleteExecute(CollectionNameOrgs).Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbOrgClosureDelete__",
		Func: func(e *RecordEvent) error {
			if !orgClosureAvailable(e.App) {
				return e.Next()
			}

			// children are refused by another hook, so this removes the self
			// row and the rows of the ancestors
			return orgClosureInTx(e, func(txApp App) error {
				_, err := txApp.NonconcurrentDB().NewQuery("DELETE FROM {{" + CollectionNameOrgClosure +
					"}} WHERE [[ancestor]]={:id} OR [[descendant]]={:id}").Bind(dbx.Params{"id": e.Record.Id}).Execute()
				if err != nil {
					return err
				}

				return e.Next()
			})
		},
		Priority: 90,
	})

	// a restored backup or a manual database edit can leave the table behind
	// the tree: say so at startup, repairing is an explicit command
	app.OnServe().Bind(&hook.Handler[*ServeEvent]{
		Id: "__pbOrgClosureCheck__",
		Func: func(e *ServeEvent) error {
			if orgClosureAvailable(e.App) {
				if rep, err := RepairOrgClosure(e.App, true); err == nil && rep.Differences() > 0 {
					e.App.Logger().Warn("The org closure table differs from the org tree; run `claviger orgs closure-repair`",
						"missing", rep.Missing, "extra", rep.Extra, "wrongDepth", rep.WrongDepth)
				}
			}

			return e.Next()
		},
	})
}

// orgClosureInTx runs fn in a transaction that also covers the org write
// (fn is expected to call e.Next()).
func orgClosureInTx(e *RecordEvent, fn func(txApp App) error) error {
	if e.App.IsTransactional() {
		return fn(e.App)
	}

	originalApp := e.App
	err := e.App.RunInTransaction(func(txApp App) error {
		e.App = txApp

		return fn(txApp)
	})
	e.App = originalApp

	return err
}

// reparentOrgClosure moves the subtree of orgId below newParent ("" = root)
// and returns how many closure rows it removed and added.
func reparentOrgClosure(app App, orgId, newParent string) (removed, added int, err error) {
	sub, err := orgSubtreeClosure(app, orgId)
	if err != nil {
		return 0, 0, err
	}

	res, err := app.NonconcurrentDB().NewQuery(
		"DELETE FROM {{" + CollectionNameOrgClosure + "}} WHERE " +
			"[[descendant]] IN (SELECT [[descendant]] FROM {{" + CollectionNameOrgClosure + "}} WHERE [[ancestor]]={:id}) AND " +
			"[[ancestor]] NOT IN (SELECT [[descendant]] FROM {{" + CollectionNameOrgClosure + "}} WHERE [[ancestor]]={:id})").
		Bind(dbx.Params{"id": orgId}).Execute()
	if err != nil {
		return 0, 0, err
	}

	if n, nerr := res.RowsAffected(); nerr == nil {
		removed = int(n)
	}

	if newParent == "" {
		return removed, 0, nil
	}

	anc, err := orgAncestorClosure(app, newParent)
	if err != nil {
		return 0, 0, err
	}

	rows := make([]orgClosureRow, 0, len(anc)*len(sub))
	for _, a := range anc {
		for _, d := range sub {
			rows = append(rows, orgClosureRow{Ancestor: a.Ancestor, Descendant: d.Descendant, Depth: a.Depth + 1 + d.Depth})
		}
	}

	if err := insertOrgClosureRows(app, rows); err != nil {
		return 0, 0, err
	}

	return removed, len(rows), nil
}

func writeOrgReparentAudit(app App, ctx context.Context, org *Record, oldParent, newParent string, removed, added int) error {
	if !auditAvailable(app) {
		return nil
	}

	entry := &AuditEntry{
		Action:         AuditActionOrgReparent,
		CollectionName: CollectionNameOrgs,
		RecordId:       org.Id,
		ChangedFields:  []string{"parent"},
		Meta: map[string]any{
			"orgId":          org.Id,
			"from":           oldParent,
			"to":             newParent,
			"closureRemoved": removed,
			"closureAdded":   added,
		},
	}
	if c, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgs); err == nil {
		entry.CollectionId = c.Id
	}
	applyAuditContext(ctx, entry)

	return WriteAuditEntry(app, ctx, entry, nil)
}

// ---- repair -----------------------------------------------------------------

// OrgClosureReport is the outcome of RepairOrgClosure.
type OrgClosureReport struct {
	// Expected and Actual are the row counts the tree implies and the table holds.
	Expected int `json:"expected"`
	Actual   int `json:"actual"`
	// Missing rows are absent, Extra rows should not exist, WrongDepth rows
	// exist with another depth.
	Missing    int `json:"missing"`
	Extra      int `json:"extra"`
	WrongDepth int `json:"wrongDepth"`
	// Cycles lists orgs whose parent chain loops or exceeds the hard cap;
	// they get only their self row.
	Cycles []string `json:"cycles"`
	// Applied is true when the table was changed (not a dry run).
	Applied bool `json:"applied"`
}

// Differences is the number of rows that differ from the tree.
func (r *OrgClosureReport) Differences() int { return r.Missing + r.Extra + r.WrongDepth }

// ExpectedOrgClosure computes the closure implied by the `parent` fields:
// a map of "ancestor|descendant" to depth, and the ids of orgs on a cycle.
func expectedOrgClosure(app App) (map[[2]string]int, []string, error) {
	type orgRow struct {
		Id     string `db:"id"`
		Parent string `db:"parent"`
	}

	var orgs []orgRow

	err := app.DB().NewQuery("SELECT [[id]], COALESCE([[parent]], '') AS [[parent]] FROM {{" + CollectionNameOrgs + "}}").All(&orgs)
	if err != nil {
		return nil, nil, err
	}

	parentOf := make(map[string]string, len(orgs))
	for _, o := range orgs {
		parentOf[o.Id] = o.Parent
	}

	want := map[[2]string]int{}
	cycles := []string{}

	for _, o := range orgs {
		want[[2]string{o.Id, o.Id}] = 0

		seen := map[string]bool{o.Id: true}
		cur, depth := parentOf[o.Id], 1

		for cur != "" {
			if seen[cur] || depth > OrgHardMaxDepth+1 {
				cycles = append(cycles, o.Id)

				break
			}

			if _, ok := parentOf[cur]; !ok {
				break // dangling parent
			}

			seen[cur] = true
			want[[2]string{cur, o.Id}] = depth
			cur = parentOf[cur]
			depth++
		}
	}

	slices.Sort(cycles)

	return want, cycles, nil
}

// RepairOrgClosure compares the closure table with the org tree and, unless
// dryRun, fixes every difference in one transaction. It is idempotent: a
// second run reports no differences.
func RepairOrgClosure(app App, dryRun bool) (*OrgClosureReport, error) {
	if !orgClosureAvailable(app) {
		return nil, fmt.Errorf("the %s collection does not exist", CollectionNameOrgClosure)
	}

	want, cycles, err := expectedOrgClosure(app)
	if err != nil {
		return nil, err
	}

	var have []orgClosureRow

	err = app.DB().NewQuery("SELECT [[ancestor]], [[descendant]], [[depth]] FROM {{" + CollectionNameOrgClosure + "}}").All(&have)
	if err != nil {
		return nil, err
	}

	rep := &OrgClosureReport{Expected: len(want), Actual: len(have), Cycles: cycles}

	var extra, fix []orgClosureRow

	seen := make(map[[2]string]bool, len(have))

	for _, r := range have {
		k := [2]string{r.Ancestor, r.Descendant}

		d, ok := want[k]

		switch {
		case !ok || seen[k]:
			rep.Extra++

			extra = append(extra, r)
		case d != r.Depth:
			rep.WrongDepth++

			fix = append(fix, orgClosureRow{Ancestor: r.Ancestor, Descendant: r.Descendant, Depth: d})
		}

		seen[k] = true
	}

	var missing []orgClosureRow

	for k, d := range want {
		if !seen[k] {
			rep.Missing++

			missing = append(missing, orgClosureRow{Ancestor: k[0], Descendant: k[1], Depth: d})
		}
	}

	if dryRun || rep.Differences() == 0 {
		return rep, nil
	}

	err = app.RunInTransaction(func(txApp App) error {
		db := txApp.NonconcurrentDB()

		for _, r := range extra {
			// rows of a dangling org are removed by key, duplicates cannot exist (unique index)
			_, err := db.NewQuery("DELETE FROM {{" + CollectionNameOrgClosure +
				"}} WHERE [[ancestor]]={:a} AND [[descendant]]={:d}").
				Bind(dbx.Params{"a": r.Ancestor, "d": r.Descendant}).Execute()
			if err != nil {
				return err
			}
		}

		for _, r := range fix {
			_, err := db.NewQuery("UPDATE {{" + CollectionNameOrgClosure +
				"}} SET [[depth]]={:depth} WHERE [[ancestor]]={:a} AND [[descendant]]={:d}").
				Bind(dbx.Params{"a": r.Ancestor, "d": r.Descendant, "depth": r.Depth}).Execute()
			if err != nil {
				return err
			}
		}

		slices.SortFunc(missing, func(a, b orgClosureRow) int {
			return strings.Compare(a.Ancestor+"|"+a.Descendant, b.Ancestor+"|"+b.Descendant)
		})

		return insertOrgClosureRows(txApp, missing)
	})
	if err != nil {
		return nil, err
	}

	rep.Applied = true

	return rep, nil
}
