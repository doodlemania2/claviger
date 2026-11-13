package migrations

import (
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, 88-2): creates the hidden _consentPurposes,
// _consents and _consentEvents system collections (consent records, see
// docs/consent.md) and registers them in the data map.
//
// It does nothing when the organizations model does not exist. All write
// rules are null: rows are written by the routes under /api/me/consents,
// /api/orgs/{id}/consents and /api/consent-purposes, which enforce who may
// consent for whom (the rules of a system collection cannot be edited
// afterwards). A model hook keeps _consentEvents append-only.
//
// The subject is a plain text id (a household member or a user) and the
// purpose is stored as its key, so that a rule can compare it to a literal:
// @collection._consents:c.purpose ?= "newsletter". Dates are DateFields so a
// withdrawn consent is the empty-date comparison withdrawnAt ?= "".
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameConsents); err == nil {
			return nil
		}

		orgs, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgs)
		if err != nil {
			return nil
		}

		// purposes
		purposes := core.NewBaseCollection(core.CollectionNameConsentPurposes)
		purposes.System = true
		purposes.Fields.Add(
			&core.TextField{Name: "key", System: true, Required: true, Pattern: `^[a-z][a-z0-9_-]{1,49}$`, Max: 50},
			&core.TextField{Name: "name", System: true, Required: true, Max: 200},
			&core.TextField{Name: "text", System: true, Required: true, Max: 20000},
			&core.NumberField{Name: "version", System: true, OnlyInt: true, Min: types.Pointer(1.0)},
			&core.SelectField{Name: "lawfulBasis", System: true, Required: true, MaxSelect: 1, Values: core.ConsentLawfulBases},
			&core.NumberField{Name: "minAge", System: true, OnlyInt: true, Min: types.Pointer(0.0), Max: types.Pointer(float64(core.MaxConsentMinAge))},
			&core.BoolField{Name: "requiresGuardian", System: true},
			&core.BoolField{Name: "marketing", System: true},
			// no cascade: an org that still has purposes cannot be deleted by accident
			&core.RelationField{Name: "org", System: true, CollectionId: orgs.Id, MaxSelect: 1},
			&core.SelectField{Name: "status", System: true, Required: true, MaxSelect: 1, Values: core.ConsentPurposeStates},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		purposes.AddIndex("idx_consentPurposes_key", true, "key", "")
		purposes.AddIndex("idx_consentPurposes_org", false, "org", "")

		// the state rows
		consents := core.NewBaseCollection(core.CollectionNameConsents)
		consents.System = true
		consents.Fields.Add(
			&core.TextField{Name: "subject", System: true, Required: true, Max: 100},
			&core.SelectField{Name: "subjectType", System: true, Required: true, MaxSelect: 1, Values: core.ConsentSubjectTypes},
			&core.TextField{Name: "purpose", System: true, Required: true, Max: 50},
			&core.NumberField{Name: "policyVersion", System: true, OnlyInt: true, Min: types.Pointer(0.0)},
			&core.DateField{Name: "grantedAt", System: true},
			&core.DateField{Name: "withdrawnAt", System: true},
			&core.TextField{Name: "givenBy", System: true, Max: 100},
			&core.SelectField{Name: "method", System: true, Required: true, MaxSelect: 1, Values: core.ConsentMethods},
			&core.TextField{Name: "evidenceRef", System: true, Max: 500},
			&core.RelationField{Name: "org", System: true, CollectionId: orgs.Id, MaxSelect: 1},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		consents.AddIndex("idx_consents_subject_purpose", true, "subject, purpose", "")
		consents.AddIndex("idx_consents_purpose", false, "purpose", "")
		consents.AddIndex("idx_consents_org", false, "org", "")

		// the append-only evidence
		events := core.NewBaseCollection(core.CollectionNameConsentEvents)
		events.System = true
		events.Fields.Add(
			&core.TextField{Name: "subject", System: true, Required: true, Max: 100},
			&core.SelectField{Name: "subjectType", System: true, Required: true, MaxSelect: 1, Values: core.ConsentSubjectTypes},
			&core.TextField{Name: "purpose", System: true, Required: true, Max: 50},
			&core.SelectField{Name: "action", System: true, Required: true, MaxSelect: 1, Values: core.ConsentActions},
			&core.NumberField{Name: "policyVersion", System: true, OnlyInt: true, Min: types.Pointer(0.0)},
			&core.TextField{Name: "text", System: true, Max: 20000},
			&core.TextField{Name: "givenBy", System: true, Max: 100},
			&core.TextField{Name: "actor", System: true, Max: 100},
			&core.SelectField{Name: "method", System: true, Required: true, MaxSelect: 1, Values: core.ConsentMethods},
			&core.TextField{Name: "evidenceRef", System: true, Max: 500},
			&core.RelationField{Name: "org", System: true, CollectionId: orgs.Id, MaxSelect: 1},
			&core.DateField{Name: "pseudonymizedAt", System: true},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
		)
		events.AddIndex("idx_consentEvents_subject", false, "subject, purpose", "")
		events.AddIndex("idx_consentEvents_org", false, "org", "")

		// read rules: a person reads their own rows; purposes are readable
		// when active and offered to everyone. The routes serve everything else.
		own := `@request.auth.id != "" && subject = @request.auth.id`
		consents.ListRule = types.Pointer(own)
		consents.ViewRule = consents.ListRule
		events.ListRule = types.Pointer(own)
		events.ViewRule = events.ListRule
		purposes.ListRule = types.Pointer(`@request.auth.id != "" && status = "active" && org = ""`)
		purposes.ViewRule = purposes.ListRule

		for _, col := range []*core.Collection{purposes, consents, events} {
			if err := txApp.Save(col); err != nil {
				return err
			}
		}

		for _, name := range []string{core.CollectionNameConsentPurposes, core.CollectionNameConsents, core.CollectionNameConsentEvents} {
			if err := core.RegisterDataMapEntry(txApp, core.BuiltinDataMapEntry(name)); err != nil {
				return err
			}
		}

		return nil
	}, func(txApp core.App) error {
		for _, name := range []string{core.CollectionNameConsentEvents, core.CollectionNameConsents, core.CollectionNameConsentPurposes} {
			col, err := txApp.FindCollectionByNameOrId(name)
			if err != nil {
				continue
			}

			if err := txApp.Delete(col); err != nil {
				return err
			}
		}

		return nil
	})
}
