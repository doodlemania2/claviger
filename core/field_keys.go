package core

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/keys"
	"github.com/doodlemania2/claviger/tools/security"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #75): envelope key management for encrypted fields.
//
// Hierarchy:
//
//	KEK (key source: CLAVIGER_KEK, CLAVIGER_KEK_FILE or HKDF(PB_ENCRYPTION_KEY))
//	  wraps -> DEK, one random 256-bit key per (collection, encrypted field)
//	  seals -> the field values, AES-256-GCM, record id bound in the AAD
//
// The wrapped DEKs live in the hidden system collection _fieldKeys, which is
// written only by this file (raw SQL inside the collection save transaction)
// and is read-only for everyone else. They are kept out of the field options
// on purpose: the options are part of the collection JSON, which is exported,
// snapshotted and shown in the dashboard.
//
// The unwrapped DEKs are cached in process memory as ready cipher.AEAD
// values. The field methods that need them (PrepareValue, DriverValue) have no
// access to the app, so the cache is process-wide. The DEK ids are random, so
// two apps in one process never share an entry; the (collection, field) ->
// current key lookup assumes one app per process, as in production.

const (
	// CollectionNameFieldKeys is the hidden system collection holding the
	// wrapped data keys of the encrypted fields.
	CollectionNameFieldKeys = "_fieldKeys"

	fieldKeySourceStoreKey = "__clavigerFieldKeySource__"

	fieldCipherPrefix = "enc:v1:"
	fieldDEKLength    = 32
)

// FieldKeySourceLoader returns the key-encryption key source of the app.
//
// It defaults to the environment sources (CLAVIGER_KEK, CLAVIGER_KEK_FILE,
// then the app encryption key). Replace it before bootstrap to plug another
// source. It must return an error wrapping [keys.ErrNoKeySource] when
// nothing is configured.
var FieldKeySourceLoader = func(app App) (keys.KeySource, error) {
	return keys.LoadFromEnv(os.Getenv, app.EncryptionEnv())
}

// ErrFieldKeyNotLoaded is returned when an encrypted value cannot be sealed
// or opened because its data key is not available.
var ErrFieldKeyNotLoaded = errors.New("the data key of the encrypted field is not loaded")

type fieldKeyEntry struct {
	collectionId string
	fieldId      string
	aead         cipher.AEAD
}

type fieldKeyring struct {
	mu      sync.RWMutex
	byID    map[string]*fieldKeyEntry
	current map[string]string // collectionId|fieldId -> key id
	strict  map[string]bool   // collectionId|fieldId -> unencrypted values are an error
}

var fieldKeys = &fieldKeyring{
	byID:    map[string]*fieldKeyEntry{},
	current: map[string]string{},
	strict:  map[string]bool{},
}

func (k *fieldKeyring) isStrict(collectionId, fieldId string) bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.strict[fieldKeyRef(collectionId, fieldId)]
}

func (k *fieldKeyring) setStrict(collectionId, fieldId string, strict bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if strict {
		k.strict[fieldKeyRef(collectionId, fieldId)] = true
	} else {
		delete(k.strict, fieldKeyRef(collectionId, fieldId))
	}
}

func fieldKeyRef(collectionId, fieldId string) string {
	return collectionId + "|" + fieldId
}

