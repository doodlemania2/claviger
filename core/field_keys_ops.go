package core

import (
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/keys"
	"github.com/doodlemania2/claviger/tools/replica"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #75, M3): key rotation and the other maintenance
// operations on the encrypted fields.
//
//	RotateFieldKeys      new data key per field, rows re-encrypted in batches (resumable)
//	RewrapFieldKeys      data keys re-wrapped under the primary KEK, row data untouched
//	ConvertFieldValues   legacy unprefixed values encrypted in place
//	SetFieldKeysStrict   unprefixed values become a read error
//	PruneFieldKeys       retired data keys no row uses are deleted
//	FieldKeysStatus      what exists and what is pending
//
// They work with raw SQL, so they fire no record hooks and write no per-row
// audit entries; each run writes ONE audit entry instead. They are safe to run
// inside the serving process: every row update is a compare-and-swap on the
// stored value, so a row saved by the app in the meantime is never overwritten
// with stale data (the app seals with the new key from the moment it is
// installed, and a final pass re-checks the whole column).

const (
	// AuditActionFieldKeyRotate and friends are the actions of the audit
	// entries written by the operations below (target collection "_fieldKeys").
	AuditActionFieldKeyRotate  = "field_key_rotate"
	AuditActionFieldKeyRewrap  = "field_key_rewrap"
	AuditActionFieldKeyConvert = "field_key_convert"
	AuditActionFieldKeyPrune   = "field_key_prune"
	AuditActionFieldKeyStrict  = "field_key_strict"

	// DefaultFieldKeyBatchSize is the number of rows re-encrypted per transaction.
	DefaultFieldKeyBatchSize = 500

	fieldKeyMaxPasses = 20
)

// ErrFieldKeyOpRunning is returned when another field key operation is in progress.
var ErrFieldKeyOpRunning = errors.New("another field key operation is already running")

var fieldKeyOpMu sync.Mutex

// fieldKeyBatchHook is a test seam: called after every committed batch with
// the 1-based batch counter of the run; an error aborts the run exactly like
// a crash between two batches would.
var fieldKeyBatchHook func(batch int) error

// FieldKeyOptions selects the fields an operation applies to and tunes it.
type FieldKeyOptions struct {
	// Collection (name or id) and Field (name or id) restrict the operation.
	// Empty Collection means every collection with an encrypted field.
	// Field requires Collection.
	Collection string
	Field      string

	// BatchSize is the number of rows per transaction (default 500).
	BatchSize int

	// Progress, if set, is called after every batch and at the end of each field.
	Progress func(FieldKeyProgress)
}

// FieldKeyProgress is a progress report.
type FieldKeyProgress struct {
	Collection string `json:"collection"`
	Field      string `json:"field"`
	Done       int64  `json:"done"`
	Total      int64  `json:"total"`
	Message    string `json:"message"`
}

func (o *FieldKeyOptions) batchSize() int {
	if o.BatchSize <= 0 {
		return DefaultFieldKeyBatchSize
	}
	return o.BatchSize
}

func (o *FieldKeyOptions) report(p FieldKeyProgress) {
	if o.Progress != nil {
		o.Progress(p)
	}
}

type fieldTarget struct {
	collection *Collection
	field      Field
}

func (t fieldTarget) label() string { return t.collection.Name + "." + t.field.GetName() }

func lockFieldKeyOp(app App) (func(), error) {
	if !fieldKeyOpMu.TryLock() {
		return nil, ErrFieldKeyOpRunning
	}

	// several replicas (#106 M7): one operation at a time across all of them,
	// and the others reload the keys when it ends (also after a partial run)
	bus := ReplicaBusOf(app)
	if bus == nil {
		return fieldKeyOpMu.Unlock, nil
	}

	release, ok, err := replica.TryLock(context.Background(), bus.DB(), "fieldkeyop")
	if err != nil || !ok {
		fieldKeyOpMu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, ErrFieldKeyOpRunning
	}

	return func() {
		PublishReplicaEvent(app, ReplicaEventFieldKeys, nil)
		release()
		fieldKeyOpMu.Unlock()
	}, nil
}

func resolveFieldTargets(app App, collection, field string) ([]fieldTarget, error) {
	if field != "" && collection == "" {
		return nil, errors.New("a field filter needs a collection")
	}

	collections, err := app.FindAllCollections()
	if err != nil {
		return nil, err
	}

	var targets []fieldTarget
	for _, c := range collections {
		if collection != "" && c.Name != collection && c.Id != collection {
			continue
		}
		for _, f := range encryptedFieldsOf(c) {
			if field != "" && f.GetName() != field && f.GetId() != field {
				continue
			}
			targets = append(targets, fieldTarget{c, f})
		}
	}

	if len(targets) == 0 {
		if collection == "" {
			return nil, errors.New("there are no encrypted fields")
		}
		return nil, fmt.Errorf("no encrypted field matches collection %q field %q", collection, field)
	}

	return targets, nil
}

func randomKeyId() (string, error) {
	b := make([]byte, 8)
	if _, err := crand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (t fieldTarget) col() string { return "[[" + t.field.GetName() + "]]" }

// countWhere counts the rows of the target's table matching exp.
func (t fieldTarget) countWhere(app App, exp dbx.Expression) (int64, error) {
	var n int64
	err := app.DB().Select("COUNT(*)").From(t.collection.Name).AndWhere(exp).Row(&n)
	return n, err
}

func (t fieldTarget) offCurrentExp(keyId string) dbx.Expression {
	return dbx.NewExp(t.col()+" LIKE {:any} AND "+t.col()+" NOT LIKE {:cur}", dbx.Params{
		"any": fieldCipherPrefix + "%",
		"cur": fieldCipherPrefix + keyId + ":%",
	})
}

func (t fieldTarget) onKeyExp(keyId string) dbx.Expression {
	return dbx.NewExp(t.col()+" LIKE {:cur}", dbx.Params{"cur": fieldCipherPrefix + keyId + ":%"})
}

func (t fieldTarget) legacyExp() dbx.Expression {
	return dbx.NewExp(t.col()+" IS NOT NULL AND "+t.col()+" != '' AND "+t.col()+" NOT LIKE {:any}", dbx.Params{
		"any": fieldCipherPrefix + "%",
	})
}

func fieldKeyRows(app App, t fieldTarget) (all []fieldKeyRow, current *fieldKeyRow, err error) {
	all, err = scanFieldKeyRows(app, dbx.HashExp{"collectionRef": t.collection.Id, "fieldRef": t.field.GetId()})
	if err != nil {
		return nil, nil, err
	}
	for i := range all {
		if all[i].Retired == "" {
			current = &all[i] // oldest first: the newest active row wins
		}
	}
	return all, current, nil
}

// writeFieldKeyAudit writes the single audit entry of a run.
func writeFieldKeyAudit(app App, ctx context.Context, action string, meta map[string]any) error {
	if !auditAvailable(app) {
		return nil
	}

	entry := &AuditEntry{
		Action:         action,
		CollectionName: CollectionNameFieldKeys,
		ChangedFields:  []string{},
		Meta:           meta,
	}
	if c, err := app.FindCachedCollectionByNameOrId(CollectionNameFieldKeys); err == nil {
		entry.CollectionId = c.Id
	}
	applyAuditContext(ctx, entry)

	return WriteAuditEntry(app, ctx, entry, nil)
}

func finishFieldKeyAudit(app App, ctx context.Context, action string, meta map[string]any, runErr error) {
	if runErr != nil {
		meta["status"] = "failed"
		meta["error"] = runErr.Error()
	} else {
		meta["status"] = "ok"
	}
	if err := writeFieldKeyAudit(app, ctx, action, meta); err != nil {
		app.Logger().Error("failed to write the field key audit entry", "action", action, "error", err)
	}
}

// ---------------------------------------------------------------------------
// DEK rotation
// ---------------------------------------------------------------------------

// FieldKeyRotateFieldResult is the outcome for one field.
type FieldKeyRotateFieldResult struct {
	Collection string `json:"collection"`
	Field      string `json:"field"`
	// OldKeyId is the key the rows were on before (empty when resuming).
	OldKeyId string `json:"oldKeyId"`
	NewKeyId string `json:"newKeyId"`
	// Resumed is true when an earlier, unfinished rotation was continued
	// instead of creating another key.
	Resumed bool `json:"resumed"`
	// Total is the number of rows that were on another key when the run started.
	Total int64 `json:"total"`
	// Reencrypted is the number of rows moved to the new key by this run.
	Reencrypted int64 `json:"reencrypted"`
	// Skipped counts rows the app rewrote while the run was reading them
	// (the app's write already used the current key).
	Skipped int64 `json:"skipped"`
	// Failed is the number of rows still on an old key at the end of the run
	// (they could not be decrypted: corrupt, or moved to another record).
	Failed int64 `json:"failed"`
}

// FieldKeyRotateResult is the outcome of [RotateFieldKeys].
type FieldKeyRotateResult struct {
	Fields []FieldKeyRotateFieldResult `json:"fields"`
}

// RotateFieldKeys creates a new data key for every selected encrypted field
// and re-encrypts the rows under it, in batches of opts.BatchSize rows per
// transaction. The previous key is marked retired: it stays available for
// reads until `prune` deletes it, which is only allowed when no row uses it.
//
// It is resumable and idempotent. If a previous run stopped half way (a crash,
// a restart, an error), running it again finds the field with a retired key
// and rows still on it, continues with the existing new key and skips the
// rows already moved. No cursor is stored: the work set is "rows whose value
// is not on the current key".
//
// Rows holding unencrypted legacy values are not touched (see
// [ConvertFieldValues]).
func RotateFieldKeys(ctx context.Context, app App, opts FieldKeyOptions) (*FieldKeyRotateResult, error) {
	unlock, err := lockFieldKeyOp(app)
	if err != nil {
		return nil, err
	}
	defer unlock()

	res := &FieldKeyRotateResult{}
	meta := map[string]any{"batchSize": opts.batchSize()}

	run := func() error {
		targets, err := resolveFieldTargets(app, opts.Collection, opts.Field)
		if err != nil {
			return err
		}

		src, err := fieldKeySource(app)
		if err != nil {
			return fmt.Errorf("the key-encryption key is unavailable: %w", err)
		}

		for _, t := range targets {
			r, err := rotateFieldTarget(ctx, app, src, t, &opts)
			res.Fields = append(res.Fields, r)
			if err != nil {
				return fmt.Errorf("%s: %w", t.label(), err)
			}
		}

		return nil
	}

	runErr := run()
	meta["fields"] = res.Fields
	finishFieldKeyAudit(app, ctx, AuditActionFieldKeyRotate, meta, runErr)

	return res, runErr
}

func rotateFieldTarget(ctx context.Context, app App, src keys.KeySource, t fieldTarget, opts *FieldKeyOptions) (FieldKeyRotateFieldResult, error) {
	r := FieldKeyRotateFieldResult{Collection: t.collection.Name, Field: t.field.GetName()}

	all, current, err := fieldKeyRows(app, t)
	if err != nil {
		return r, err
	}
	if current == nil {
		return r, errors.New("the field has no active data key")
	}

	retired := 0
	for _, row := range all {
		if row.Retired != "" {
			retired++
		}
	}

	if retired > 0 {
		pending, err := t.countWhere(app, t.offCurrentExp(current.KeyId))
		if err != nil {
			return r, err
		}
		r.Resumed = pending > 0
	}

	if r.Resumed {
		r.NewKeyId = current.KeyId
	} else {
		newKeyId, err := installRotatedKey(ctx, app, src, t, current)
		if err != nil {
			return r, err
		}
		r.OldKeyId, r.NewKeyId = current.KeyId, newKeyId
	}

	r.Total, err = t.countWhere(app, t.offCurrentExp(r.NewKeyId))
	if err != nil {
		return r, err
	}

	app.Logger().Info("field key rotation started", "field", t.label(), "newKeyId", r.NewKeyId, "rows", r.Total, "resumed", r.Resumed)
	opts.report(FieldKeyProgress{Collection: r.Collection, Field: r.Field, Total: r.Total, Message: "started"})

	batchCounter := 0
	for pass := 0; ; pass++ {
		if pass >= fieldKeyMaxPasses {
			return r, fmt.Errorf("the column is still being rewritten with other keys after %d passes; run the rotation again", pass)
		}

		moved, err := reencryptPass(ctx, app, t, opts, &r, &batchCounter)
		if err != nil {
			return r, err
		}
		if moved == 0 {
			break // nothing left that can be moved (done, or only failures)
		}
	}

	left, err := t.countWhere(app, t.offCurrentExp(r.NewKeyId))
	if err != nil {
		return r, err
	}

	app.Logger().Info("field key rotation finished", "field", t.label(), "reencrypted", r.Reencrypted, "skipped", r.Skipped, "failed", r.Failed, "left", left)
	opts.report(FieldKeyProgress{Collection: r.Collection, Field: r.Field, Done: r.Reencrypted, Total: r.Total, Message: "finished"})

	r.Failed = left

	if left > 0 {
		return r, fmt.Errorf("%d row(s) are still on an old key (%d of them could not be decrypted); run the rotation again after fixing them", left, r.Failed)
	}

	return r, nil
}

// installRotatedKey adds the new data key and retires the current one, in one
// transaction, and makes the new key the one that seals.
func installRotatedKey(ctx context.Context, app App, src keys.KeySource, t fieldTarget, current *fieldKeyRow) (string, error) {
	dek := make([]byte, fieldDEKLength)
	if _, err := crand.Read(dek); err != nil {
		return "", err
	}

	wrapped, err := src.Wrap(ctx, dek)
	if err != nil {
		return "", fmt.Errorf("failed to wrap the new data key: %w", err)
	}

	keyId, err := randomKeyId()
	if err != nil {
		return "", err
	}

	now := types.NowDateTime().String()

	err = app.RunInTransaction(func(txApp App) error {
		res, err := txApp.NonconcurrentDB().Update(CollectionNameFieldKeys,
			dbx.Params{"retired": now},
			dbx.HashExp{"id": current.Id, "retired": ""},
		).Execute()
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("the data key changed while rotating; run the rotation again")
		}

		_, err = txApp.NonconcurrentDB().Insert(CollectionNameFieldKeys, dbx.Params{
			"id":            randomFieldKeyRowId(),
			"collectionRef": t.collection.Id,
			"fieldRef":      t.field.GetId(),
			"keyId":         keyId,
			"kekId":         src.KeyID(),
			"wrapped":       base64.StdEncoding.EncodeToString(wrapped),
			"retired":       "",
			"created":       now,
			"strictMode":    current.StrictMode,
		}).Execute()
		return err
	})
	if err != nil {
		return "", fmt.Errorf("failed to install the new data key: %w", err)
	}

	// after the commit: from here on the app seals new writes with the new key
	if err := fieldKeys.add(keyId, t.collection.Id, t.field.GetId(), dek, true); err != nil {
		return "", err
	}

	return keyId, nil
}

type fieldValueRow struct {
	id, value string
}

func loadFieldValueRows(app App, t fieldTarget, where dbx.Expression, after string, limit int) ([]fieldValueRow, error) {
	q := app.DB().Select("id", t.field.GetName()).From(t.collection.Name).
		AndWhere(where).
		AndWhere(dbx.NewExp("[[id]] > {:after}", dbx.Params{"after": after})).
		OrderBy("id ASC").
		Limit(int64(limit))

	var maps []dbx.NullStringMap
	if err := q.All(&maps); err != nil {
		return nil, err
	}

	rows := make([]fieldValueRow, len(maps))
	for i, m := range maps {
		rows[i] = fieldValueRow{id: m["id"].String, value: m[t.field.GetName()].String}
	}
	return rows, nil
}

// updateFieldValue is a compare-and-swap: it only writes if the stored value
// is still the one that was read.
func updateFieldValue(app App, t fieldTarget, id, oldValue, newValue string) (bool, error) {
	res, err := app.NonconcurrentDB().Update(t.collection.Name,
		dbx.Params{t.field.GetName(): newValue},
		dbx.HashExp{"id": id, t.field.GetName(): oldValue},
	).Execute()
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// reencryptPass walks the column once (by id) and moves every row that is not
// on the new key. It returns how many rows it moved.
func reencryptPass(ctx context.Context, app App, t fieldTarget, opts *FieldKeyOptions, r *FieldKeyRotateFieldResult, batchCounter *int) (int64, error) {
	var moved int64
	last := ""

	for {
		if err := ctx.Err(); err != nil {
			return moved, err
		}

		var batchLast string
		var n int
		err := app.RunInTransaction(func(txApp App) error {
			rows, err := loadFieldValueRows(txApp, t, t.offCurrentExp(r.NewKeyId), last, opts.batchSize())
			if err != nil {
				return err
			}

			n = len(rows)
			for _, row := range rows {
				batchLast = row.id

				plain, _, err := fieldKeys.open(t.collection.Id, t.field.GetId(), row.id, row.value)
				if err != nil {
					app.Logger().Warn("field key rotation: cannot decrypt a row, leaving it unchanged", "field", t.label(), "record", row.id, "error", err)
					continue
				}

				stored, keyId, err := fieldKeys.seal(t.collection.Id, t.field.GetId(), row.id, plain)
				if err != nil {
					return err
				}
				if keyId != r.NewKeyId {
					return errors.New("the current data key changed during the rotation")
				}

				ok, err := updateFieldValue(txApp, t, row.id, row.value, stored)
				if err != nil {
					return err
				}
				if ok {
					r.Reencrypted++
					moved++
				} else {
					r.Skipped++
				}
			}

			return nil
		})
		if err != nil {
			return moved, err
		}

		if n == 0 {
			return moved, nil
		}
		last = batchLast

		*batchCounter++
		opts.report(FieldKeyProgress{Collection: r.Collection, Field: r.Field, Done: r.Reencrypted, Total: r.Total, Message: "batch"})
		app.Logger().Info("field key rotation progress", "field", t.label(), "reencrypted", r.Reencrypted, "of", r.Total)

		if fieldKeyBatchHook != nil {
			if err := fieldKeyBatchHook(*batchCounter); err != nil {
				return moved, err
			}
		}
	}
}

func randomFieldKeyRowId() string {
	b := make([]byte, 8)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)[:15]
}

// ---------------------------------------------------------------------------
// KEK rotation
// ---------------------------------------------------------------------------

// FieldKeyRewrapResult is the outcome of [RewrapFieldKeys].
type FieldKeyRewrapResult struct {
	// KekId is the id of the KEK every data key is wrapped under afterwards.
	KekId string `json:"kekId"`
	// Rewrapped is the number of data keys that were re-wrapped.
	Rewrapped int `json:"rewrapped"`
	// Unchanged is the number that already used the primary KEK.
	Unchanged int `json:"unchanged"`
	// From lists the KEK ids the re-wrapped keys came from.
	From []string `json:"from"`
}

// RewrapFieldKeys re-wraps every data key (retired ones too) under the primary
// KEK. The rows are not touched: only _fieldKeys changes.
//
// The previous KEK must be available too (CLAVIGER_KEK_OLD,
// CLAVIGER_KEK_FILE_OLD or CLAVIGER_KEK_OLD_FROM_APP_KEY); if a data key was
// wrapped under a KEK that is not available, nothing is changed and an error
// names the missing KEK id. All the keys are re-wrapped in one transaction and
// each result is unwrapped again and compared before the commit.
func RewrapFieldKeys(ctx context.Context, app App) (*FieldKeyRewrapResult, error) {
	unlock, err := lockFieldKeyOp(app)
	if err != nil {
		return nil, err
	}
	defer unlock()

	res := &FieldKeyRewrapResult{}
	meta := map[string]any{}

	runErr := rewrapFieldKeys(ctx, app, res)
	meta["kekId"] = res.KekId
	meta["rewrapped"] = res.Rewrapped
	meta["unchanged"] = res.Unchanged
	meta["from"] = res.From
	finishFieldKeyAudit(app, ctx, AuditActionFieldKeyRewrap, meta, runErr)

	return res, runErr
}

func rewrapFieldKeys(ctx context.Context, app App, res *FieldKeyRewrapResult) error {
	src, err := fieldKeySource(app)
	if err != nil {
		return fmt.Errorf("the key-encryption key is unavailable: %w", err)
	}

	res.KekId = src.KeyID()

	rows, err := scanFieldKeyRows(app, nil)
	if err != nil {
		return err
	}

	// live fields only matter for the message; orphaned keys are re-wrapped
	// too so that no KEK needs to be kept for them
	type work struct {
		row   fieldKeyRow
		dek   []byte
		newWr []byte
	}
	var todo []work
	from := map[string]bool{}

	for _, row := range rows {
		if row.KekId == src.KeyID() {
			res.Unchanged++
			continue
		}

		wrapped, err := base64.StdEncoding.DecodeString(row.Wrapped)
		if err != nil {
			return fmt.Errorf("data key %s is corrupted (bad encoding)", row.KeyId)
		}

		if multi, ok := src.(*keys.Multi); ok && !multi.Has(row.KekId) {
			return fmt.Errorf("data key %s is wrapped under key-encryption key %s, which is not available: set %s, %s or %s to the previous KEK (nothing was changed)",
				row.KeyId, row.KekId, keys.EnvKEKOld, keys.EnvKEKFileOld, keys.EnvKEKOldFromAppKey)
		}

		dek, err := src.Unwrap(ctx, row.KekId, wrapped)
		if err != nil {
			return fmt.Errorf("cannot unwrap data key %s (wrapped under %s): %w (nothing was changed)", row.KeyId, row.KekId, err)
		}

		newWrapped, err := src.Wrap(ctx, dek)
		if err != nil {
			return fmt.Errorf("failed to wrap data key %s: %w", row.KeyId, err)
		}

		// prove the new wrapping before it replaces the old one
		check, err := src.Unwrap(ctx, src.KeyID(), newWrapped)
		if err != nil || string(check) != string(dek) {
			return fmt.Errorf("the re-wrapped data key %s does not unwrap to the same key (nothing was changed)", row.KeyId)
		}

		todo = append(todo, work{row: row, dek: dek, newWr: newWrapped})
		from[row.KekId] = true
	}

	if len(todo) == 0 {
		return nil
	}

	err = app.RunInTransaction(func(txApp App) error {
		for _, w := range todo {
			r, err := txApp.NonconcurrentDB().Update(CollectionNameFieldKeys,
				dbx.Params{"wrapped": base64.StdEncoding.EncodeToString(w.newWr), "kekId": src.KeyID()},
				dbx.HashExp{"id": w.row.Id, "kekId": w.row.KekId},
			).Execute()
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n != 1 {
				return errors.New("a data key changed while re-wrapping; run it again")
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to store the re-wrapped data keys: %w", err)
	}

	res.Rewrapped = len(todo)
	for id := range from {
		res.From = append(res.From, id)
	}

	app.Logger().Info("field data keys re-wrapped", "keys", res.Rewrapped, "kekId", res.KekId)

	return nil
}

// ---------------------------------------------------------------------------
// convert + strict
// ---------------------------------------------------------------------------

// FieldKeyConvertFieldResult is the outcome of converting one field.
type FieldKeyConvertFieldResult struct {
	Collection string `json:"collection"`
	Field      string `json:"field"`
	Total      int64  `json:"total"`
	Converted  int64  `json:"converted"`
	Skipped    int64  `json:"skipped"`
}

// FieldKeyConvertResult is the outcome of [ConvertFieldValues].
type FieldKeyConvertResult struct {
	Fields []FieldKeyConvertFieldResult `json:"fields"`
}

// ConvertFieldValues encrypts, in place and in batches, the values of the
// selected fields that are still stored unencrypted: what remains in the
// column after a text or json field was changed to an encrypted type. The
// run is idempotent and resumable (rows already encrypted are not in the work
// set). It does not turn strict mode on; see [SetFieldKeysStrict].
func ConvertFieldValues(ctx context.Context, app App, opts FieldKeyOptions) (*FieldKeyConvertResult, error) {
	unlock, err := lockFieldKeyOp(app)
	if err != nil {
		return nil, err
	}
	defer unlock()

	res := &FieldKeyConvertResult{}
	meta := map[string]any{"batchSize": opts.batchSize()}

	run := func() error {
		targets, err := resolveFieldTargets(app, opts.Collection, opts.Field)
		if err != nil {
			return err
		}

		for _, t := range targets {
			r, err := convertFieldTarget(ctx, app, t, &opts)
			res.Fields = append(res.Fields, r)
			if err != nil {
				return fmt.Errorf("%s: %w", t.label(), err)
			}
		}
		return nil
	}

	runErr := run()
	meta["fields"] = res.Fields
	finishFieldKeyAudit(app, ctx, AuditActionFieldKeyConvert, meta, runErr)

	return res, runErr
}

func convertFieldTarget(ctx context.Context, app App, t fieldTarget, opts *FieldKeyOptions) (FieldKeyConvertFieldResult, error) {
	r := FieldKeyConvertFieldResult{Collection: t.collection.Name, Field: t.field.GetName()}

	if _, ok := fieldKeys.currentKeyId(t.collection.Id, t.field.GetId()); !ok {
		return r, ErrFieldKeyNotLoaded
	}

	var err error
	r.Total, err = t.countWhere(app, t.legacyExp())
	if err != nil {
		return r, err
	}

	_, isJSON := t.field.(*EncryptedJSONField)

	for pass := 0; pass < fieldKeyMaxPasses; pass++ {
		var movedInPass int64
		last := ""

		for {
			if err := ctx.Err(); err != nil {
				return r, err
			}

			var n int
			var batchLast string
			err := app.RunInTransaction(func(txApp App) error {
				rows, err := loadFieldValueRows(txApp, t, t.legacyExp(), last, opts.batchSize())
				if err != nil {
					return err
				}
				n = len(rows)

				for _, row := range rows {
					batchLast = row.id

					plain := row.value
					if isJSON {
						v, err := (&JSONField{}).PrepareValue(nil, plain)
						if err != nil {
							return fmt.Errorf("record %s: %w", row.id, err)
						}
						plain = strings.TrimSpace(fmt.Sprint(v))
						if plain == "null" {
							plain = ""
						}
					}

					stored := ""
					if plain != "" {
						s, _, err := fieldKeys.seal(t.collection.Id, t.field.GetId(), row.id, []byte(plain))
						if err != nil {
							return err
						}
						stored = s
					}

					ok, err := updateFieldValue(txApp, t, row.id, row.value, stored)
					if err != nil {
						return err
					}
					if ok {
						r.Converted++
						movedInPass++
					} else {
						r.Skipped++
					}
				}
				return nil
			})
			if err != nil {
				return r, err
			}
			if n == 0 {
				break
			}
			last = batchLast

			opts.report(FieldKeyProgress{Collection: r.Collection, Field: r.Field, Done: r.Converted, Total: r.Total, Message: "batch"})
			app.Logger().Info("field conversion progress", "field", t.label(), "converted", r.Converted, "of", r.Total)

			if fieldKeyBatchHook != nil {
				if err := fieldKeyBatchHook(int(r.Converted)); err != nil {
					return r, err
				}
			}
		}

		if movedInPass == 0 {
			return r, nil
		}
	}

	return r, fmt.Errorf("the column keeps receiving unencrypted values; run the conversion again")
}

// SetFieldKeysStrict switches the strict mode of one field. In strict mode a
// stored value without the encryption prefix is a read error. Turning it on is
// refused while such values exist (run [ConvertFieldValues] first). The flag
// is stored with the field's data key (column strictMode of _fieldKeys), not
// in the field options, and survives key rotation.
func SetFieldKeysStrict(ctx context.Context, app App, collection, field string, strict bool) error {
	unlock, err := lockFieldKeyOp(app)
	if err != nil {
		return err
	}
	defer unlock()

	meta := map[string]any{"collection": collection, "field": field, "strict": strict}

	run := func() error {
		if collection == "" || field == "" {
			return errors.New("strict mode is set per field: a collection and a field are required")
		}

		targets, err := resolveFieldTargets(app, collection, field)
		if err != nil {
			return err
		}
		t := targets[0]

		_, current, err := fieldKeyRows(app, t)
		if err != nil {
			return err
		}
		if current == nil {
			return errors.New("the field has no active data key")
		}

		if strict {
			n, err := t.countWhere(app, t.legacyExp())
			if err != nil {
				return err
			}
			if n > 0 {
				return fmt.Errorf("%d row(s) still hold unencrypted values; run `keys convert` first", n)
			}
		}

		value := ""
		if strict {
			value = fieldKeyStrictOn
		}

		if _, err := app.NonconcurrentDB().Update(CollectionNameFieldKeys,
			dbx.Params{"strictMode": value},
			dbx.HashExp{"id": current.Id},
		).Execute(); err != nil {
			return err
		}

		fieldKeys.setStrict(t.collection.Id, t.field.GetId(), strict)
		meta["collection"], meta["field"] = t.collection.Name, t.field.GetName()

		return nil
	}

	runErr := run()
	finishFieldKeyAudit(app, ctx, AuditActionFieldKeyStrict, meta, runErr)

	return runErr
}

// ---------------------------------------------------------------------------
// prune
// ---------------------------------------------------------------------------

// FieldKeyPruneResult is the outcome of [PruneFieldKeys].
type FieldKeyPruneResult struct {
	// Pruned lists the deleted key ids.
	Pruned []string `json:"pruned"`
	// InUse maps a retired key id to the number of rows that still use it.
	InUse map[string]int64 `json:"inUse"`
}

// PruneFieldKeys deletes the retired data keys that no row uses any more.
// A retired key that is still in use is kept (and reported). The active key
// is never deleted. Deleted keys are gone for good: a backup taken before the
// prune still holds them, which is also why a backup can only be restored
// together with the rows of its own time.
func PruneFieldKeys(ctx context.Context, app App, opts FieldKeyOptions) (*FieldKeyPruneResult, error) {
	unlock, err := lockFieldKeyOp(app)
	if err != nil {
		return nil, err
	}
	defer unlock()

	res := &FieldKeyPruneResult{InUse: map[string]int64{}}
	meta := map[string]any{}

	run := func() error {
		targets, err := resolveFieldTargets(app, opts.Collection, opts.Field)
		if err != nil {
			return err
		}

		for _, t := range targets {
			all, _, err := fieldKeyRows(app, t)
			if err != nil {
				return err
			}

			for _, row := range all {
				if row.Retired == "" {
					continue
				}

				err := app.RunInTransaction(func(txApp App) error {
					n, err := t.countWhere(txApp, t.onKeyExp(row.KeyId))
					if err != nil {
						return err
					}
					if n > 0 {
						res.InUse[row.KeyId] = n
						return nil
					}

					_, err = txApp.NonconcurrentDB().Delete(CollectionNameFieldKeys, dbx.HashExp{"id": row.Id, "retired": row.Retired}).Execute()
					if err == nil {
						res.Pruned = append(res.Pruned, row.KeyId)
					}
					return err
				})
				if err != nil {
					return fmt.Errorf("%s: %w", t.label(), err)
				}
			}
		}
		return nil
	}

	runErr := run()

	// forget the pruned keys in memory (their deletes committed, even when a later one failed)
	for _, id := range res.Pruned {
		fieldKeys.remove(id)
	}

	meta["pruned"] = res.Pruned
	meta["inUse"] = res.InUse
	finishFieldKeyAudit(app, ctx, AuditActionFieldKeyPrune, meta, runErr)

	return res, runErr
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

// FieldKeyRetiredStatus describes a retired data key.
type FieldKeyRetiredStatus struct {
	KeyId   string `json:"keyId"`
	Retired string `json:"retired"`
	Rows    int64  `json:"rows"`
}

// FieldKeyStatus describes the keys of one encrypted field.
type FieldKeyStatus struct {
	Collection string `json:"collection"`
	Field      string `json:"field"`
	Type       string `json:"type"`
	KeyId      string `json:"keyId"`
	KekId      string `json:"kekId"`
	Created    string `json:"created"`
	Strict     bool   `json:"strict"`
	// NeedsRewrap is true when the key is wrapped under a KEK other than the primary one.
	NeedsRewrap bool `json:"needsRewrap"`
	// Rows counts the rows by state (non-empty values only).
	RowsOnCurrentKey int64                   `json:"rowsOnCurrentKey"`
	RowsOnOldKeys    int64                   `json:"rowsOnOldKeys"`
	RowsUnencrypted  int64                   `json:"rowsUnencrypted"`
	Retired          []FieldKeyRetiredStatus `json:"retired"`
}

// FieldKeysStatusReport is the result of [FieldKeysStatus].
type FieldKeysStatusReport struct {
	// KekId is the id of the primary key-encryption key.
	KekId string `json:"kekId"`
	// HasPreviousKek is true when a previous KEK is configured (rotation in progress).
	HasPreviousKek bool             `json:"hasPreviousKek"`
	Fields         []FieldKeyStatus `json:"fields"`
}

// FieldKeysStatus reports the keys and the pending work of every encrypted
// field. It counts rows, so it scans the encrypted columns.
func FieldKeysStatus(ctx context.Context, app App) (*FieldKeysStatusReport, error) {
	report := &FieldKeysStatusReport{Fields: []FieldKeyStatus{}}

	src, err := fieldKeySource(app)
	if err != nil {
		if errors.Is(err, keys.ErrNoKeySource) {
			// no encrypted field can exist without a key; report nothing
			targets, _ := resolveFieldTargets(app, "", "")
			if len(targets) == 0 {
				return report, nil
			}
		}
		return nil, err
	}

	report.KekId = src.KeyID()
	if m, ok := src.(*keys.Multi); ok {
		report.HasPreviousKek = m.HasOld()
	}

	targets, err := resolveFieldTargets(app, "", "")
	if err != nil {
		return report, nil // no encrypted fields
	}

	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		all, current, err := fieldKeyRows(app, t)
		if err != nil {
			return nil, err
		}
		if current == nil {
			continue
		}

		st := FieldKeyStatus{
			Collection:  t.collection.Name,
			Field:       t.field.GetName(),
			Type:        t.field.Type(),
			KeyId:       current.KeyId,
			KekId:       current.KekId,
			Created:     current.Created,
			Strict:      current.StrictMode == fieldKeyStrictOn,
			NeedsRewrap: current.KekId != src.KeyID(),
			Retired:     []FieldKeyRetiredStatus{},
		}

		if st.RowsOnCurrentKey, err = t.countWhere(app, t.onKeyExp(current.KeyId)); err != nil {
			return nil, err
		}
		if st.RowsOnOldKeys, err = t.countWhere(app, t.offCurrentExp(current.KeyId)); err != nil {
			return nil, err
		}
		if st.RowsUnencrypted, err = t.countWhere(app, t.legacyExp()); err != nil {
			return nil, err
		}

		for _, row := range all {
			if row.KekId != src.KeyID() {
				st.NeedsRewrap = true
			}
			if row.Retired == "" {
				continue
			}
			n, err := t.countWhere(app, t.onKeyExp(row.KeyId))
			if err != nil {
				return nil, err
			}
			st.Retired = append(st.Retired, FieldKeyRetiredStatus{KeyId: row.KeyId, Retired: row.Retired, Rows: n})
		}

		report.Fields = append(report.Fields, st)
	}

	return report, nil
}
