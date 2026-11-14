package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #74): append-only audit log of record changes.
//
// Entries are stored in the hidden system collection _auditLog. The only
// writer is WriteAuditEntry (reached from the record hooks below, or from
// future callers such as the admin-action audit); every other attempt to
// create, update or delete an entry is rejected at the model-hook level, so
// the guarantee holds for the records API, Go hooks and JS hooks alike.
// Direct SQL (the superuser SQL endpoint, $dbx) is outside that guarantee.

const (
	// CollectionNameAuditLog is the hidden system collection holding the audit entries.
	CollectionNameAuditLog = "_auditLog"

	AuditActionCreate = "create"
	AuditActionUpdate = "update"
	AuditActionDelete = "delete"

	AuditActorTypeSuperuser = "superuser"
	AuditActorTypeRecord    = "record"
	AuditActorTypeSystem    = "system"
	AuditActorTypeSCIM      = "scim" // #79c: a SCIM token, the actor id is "scim:<tokenId>"

	// AuditRetentionDaysEnv is the environment variable that overrides the
	// retention in days from Settings.Audit.RetentionDays. When it is set to a
	// valid non-negative number it wins (0 = keep forever); unset, empty or
	// invalid means "use the setting".
	AuditRetentionDaysEnv = "CLAVIGER_AUDIT_RETENTION_DAYS"

	auditRedactedValue   = "[redacted]"
	auditMaxValueLength  = 4096
	auditCleanupCronId   = "__pbAuditCleanup__"
	auditCleanupCronExpr = "23 3 * * *" // daily at 03:23 (off :00 and away from :07/:37, see #60)
)

// auditExcludedCollections are never audited:
//   - _auditLog itself (would recurse);
//   - _mfas, _otps, _authOrigins: per-login ephemeral rows, high churn and
//     no information beyond the auth event itself (auth events are not record changes).
//
// _externalAuths (account linking) and _webauthnCredentials (passkey add/remove)
// are security relevant and low churn, so they are audited. So is _totps (TOTP
// enrollment, disable, backup code use): all its fields are hidden, hence redacted.
var auditExcludedCollections = []string{
	CollectionNameAuditLog,
	CollectionNameMFAs,
	CollectionNameOTPs,
	CollectionNameAuthOrigins,
	CollectionNameWebhookDeliveries, // #77: bookkeeping rows of the webhook outbox
	CollectionNameOrgClosure,        // #84: derived table; a re-parent writes one org_reparent entry
	CollectionNameDataRequests,      // #88: the gdpr_export* entries describe the requests
	CollectionNameRecoveryCodes,     // #83: explicit recovery_* entries; no hash in the log
	CollectionNameOrgTransfers,      // #84: explicit org_transfer_* entries
	CollectionNameBreakGlass,        // #86: explicit breakglass_* entries (a read raises readCount)
	CollectionNamePastoralHolds,     // #86: explicit pastoral_* entries
}

// auditNoiseFields are fields whose changes alone do not warrant an entry
// (they change on every login).
var auditNoiseFields = map[string][]string{
	CollectionNameWebAuthnCredentials: {"signCount", "lastUsed"},
	CollectionNameAPIKeys:             {"lastUsed"},
	// replay guard, moves on every successful TOTP sign-in
	CollectionNameTOTPs: {"lastStep"},
}

var auditSensitiveNameRegex = regexp.MustCompile(`(?i)(password|passwd|passphrase|secret|token|api[_-]?key|private[_-]?key|credential)`)

type auditCtxKey struct{}
type auditWriterCtxKey struct{}
type auditSchemaCtxKey struct{}

// WithAuditSchemaChange returns a copy of ctx that lets a change of the
// _auditLog collection definition (fields, indexes, rules) through its
// update guard (#110). It is meant for fork migrations that evolve the audit
// schema:
//
//	txApp.SaveWithContext(core.WithAuditSchemaChange(context.Background()), auditCollection)
//
// It does not allow deleting the collection, and it does not allow writing,
// changing or deleting entries (the record guards ignore it).
func WithAuditSchemaChange(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	return context.WithValue(ctx, auditSchemaCtxKey{}, true)
}