func newFieldAEAD(dek []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// add registers an unwrapped DEK. If current is true it also becomes the key
// used for new values of the field.
func (k *fieldKeyring) add(keyId, collectionId, fieldId string, dek []byte, current bool) error {
	aead, err := newFieldAEAD(dek)
	if err != nil {
		return err
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	k.byID[keyId] = &fieldKeyEntry{collectionId: collectionId, fieldId: fieldId, aead: aead}
	if current {
		k.current[fieldKeyRef(collectionId, fieldId)] = keyId
	}

	return nil
}

func (k *fieldKeyring) currentKeyId(collectionId, fieldId string) (string, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	id, ok := k.current[fieldKeyRef(collectionId, fieldId)]
	return id, ok
}

func fieldCipherAAD(collectionId, fieldId, recordId, keyId string) []byte {
	return []byte("claviger/field/v1|" + collectionId + "|" + fieldId + "|" + recordId + "|" + keyId)
}

// seal encrypts plain with the current key of the field and returns the
// stored form "enc:v1:<keyId>:<base64(nonce|ciphertext)>".
func (k *fieldKeyring) seal(collectionId, fieldId, recordId string, plain []byte) (string, string, error) {
	if recordId == "" {
		return "", "", errors.New("the record id is required to encrypt a field value")
	}

	k.mu.RLock()
	keyId, ok := k.current[fieldKeyRef(collectionId, fieldId)]
	var entry *fieldKeyEntry
	if ok {
		entry = k.byID[keyId]
	}
	k.mu.RUnlock()

	if entry == nil {
		return "", "", ErrFieldKeyNotLoaded
	}

	nonce := make([]byte, entry.aead.NonceSize())
	if _, err := crand.Read(nonce); err != nil {
		return "", "", err
	}

	sealed := entry.aead.Seal(nonce, nonce, plain, fieldCipherAAD(collectionId, fieldId, recordId, keyId))

	return fieldCipherPrefix + keyId + ":" + base64.RawURLEncoding.EncodeToString(sealed), keyId, nil
}

// open decrypts a stored value. It returns the plaintext and the key id.
func (k *fieldKeyring) open(collectionId, fieldId, recordId, stored string) ([]byte, string, error) {
	rest, ok := strings.CutPrefix(stored, fieldCipherPrefix)
	if !ok {
		return nil, "", errors.New("not an encrypted value")
	}

	keyId, b64, ok := strings.Cut(rest, ":")
	if !ok || keyId == "" {
		return nil, "", errors.New("malformed encrypted value")
	}

	k.mu.RLock()
	entry := k.byID[keyId]
	k.mu.RUnlock()

	if entry == nil {
		return nil, keyId, fmt.Errorf("%w (key id %s)", ErrFieldKeyNotLoaded, keyId)
	}

	if entry.collectionId != collectionId || entry.fieldId != fieldId {
		return nil, keyId, fmt.Errorf("the encrypted value belongs to a different field (key id %s)", keyId)
	}

	raw, err := base64.RawURLEncoding.DecodeString(b64)
	nonceSize := entry.aead.NonceSize()
	if err != nil || len(raw) < nonceSize+entry.aead.Overhead() {
		return nil, keyId, errors.New("malformed encrypted value")
	}

	plain, err := entry.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], fieldCipherAAD(collectionId, fieldId, recordId, keyId))
	if err != nil {
		return nil, keyId, fmt.Errorf("decryption failed for key id %s (wrong key, or the value was moved or altered)", keyId)
	}

	return plain, keyId, nil
}

// ---------------------------------------------------------------------------
// storage (_fieldKeys)
// ---------------------------------------------------------------------------

type fieldKeyRow struct {
	Id           string
	CollectionId string
	FieldId      string
	KeyId        string
	KekId        string
	Wrapped      string
	Retired      string
	Created      string
}

func scanFieldKeyRows(app App, where dbx.Expression) ([]fieldKeyRow, error) {
	q := app.DB().
		Select("id", "collectionRef", "fieldRef", "keyId", "kekId", "wrapped", "retired", "created").
		From(CollectionNameFieldKeys).
		OrderBy("created ASC", dialectOf(app).RowOrder())
	if where != nil {
		q.AndWhere(where)
	}

	var maps []dbx.NullStringMap
	if err := q.All(&maps); err != nil {
		return nil, err
	}

	rows := make([]fieldKeyRow, len(maps))
	for i, m := range maps {
		rows[i] = fieldKeyRow{
			Id:           m["id"].String,
			CollectionId: m["collectionRef"].String,
			FieldId:      m["fieldRef"].String,
			KeyId:        m["keyId"].String,
			KekId:        m["kekId"].String,
			Wrapped:      m["wrapped"].String,
			Retired:      m["retired"].String,
			Created:      m["created"].String,
		}
	}

	return rows, nil
}

