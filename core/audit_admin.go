package core

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #110): audit entries for admin actions, namely
// settings changes and collection schema changes.
//
// Both are captured at the model execute level (so the entry joins the same
// transaction as the change and covers the API, the schema import, Go hooks
// and JS hooks alike). The actor and IP come from WithAuditContext when the
// change was initiated over the API; anything else is attributed to "system".
//
// Action names:
//   - settings.update (target "_params", id "settings")
//   - collection.create, collection.update, collection.delete (target "_collections",
//     meta.import is true when the change came from a collections import)

const (
	AuditActionSettingsUpdate   = "settings.update"
	AuditActionCollectionCreate = "collection.create"
	AuditActionCollectionUpdate = "collection.update"
	AuditActionCollectionDelete = "collection.delete"

	// CollectionNameCollections is the audit target name of collection schema entries.
	auditTargetCollections = "_collections"
	auditTargetSettings    = paramsTable
)

type auditImportCtxKey struct{}

func (app *BaseApp) registerAdminAuditHooks() {
	app.OnModelUpdateExecute().Bind(&hook.Handler[*ModelEvent]{
		Id: "__pbAuditSettings__",
		Func: func(e *ModelEvent) error {
			settings, ok := e.Model.(*Settings)
			if !ok {
				return e.Next()
			}

			return auditInTx(e.App, func(txApp App) error {
				e.App = txApp
				return auditSettingsUpdate(e, settings)
			}, func(a App) { e.App = a })
		},
		Priority: 100,
	})

	app.OnCollectionCreateExecute().Bind(&hook.Handler[*CollectionEvent]{
		Id:       "__pbAuditCollectionCreate__",
		Func:     func(e *CollectionEvent) error { return auditCollectionChange(e, AuditActionCollectionCreate) },
		Priority: 100,
	})
	app.OnCollectionUpdateExecute().Bind(&hook.Handler[*CollectionEvent]{
		Id:       "__pbAuditCollectionUpdate__",
		Func:     func(e *CollectionEvent) error { return auditCollectionChange(e, AuditActionCollectionUpdate) },
		Priority: 100,
	})
	app.OnCollectionDeleteExecute().Bind(&hook.Handler[*CollectionEvent]{
		Id:       "__pbAuditCollectionDelete__",
		Func:     func(e *CollectionEvent) error { return auditCollectionChange(e, AuditActionCollectionDelete) },
		Priority: 100,
	})
}

// auditInTx runs fn inside a transaction (reusing the current one if any),
// so that the change and its entry are atomic. restore resets the event app.
func auditInTx(app App, fn func(txApp App) error, restore func(App)) error {
	if app.IsTransactional() {
		return fn(app)
	}

	err := app.RunInTransaction(fn)
	restore(app)

	return err
}

// applyAuditContext fills the entry actor/ip from the context (system if none).
func applyAuditContext(ctx context.Context, entry *AuditEntry) {
	if ctx == nil {
		ctx = context.Background()
	}

	if v, _ := ctx.Value(auditCtxKey{}).(auditCtxValue); v.actor.Type != "" {
		entry.Actor = v.actor
		entry.IP = v.ip
		if v.actor.APIKeyId != "" {
			entry.Meta["apiKeyId"] = v.actor.APIKeyId
		}
		if v.actor.OrgId != "" {
			entry.Meta["orgId"] = v.actor.OrgId
		}
		resolveSelfAuditActor(entry, nil)
	} else {
		entry.Actor = AuditActor{Type: AuditActorTypeSystem}
	}
}

func auditAvailable(app App) bool {
	_, err := app.FindCachedCollectionByNameOrId(CollectionNameAuditLog)
	return err == nil
}

// -------------------------------------------------------------------
// settings

