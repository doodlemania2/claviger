package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #86, 86-5): adds the kind and archivedAt fields that
// expiring prayer requests need to the pastoral notes collections made before
// them, and moves their update rule to the new locked text when it is exactly
// the old locked one (a moderator that archives still changes nothing else,
// and not kind or archivedAt either). New collections get both from the
// template. There is no down step: the fields are harmless.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		_, err := core.UpgradePastoralExpiry(txApp)

		return err
	}, func(txApp core.App) error {
		return nil
	})
}
