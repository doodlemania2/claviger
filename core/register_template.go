package core

import (
	"fmt"
	"strings"

	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #87): set-up of a register, and the "sacramental
// register" template.

// DefaultSacramentalRetainReason is the placeholder reason of the template. The
// church has to replace it with the reason that applies to it (see
// docs/registers.md); it is a statement of intent, not legal advice.
const DefaultSacramentalRetainReason = "Sacramental register kept under the church's own register-keeping rules. The controller must confirm the legal basis for the jurisdiction."

// setupRegister runs when a _permanentCollections row is created: it adds the
// managed fields to the register, creates the annotation collection when it
// does not exist, and registers both in the data map as retained.
func setupRegister(app App, cfg *PermanentConfig) error {
	reg, err := app.FindCollectionByNameOrId(cfg.Collection)
	if err != nil {
		return err
	}

	if err := ensureRegisterFields(app, reg, cfg); err != nil {
		return err
	}

	ann, err := app.FindCollectionByNameOrId(cfg.AnnotationCollection)
	if err != nil {
		if ann, err = createAnnotationCollection(app, reg, cfg); err != nil {
			return err
		}
	}

	personal := append([]string{}, cfg.PersonalFields...)
	if err := registerDataMapRetain(app, cfg.Collection, cfg.SubjectPaths, personal, cfg.RetainReason,
		"A permanent register (#87). Final entries are never deleted or changed; an erasure request is recorded as a data_subject_note annotation."); err != nil {
		return err
	}

	paths := []string{"author"}
	if ann.Fields.GetByName("subject") != nil {
		paths = []string{"subject", "author"}
	}

	return registerDataMapRetain(app, ann.Name, paths, []string{"text", "reason", "author"}, cfg.RetainReason,
		"Annotations of a permanent register (#87). Append-only; kept with the entry they belong to.")
}

func ensureRegisterFields(app App, reg *Collection, cfg *PermanentConfig) error {
	changed := false

	add := func(f Field) {
		if reg.Fields.GetByName(f.GetName()) == nil {
			reg.Fields.Add(f)

			changed = true
		}
	}

	add(&DateField{Name: cfg.FinalizeField})
	add(&TextField{Name: "finalizedBy", Max: 100})
	add(&TextField{Name: "secondSigner", Max: 100})
	add(&TextField{Name: "pendingBy", Max: 100})
	add(&DateField{Name: "pendingAt"})
	add(&TextField{Name: "pendingHash", Max: 64})
	add(&BoolField{Name: "restricted"})

	if cfg.Chain {
		add(&NumberField{Name: "seq", OnlyInt: true, Min: types.Pointer(0.0)})
		add(&TextField{Name: "hash", Max: 64})

		// seq is unique within the chain; drafts have 0
		idx := "idx_" + reg.Name + "_seq"
		if _, ok := indexByName(reg, idx); !ok {
			reg.AddIndex(idx, true, "seq", "`seq` > 0")

			changed = true
		}
	}

	if !changed {
		return nil
	}

	return app.Save(reg)
}

func indexByName(c *Collection, name string) (string, bool) {
	for _, idx := range c.Indexes {
		if strings.Contains(idx, "`"+name+"`") || strings.Contains(idx, "\""+name+"\"") {
			return idx, true
		}
	}

	return "", false
}

// createAnnotationCollection creates the annotation collection of spec 9.5.
// Writes go through AddRegisterAnnotation (the rules are null for writing);
// org staff can read.
func createAnnotationCollection(app App, reg *Collection, cfg *PermanentConfig) (*Collection, error) {
	orgs, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgs)
	if err != nil {
		return nil, err
	}

	col := NewBaseCollection(cfg.AnnotationCollection)
	col.Fields.Add(
		&RelationField{Name: "entry", Required: true, CollectionId: reg.Id, MaxSelect: 1},
		&SelectField{Name: "kind", Required: true, MaxSelect: 1, Values: RegisterAnnotationKinds},
		&TextField{Name: "text", Required: true, Max: registerAnnotationTextMax},
		&TextField{Name: "author", Max: 100},
		&TextField{Name: "reason", Required: true, Max: registerAnnotationReasonMx},
		// copies, filled by the model hook: the org keeps the rules to one hop, the subject the data export
		&RelationField{Name: "org", CollectionId: orgs.Id, MaxSelect: 1},
		&AutodateField{Name: "created", OnCreate: true},
	)

	if userCol, err := OrgUserCollection(app); err == nil {
		col.Fields.Add(&RelationField{Name: "subject", CollectionId: userCol.Id, MaxSelect: 1})
	}

	staff := HouseholdStaffRule("org")
	col.ListRule = &staff
	col.ViewRule = &staff

	col.AddIndex("idx_"+col.Name+"_entry", false, "entry, created", "")

	if err := app.Save(col); err != nil {
		return nil, err
	}

	// the self reference needs the collection id, so it is added in a second step
	col.Fields.Add(&RelationField{Name: "supersedes", CollectionId: col.Id, MaxSelect: 1})

	if err := app.Save(col); err != nil {
		return nil, err
	}

	return col, nil
}

