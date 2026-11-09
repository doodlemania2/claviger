package core

import (
	"fmt"
	"slices"
	"strings"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/router"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, milestone 88-0): the data map.
//
// The _dataMap system collection holds one row per collection: does it
// contain personal data, which field links a row to a person (the subject
// path), and what happens to the rows when that person asks for an export or
// an erasure. Export and erasure (later milestones) are only correct if every
// collection is listed, so UnmappedCollections reports the ones that are not,
// and a test fails when a system collection has no entry.
//
// Built-in rows (builtin = true) are written by migrations from
// builtinDataMap (data_map_builtin.go). The records API refuses to change or
// delete them. Superusers register their own collections through the records
// API; those rows have builtin = false.
//
// See docs/data-map.md.

const (
	// CollectionNameDataMap is the system collection holding the registry.
	CollectionNameDataMap = "_dataMap"

	// DataMapTreatment values: what happens to the rows of the collection
	// when a person is erased. Everything except "none" is exported.
	DataMapTreatmentExport    = "export"    // exported on request, left alone on erasure
	DataMapTreatmentErase     = "erase"     // exported, then deleted on erasure
	DataMapTreatmentAnonymize = "anonymize" // exported, then the personal fields are blanked
	DataMapTreatmentRetain    = "retain"    // exported, kept on erasure for the stated reason
	DataMapTreatmentNone      = "none"      // reviewed: no personal data
)

// DataMapTreatments lists the allowed treatment values.
var DataMapTreatments = []string{
	DataMapTreatmentExport,
	DataMapTreatmentErase,
	DataMapTreatmentAnonymize,
	DataMapTreatmentRetain,
	DataMapTreatmentNone,
}

// DataMapEntry is the code form of one _dataMap row.
type DataMapEntry struct {
	// Collection is the collection name (not the id, so that the entry can
	// be written by a migration before the id is known to the reader).
	Collection string

	Treatment string

	// SubjectPaths are the fields linking a row to the person it is about
	// ("recordRef", "user", "id" for an auth record, ...). Empty for "none".
	SubjectPaths []string

	// PersonalFields are the fields that hold personal data.
	PersonalFields []string

	// RetainReason is required for the "retain" treatment.
	RetainReason string

	// AgeOutBasis names the date after which the rows age out (optional).
	AgeOutBasis string

	// Note is free text: what the export contains, why a collection is
	// not personal, and so on.
	Note string
}

// RegisterDataMapEntry inserts the entry as a built-in row, or updates the
// built-in row of the same collection. It does nothing when the _dataMap
// collection does not exist yet, so a migration that creates a system
// collection can call it unconditionally. A custom (non built-in) row of the
// same collection is left alone.
func RegisterDataMapEntry(app App, entry DataMapEntry) error {
	col, err := app.FindCachedCollectionByNameOrId(CollectionNameDataMap)
	if err != nil {
		return nil
	}

	record, err := app.FindFirstRecordByData(col, "collection", entry.Collection)
	if err != nil {
		record = NewRecord(col)
	} else if !record.GetBool("builtin") {
		return nil
	}

	record.Set("collection", entry.Collection)
	record.Set("treatment", entry.Treatment)
	record.Set("subjectPaths", nonNilStrings(entry.SubjectPaths))
	record.Set("personalFields", nonNilStrings(entry.PersonalFields))
	record.Set("retainReason", entry.RetainReason)
	record.Set("ageOutBasis", entry.AgeOutBasis)
	record.Set("note", entry.Note)
	record.Set("builtin", true)

	return app.Save(record)
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// SeedBuiltinDataMap registers every entry of builtinDataMap whose
// collection exists. Used by the migration that creates _dataMap.
func SeedBuiltinDataMap(app App) error {
	for _, entry := range builtinDataMap {
		if _, err := app.FindCachedCollectionByNameOrId(entry.Collection); err != nil {
			continue // the collection is not created (yet): nothing to map
		}
		if err := RegisterDataMapEntry(app, entry); err != nil {
			return fmt.Errorf("data map entry %s: %w", entry.Collection, err)
		}
	}
	return nil
}

// UnmappedCollection is a collection without a _dataMap entry.
type UnmappedCollection struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	System bool   `json:"system"`
}

// UnmappedCollections lists the stored (non view) collections that have no
// _dataMap entry, system collections first. A "none" entry counts as mapped:
// it means somebody reviewed the collection.
func UnmappedCollections(app App) ([]UnmappedCollection, error) {
	mapped := map[string]struct{}{}

	var names []string
	err := app.RecordQuery(CollectionNameDataMap).
		Select("collection").
		Column(&names)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		mapped[n] = struct{}{}
	}

	var collections []*Collection
	if err := app.CollectionQuery().OrderBy("name ASC").All(&collections); err != nil {
		return nil, err
	}

	result := []UnmappedCollection{}
	for _, c := range collections {
		if c.IsView() || c.Name == CollectionNameDataMap {
			continue
		}
		if _, ok := mapped[c.Name]; ok {
			continue
		}
		result = append(result, UnmappedCollection{Name: c.Name, Type: c.Type, System: c.System})
	}

	slices.SortStableFunc(result, func(a, b UnmappedCollection) int {
		if a.System != b.System {
			if a.System {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})

	return result, nil
}

// ---------------------------------------------------------------------------
// hooks
// ---------------------------------------------------------------------------

func (app *BaseApp) registerDataMapHooks() {
	app.OnRecordValidate(CollectionNameDataMap).Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbDataMapValidate__",
		Func: func(e *RecordEvent) error {
			if err := validateDataMapRecord(e.App, e.Record); err != nil {
				return err
			}
			return e.Next()
		},
	})

	// built-in rows are written by migrations only
	guard := func(e *RecordRequestEvent) error {
		if e.Record.GetBool("builtin") || (e.Record.Original() != nil && e.Record.Original().GetBool("builtin")) {
			return router.NewForbiddenError("Built-in data map entries are managed by the server.", nil)
		}
		return e.Next()
	}
	for _, bindFn := range []func(tags ...string) *hook.TaggedHook[*RecordRequestEvent]{
		app.OnRecordCreateRequest,
		app.OnRecordUpdateRequest,
		app.OnRecordDeleteRequest,
	} {
		bindFn(CollectionNameDataMap).Bind(&hook.Handler[*RecordRequestEvent]{
			Id:       "__pbDataMapGuard__",
			Func:     guard,
			Priority: -100000,
		})
	}
}

func validateDataMapRecord(app App, record *Record) error {
	errs := validation.Errors{}

	name := record.GetString("collection")
	// _logs (auxiliary database) is not a collection but may be mapped
	if name != "" {
		if _, err := app.FindCachedCollectionByNameOrId(name); err != nil && name != LogsTableName {
			errs["collection"] = validation.NewError("validation_unknown_collection", "No collection with this name exists.")
		}
	}

	treatment := record.GetString("treatment")
	if treatment == DataMapTreatmentRetain && strings.TrimSpace(record.GetString("retainReason")) == "" {
		errs["retainReason"] = validation.NewError("validation_required", "A reason is required when the treatment is retain.")
	}

	if treatment != "" && treatment != DataMapTreatmentNone {
		var paths types.JSONArray[string]
		if err := record.UnmarshalJSONField("subjectPaths", &paths); err != nil || len(paths) == 0 {
			errs["subjectPaths"] = validation.NewError("validation_required", "At least one subject path is required unless the treatment is none.")
		}
	}

	return errs.Filter()
}
