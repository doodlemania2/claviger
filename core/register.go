package core

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"time"
)

// Fork-local (Claviger #87): permanent records (registers) with
// annotation-only corrections. See docs/registers.md and
// docs/specs/80-88-congregations.md section 9.
//
// A "register" is an ordinary base collection that is listed in the
// _permanentCollections system collection. Its entries are drafts until they
// are finalized through the finalize route. A finalized entry can never be
// deleted or changed again, except for the mutable fields (default
// "restricted"). Later corrections and events are annotations in a second
// collection that is append-only.
//
// The guarantee sits at the model layer (register_hooks.go), so it holds for
// the records API, the admin UI, Go code (app.Save) and JS hooks. It does not
// hold for raw SQL, the SQL console, or Go code that runs on
// app.UnsafeWithoutHooks(), exactly like the audit log. The optional hash
// chain (register_chain.go) makes such a change detectable.
//
// Nothing is added to upstream's Collection struct and no hook is added to
// core.App.

const (
	// CollectionNamePermanentCollections is the system collection that lists the registers.
	CollectionNamePermanentCollections = "_permanentCollections"

	// RegistrarTag is the org tag (an _orgRoles row of the entry's org) that
	// lets a member finalize and annotate entries. It is the same tag that
	// makes a member staff for households.
	RegistrarTag = HouseholdRegistrarTag

	// DefaultFinalizeField is the date field that marks an entry final.
	DefaultFinalizeField = "finalizedAt"

	// audit actions
	AuditActionRegisterFinalizeRequest  = "register_finalize_request"
	AuditActionRegisterFinalizeWithdraw = "register_finalize_withdraw"
	AuditActionRegisterFinalize         = "register_finalize"
	AuditActionRegisterAnnotate         = "register_annotate"
	AuditActionRegisterCertificate      = "register_certificate"
	AuditActionRegisterErasureNote      = "register_erasure_note"

	// annotation kinds
	RegisterKindCorrection     = "correction"
	RegisterKindLaterEvent     = "later_event"
	RegisterKindLegalNote      = "legal_note"
	RegisterKindRetraction     = "retraction"
	RegisterKindDataSubject    = "data_subject_note"
	registerAnnotationTextMax  = 5000
	registerAnnotationReasonMx = 1000

	registerCacheTTL = time.Minute
	registerCacheKey = "__clvPermanentCollections__"
)

// RegisterAnnotationKinds are the values of the kind field of an annotation.
var RegisterAnnotationKinds = []string{
	RegisterKindCorrection, RegisterKindLaterEvent, RegisterKindLegalNote,
	RegisterKindRetraction, RegisterKindDataSubject,
}

// registerManagedBase are the fields next to the finalize field that only the
// finalize functions may write.
var registerManagedBase = []string{"finalizedBy", "secondSigner", "pendingBy", "pendingAt", "pendingHash", "seq", "hash"}

