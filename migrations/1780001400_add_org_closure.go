package migrations

import (
	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #84): creates the hidden _orgClosure system collection
// (ancestor, descendant, depth) that makes the org tree queryable by rules,
// and fills it from the current `_orgs.parent` values.
//
// It does nothing when the organizations model does not exist. The collection
// holds no personal data. Its list and view rules let a member of the
// descendant or of the ancestor org read the row; write rules are null and
// the rows are maintained by hooks in core/org_closure.go (and rebuilt by
// `claviger orgs closure-repair`).
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgClosure); err == nil {
			return nil
		}

		orgs, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgs)
		if err != nil {
			return nil
		}

		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgMembers); err != nil {
			return nil
		}

		closure := core.NewBaseCollection(core.CollectionNameOrgClosure)
		closure.System = true
		closure.Fields.Add(
			&core.RelationField{Name: "ancestor", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
			&core.RelationField{Name: "descendant", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
			// not Required: a required number rejects 0, the depth of the self row
			&core.NumberField{Name: "depth", System: true, OnlyInt: true, Min: types.Pointer(0.0)},
		)
		closure.AddIndex("idx_orgClosure_pair", true, "ancestor, descendant", "")
		closure.AddIndex("idx_orgClosure_descendant", false, "descendant", "")
		closure.AddIndex("idx_orgClosure_ancestor_depth", false, "ancestor, depth", "")

		rule := `@request.auth.id != "" && ((descendant._orgMembers_via_org.user ?= @request.auth.id && ` +
			`descendant._orgMembers_via_org.status ?= "active") || (ancestor._orgMembers_via_org.user ?= @request.auth.id && ` +
			`ancestor._orgMembers_via_org.status ?= "active"))`
		closure.ListRule = types.Pointer(rule)
		closure.ViewRule = closure.ListRule

		if err := txApp.Save(closure); err != nil {
			return err
		}

		// existing deployments: derive the table from the current parents
		// (plain SQL, one level was possible before, but the walk handles any)
		type orgRow struct {
			Id     string `db:"id"`
			Parent string `db:"parent"`
		}

		var rows []orgRow
		if err := txApp.DB().NewQuery("SELECT [[id]], COALESCE([[parent]], '') AS [[parent]] FROM {{" +
			core.CollectionNameOrgs + "}}").All(&rows); err != nil {
			return err
		}

		parentOf := make(map[string]string, len(rows))
		for _, r := range rows {
			parentOf[r.Id] = r.Parent
		}

		for _, r := range rows {
			depth, cur, seen := 0, r.Id, map[string]bool{}

			for cur != "" && !seen[cur] {
				seen[cur] = true

				if _, ok := parentOf[cur]; !ok {
					break
				}

				_, err := txApp.NonconcurrentDB().Insert(core.CollectionNameOrgClosure, dbx.Params{
					"id":         core.GenerateDefaultRandomId(),
					"ancestor":   cur,
					"descendant": r.Id,
					"depth":      depth,
				}).Execute()
				if err != nil {
					return err
				}

				cur = parentOf[cur]
				depth++
			}
		}

		return nil
	}, func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgClosure)
		if err != nil {
			return nil
		}

		return txApp.Delete(col)
	})
}