type auditCtxValue struct {
	actor AuditActor
	ip    string
}

// AuditActor identifies who performed an audited change.
type AuditActor struct {
	// Type is one of the AuditActorType* constants.
	Type string `json:"type"`

	// Collection and Id are set for auth records and superusers.
	Collection string `json:"collection"`
	Id         string `json:"id"`

	// APIKeyId is set when the request was authenticated with an API key
	// (#95). It is written to the entry's meta.apiKeyId.
	APIKeyId string `json:"apiKeyId,omitempty"`

	// OrgId is set for actors that act on behalf of one organization (a SCIM
	// token, #79c). It is written to the entry's meta.orgId.
	OrgId string `json:"orgId,omitempty"`

	// Self is set when the record acted on itself without an authenticated
	// session (password reset confirm, sign-up, ...; #110). It is written to
	// the entry's meta.self.
	Self bool `json:"self,omitempty"`
}

// NewAuditActorFromRecord returns the actor for the provided auth record
// (nil means the "system" actor).
func NewAuditActorFromRecord(authRecord *Record) AuditActor {
	if authRecord == nil {
		return AuditActor{Type: AuditActorTypeSystem}
	}

	t := AuditActorTypeRecord
	if authRecord.Collection().Name == CollectionNameSuperusers {
		t = AuditActorTypeSuperuser
	}

	return AuditActor{Type: t, Collection: authRecord.Collection().Name, Id: authRecord.Id}
}

// WithAuditContext returns a copy of ctx carrying the actor and request IP
// that will be attributed to audited changes saved with it
// (e.g. app.SaveWithContext(ctx, record)).
func WithAuditContext(ctx context.Context, actor AuditActor, ip string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	return context.WithValue(ctx, auditCtxKey{}, auditCtxValue{actor: actor, ip: ip})
}

// AuditEntry is a single audit log entry.
//
// Record changes fill CollectionId/CollectionName/RecordId. Other kinds of
// entries (the planned admin actions: settings, schema, sessions, API keys...)
// reuse the same shape with their own Action and CollectionName
// (e.g. "_collections" / "_params") and put the details in Meta.
type AuditEntry struct {
	Action         string         `json:"action"`
	Actor          AuditActor     `json:"actor"`
	CollectionId   string         `json:"collectionId"`
	CollectionName string         `json:"collectionName"`
	RecordId       string         `json:"recordId"`
	ChangedFields  []string       `json:"changedFields"`
	Before         map[string]any `json:"before"`
	After          map[string]any `json:"after"`
	IP             string         `json:"ip"`
	Meta           map[string]any `json:"meta"`
}

// AuditEntryEvent is the event of the OnAuditEntry hook.
type AuditEntryEvent struct {
	hook.Event

	App     App
	Context context.Context

	// Entry is the entry that is about to be written; handlers may modify it.
	Entry *AuditEntry

	// Record is the record that was changed (nil for non-record entries).
	Record *Record
}

// -------------------------------------------------------------------

// WriteAuditEntry triggers the OnAuditEntry hook and, unless a handler
// skipped it (by not calling e.Next()), persists the entry.
//
// If app is transactional the entry is part of the same transaction.
func WriteAuditEntry(app App, ctx context.Context, entry *AuditEntry, record *Record) error {
	if ctx == nil {
		ctx = context.Background()
	}

	event := &AuditEntryEvent{App: app, Context: ctx, Entry: entry, Record: record}

	return app.OnAuditEntry().Trigger(event, func(e *AuditEntryEvent) error {
		return saveAuditEntry(e.App, e.Context, e.Entry)
	})
}