func auditSettingsUpdate(e *ModelEvent, updated *Settings) error {
	if updated.IsNew() || !auditAvailable(e.App) {
		return e.Next()
	}

	// the stored state, not the cached one, so that in-place edits of
	// App.Settings() before the save are still diffed
	old := newDefaultSettings()
	param := &Param{}
	if err := e.App.ModelQuery(param).Model(paramsKeySettings, param); err == nil {
		if err := old.loadParam(e.App, param); err != nil {
			old = nil
		}
	} else {
		old = nil
	}

	var entry *AuditEntry
	if old != nil {
		entry = buildSettingsAuditEntry(old, updated)
	}

	if entry != nil {
		applyAuditContext(e.Context, entry)
	}

	if err := e.Next(); err != nil {
		return err
	}

	if entry == nil {
		return nil
	}

	return WriteAuditEntry(e.App, e.Context, entry, nil)
}

// buildSettingsAuditEntry returns the settings.update entry, or nil if nothing changed.
//
// Changed keys are flattened JSON paths (e.g. "smtp.host"). Secrets (the ones
// Settings.MarshalJSON masks) are compared by their real value but only ever
// stored as "[redacted]".
func buildSettingsAuditEntry(old, updated *Settings) *AuditEntry {
	oldFlat, oldSecrets := old.auditSnapshot()
	newFlat, newSecrets := updated.auditSnapshot()

	entry := &AuditEntry{
		Action:         AuditActionSettingsUpdate,
		CollectionId:   "",
		CollectionName: auditTargetSettings,
		RecordId:       paramsKeySettings,
		ChangedFields:  []string{},
		Before:         map[string]any{},
		After:          map[string]any{},
		Meta:           map[string]any{},
	}

	keys := map[string]struct{}{}
	for k := range oldFlat {
		keys[k] = struct{}{}
	}
	for k := range newFlat {
		keys[k] = struct{}{}
	}
	for k := range oldSecrets {
		keys[k] = struct{}{}
	}
	for k := range newSecrets {
		keys[k] = struct{}{}
	}

	for _, k := range slices.Sorted(maps.Keys(keys)) {
		if _, isSecret := oldSecrets[k]; isSecret {
			if oldSecrets[k] == newSecrets[k] {
				continue
			}
			entry.ChangedFields = append(entry.ChangedFields, k)
			entry.Before[k] = auditSafe(oldSecrets[k], true)
			entry.After[k] = auditSafe(newSecrets[k], true)
			continue
		}

		if reflect.DeepEqual(oldFlat[k], newFlat[k]) {
			continue
		}
		entry.ChangedFields = append(entry.ChangedFields, k)
		entry.Before[k] = auditSafe(oldFlat[k], false)
		entry.After[k] = auditSafe(newFlat[k], false)
	}

	if len(entry.ChangedFields) == 0 {
		return nil
	}

	return entry
}

// auditSnapshot returns the flattened (masked) JSON view of the settings and,
// separately, the real values of the secret paths.
func (s *Settings) auditSnapshot() (flat map[string]any, secrets map[string]string) {
	raw, err := s.MarshalJSON()
	if err != nil {
		return map[string]any{}, map[string]string{}
	}

	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return map[string]any{}, map[string]string{}
	}

	flat = map[string]any{}
	auditFlatten("", tree, flat)

	s.mu.RLock()
	copy := s.settings
	s.mu.RUnlock()

	secrets = map[string]string{}
	for path, v := range copy.secretFields() {
		secrets[path] = *v
		delete(flat, path) // the masked blank, superseded
	}

	return flat, secrets
}

// auditFlatten flattens nested objects into dotted paths (arrays are leaves).
func auditFlatten(prefix string, v any, out map[string]any) {
	m, ok := v.(map[string]any)
	if !ok {
		out[prefix] = v
		return
	}

	for k, child := range m {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		auditFlatten(path, child, out)
	}
}

// -------------------------------------------------------------------
// collections

