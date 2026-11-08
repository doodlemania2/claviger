package core

import (
	"context"
	"database/sql/driver"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"ganigeorgiev.com/pocketbase/ozzo-validation/v4/is"
	"github.com/doodlemania2/claviger/core/validators"
	"github.com/doodlemania2/claviger/tools/types"
)

func init() {
	Fields[FieldTypeEncryptedJSON] = func() Field {
		return &EncryptedJSONField{}
	}
}

// FieldTypeEncryptedJSON is the fork-only "encryptedJSON" field type (#75).
const FieldTypeEncryptedJSON = "encryptedJSON"

var (
	_ Field                 = (*EncryptedJSONField)(nil)
	_ SetterFinder          = (*EncryptedJSONField)(nil)
	_ DriverValuer          = (*EncryptedJSONField)(nil)
	_ MaxBodySizeCalculator = (*EncryptedJSONField)(nil)
)

// EncryptedJSONField defines the "encryptedJSON" type field: a JSON value that
// is stored encrypted, with the same envelope scheme, ciphertext format and
// record binding as [EncryptedTextField]. The JSON text is sealed as a whole;
// above the database layer the value is an ordinary [types.JSONRaw].
//
// The column is plain TEXT (not a JSON column): it holds an opaque string. The
// null and empty JSON values are stored as an empty string.
//
// The field cannot be filtered on, sorted by, indexed or used in API rules.
type EncryptedJSONField struct {
	// Name (required) is the unique name of the field.
	Name string `form:"name" json:"name"`

	// Id is the unique stable field identifier (bound into the ciphertexts,
	// so renaming the field is safe).
	Id string `form:"id" json:"id"`

	// System prevents the renaming and removal of the field.
	System bool `form:"system" json:"system"`

	// Hidden hides the field from the API response.
	Hidden bool `form:"hidden" json:"hidden"`

	// Help is an extra text explaining what the field is about.
	Help string `form:"help" json:"help"`

	// MaxSize specifies the maximum size of the plaintext JSON in bytes.
	//
	// If zero, a default limit of [DefaultJSONFieldMaxSize] (1MB) is applied.
	MaxSize int64 `form:"maxSize" json:"maxSize"`

	// Required will require the field value to be non-empty JSON value
	// (aka. not "null", `""`, "[]", "{}").
	Required bool `form:"required" json:"required"`
}

// Type implements [Field.Type] interface method.
func (f *EncryptedJSONField) Type() string { return FieldTypeEncryptedJSON }

// GetId implements [Field.GetId] interface method.
func (f *EncryptedJSONField) GetId() string { return f.Id }

// SetId implements [Field.SetId] interface method.
func (f *EncryptedJSONField) SetId(id string) { f.Id = id }

// GetName implements [Field.GetName] interface method.
func (f *EncryptedJSONField) GetName() string { return f.Name }

// SetName implements [Field.SetName] interface method.
func (f *EncryptedJSONField) SetName(name string) { f.Name = name }

// GetSystem implements [Field.GetSystem] interface method.
func (f *EncryptedJSONField) GetSystem() bool { return f.System }

// SetSystem implements [Field.SetSystem] interface method.
func (f *EncryptedJSONField) SetSystem(system bool) { f.System = system }

// GetHidden implements [Field.GetHidden] interface method.
func (f *EncryptedJSONField) GetHidden() bool { return f.Hidden }

// SetHidden implements [Field.SetHidden] interface method.
func (f *EncryptedJSONField) SetHidden(hidden bool) { f.Hidden = hidden }

// ColumnType implements [Field.ColumnType] interface method.
//
// Plain TEXT on purpose: the stored value is an opaque string, and a JSON
// column type would make the database validate or index ciphertext.
func (f *EncryptedJSONField) ColumnType(app App) string {
	return "TEXT DEFAULT '' NOT NULL"
}