// encryptedFieldsOf returns the encrypted fields (any encrypted type) of a collection.
func encryptedFieldsOf(collection *Collection) []Field {
	var result []Field
	for _, f := range collection.Fields {
		if IsEncryptedFieldType(f.Type()) {
			result = append(result, f)
		}
	}
	return result
}

func fieldKeySource(app App) (keys.KeySource, error) {
	if v, ok := app.Store().GetOk(fieldKeySourceStoreKey); ok {
		if src, ok := v.(keys.KeySource); ok {
			return src, nil
		}
	}

	src, err := FieldKeySourceLoader(app)
	if err != nil {
		return nil, err
	}

	app.Store().Set(fieldKeySourceStoreKey, src)

	return src, nil
}

// ensureFieldKeys creates (inside the current transaction) the missing data
// keys of the collection's encrypted fields and registers them.
func ensureFieldKeys(app App, collection *Collection) error {
	fields := encryptedFieldsOf(collection)
	if len(fields) == 0 {
		return nil
	}

	src, err := fieldKeySource(app)
	if err != nil {
		return fmt.Errorf("encrypted fields need a key-encryption key: %w", err)
	}

	for _, f := range fields {
		rows, err := scanFieldKeyRows(app, dbx.HashExp{"collectionRef": collection.Id, "fieldRef": f.GetId(), "retired": ""})
		if err != nil {
			return err
		}

		if len(rows) > 0 {
			// make sure it is registered (e.g. a freshly imported database)
			if _, ok := fieldKeys.currentKeyId(collection.Id, f.GetId()); ok {
				continue
			}
			if err := registerFieldKeyRows(src, rows, true); err != nil {
				return fmt.Errorf("field %s.%s: %w", collection.Name, f.GetName(), err)
			}
			continue
		}

		dek := make([]byte, fieldDEKLength)
		if _, err := crand.Read(dek); err != nil {
			return err
		}

		wrapped, err := src.Wrap(context.Background(), dek)
		if err != nil {
			return fmt.Errorf("failed to wrap the data key of %s.%s: %w", collection.Name, f.GetName(), err)
		}

		idBytes := make([]byte, 8)
		if _, err := crand.Read(idBytes); err != nil {
			return err
		}
		keyId := hex.EncodeToString(idBytes)

		_, err = app.NonconcurrentDB().Insert(CollectionNameFieldKeys, dbx.Params{
			"id":            security.RandomString(15),
			"collectionRef": collection.Id,
			"fieldRef":      f.GetId(),
			"keyId":         keyId,
			"kekId":         src.KeyID(),
			"wrapped":       base64.StdEncoding.EncodeToString(wrapped),
			"retired":       "",
			"created":       types.NowDateTime().String(),
		}).Execute()
		if err != nil {
			return fmt.Errorf("failed to store the data key of %s.%s: %w", collection.Name, f.GetName(), err)
		}

		// registered right away so that records can be saved in the same
		// transaction. If the transaction rolls back, the field does not
		// exist either, and the next save of it creates a new key.
		if err := fieldKeys.add(keyId, collection.Id, f.GetId(), dek, true); err != nil {
			return err
		}
	}

	return nil
}

func registerFieldKeyRows(src keys.KeySource, rows []fieldKeyRow, markCurrent bool) error {
	for _, row := range rows {
		wrapped, err := base64.StdEncoding.DecodeString(row.Wrapped)
		if err != nil {
			return fmt.Errorf("data key %s is corrupted (bad encoding)", row.KeyId)
		}

		dek, err := src.Unwrap(context.Background(), row.KekId, wrapped)
		if err != nil {
			return fmt.Errorf("cannot unwrap data key %s with the %s key source (key id %s, data wrapped under %s): %w", row.KeyId, src.Name(), src.KeyID(), row.KekId, err)
		}

		// rows are ordered oldest first, so the newest active row wins
		if err := fieldKeys.add(row.KeyId, row.CollectionId, row.FieldId, dek, markCurrent && row.Retired == ""); err != nil {
			return err
		}
	}

	return nil
}

