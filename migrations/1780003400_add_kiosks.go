package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #82, 82-4): the _kiosks system collection, one row per
// check-in device (a shared tablet at a children's ministry door).
//
// A kiosk is not a user. Its token is a restricted principal that may call the
// /api/checkin routes of its own org and nothing else (it cannot read the
// records API). The token is shown once at registration; only the hash is
// stored. All rules are null: org admins register and revoke kiosks through
// /api/checkin/kiosks, with a fresh passkey. See docs/checkin.md.
//
// It does nothing when the organizations model does not exist.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameKiosks); err == nil {
			return nil
		}

		orgs, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgs)
		if err != nil {
			return nil
		}

		col := core.NewBaseCollection(core.CollectionNameKiosks)
		col.System = true
		col.Fields.Add(
			&core.RelationField{Name: "org", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
			&core.TextField{Name: "name", System: true, Required: true, Max: 100},
			&core.TextField{Name: "prefix", System: true, Required: true},
			&core.TextField{Name: "deviceTokenHash", System: true, Required: true, Hidden: true},
			// the id of the org admin, as text: deleting an account must never be blocked
			&core.TextField{Name: "registeredBy", System: true, Max: 100},
			&core.DateField{Name: "lastSeen", System: true},
			&core.SelectField{Name: "status", System: true, Required: true, MaxSelect: 1, Values: core.KioskStatuses},
			// ids of checkin_events the kiosk may serve; empty means every event of its org
			&core.JSONField{Name: "allowedEvents", System: true, MaxSize: 64 << 10},
			&core.TextField{Name: "revokedBy", System: true, Max: 100},
			&core.DateField{Name: "revokedAt", System: true},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		col.AddIndex("idx_kiosks_prefix", true, "prefix", "")
		col.AddIndex("idx_kiosks_org", false, "org, status", "")

		if err := txApp.Save(col); err != nil {
			return err
		}

		return core.RegisterDataMapEntry(txApp, core.DataMapEntry{
			Collection: core.CollectionNameKiosks, Treatment: core.DataMapTreatmentAnonymize,
			SubjectPaths:   []string{"registeredBy"},
			PersonalFields: []string{"registeredBy", "revokedBy"},
			Note:           "Check-in kiosks (#82). A device, not a person; only the admin who registered or revoked it is personal data and is blanked on erasure. The token hash is never exported.",
		})
	}, func(txApp core.App) error {
		if col, err := txApp.FindCollectionByNameOrId(core.CollectionNameKiosks); err == nil {
			return txApp.Delete(col)
		}

		return nil
	})
}
