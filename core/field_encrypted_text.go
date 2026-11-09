package core

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core/validators"
	"github.com/doodlemania2/claviger/tools/router"
	"github.com/spf13/cast"
)

func init() {
	Fields[FieldTypeEncryptedText] = func() Field {
		return &EncryptedTextField{}
	}
}

// FieldTypeEncryptedText is the fork-only "encryptedText" field type (#75).
const FieldTypeEncryptedText = "encryptedText"

const (
	encryptedTextDefaultMax = 5000

	encryptedFieldNotFilterableMsg = "Encrypted fields cannot be used in filters, sorts or API rules"
)

var (
	_ Field        = (*EncryptedTextField)(nil)
	_ SetterFinder = (*EncryptedTextField)(nil)
	_ DriverValuer = (*EncryptedTextField)(nil)
)

// EncryptedTextField defines the "encryptedText" type field: a string that is
// stored encrypted (AES-256-GCM) in the database and is plaintext everywhere
// above the database layer (records API, hooks, realtime).
//
// The stored form is "enc:v1:<keyId>:<base64url(nonce|ciphertext)>". The
// authenticated data binds the ciphertext to the collection id, the field id,
// the record id and the key id, so a ciphertext copied into another record or
// field does not decrypt.
//
// The respective zero record field value is empty string, which is stored as
// an empty string (so which rows are empty is not hidden).
//
// The field cannot be filtered on, sorted by, indexed or used in API rules.
type EncryptedTextField struct {
	// Name (required) is the unique name of the field.
	Name string `form:"name" json:"name"`

	// Id is the unique stable field identifier.
	//
	// It is automatically generated from the name when adding to a collection FieldsList.
	// The id (not the name) is bound into the ciphertexts, so renaming the field is safe.
	Id string `form:"id" json:"id"`

	// System prevents the renaming and removal of the field.
	System bool `form:"system" json:"system"`

	// Hidden hides the field from the API response.
	Hidden bool `form:"hidden" json:"hidden"`

	// Help is an extra text explaining what the field is about.
	// It is usually shown in Dashboard UI under the field input.
	Help string `form:"help" json:"help"`

	// Min specifies the minimum required plaintext characters.
	//
	// if zero value, no min limit is applied.
	Min int `form:"min" json:"min"`

	// Max specifies the maximum allowed plaintext characters.
	//
	// If zero, a default limit of 5000 is applied.
	Max int `form:"max" json:"max"`

	// Required will require the field value to be non-empty string.
	Required bool `form:"required" json:"required"`
}

// Type implements [Field.Type] interface method.
func (f *EncryptedTextField) Type() string { return FieldTypeEncryptedText }

// GetId implements [Field.GetId] interface method.
func (f *EncryptedTextField) GetId() string { return f.Id }

// SetId implements [Field.SetId] interface method.
func (f *EncryptedTextField) SetId(id string) { f.Id = id }

// GetName implements [Field.GetName] interface method.
func (f *EncryptedTextField) GetName() string { return f.Name }

// SetName implements [Field.SetName] interface method.
func (f *EncryptedTextField) SetName(name string) { f.Name = name }

// GetSystem implements [Field.GetSystem] interface method.
func (f *EncryptedTextField) GetSystem() bool { return f.System }

// SetSystem implements [Field.SetSystem] interface method.
func (f *EncryptedTextField) SetSystem(system bool) { f.System = system }

// GetHidden implements [Field.GetHidden] interface method.
func (f *EncryptedTextField) GetHidden() bool { return f.Hidden }

// SetHidden implements [Field.SetHidden] interface method.
func (f *EncryptedTextField) SetHidden(hidden bool) { f.Hidden = hidden }

// ColumnType implements [Field.ColumnType] interface method.
//
// Plain TEXT on purpose: the stored value is an opaque string.
func (f *EncryptedTextField) ColumnType(app App) string {
	return dialectOf(app).TextColumn()
}