// loadFieldKeys unwraps every data key at bootstrap. It fails closed: if any
// collection uses encrypted fields and the key-encryption key is missing or
// wrong, it returns an error and the app does not start.
func loadFieldKeys(app App) error {
	collections, err := app.FindAllCollections()
	if err != nil {
		return err
	}

	live := map[string]string{} // collectionId|fieldId -> "collection.field"
	for _, c := range collections {
		for _, f := range encryptedFieldsOf(c) {
			live[fieldKeyRef(c.Id, f.GetId())] = c.Name + "." + f.GetName()
		}
	}

	rows, err := scanFieldKeyRows(app, nil)
	if err != nil {
		return err
	}

	if len(live) == 0 && len(rows) == 0 {
		return nil // encryption is not in use: nothing to do, nothing to require
	}

	src, srcErr := fieldKeySource(app)
	if srcErr != nil {
		if len(live) == 0 {
			// only orphaned keys of removed fields remain
			return nil
		}
		return fmt.Errorf("cannot start: %d encrypted field(s) are defined but the key-encryption key is unavailable: %w", len(live), srcErr)
	}

	for _, row := range rows {
		ref := fieldKeyRef(row.CollectionId, row.FieldId)
		name, isLive := live[ref]

		err := registerFieldKeyRows(src, []fieldKeyRow{row}, true)
		if err != nil {
			if !isLive {
				app.Logger().Warn("skipping the data key of a removed encrypted field", "keyId", row.KeyId, "error", err)
				continue
			}
			return fmt.Errorf("cannot start: encrypted field %s: %w", name, err)
		}
	}

	for ref, name := range live {
		colId, fieldId, _ := strings.Cut(ref, "|")
		if _, ok := fieldKeys.currentKeyId(colId, fieldId); !ok {
			return fmt.Errorf("cannot start: encrypted field %s has no data key in %s (restore it from a backup of the database)", name, CollectionNameFieldKeys)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// hooks
// ---------------------------------------------------------------------------

func (app *BaseApp) registerFieldEncryptionHooks() {
	app.OnBootstrap().Bind(&hook.Handler[*BootstrapEvent]{
		Id: "__pbFieldKeysBootstrap__",
		Func: func(e *BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}

			if err := loadFieldKeys(e.App); err != nil {
				// do not leave the database connections of a refused start open
				if clearErr := e.App.ClearBootstrap(); clearErr != nil {
					return errors.Join(err, clearErr)
				}
				return err
			}

			return nil
		},
	})

	// create the data keys inside the collection save transaction
	ensure := func(e *CollectionEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		return ensureFieldKeys(e.App, e.Collection)
	}
	app.OnCollectionCreateExecute().Bind(&hook.Handler[*CollectionEvent]{Id: "__pbFieldKeysCreate__", Func: ensure})
	app.OnCollectionUpdateExecute().Bind(&hook.Handler[*CollectionEvent]{Id: "__pbFieldKeysUpdate__", Func: ensure})

	// _fieldKeys is written only by ensureFieldKeys (raw SQL); everything
	// else, including superusers through the records API, is refused
	guard := func(me *ModelEvent) error {
		re, ok := newRecordEventFromModelEvent(me)
		if ok && re.Record.Collection().Name == CollectionNameFieldKeys {
			return fmt.Errorf("the %s collection is managed by the server: %s is not allowed", CollectionNameFieldKeys, me.Type)
		}
		return me.Next()
	}
	handler := func() *hook.Handler[*ModelEvent] {
		return &hook.Handler[*ModelEvent]{Id: "__pbFieldKeysGuard__", Func: guard, Priority: -100000}
	}
	app.OnModelCreate().Bind(handler())
	app.OnModelUpdate().Bind(handler())
	app.OnModelDelete().Bind(handler())
}