// PrepareValue implements [Field.PrepareValue] interface method.
//
// It is called for values scanned from the database (and for the zero value),
// never for user input, which goes through the setter.
func (f *EncryptedJSONField) PrepareValue(record *Record, raw any) (any, error) {
	switch v := raw.(type) {
	case types.JSONRaw:
		return v, nil
	case nil:
		return types.JSONRaw{}, nil
	case string:
		plain, err := sealedPrepare(record, f.Name, f.Id, v)
		if err != nil {
			return nil, err
		}
		if plain == "" {
			return types.JSONRaw{}, nil
		}
		return types.JSONRaw(plain), nil
	default:
		return (&JSONField{}).PrepareValue(record, raw)
	}
}

// DriverValue implements the [DriverValuer] interface.
func (f *EncryptedJSONField) DriverValue(record *Record) (driver.Value, error) {
	raw, _ := record.GetRaw(f.Name).(types.JSONRaw)

	plain := string(raw)
	if strings.TrimSpace(plain) == "null" {
		plain = ""
	}

	return sealedDriverValue(record, f.Name, f.Id, plain)
}

// FindSetter implements the [SetterFinder] interface.
//
// User input is JSON, never ciphertext: it is parsed like a "json" field
// value (no decryption), and a plain string that merely looks like
// ciphertext is stored as a JSON string.
func (f *EncryptedJSONField) FindSetter(key string) SetterFunc {
	if key != f.Name {
		return nil
	}

	return func(record *Record, raw any) {
		v, err := (&JSONField{}).PrepareValue(record, raw)
		if err != nil {
			record.SetRaw(f.Name, raw) // fails validation with an unsupported type
			return
		}
		record.SetRaw(f.Name, v)
	}
}

// ValidateValue implements [Field.ValidateValue] interface method.
func (f *EncryptedJSONField) ValidateValue(ctx context.Context, app App, record *Record) error {
	raw, ok := record.GetRaw(f.Name).(types.JSONRaw)
	if !ok {
		return validators.ErrUnsupportedValueType
	}

	maxSize := f.CalculateMaxBodySize()

	if int64(len(raw)) > maxSize {
		return validation.NewError(
			"validation_json_size_limit",
			"The maximum allowed JSON size is {{.maxSize}} bytes",
		).SetParams(map[string]any{"maxSize": maxSize})
	}

	if is.JSON.Validate(raw) != nil {
		return validation.NewError("validation_invalid_json", "Must be a valid json value")
	}

	if len(raw) > 0 {
		var dummy any
		if err := json.Unmarshal(raw, &dummy, jsontext.AllowInvalidUTF8(true)); err != nil {
			return validation.NewError("validation_invalid_json", "Must be a valid json value")
		}
	}

	if f.Required && slices.Contains(emptyJSONValues, strings.TrimSpace(raw.String())) {
		return validation.ErrRequired
	}

	return nil
}

// ValidateSettings implements [Field.ValidateSettings] interface method.
func (f *EncryptedJSONField) ValidateSettings(ctx context.Context, app App, collection *Collection) error {
	return validation.ValidateStruct(f,
		validation.Field(&f.Id, validation.By(DefaultFieldIdValidationRule)),
		validation.Field(&f.Name,
			validation.By(DefaultFieldNameValidationRule),
			validation.By(sealedCheckAfterId(collection, f.Name)),
			validation.By(sealedCheckNotIndexed(collection, f.Name)),
			validation.By(sealedCheckKeySource(app)),
		),
		validation.Field(&f.Help, validation.By(DefaultFieldHelpValidationRule)),
		validation.Field(&f.MaxSize, validation.Min(0), validation.Max(maxSafeJSONInt)),
	)
}

// CalculateMaxBodySize implements the [MaxBodySizeCalculator] interface.
func (f *EncryptedJSONField) CalculateMaxBodySize() int64 {
	if f.MaxSize <= 0 {
		return DefaultJSONFieldMaxSize
	}
	return f.MaxSize
}
