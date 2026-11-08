package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/security"
	"github.com/doodlemania2/claviger/tools/types"
)

// CollectionNameTOTPs is the name of the system collection that stores the
// TOTP (authenticator app) enrollment of auth records: one row per record.
//
// Every field is hidden and there are no API rules, so the generic records API
// never exposes it to anyone but superusers, and not even they see its values.
const CollectionNameTOTPs = "_totps"

const totpEncryptedPrefix = "enc:"

var (
	_ Model        = (*TOTP)(nil)
	_ PreValidator = (*TOTP)(nil)
	_ RecordProxy  = (*TOTP)(nil)
)

// TOTP defines a Record proxy for working with the _totps collection.
//
// A row exists from the moment enrollment begins (Enabled() == false, the
// "pending" state) and becomes active when the first code is confirmed.
type TOTP struct {
	*Record
}

// NewTOTP instantiates and returns a new blank *TOTP model.
func NewTOTP(app App) *TOTP {
	m := &TOTP{}

	c, err := app.FindCachedCollectionByNameOrId(CollectionNameTOTPs)
	if err != nil {
		// system collection, always expected to exist (checked in PreValidate)
		c = NewBaseCollection("@__invalid__")
	}

	m.Record = NewRecord(c)

	return m
}

// PreValidate implements the [PreValidator] interface.
func (m *TOTP) PreValidate(ctx context.Context, app App) error {
	if m.Record == nil || m.Record.Collection().Name != CollectionNameTOTPs {
		return errors.New("missing or invalid totp ProxyRecord")
	}

	return nil
}

// ProxyRecord returns the proxied Record model.
func (m *TOTP) ProxyRecord() *Record {
	return m.Record
}

// SetProxyRecord loads the specified record model into the current proxy.
func (m *TOTP) SetProxyRecord(record *Record) {
	m.Record = record
}

// CollectionRef returns the "collectionRef" field value.
func (m *TOTP) CollectionRef() string {
	return m.GetString("collectionRef")
}

// SetCollectionRef updates the "collectionRef" field value.
func (m *TOTP) SetCollectionRef(collectionId string) {
	m.Set("collectionRef", collectionId)
}

// RecordRef returns the "recordRef" field value.
func (m *TOTP) RecordRef() string {
	return m.GetString("recordRef")
}

// SetRecordRef updates the "recordRef" field value.
func (m *TOTP) SetRecordRef(recordId string) {
	m.Set("recordRef", recordId)
}

// Enabled reports whether the enrollment was confirmed (TOTP is active).
func (m *TOTP) Enabled() bool {
	return m.GetBool("enabled")
}

// SetEnabled updates the "enabled" field value.
func (m *TOTP) SetEnabled(enabled bool) {
	m.Set("enabled", enabled)
}

// LastStep returns the time step of the last accepted code (replay guard).
func (m *TOTP) LastStep() int64 {
	return int64(m.GetFloat("lastStep"))
}

// SetLastStep updates the "lastStep" field value.
func (m *TOTP) SetLastStep(step int64) {
	m.Set("lastStep", step)
}

// SetSecret stores the base32 secret.
//
// When the app encryption key is configured (the env var named by
// App.EncryptionEnv(), the same key and AES-256-GCM helper used for the
// settings) the secret is encrypted and stored as "enc:<base64>". Otherwise
// it is stored as is.
func (m *TOTP) SetSecret(app App, secret string) error {
	key := os.Getenv(app.EncryptionEnv())
	if key == "" {
		m.Set("secret", secret)
		return nil
	}

	encrypted, err := security.Encrypt([]byte(secret), key)
	if err != nil {
		return fmt.Errorf("failed to encrypt the TOTP secret: %w", err)
	}

	m.Set("secret", totpEncryptedPrefix+encrypted)

	return nil
}

// Secret returns the plain base32 secret, decrypting it when needed.
//
// A secret stored encrypted needs the encryption key to be configured.
// Secrets stored before the key was configured stay readable.
func (m *TOTP) Secret(app App) (string, error) {
	stored := m.GetString("secret")

	encrypted, ok := strings.CutPrefix(stored, totpEncryptedPrefix)
	if !ok {
		return stored, nil
	}

	key := os.Getenv(app.EncryptionEnv())
	if key == "" {
		return "", fmt.Errorf("the TOTP secret is encrypted but the encryption key env %q is not set", app.EncryptionEnv())
	}

	plain, err := security.Decrypt(encrypted, key)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt the TOTP secret: %w", err)
	}

	return string(plain), nil
}

// BackupCodeHashes returns the stored bcrypt hashes of the unused backup codes.
func (m *TOTP) BackupCodeHashes() []string {
	raw := m.GetRaw("backupCodes")

	var data []byte
	switch v := raw.(type) {
	case nil:
		return nil
	case types.JSONRaw:
		data = v
	case []string:
		return v
	case string:
		data = []byte(v)
	case []byte:
		data = v
	default:
		var err error
		if data, err = json.Marshal(v); err != nil {
			return nil
		}
	}

	var result []string
	if err := json.Unmarshal(data, &result); err != nil {
		return nil
	}

	return result
}

// SetBackupCodeHashes replaces the stored backup code hashes.
func (m *TOTP) SetBackupCodeHashes(hashes []string) {
	if hashes == nil {
		hashes = []string{}
	}
	m.Set("backupCodes", hashes)
}

// -------------------------------------------------------------------

// FindTOTPByRecord returns the TOTP enrollment of the provided auth record
// (sql.ErrNoRows if there is none).
func FindTOTPByRecord(app App, authRecord *Record) (*TOTP, error) {
	result := &TOTP{}

	err := app.RecordQuery(CollectionNameTOTPs).
		AndWhere(dbx.HashExp{
			"collectionRef": authRecord.Collection().Id,
			"recordRef":     authRecord.Id,
		}).
		Limit(1).
		One(result)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// HasActiveTOTP reports whether the auth record has a confirmed TOTP enrollment.
func HasActiveTOTP(app App, authRecord *Record) (bool, error) {
	var exists int

	err := app.DB().
		Select("(1)").
		From(CollectionNameTOTPs).
		Where(dbx.HashExp{
			"collectionRef": authRecord.Collection().Id,
			"recordRef":     authRecord.Id,
			"enabled":       true,
		}).
		Limit(1).
		Row(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}

	return exists > 0, nil
}

// DeleteTOTPByRecord removes the TOTP enrollment of the auth record (if any).
// The delete goes through the model hooks, so it is audited.
func DeleteTOTPByRecord(app App, authRecord *Record) error {
	m, err := FindTOTPByRecord(app, authRecord)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}

	return app.Delete(m)
}

func (app *BaseApp) registerTOTPHooks() {
	recordRefHooks[*TOTP](app, CollectionNameTOTPs, CollectionTypeAuth)
}
