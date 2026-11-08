package core

import (
	"database/sql/driver"
	"fmt"
	"regexp"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/tools/dbutils"
	"strings"
)

// Fork-local (Claviger #75): the helpers shared by the encrypted field types
// (encryptedText, encryptedJSON). A field stores a string; the helpers seal it
// on write and open it on read, reuse the stored ciphertext when the value did
// not change, and validate the schema rules every encrypted field shares.

// IsEncryptedFieldType reports whether the field type stores its value
// encrypted (and therefore cannot be filtered, sorted, indexed, used in rules,
// and must be redacted by type in the audit log and webhooks).
func IsEncryptedFieldType(fieldType string) bool {
	return fieldType == FieldTypeEncryptedText || fieldType == FieldTypeEncryptedJSON
}

// sealedStash remembers the ciphertext a value was loaded from (or last
// sealed to), so that saving an unchanged value stores the identical
// ciphertext instead of sealing again. It lives in the record's original data
// under a key no field can have, which Record.Original() copies.
type sealedStash struct {
	plain    string
	stored   string
	recordId string
	keyId    string
}

func sealedStashKey(fieldId string) string {
	return "\x00enc:" + fieldId
}

func sealedGetStash(record *Record, fieldId string) *sealedStash {
	s, _ := record.originalData[sealedStashKey(fieldId)].(*sealedStash)
	return s
}

func sealedSetStash(record *Record, fieldId string, s *sealedStash) {
	if record.originalData == nil {
		return
	}
	record.originalData[sealedStashKey(fieldId)] = s
}

// sealedPrepare turns a stored column value into plaintext.
//
// A value with the "enc:v1:" prefix is decrypted. Anything else is legacy
// plaintext (a column converted from text/json), returned as is, unless the
// field is in strict mode, where it is an error.
func sealedPrepare(record *Record, name, fieldId, stored string) (string, error) {
	if !strings.HasPrefix(stored, fieldCipherPrefix) {
		if stored != "" && fieldKeys.isStrict(record.Collection().Id, fieldId) {
			return "", fmt.Errorf("encrypted field %q of record %q holds an unencrypted value and the field is in strict mode (run `keys convert` to encrypt the legacy values)", name, record.Id)
		}
		return stored, nil
	}

	if record.Id == "" {
		return "", fmt.Errorf("encrypted field %q: the record id is not available yet (the \"id\" field must come first in the collection)", name)
	}

	plain, keyId, err := fieldKeys.open(record.Collection().Id, fieldId, record.Id, stored)
	if err != nil {
		return "", fmt.Errorf("encrypted field %q of record %q: %w", name, record.Id, err)
	}

	sealedSetStash(record, fieldId, &sealedStash{plain: string(plain), stored: stored, recordId: record.Id, keyId: keyId})

	return string(plain), nil
}

// sealedDriverValue returns what is stored in the column for the plaintext.
// The empty plaintext is stored as an empty string.
func sealedDriverValue(record *Record, name, fieldId, plain string) (driver.Value, error) {
	if plain == "" {
		return "", nil
	}

	collectionId := record.Collection().Id

	// unchanged value, same record and still the current key: keep the stored ciphertext
	if s := sealedGetStash(record, fieldId); s != nil && s.plain == plain && s.recordId == record.Id {
		if cur, _ := fieldKeys.currentKeyId(collectionId, fieldId); cur == s.keyId {
			return s.stored, nil
		}
	}

	stored, keyId, err := fieldKeys.seal(collectionId, fieldId, record.Id, []byte(plain))
	if err != nil {
		return nil, fmt.Errorf("encrypted field %q: %w", name, err)
	}

	sealedSetStash(record, fieldId, &sealedStash{plain: plain, stored: stored, recordId: record.Id, keyId: keyId})

	return stored, nil
}

// sealedCheckAfterId requires the "id" field to precede the field: the record
// id is part of the authenticated data, and values are decrypted field by
// field while a row is scanned.
func sealedCheckAfterId(collection *Collection, name string) validation.RuleFunc {
	return func(any) error {
		for _, field := range collection.Fields {
			if field.GetName() == FieldNameId {
				return nil
			}
			if field.GetName() == name {
				break
			}
		}
		return validation.NewError("validation_encrypted_field_before_id", `An encrypted field must come after the "id" field.`)
	}
}

// sealedCheckNotIndexed rejects every index that mentions the field, unique
// ones included: the column holds random ciphertext, so an index could never
// enforce or accelerate anything, only give a false sense of it.
func sealedCheckNotIndexed(collection *Collection, name string) validation.RuleFunc {
	return func(any) error {
		if name == "" {
			return nil
		}

		ref := regexp.MustCompile(`(?i)(^|[^\w])` + regexp.QuoteMeta(name) + `([^\w]|$)`)

		for _, idx := range collection.Indexes {
			parsed := dbutils.ParseIndex(idx)

			hit := ref.MatchString(parsed.Where)
			for _, col := range parsed.Columns {
				if ref.MatchString(col.Name) {
					hit = true
				}
			}

			if hit {
				return validation.NewError(
					"validation_encrypted_field_indexed",
					"Encrypted fields cannot be indexed (the index "+parsed.IndexName+" uses it). Remove the index.",
				)
			}
		}

		return nil
	}
}

// sealedCheckKeySource fails the collection save early, with a clear message,
// when there is no key-encryption key to protect the field's data key with.
func sealedCheckKeySource(app App) validation.RuleFunc {
	return func(any) error {
		if app == nil {
			return nil
		}

		if _, err := fieldKeySource(app); err != nil {
			return validation.NewError("validation_encrypted_field_no_key", "Encrypted fields need a key-encryption key: "+err.Error())
		}

		return nil
	}
}