// PrepareValue implements [Field.PrepareValue] interface method.
//
// It is called for values scanned from the database (and for the zero value),
// never for user input, which goes through the setter. A stored value that
// carries the "enc:v1:" prefix is decrypted; anything else is taken as
// plaintext, which is how a column converted from "text" stays readable until
// its rows are saved again (unless the field is in strict mode).
func (f *EncryptedTextField) PrepareValue(record *Record, raw any) (any, error) {
	return sealedPrepare(record, f.Name, f.Id, cast.ToString(raw))
}

// DriverValue implements the [DriverValuer] interface.
func (f *EncryptedTextField) DriverValue(record *Record) (driver.Value, error) {
	plain, ok := record.GetRaw(f.Name).(string)
	if !ok {
		plain = cast.ToString(record.GetRaw(f.Name))
	}

	return sealedDriverValue(record, f.Name, f.Id, plain)
}

// FindSetter implements the [SetterFinder] interface.
//
// Without it the default setter would call PrepareValue on user input, which
// would try to decrypt strings that merely look like ciphertext.
func (f *EncryptedTextField) FindSetter(key string) SetterFunc {
	if key != f.Name {
		return nil
	}

	return func(record *Record, raw any) {
		record.SetRaw(f.Name, cast.ToString(raw))
	}
}

// ValidateValue implements [Field.ValidateValue] interface method.
func (f *EncryptedTextField) ValidateValue(ctx context.Context, app App, record *Record) error {
	val, ok := record.GetRaw(f.Name).(string)
	if !ok {
		return validators.ErrUnsupportedValueType
	}

	if f.Required {
		if err := validation.Required.Validate(val); err != nil {
			return err
		}
	}

	if val == "" {
		return nil // nothing to check
	}

	// the prefix marks stored ciphertext; refusing it on input means a
	// ciphertext pasted from another record is rejected instead of being
	// stored as (and later returned as) a literal string
	if strings.HasPrefix(val, fieldCipherPrefix) {
		return validation.NewError("validation_reserved_prefix", "The value cannot start with the reserved prefix {{.prefix}}.").
			SetParams(map[string]any{"prefix": fieldCipherPrefix})
	}

	length := len([]rune(val))

	if f.Min > 0 && length < f.Min {
		return validation.NewError("validation_min_text_constraint", "Must be at least {{.min}} character(s).").
			SetParams(map[string]any{"min": f.Min})
	}

	max := f.Max
	if max == 0 {
		max = encryptedTextDefaultMax
	}

	if max > 0 && length > max {
		return validation.NewError("validation_max_text_constraint", "Must be no more than {{.max}} character(s).").
			SetParams(map[string]any{"max": max})
	}

	return nil
}

// ValidateSettings implements [Field.ValidateSettings] interface method.
func (f *EncryptedTextField) ValidateSettings(ctx context.Context, app App, collection *Collection) error {
	return validation.ValidateStruct(f,
		validation.Field(&f.Id, validation.By(DefaultFieldIdValidationRule)),
		validation.Field(&f.Name,
			validation.By(DefaultFieldNameValidationRule),
			validation.By(sealedCheckAfterId(collection, f.Name)),
			validation.By(sealedCheckNotIndexed(collection, f.Name)),
			validation.By(sealedCheckKeySource(app)),
		),
		validation.Field(&f.Help, validation.By(DefaultFieldHelpValidationRule)),
		validation.Field(&f.Min, validation.Min(0), validation.Max(maxSafeJSONInt)),
		validation.Field(&f.Max, validation.Min(f.Min), validation.Max(maxSafeJSONInt)),
	)
}

// checkEncryptedFieldFilterable is called by the record field resolver for the
// last segment of every filter, sort and API rule path. It is the only
// upstream hook of the feature.
func checkEncryptedFieldFilterable(field Field) error {
	if field != nil && IsEncryptedFieldType(field.Type()) {
		return newEncryptedFieldFilterError(field.GetName())
	}
	return nil
}

func newEncryptedFieldFilterError(name string) error {
	return router.NewBadRequestError(fmt.Sprintf("%s: %q is an encrypted field.", encryptedFieldNotFilterableMsg, name), nil)
}