func saveAuditEntry(app App, ctx context.Context, entry *AuditEntry) error {
	collection, err := app.FindCachedCollectionByNameOrId(CollectionNameAuditLog)
	if err != nil {
		return fmt.Errorf("audit log collection is not available: %w", err)
	}

	record := NewRecord(collection)
	record.Id = GenerateDefaultRandomId()
	record.SetRaw("created", types.NowDateTime()) // autodate fields ignore Set()
	record.Set("action", entry.Action)
	record.Set("actorType", entry.Actor.Type)
	record.Set("actorCollection", entry.Actor.Collection)
	record.Set("actorId", entry.Actor.Id)
	record.Set("targetCollectionId", entry.CollectionId)
	record.Set("targetCollection", entry.CollectionName)
	record.Set("targetId", entry.RecordId)
	record.Set("changedFields", entry.ChangedFields)
	record.Set("before", entry.Before)
	record.Set("after", entry.After)
	record.Set("ip", entry.IP)
	record.Set("meta", entry.Meta)

	// detach from the request cancellation: a half-written change must not
	// be possible just because the client went away
	writeCtx := context.WithValue(context.WithoutCancel(ctx), auditWriterCtxKey{}, true)

	// Saved through a hook-less clone of the (possibly transactional) app: the
	// record hooks are not needed for a record we fully construct here, and
	// skipping them keeps the per-write overhead low and keeps internal
	// writes from re-entering the audit hooks. The clone shares the same db or
	// transaction, so rollbacks still discard the entry.
	return app.UnsafeWithoutHooks().SaveNoValidateWithContext(writeCtx, record)
}

// DeleteExpiredAuditEntries removes the audit entries created before the
// provided time and returns how many were removed.
//
// This is the only deletion path and it is not reachable from the API.
func DeleteExpiredAuditEntries(app App, createdBefore time.Time) (int64, error) {
	formatted := createdBefore.UTC().Format(types.DefaultDateLayout)

	res, err := app.NonconcurrentDB().Delete(
		CollectionNameAuditLog,
		dbx.NewExp("[[created]] < {:date}", dbx.Params{"date": formatted}),
	).Execute()
	if err != nil {
		return 0, err
	}

	return res.RowsAffected()
}

// auditRetentionDays returns the effective retention: the environment
// override if valid, otherwise Settings.Audit.RetentionDays. 0 means forever.
func auditRetentionDays(app App) int {
	if days, err := strconv.Atoi(strings.TrimSpace(os.Getenv(AuditRetentionDaysEnv))); err == nil && days >= 0 {
		return days
	}

	return max(app.Settings().Audit.RetentionDays, 0)
}

// -------------------------------------------------------------------