var registerNameRegex = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,99}$`)

type registerCtxKey struct{}

// registerWriteContext marks a save as made by the finalize functions. The key
// type is unexported, so no hook, route or script can forge it.
func registerWriteContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	return context.WithValue(ctx, registerCtxKey{}, true)
}

// PermanentConfig is the code form of one _permanentCollections row.
type PermanentConfig struct {
	Id                   string
	Collection           string
	FinalizeField        string
	MutableFields        []string
	AnnotationCollection string
	Chain                bool
	// Signers is 1 (a registrar finalizes alone) or 2 (a registrar requests, a
	// different person confirms).
	Signers        int
	SubjectPaths   []string
	PersonalFields []string
	RetainReason   string
}

// Managed returns the fields that only the finalize functions may write.
func (c *PermanentConfig) Managed() []string {
	return append([]string{c.FinalizeField}, registerManagedBase...)
}

func (c *PermanentConfig) isManaged(name string) bool { return slices.Contains(c.Managed(), name) }

func (c *PermanentConfig) isMutable(name string) bool { return slices.Contains(c.MutableFields, name) }

func permanentConfigFromRecord(r *Record) *PermanentConfig {
	c := &PermanentConfig{
		Id:                   r.Id,
		Collection:           r.GetString("collection"),
		FinalizeField:        r.GetString("finalizeField"),
		AnnotationCollection: r.GetString("annotationCollection"),
		Chain:                r.GetBool("chain"),
		Signers:              r.GetInt("signers"),
		RetainReason:         r.GetString("retainReason"),
	}

	if c.FinalizeField == "" {
		c.FinalizeField = DefaultFinalizeField
	}

	if c.Signers != 2 {
		c.Signers = 1
	}

	_ = r.UnmarshalJSONField("mutableFields", &c.MutableFields)
	_ = r.UnmarshalJSONField("subjectPaths", &c.SubjectPaths)
	_ = r.UnmarshalJSONField("personalFields", &c.PersonalFields)

	return c
}

type permanentCache struct {
	at           time.Time
	byRegister   map[string]*PermanentConfig
	byAnnotation map[string]*PermanentConfig
}

// permanentConfigs returns the configured registers, cached for a minute and
// reset whenever a row of _permanentCollections changes.
func permanentConfigs(app App) *permanentCache {
	if c, ok := app.Store().Get(registerCacheKey).(*permanentCache); ok && c != nil && time.Since(c.at) < registerCacheTTL {
		return c
	}

	c := &permanentCache{at: time.Now(), byRegister: map[string]*PermanentConfig{}, byAnnotation: map[string]*PermanentConfig{}}

	if _, err := app.FindCachedCollectionByNameOrId(CollectionNamePermanentCollections); err == nil {
		rows, err := app.FindAllRecords(CollectionNamePermanentCollections)
		if err != nil {
			// do not cache a failed read; hooks treat this as "no registers"
			return c
		}

		for _, r := range rows {
			cfg := permanentConfigFromRecord(r)
			c.byRegister[cfg.Collection] = cfg

			if cfg.AnnotationCollection != "" {
				c.byAnnotation[cfg.AnnotationCollection] = cfg
			}
		}
	}

	app.Store().Set(registerCacheKey, c)

	return c
}

func resetPermanentConfigs(app App) { app.Store().Remove(registerCacheKey) }

// PermanentConfigFor returns the register configuration of a collection name,
// or nil when the collection is not a register.
func PermanentConfigFor(app App, collection string) *PermanentConfig {
	return permanentConfigs(app).byRegister[collection]
}

// PermanentConfigs lists the configured registers.
func PermanentConfigs(app App) []*PermanentConfig {
	c := permanentConfigs(app)
	out := make([]*PermanentConfig, 0, len(c.byRegister))

	for _, cfg := range c.byRegister {
		out = append(out, cfg)
	}

	slices.SortFunc(out, func(a, b *PermanentConfig) int {
		switch {
		case a.Collection < b.Collection:
			return -1
		case a.Collection > b.Collection:
			return 1
		}

		return 0
	})

	return out
}

// IsFinalized reports whether the entry is final.
func (c *PermanentConfig) IsFinalized(r *Record) bool {
	return !r.GetDateTime(c.FinalizeField).IsZero()
}

// ---- value helpers ---------------------------------------------------------

// registerNorm round-trips a value through JSON so that two representations
// of the same content (a map and the stored raw JSON, a string slice and the
// decoded array) compare and hash the same.
func registerNorm(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}

	var x any
	if err := json.Unmarshal(b, &x); err != nil {
		return string(b)
	}

	return x
}

func registerIsZero(x any) bool {
	switch t := x.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case bool:
		return !t
	case float64:
		return t == 0
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}

	return false
}

func registerFieldValue(r *Record, name string) any { return registerNorm(r.GetRaw(name)) }

func registerFieldEqual(a, b *Record, name string) bool {
	return reflect.DeepEqual(registerFieldValue(a, name), registerFieldValue(b, name))
}

// registerChangedFields lists the fields (except autodate fields) whose value
// differs between the stored row and the record about to be saved.
func registerChangedFields(stored, rec *Record) []string {
	var changed []string

	for _, f := range rec.Collection().Fields {
		if _, ok := f.(*AutodateField); ok {
			continue
		}

		if !registerFieldEqual(stored, rec, f.GetName()) {
			changed = append(changed, f.GetName())
		}
	}

	return changed
}

// ---- audit -------------------------------------------------------------------

func writeRegisterAudit(app App, ctx context.Context, action string, cfg *PermanentConfig, entry *Record, extra map[string]any) error {
	if !auditAvailable(app) {
		return nil
	}

	meta := map[string]any{
		"orgId":      entry.GetString("org"),
		"collection": cfg.Collection,
		"entry":      entry.Id,
	}

	for k, v := range extra {
		meta[k] = v
	}

	e := &AuditEntry{
		Action:         action,
		CollectionName: cfg.Collection,
		RecordId:       entry.Id,
		Meta:           meta,
	}

	if c, err := app.FindCachedCollectionByNameOrId(cfg.Collection); err == nil {
		e.CollectionId = c.Id
	}

	applyAuditContext(ctx, e)

	return WriteAuditEntry(app, ctx, e, nil)
}

// WriteRegisterCertificateAudit records that a certificate was issued.
func WriteRegisterCertificateAudit(app App, ctx context.Context, cfg *PermanentConfig, entry *Record, extra map[string]any) error {
	return writeRegisterAudit(app, ctx, AuditActionRegisterCertificate, cfg, entry, extra)
}

// PersonName returns a display name for the id of a user or a superuser (the
// name field, else the email), "" for an empty or unknown id.
func PersonName(app App, id string) string {
	if id == "" || id == "system" {
		return ""
	}

	look := func(r *Record) string {
		if n := r.GetString("name"); n != "" {
			return n
		}

		return r.Email()
	}

	if col, err := OrgUserCollection(app); err == nil {
		if r, err := app.FindRecordById(col, id); err == nil {
			return look(r)
		}
	}

	if r, err := app.FindRecordById(CollectionNameSuperusers, id); err == nil {
		return look(r)
	}

	return ""
}
