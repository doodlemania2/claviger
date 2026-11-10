package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #84, 84-3): adds the optional free-text `kind` label to
// _orgs ("region", "congregation", "ministry", ...). The engine never reads it
// and no rule depends on it: each deployment picks its own vocabulary, the
// admin UI and the status route only group by it.
//
// It does nothing when the organizations model does not exist, and when the
// field is already there.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		orgs, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgs)
		if err != nil {
			return nil
		}

		if orgs.Fields.GetByName("kind") != nil {
			return nil
		}

		orgs.Fields.Add(&core.TextField{Name: "kind", System: true, Max: core.OrgKindMaxLength})

		return txApp.Save(orgs)
	}, nil)
}
