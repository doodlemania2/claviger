package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"
)

// Backfills the new PasskeyResetTemplate and PasskeyResetToken config on
// existing auth collections that pre-date the passkey-reset email flow.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		collections, err := txApp.FindAllCollections(core.CollectionTypeAuth)
		if err != nil {
			return err
		}

		// Defaults taken from a freshly seeded auth collection.
		defaults := core.NewAuthCollection("__passkey_reset_defaults__")
		defaultTemplate := defaults.PasskeyResetTemplate

		for _, c := range collections {
			changed := false

			if c.PasskeyResetTemplate.Subject == "" && c.PasskeyResetTemplate.Body == "" {
				c.PasskeyResetTemplate = defaultTemplate
				changed = true
			}

			if c.PasskeyResetToken.Secret == "" {
				c.PasskeyResetToken.Secret = security.RandomString(50)
				changed = true
			}
			if c.PasskeyResetToken.Duration == 0 {
				c.PasskeyResetToken.Duration = 1800 // 30min
				changed = true
			}

			if !changed {
				continue
			}

			if err := txApp.Save(c); err != nil {
				return err
			}
		}

		return nil
	}, nil)
}
