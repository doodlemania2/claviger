package core

import (
	"fmt"
	"slices"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #87, 87-1 and 87-2): the model-layer guard of the
// registers, and the validation and set-up of a _permanentCollections row.

func registerRefuse(format string, args ...any) error {
	return router.NewBadRequestError(fmt.Sprintf(format, args...), nil)
}

func (app *BaseApp) registerPermanentHooks() {
	guardPriority := -90000 // after the audit guards, before everything else

	guard := &hook.Handler[*ModelEvent]{Id: "__clvRegisterGuard__", Func: registerModelGuard, Priority: guardPriority}
	app.OnModelCreate().Bind(guard)
	app.OnModelUpdate().Bind(guard)
	app.OnModelDelete().Bind(guard)

	// again right before the write: a handler that runs after the first check
	// (a Go or JS hook with a later priority) can still change the record
	exec := &hook.Handler[*ModelEvent]{Id: "__clvRegisterGuardExecute__", Func: registerModelGuard, Priority: -guardPriority}
	app.OnModelCreateExecute().Bind(exec)
	app.OnModelUpdateExecute().Bind(exec)
	app.OnModelDeleteExecute().Bind(exec)

	app.OnCollectionUpdate().Bind(&hook.Handler[*CollectionEvent]{
		Id: "__clvRegisterCollectionGuard__", Func: registerCollectionUpdateGuard, Priority: guardPriority,
	})
	app.OnCollectionDelete().Bind(&hook.Handler[*CollectionEvent]{
		Id: "__clvRegisterCollectionDeleteGuard__", Func: registerCollectionDeleteGuard, Priority: guardPriority,
	})

	// a change of the configuration is visible at once
	reset := func(e *ModelEvent) error {
		if re, ok := newRecordEventFromModelEvent(e); ok && re.Record.Collection().Name == CollectionNamePermanentCollections {
			resetPermanentConfigs(e.App)
		}

		return e.Next()
	}
	for _, bind := range []func(tags ...string) *hook.TaggedHook[*ModelEvent]{
		app.OnModelAfterCreateSuccess, app.OnModelAfterUpdateSuccess, app.OnModelAfterDeleteSuccess,
	} {
		bind().Bind(&hook.Handler[*ModelEvent]{Id: "__clvRegisterReset__", Func: reset})
	}

	app.OnRecordValidate(CollectionNamePermanentCollections).Bind(&hook.Handler[*RecordEvent]{
		Id: "__clvRegisterConfigValidate__",
		Func: func(e *RecordEvent) error {
			if err := validatePermanentConfig(e.App, e.Record); err != nil {
				return err
			}

			return e.Next()
		},
	})

	// set-up runs in the same transaction as the insert, so a failure leaves nothing behind
	app.OnRecordCreateExecute(CollectionNamePermanentCollections).Bind(&hook.Handler[*RecordEvent]{
		Id: "__clvRegisterConfigSetup__",
		Func: func(e *RecordEvent) error {
			original := e.App

			err := e.App.RunInTransaction(func(txApp App) error {
				e.App = txApp

				if err := setupRegister(txApp, permanentConfigFromRecord(e.Record)); err != nil {
					return err
				}

				return e.Next()
			})

			e.App = original

			return err
		},
	})
}

// ---- record guard ------------------------------------------------------------

func registerModelGuard(me *ModelEvent) error {
	re, ok := newRecordEventFromModelEvent(me)
	if !ok {
		return me.Next()
	}

	rec := re.Record
	name := rec.Collection().Name

	if name == CollectionNamePermanentCollections {
		return guardPermanentConfig(me, rec)
	}

	cache := permanentConfigs(me.App)

	if len(cache.byRegister) == 0 {
		return me.Next()
	}

	if cfg := cache.byRegister[name]; cfg != nil {
		return guardRegisterEntry(me, cfg, rec)
	}

	if cfg := cache.byAnnotation[name]; cfg != nil {
		return guardAnnotation(me, cfg, rec)
	}

	return me.Next()
}

func registerWriteAllowed(me *ModelEvent) bool {
	return me.Context != nil && me.Context.Value(registerCtxKey{}) != nil
}

func guardRegisterEntry(me *ModelEvent, cfg *PermanentConfig, rec *Record) error {
	switch me.Type {
	case ModelEventTypeCreate:
		if !registerWriteAllowed(me) {
			for _, f := range cfg.Managed() {
				if !registerIsZero(registerFieldValue(rec, f)) {
					return registerRefuse("The field %q is set by finalizing the entry and cannot be written directly.", f)
				}
			}
		}
	case ModelEventTypeUpdate:
		stored, err := me.App.FindRecordById(rec.Collection(), rec.Id)
		if err != nil {
			return me.Next() // not stored (yet): nothing to protect
		}

		changed := registerChangedFields(stored, rec)

		if cfg.IsFinalized(stored) {
			for _, f := range changed {
				if !cfg.isMutable(f) {
					return registerRefuse("This entry is final. The field %q cannot change; add an annotation instead.", f)
				}
			}

			break
		}

		if !registerWriteAllowed(me) {
			for _, f := range changed {
				if cfg.isManaged(f) {
					return registerRefuse("The field %q is set by finalizing the entry and cannot be written directly.", f)
				}
			}
		}
	case ModelEventTypeDelete:
		if stored, err := me.App.FindRecordById(rec.Collection(), rec.Id); err == nil && cfg.IsFinalized(stored) {
			return registerRefuse("This entry is final and cannot be deleted.")
		}
	}

	return me.Next()
}

// ---- annotation guard ----------------------------------------------------------

func guardAnnotation(me *ModelEvent, cfg *PermanentConfig, rec *Record) error {
	if me.Type != ModelEventTypeCreate {
		return registerRefuse("Annotations are permanent: they cannot be changed or deleted. Add a retraction instead.")
	}

	entry, err := validateAnnotationRecord(me.App, cfg, rec)
	if err != nil {
		return err
	}

	// the copies that keep the rules and the data map one hop away
	if rec.Collection().Fields.GetByName("org") != nil {
		rec.Set("org", entry.GetString("org"))
	}

	if rec.Collection().Fields.GetByName("subject") != nil {
		rec.Set("subject", registerSubjectOf(me.App, cfg, entry))
	}

	return me.Next()
}

// registerSubjectOf returns the id of the first single user relation among the
// subject paths of the register, "" when there is none.
func registerSubjectOf(app App, cfg *PermanentConfig, entry *Record) string {
	userCol, err := OrgUserCollection(app)
	if err != nil {
		return ""
	}

	for _, p := range cfg.SubjectPaths {
		if rf, ok := entry.Collection().Fields.GetByName(p).(*RelationField); ok && rf.CollectionId == userCol.Id && !rf.IsMultiple() {
			if id := entry.GetString(p); id != "" {
				return id
			}
		}
	}

	return ""
}

// validateAnnotationRecord checks everything about a new annotation that does
// not depend on who writes it, and returns the entry it belongs to.
func validateAnnotationRecord(app App, cfg *PermanentConfig, rec *Record) (*Record, error) {
	entry, err := app.FindRecordById(cfg.Collection, rec.GetString("entry"))
	if err != nil {
		return nil, registerRefuse("The entry does not exist.")
	}

	if !cfg.IsFinalized(entry) {
		return nil, registerRefuse("Only a final entry can be annotated. Edit the draft instead.")
	}

	kind := rec.GetString("kind")
	if !slices.Contains(RegisterAnnotationKinds, kind) {
		return nil, registerRefuse("Invalid annotation kind.")
	}

	if strings.TrimSpace(rec.GetString("text")) == "" {
		return nil, registerRefuse("The annotation needs a text.")
	}

	if strings.TrimSpace(rec.GetString("reason")) == "" {
		return nil, registerRefuse("The annotation needs a reason.")
	}

	sup := rec.GetString("supersedes")

	if kind == RegisterKindRetraction && sup == "" {
		return nil, registerRefuse("A retraction must say which annotation it retracts.")
	}

	if sup != "" {
		target, err := app.FindRecordById(rec.Collection(), sup)
		if err != nil || target.GetString("entry") != entry.Id {
			return nil, registerRefuse("The annotation to supersede does not belong to this entry.")
		}

		if target.GetString("kind") == RegisterKindRetraction {
			return nil, registerRefuse("A retraction cannot be superseded.")
		}
	}

	return entry, nil
}

// ---- configuration guard -------------------------------------------------------

func registerFinalizedCount(app App, cfg *PermanentConfig) int {
	if !registerNameRegex.MatchString(cfg.FinalizeField) {
		return 1 // refuse rather than guess
	}

	n, err := app.CountRecords(cfg.Collection, dbx.NewExp(fmt.Sprintf("[[%s]] != ''", cfg.FinalizeField)))
	if err != nil {
		return 1
	}

	return int(n)
}

func guardPermanentConfig(me *ModelEvent, rec *Record) error {
	switch me.Type {
	case ModelEventTypeUpdate:
		stored, err := me.App.FindRecordById(rec.Collection(), rec.Id)
		if err != nil {
			return me.Next()
		}

		for _, f := range []string{"collection", "finalizeField", "annotationCollection", "mutableFields", "chain"} {
			if !registerFieldEqual(stored, rec, f) {
				return registerRefuse("The field %q of a register cannot change after it is set up.", f)
			}
		}
	case ModelEventTypeDelete:
		stored, err := me.App.FindRecordById(rec.Collection(), rec.Id)
		if err != nil {
			return me.Next()
		}

		cfg := permanentConfigFromRecord(stored)
		if _, err := me.App.FindCachedCollectionByNameOrId(cfg.Collection); err == nil && registerFinalizedCount(me.App, cfg) > 0 {
			return registerRefuse("This register has final entries and cannot be taken out of permanent mode.")
		}
	}

	return me.Next()
}

// ---- collection guards ---------------------------------------------------------

func registerCollectionUpdateGuard(e *CollectionEvent) error {
	if e.Collection.IsNew() {
		return e.Next()
	}

	cache := permanentConfigs(e.App)
	if len(cache.byRegister) == 0 {
		return e.Next()
	}

	original := &Collection{}
	if err := e.App.ModelQuery(original).Model(e.Collection.Id, original); err != nil {
		return e.Next()
	}

	if cfg := cache.byRegister[original.Name]; cfg != nil {
		if e.Collection.Name != original.Name || e.Collection.Type != original.Type {
			return registerRefuse("The register %q cannot be renamed or change its type.", original.Name)
		}

		strict := registerFinalizedCount(e.App, cfg) > 0

		for _, f := range original.Fields {
			nf := e.Collection.Fields.GetById(f.GetId())

			managed := cfg.isManaged(f.GetName())

			switch {
			case nf == nil && (managed || strict):
				return registerRefuse("The field %q of a register cannot be removed.", f.GetName())
			case nf != nil && (managed || strict) && (nf.GetName() != f.GetName() || nf.Type() != f.Type()):
				return registerRefuse("The field %q of a register cannot be renamed or change its type.", f.GetName())
			}
		}
	}

	if cfg := cache.byAnnotation[original.Name]; cfg != nil {
		if e.Collection.Name != original.Name || e.Collection.Type != original.Type {
			return registerRefuse("The annotation collection %q cannot be renamed or change its type.", original.Name)
		}

		for _, f := range original.Fields {
			nf := e.Collection.Fields.GetById(f.GetId())
			if nf == nil || nf.GetName() != f.GetName() || nf.Type() != f.Type() {
				return registerRefuse("The field %q of an annotation collection cannot be removed, renamed or retyped.", f.GetName())
			}
		}
	}

	return e.Next()
}

func registerCollectionDeleteGuard(e *CollectionEvent) error {
	cache := permanentConfigs(e.App)

	if cache.byRegister[e.Collection.Name] != nil {
		return registerRefuse("The register %q cannot be deleted.", e.Collection.Name)
	}

	if cache.byAnnotation[e.Collection.Name] != nil {
		return registerRefuse("The annotation collection %q cannot be deleted.", e.Collection.Name)
	}

	return e.Next()
}

// ---- validation of a _permanentCollections row ----------------------------------

func validatePermanentConfig(app App, rec *Record) error {
	// defaults first, so that the stored row is complete
	if rec.GetString("finalizeField") == "" {
		rec.Set("finalizeField", DefaultFinalizeField)
	}

	if rec.GetInt("signers") == 0 {
		rec.Set("signers", 1)
	}

	if raw := rec.GetString("mutableFields"); raw == "" || raw == "null" {
		rec.Set("mutableFields", []string{"restricted"})
	}

	if rec.GetString("annotationCollection") == "" && rec.GetString("collection") != "" {
		rec.Set("annotationCollection", rec.GetString("collection")+"_annotations")
	}

	if !rec.IsNew() {
		return nil // the immutable fields are guarded; the rest is free text
	}

	cfg := permanentConfigFromRecord(rec)
	errs := validation.Errors{}
	bad := func(field, code, msg string) { errs[field] = validation.NewError(code, msg) }

	col, err := app.FindCollectionByNameOrId(cfg.Collection)

	switch {
	case cfg.Collection == "":
		bad("collection", "validation_required", "Cannot be blank.")
	case err != nil:
		bad("collection", "validation_unknown_collection", "No collection with this name exists.")
	case !col.IsBase() || col.System || strings.HasPrefix(col.Name, "_"):
		bad("collection", "validation_not_base", "A register must be an ordinary base collection.")
	default:
		validateRegisterShape(app, col, cfg, bad)
	}

	if rf := rec.GetInt("signers"); rf != 1 && rf != 2 {
		bad("signers", "validation_invalid", "Signers must be 1 or 2.")
	}

	if strings.TrimSpace(cfg.RetainReason) == "" {
		bad("retainReason", "validation_required", "The legal reason for keeping the entries is required.")
	}

	if len(cfg.SubjectPaths) == 0 {
		bad("subjectPaths", "validation_required", "At least one subject path is required.")
	} else if col != nil {
		for _, p := range cfg.SubjectPaths {
			if p != "id" && col.Fields.GetByName(p) == nil {
				bad("subjectPaths", "validation_unknown_field", "No field "+p+" in the register.")
				break
			}
		}
	}

	return errs.Filter()
}

func validateRegisterShape(app App, col *Collection, cfg *PermanentConfig, bad func(field, code, msg string)) {
	orgs, _ := app.FindCachedCollectionByNameOrId(CollectionNameOrgs)
	if rf, ok := col.Fields.GetByName("org").(*RelationField); !ok || orgs == nil || rf.CollectionId != orgs.Id || rf.IsMultiple() {
		bad("collection", "validation_no_org", "A register needs a field named org that points at one organization.")
	}

	if !registerNameRegex.MatchString(cfg.FinalizeField) || cfg.FinalizeField == "org" {
		bad("finalizeField", "validation_invalid", "Invalid field name.")
	} else if f := col.Fields.GetByName(cfg.FinalizeField); f != nil && f.Type() != FieldTypeDate {
		bad("finalizeField", "validation_invalid_type", "The finalize field must be a date field.")
	}

	want := map[string]string{
		"finalizedBy": FieldTypeText, "secondSigner": FieldTypeText, "pendingBy": FieldTypeText, "pendingHash": FieldTypeText,
		"pendingAt": FieldTypeDate, "restricted": FieldTypeBool,
	}
	if cfg.Chain {
		want["seq"] = FieldTypeNumber
		want["hash"] = FieldTypeText
	}

	for name, typ := range want {
		if f := col.Fields.GetByName(name); f != nil && f.Type() != typ {
			bad("collection", "validation_invalid_type", fmt.Sprintf("The field %s must be of type %s.", name, typ))
		}
	}

	for _, name := range cfg.MutableFields {
		if cfg.isManaged(name) || name == "id" || name == "org" {
			bad("mutableFields", "validation_invalid", name+" cannot be a mutable field.")
		} else if col.Fields.GetByName(name) == nil && name != "restricted" {
			bad("mutableFields", "validation_unknown_field", "No field "+name+" in the register.")
		}
	}

	if !registerNameRegex.MatchString(cfg.AnnotationCollection) || cfg.AnnotationCollection == col.Name {
		bad("annotationCollection", "validation_invalid", "Invalid collection name.")

		return
	}

	if ann, err := app.FindCollectionByNameOrId(cfg.AnnotationCollection); err == nil {
		if !ann.IsBase() {
			bad("annotationCollection", "validation_not_base", "The annotation collection must be a base collection.")
		}

		for _, name := range []string{"entry", "kind", "text", "author", "reason", "supersedes"} {
			if ann.Fields.GetByName(name) == nil {
				bad("annotationCollection", "validation_missing_field", "The annotation collection needs a field "+name+".")
			}
		}

		if rf, ok := ann.Fields.GetByName("entry").(*RelationField); ok && rf.CollectionId != col.Id {
			bad("annotationCollection", "validation_invalid", "The entry field must point at the register.")
		}
	}
}