func (app *BaseApp) registerAuditHooks() {
	// guards (outermost, so they run before every other handler)
	guardPriority := -100000

	guard := func(me *ModelEvent) error {
		re, ok := newRecordEventFromModelEvent(me)
		if !ok || re.Record.Collection().Name != CollectionNameAuditLog {
			return me.Next()
		}

		if me.Type == ModelEventTypeCreate && me.Context != nil && me.Context.Value(auditWriterCtxKey{}) != nil {
			return me.Next()
		}

		return fmt.Errorf("the %s collection is append-only: %s is not allowed", CollectionNameAuditLog, me.Type)
	}

	guardHandler := func() *hook.Handler[*ModelEvent] {
		return &hook.Handler[*ModelEvent]{Id: "__pbAuditGuard__", Func: guard, Priority: guardPriority}
	}
	app.OnModelCreate().Bind(guardHandler())
	app.OnModelUpdate().Bind(guardHandler())
	app.OnModelDelete().Bind(guardHandler())

	app.OnCollectionUpdate().Bind(&hook.Handler[*CollectionEvent]{
		Id: "__pbAuditCollectionGuard__",
		Func: func(e *CollectionEvent) error {
			if e.Collection.IsNew() || (e.Context != nil && e.Context.Value(auditSchemaCtxKey{}) != nil) {
				return e.Next()
			}

			// match by the stored definition so that renames are caught too
			original := &Collection{}
			if err := e.App.ModelQuery(original).Model(e.Collection.Id, original); err != nil || original.Name != CollectionNameAuditLog {
				return e.Next()
			}

			if auditCollectionSignature(original) != auditCollectionSignature(e.Collection) {
				return fmt.Errorf("the %s collection is read-only and its definition cannot be changed", CollectionNameAuditLog)
			}

			return e.Next()
		},
		Priority: guardPriority,
	})

	app.OnCollectionDelete().Bind(&hook.Handler[*CollectionEvent]{
		Id: "__pbAuditCollectionDeleteGuard__",
		Func: func(e *CollectionEvent) error {
			if e.Collection.Name == CollectionNameAuditLog {
				return fmt.Errorf("the %s collection cannot be deleted", CollectionNameAuditLog)
			}
			return e.Next()
		},
		Priority: guardPriority,
	})

	// record change capture
	// (inside the system execute handlers so that, for deletes, the
	// cascade transaction is already active)
	app.OnRecordCreateExecute().Bind(&hook.Handler[*RecordEvent]{
		Id:       "__pbAuditCreate__",
		Func:     func(e *RecordEvent) error { return auditRecordChange(e, AuditActionCreate) },
		Priority: 100,
	})
	app.OnRecordUpdateExecute().Bind(&hook.Handler[*RecordEvent]{
		Id:       "__pbAuditUpdate__",
		Func:     func(e *RecordEvent) error { return auditRecordChange(e, AuditActionUpdate) },
		Priority: 100,
	})
	app.OnRecordDeleteExecute().Bind(&hook.Handler[*RecordEvent]{
		Id:       "__pbAuditDelete__",
		Func:     func(e *RecordEvent) error { return auditRecordChange(e, AuditActionDelete) },
		Priority: 100,
	})

	// admin actions: settings and schema changes (#110)
	app.registerAdminAuditHooks()

	// retention
	app.Cron().Add(auditCleanupCronId, auditCleanupCronExpr, func() {
		days := auditRetentionDays(app)
		if days == 0 {
			return // keep forever
		}

		if _, err := DeleteExpiredAuditEntries(app, time.Now().AddDate(0, 0, -days)); err != nil {
			app.Logger().Warn("Failed to delete expired audit log entries", "error", err)
		}
	})
}

// auditCollectionSignature returns a comparable representation of everything
// that defines the collection except its timestamps.
func auditCollectionSignature(c *Collection) string {
	sig := struct {
		Name, Type              string
		System                  bool
		Fields                  FieldsList
		Indexes                 types.JSONArray[string]
		List, View, Create, Upd *string
		Delete                  *string
		Options                 string
	}{
		Name: c.Name, Type: c.Type, System: c.System, Fields: c.Fields, Indexes: c.Indexes,
		List: c.ListRule, View: c.ViewRule, Create: c.CreateRule, Upd: c.UpdateRule, Delete: c.DeleteRule,
	}

	raw, _ := json.Marshal(sig)

	return string(raw)
}

func auditRecordChange(e *RecordEvent, action string) error {
	collection := e.Record.Collection()

	if slices.Contains(auditExcludedCollections, collection.Name) {
		return e.Next()
	}

	// not available yet (e.g. while the system migrations are running)
	if _, err := e.App.FindCachedCollectionByNameOrId(CollectionNameAuditLog); err != nil {
		return e.Next()
	}

	// make the change and its entry atomic
	if e.App.IsTransactional() {
		return auditNextAndWrite(e, action)
	}

	originalApp := e.App
	txErr := e.App.RunInTransaction(func(txApp App) error {
		e.App = txApp
		return auditNextAndWrite(e, action)
	})
	e.App = originalApp

	return txErr
}