func auditCollectionChange(e *CollectionEvent, action string) error {
	c := e.Collection

	if c.Name == CollectionNameAuditLog || !auditAvailable(e.App) {
		return e.Next()
	}

	run := func(txApp App) error {
		e.App = txApp

		var original *Collection
		if action == AuditActionCollectionUpdate {
			stored := &Collection{}
			if err := txApp.ModelQuery(stored).Model(c.Id, stored); err == nil {
				original = stored
			}
		}

		entry := buildCollectionAuditEntry(c, original, action)
		if entry != nil {
			applyAuditContext(e.Context, entry)
			if e.Context != nil && e.Context.Value(auditImportCtxKey{}) != nil {
				entry.Meta["import"] = true
			}
		}

		if err := e.Next(); err != nil {
			return err
		}

		if entry == nil {
			return nil
		}

		return WriteAuditEntry(txApp, e.Context, entry, nil)
	}

	return auditInTx(e.App, run, func(a App) { e.App = a })
}

func buildCollectionAuditEntry(c, original *Collection, action string) *AuditEntry {
	entry := &AuditEntry{
		Action:         action,
		CollectionId:   c.Id,
		CollectionName: auditTargetCollections,
		RecordId:       c.Id,
		ChangedFields:  []string{},
		Before:         map[string]any{},
		After:          map[string]any{},
		Meta:           map[string]any{"collectionName": c.Name, "collectionType": c.Type},
	}

	switch action {
	case AuditActionCollectionCreate:
		entry.Before = nil
		entry.After = auditCollectionTree(c)
		entry.ChangedFields = slices.Sorted(maps.Keys(entry.After))
		entry.Meta["schema"] = auditFieldSummary(c.Fields)
	case AuditActionCollectionDelete:
		entry.After = nil
		entry.Before = auditCollectionTree(c)
		entry.ChangedFields = slices.Sorted(maps.Keys(entry.Before))
		entry.Meta["schema"] = auditFieldSummary(c.Fields)
	default:
		if original == nil {
			return nil
		}

		oldTree, newTree := auditCollectionTree(original), auditCollectionTree(c)
		// secrets are masked in the tree, so compare the raw client secrets too
		if auditOAuthSecretsSig(original) != auditOAuthSecretsSig(c) {
			entry.Meta["oauth2SecretChanged"] = true
		}

		for _, k := range slices.Sorted(maps.Keys(auditUnion(oldTree, newTree))) {
			if reflect.DeepEqual(oldTree[k], newTree[k]) {
				continue
			}
			entry.ChangedFields = append(entry.ChangedFields, k)
			entry.Before[k] = oldTree[k]
			entry.After[k] = newTree[k]
		}

		if v, ok := entry.Meta["oauth2SecretChanged"]; ok && v == true && !slices.Contains(entry.ChangedFields, "oauth2") {
			entry.ChangedFields = append(entry.ChangedFields, "oauth2")
			entry.Before["oauth2"] = newTree["oauth2"]
			entry.After["oauth2"] = newTree["oauth2"]
		}

		if diff := auditDiffFields(original.Fields, c.Fields); diff != nil {
			entry.Meta["fields"] = diff
			if !slices.Contains(entry.ChangedFields, "fields") {
				entry.ChangedFields = append(entry.ChangedFields, "fields")
				entry.Before["fields"] = oldTree["fields"]
				entry.After["fields"] = newTree["fields"]
			}
		}
		if added, removed := auditDiffStrings(original.Indexes, c.Indexes); len(added)+len(removed) > 0 {
			entry.Meta["indexes"] = map[string]any{"added": added, "removed": removed}
		}

		if len(entry.ChangedFields) == 0 {
			return nil
		}
	}

	return entry
}

func auditUnion(a, b map[string]any) map[string]any {
	out := maps.Clone(a)
	maps.Copy(out, b)
	return out
}

