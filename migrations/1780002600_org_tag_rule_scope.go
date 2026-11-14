package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #84): a role tag in a generated collection rule only
// counts when the tag belongs to the membership's org (core.OrgTagRule,
// core.OrgAncestorRule with a tag, core.HouseholdStaffRule). Installs created
// before this carry the old text, which matched the tag name alone, so a tag
// of another org written around the model hooks (a raw database write, an
// import) would still satisfy it.
//
// The migration rewrites every rule that is exactly the text a generator
// printed back then: the list and view rules of _households,
// _householdMembers and _credentials, and any collection whose rule is a
// plain tag rule, an ancestor tag rule or a staff rule (register and consent
// collections made from the templates). A rule somebody edited, or wrote by
// hand, is left alone; it keeps working but does not check the tag's org, and
// docs/orgs.md shows the clause to add. Every rewritten rule is logged with
// its old and new text, and each collection change goes through the normal
// collection audit.
//
// It does nothing when the organizations model does not exist. There is no
// down step: the old text is the weaker one.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgs); err != nil {
			return nil
		}

		changes, err := core.UpgradeOrgTagRules(txApp)
		if err != nil {
			return err
		}

		for _, c := range changes {
			txApp.Logger().Info("org tag rule scoped to the tag's org",
				"collection", c.Collection, "rule", c.Rule, "old", c.Old, "new", c.New)
		}

		return nil
	}, func(txApp core.App) error {
		return nil
	})
}