func auditNextAndWrite(e *RecordEvent, action string) error {
	// For updates the "before" state is read from the db right before the
	// write: Record.Original() only reflects the state at load time, which is
	// stale for a record instance that is saved more than once.
	var original *Record
	if action == AuditActionUpdate {
		if stored, err := e.App.FindRecordById(e.Record.Collection(), e.Record.Id); err == nil {
			original = stored
		} else {
			original = e.Record.Original()
		}
	}

	entry, ok := buildRecordAuditEntry(e.Record, original, action)
	if !ok {
		return e.Next()
	}

	auditOrgMeta(entry, e.Record)              // #79a: meta.orgId / meta.orgIds
	auditHouseholdMeta(e.App, entry, e.Record) // #80: the org of a household member

	if v, _ := e.Context.Value(auditCtxKey{}).(auditCtxValue); v.actor.Type != "" {
		entry.Actor = v.actor
		entry.IP = v.ip
		if v.actor.APIKeyId != "" {
			entry.Meta["apiKeyId"] = v.actor.APIKeyId
		}
		if v.actor.OrgId != "" {
			entry.Meta["orgId"] = v.actor.OrgId
		}
		resolveSelfAuditActor(entry, e.Record)
	} else {
		entry.Actor = AuditActor{Type: AuditActorTypeSystem}
	}

	if err := e.Next(); err != nil {
		return err
	}

	return WriteAuditEntry(e.App, e.Context, entry, e.Record)
}

// buildRecordAuditEntry computes the entry for the record change (without actor).
//
// It must be called before the record is persisted; original is the stored
// state (updates only). It returns false if there is nothing to record.
func buildRecordAuditEntry(record, original *Record, action string) (*AuditEntry, bool) {
	collection := record.Collection()
	noise := auditNoiseFields[collection.Name]

	entry := &AuditEntry{
		Action:         action,
		CollectionId:   collection.Id,
		CollectionName: collection.Name,
		RecordId:       record.Id,
		ChangedFields:  []string{},
		Before:         map[string]any{},
		After:          map[string]any{},
		Meta:           map[string]any{},
	}

	for _, f := range collection.Fields {
		name := f.GetName()

		if name == FieldNameId || slices.Contains(noise, name) {
			continue
		}
		if _, isAutodate := f.(*AutodateField); isAutodate {
			continue
		}

		redact := f.GetHidden() || auditSensitiveNameRegex.MatchString(name)
		if _, isPassword := f.(*PasswordField); isPassword || IsEncryptedFieldType(f.Type()) {
			redact = true // encrypted fields are redacted by type (#75), whatever their name
		}

		var oldRaw, newRaw any
		switch action {
		case AuditActionCreate:
			newRaw = auditFieldValue(record, f)
		case AuditActionDelete:
			oldRaw = auditFieldValue(record, f)
		default:
			oldRaw = auditFieldValue(original, f)
			newRaw = auditFieldValue(record, f)
		}

		if action == AuditActionUpdate && auditEqual(oldRaw, newRaw) {
			continue
		}

		entry.ChangedFields = append(entry.ChangedFields, name)

		if action != AuditActionCreate {
			entry.Before[name] = auditSafe(oldRaw, redact)
		}
		if action != AuditActionDelete {
			entry.After[name] = auditSafe(newRaw, redact)
		}
	}

	if action == AuditActionUpdate && len(entry.ChangedFields) == 0 {
		return nil, false // no-op or noise-only save
	}

	if action == AuditActionCreate {
		entry.Before = nil
	}
	if action == AuditActionDelete {
		entry.After = nil
	}

	return entry, true
}

// auditFieldValue returns a comparable value for the field. Password fields
// are represented by their hash (the plain value never leaves this function).
func auditFieldValue(record *Record, f Field) any {
	if record == nil {
		return nil
	}

	if _, ok := f.(*PasswordField); ok {
		return record.GetString(f.GetName() + ":hash")
	}

	return record.GetRaw(f.GetName())
}

func auditEqual(a, b any) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}

	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)

	return errA == nil && errB == nil && string(ja) == string(jb)
}

// auditSafe prepares the value for storage: redacts sensitive values and
// caps oversized ones.
func auditSafe(v any, redact bool) any {
	if redact {
		if s, ok := v.(string); ok && s == "" {
			return "" // keep "was empty" visible, it leaks nothing
		}
		if v == nil {
			return nil
		}
		return auditRedactedValue
	}

	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}

	if len(raw) > auditMaxValueLength {
		return fmt.Sprintf("[truncated: %d bytes]", len(raw))
	}

	return json.RawMessage(raw)
}