// registerDataMapRetain writes (or updates) the custom _dataMap row of a
// collection with the treatment "retain". A built-in row is left alone.
func registerDataMapRetain(app App, collection string, subjectPaths, personal []string, reason, note string) error {
	col, err := app.FindCachedCollectionByNameOrId(CollectionNameDataMap)
	if err != nil {
		return nil
	}

	rec, err := app.FindFirstRecordByData(col, "collection", collection)
	if err != nil {
		rec = NewRecord(col)
	} else if rec.GetBool("builtin") {
		return nil
	}

	rec.Set("collection", collection)
	rec.Set("treatment", DataMapTreatmentRetain)
	rec.Set("subjectPaths", nonNilStrings(subjectPaths))
	rec.Set("personalFields", nonNilStrings(personal))
	rec.Set("retainReason", reason)
	rec.Set("note", note)
	rec.Set("builtin", false)

	return app.Save(rec)
}

// ---- template ---------------------------------------------------------------

// SacramentalRegisterOpts configures CreateSacramentalRegister.
type SacramentalRegisterOpts struct {
	// Name of the collection, for example "baptisms".
	Name string
	// Signers is 1 (default) or 2.
	Signers int
	// RetainReason defaults to DefaultSacramentalRetainReason.
	RetainReason string
	// NoChain turns the hash chain off (it is on for the template).
	NoChain bool
}

// CreateSacramentalRegister creates a base collection for a sacramental
// register (baptism, confirmation, marriage and similar) and registers it as a
// permanent collection with the hash chain on. The entries carry a plain
// searchable surname; the details (parents, godparents, ...) go in the JSON
// field, which a church may change to an encrypted one before the first entry.
func CreateSacramentalRegister(app App, opts SacramentalRegisterOpts) (*PermanentConfig, error) {
	var cfg *PermanentConfig

	err := app.RunInTransaction(func(txApp App) error {
		var err error

		cfg, err = createSacramentalRegister(txApp, opts)

		return err
	})
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

func createSacramentalRegister(app App, opts SacramentalRegisterOpts) (*PermanentConfig, error) {
	if !registerNameRegex.MatchString(opts.Name) {
		return nil, fmt.Errorf("invalid collection name %q", opts.Name)
	}

	if opts.Signers == 0 {
		opts.Signers = 1
	}

	if opts.RetainReason == "" {
		opts.RetainReason = DefaultSacramentalRetainReason
	}

	orgs, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgs)
	if err != nil {
		return nil, err
	}

	col := NewBaseCollection(opts.Name)
	col.Fields.Add(
		&RelationField{Name: "org", Required: true, CollectionId: orgs.Id, MaxSelect: 1},
		&SelectField{Name: "sacrament", Required: true, MaxSelect: 1, Values: []string{"baptism", "confirmation", "marriage", "other"}},
		&TextField{Name: "surname", Required: true, Max: 200},
		&TextField{Name: "givenNames", Max: 300},
		&DateField{Name: "eventDate"},
		&TextField{Name: "eventPlace", Max: 300},
		&TextField{Name: "officiant", Max: 300},
		&TextField{Name: "bookRef", Max: 100}, // volume, page, number
		&JSONField{Name: "details", MaxSize: 20000},
		&AutodateField{Name: "created", OnCreate: true},
		&AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)

	if userCol, err := OrgUserCollection(app); err == nil {
		col.Fields.Add(&RelationField{Name: "subject", CollectionId: userCol.Id, MaxSelect: 1})
	}

	// org staff (owners, admins, registrars) read and write drafts; the model
	// hook decides what is still allowed once an entry is final
	staff := HouseholdStaffRule("org")
	col.ListRule = &staff
	col.ViewRule = &staff
	col.CreateRule = &staff
	col.UpdateRule = &staff
	col.DeleteRule = &staff

	col.AddIndex("idx_"+opts.Name+"_surname", false, "surname", "")

	if err := app.Save(col); err != nil {
		return nil, err
	}

	rows, err := app.FindCachedCollectionByNameOrId(CollectionNamePermanentCollections)
	if err != nil {
		return nil, err
	}

	if col.Fields.GetByName("subject") == nil {
		return nil, fmt.Errorf("the user collection is not available")
	}

	paths := []string{"subject"}

	rec := NewRecord(rows)
	rec.Set("collection", opts.Name)
	rec.Set("chain", !opts.NoChain)
	rec.Set("signers", opts.Signers)
	rec.Set("subjectPaths", paths)
	rec.Set("personalFields", []string{"surname", "givenNames", "eventDate", "eventPlace", "details"})
	rec.Set("retainReason", opts.RetainReason)

	if err := app.Save(rec); err != nil {
		return nil, err
	}

	return permanentConfigFromRecord(rec), nil
}