// auditCollectionTree returns the redacted top-level JSON view of the
// collection (without volatile or bulky members). Fields are reduced to
// name/type pairs; their detailed diff lives in meta.fields.
func auditCollectionTree(c *Collection) map[string]any {
	raw, err := c.MarshalJSON()
	if err != nil {
		return map[string]any{}
	}

	tree := map[string]any{}
	if err := json.Unmarshal(raw, &tree); err != nil {
		return map[string]any{}
	}

	for _, k := range []string{"id", "created", "updated"} {
		delete(tree, k)
	}

	tree["fields"] = auditFieldSummary(c.Fields)

	return auditRedactTree(tree).(map[string]any)
}

// auditRedactTree masks every non-empty string leaf stored under a sensitive
// key name (OAuth2 clientSecret, token secrets, ...).
func auditRedactTree(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if s, ok := child.(string); ok {
				if s != "" && auditSensitiveNameRegex.MatchString(k) {
					t[k] = auditRedactedValue
				}
				continue
			}
			t[k] = auditRedactTree(child)
		}
		return t
	case []any:
		for i := range t {
			t[i] = auditRedactTree(t[i])
		}
		return t
	default:
		return v
	}
}

func auditOAuthSecretsSig(c *Collection) string {
	if !c.IsAuth() {
		return ""
	}

	var b strings.Builder
	for _, p := range c.OAuth2.Providers {
		fmt.Fprintf(&b, "%s=%s;", p.Name, p.ClientSecret)
	}

	return b.String()
}

func auditFieldSummary(fields FieldsList) []map[string]any {
	out := make([]map[string]any, 0, len(fields))
	for _, f := range fields {
		out = append(out, map[string]any{"name": f.GetName(), "type": f.Type()})
	}

	return out
}

func auditFieldMap(f Field) map[string]any {
	raw, err := json.Marshal(f)
	if err != nil {
		return nil
	}

	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}

	delete(m, "id")

	return m
}

// auditDiffFields compares two field lists by field id.
func auditDiffFields(oldFields, newFields FieldsList) map[string]any {
	added, removed := []map[string]any{}, []map[string]any{}
	renamed, typeChanged, modified := []map[string]any{}, []map[string]any{}, []map[string]any{}

	for _, nf := range newFields {
		of := oldFields.GetById(nf.GetId())
		if of == nil {
			added = append(added, map[string]any{"name": nf.GetName(), "type": nf.Type()})
			continue
		}

		if of.GetName() != nf.GetName() {
			renamed = append(renamed, map[string]any{"from": of.GetName(), "to": nf.GetName()})
		}

		if of.Type() != nf.Type() {
			typeChanged = append(typeChanged, map[string]any{"name": nf.GetName(), "from": of.Type(), "to": nf.Type()})
			continue
		}

		om, nm := auditFieldMap(of), auditFieldMap(nf)
		props := []string{}
		for _, k := range slices.Sorted(maps.Keys(auditUnion(om, nm))) {
			if k == "name" {
				continue
			}
			if !reflect.DeepEqual(om[k], nm[k]) {
				props = append(props, k)
			}
		}
		if len(props) > 0 {
			modified = append(modified, map[string]any{"name": nf.GetName(), "properties": props})
		}
	}

	for _, of := range oldFields {
		if newFields.GetById(of.GetId()) == nil {
			removed = append(removed, map[string]any{"name": of.GetName(), "type": of.Type()})
		}
	}

	if len(added)+len(removed)+len(renamed)+len(typeChanged)+len(modified) == 0 {
		return nil
	}

	return map[string]any{
		"added": added, "removed": removed, "renamed": renamed,
		"typeChanged": typeChanged, "modified": modified,
	}
}

func auditDiffStrings(oldList, newList []string) (added, removed []string) {
	added, removed = []string{}, []string{}
	for _, s := range newList {
		if !slices.Contains(oldList, s) {
			added = append(added, s)
		}
	}
	for _, s := range oldList {
		if !slices.Contains(newList, s) {
			removed = append(removed, s)
		}
	}

	return
}
