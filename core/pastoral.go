package core

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
)

// Fork-local (Claviger #86, 86-1): the pastoral notes template.
//
// A pastoral notes collection holds prayer requests and pastoral care notes
// with four visibility levels (private, clergy, care, congregation), an
// optional seal (author only) and break-glass access for emergencies
// (breakglass.go). The rules are locked: PastoralRules builds the one rule set
// the template creates, CreatePastoralNotes refuses any looser rule unless the
// caller says so on purpose, and nothing in this file gives an org owner or
// admin an automatic right to read. The org presets of #79a and the ancestor
// presets of #84 must never be applied to this collection.
//
// Sacramental confession (the seal of confession) must not be stored in
// Claviger at all. See docs/pastoral-notes.md.

const (
	CollectionNameBreakGlass    = "_breakGlass"
	CollectionNamePastoralHolds = "_pastoralHolds"

	// the org tags (rows of _orgRoles) the rules and routes look at
	PastoralTagClergy     = "clergy"
	PastoralTagCare       = "care-team"
	PastoralTagBreakGlass = "breakglass"
	PastoralTagModerator  = "moderator"

	PastoralVisibilityPrivate      = "private"
	PastoralVisibilityClergy       = "clergy"
	PastoralVisibilityCare         = "care"
	PastoralVisibilityCongregation = "congregation"

	// DefaultPastoralNotesCollection is the name the template suggests.
	DefaultPastoralNotesCollection = "pastoral_notes"

	pastoralBodyMax = 20000
)

// PastoralVisibilities are the values of the visibility field.
var PastoralVisibilities = []string{
	PastoralVisibilityPrivate, PastoralVisibilityClergy, PastoralVisibilityCare, PastoralVisibilityCongregation,
}

// PastoralTags are the tag names the template uses.
var PastoralTags = []string{PastoralTagClergy, PastoralTagCare, PastoralTagBreakGlass, PastoralTagModerator}

// IsPastoralCollection reports whether the collection has the shape of the
// pastoral notes template: an org relation, an author relation, a visibility
// select, a sealed flag and an encrypted body. The shape is the marker, so a
// collection made by hand with those fields is protected the same way.
func IsPastoralCollection(c *Collection) bool {
	if c == nil || !c.IsBase() {
		return false
	}

	if _, ok := c.Fields.GetByName("org").(*RelationField); !ok {
		return false
	}

	if _, ok := c.Fields.GetByName("author").(*RelationField); !ok {
		return false
	}

	if sel, ok := c.Fields.GetByName("visibility").(*SelectField); !ok || len(sel.Values) == 0 {
		return false
	}

	if _, ok := c.Fields.GetByName("sealed").(*BoolField); !ok {
		return false
	}

	_, ok := c.Fields.GetByName("body").(*EncryptedTextField)

	return ok
}

// ---- rules -------------------------------------------------------------------

func pastoralTagRule(tag string) string {
	return fmt.Sprintf(`org._orgMembers_via_org.tags.name ?= "%s"`, tag)
}

// PastoralBreakGlassClause is the grant clause of the read rules: a live,
// approved, unrevoked grant of this grantee on this very note, held by an
// active member of the note's org. The membership is in the clause on
// purpose (defense in depth; the routes also revoke grants when a member is
// suspended or removed). Empty dates are excluded explicitly: an empty date
// compares lower than every real one.
func PastoralBreakGlassClause(collection string) string {
	g := "@collection." + CollectionNameBreakGlass + ":g."

	return "(" + OrgMemberRule("org") +
		" && " + g + "recordId ?= id" +
		" && " + g + `collection ?= "` + collection + `"` +
		" && " + g + "grantee ?= @request.auth.id" +
		" && " + g + `approvedBy ?!= ""` +
		" && " + g + `activeFrom ?!= ""` +
		" && " + g + "activeFrom ?<= @now" +
		" && " + g + "expires ?> @now" +
		" && " + g + "revoked ?= false)"
}

// PastoralReadRule is the list and view rule of the template (spec 8.3).
func PastoralReadRule(collection string) string {
	m := OrgMemberRule("org")

	return `@request.auth.id != "" && (author = @request.auth.id` +
		` || (sealed = false && archived = false && (` +
		`(visibility = "congregation" && ` + m + `)` +
		` || (visibility = "care" && ` + m + ` && ` + pastoralTagRule(PastoralTagCare) + `)` +
		` || (visibility = "clergy" && ` + m + ` && ` + pastoralTagRule(PastoralTagClergy) + `)))` +
		` || ` + PastoralBreakGlassClause(collection) + `)`
}

// PastoralRules is the complete locked rule set of the template.
//
// Create: any member, as author. Update: the author (who cannot move the note
// to another org or hand it to someone else), or a moderator who changes
// nothing but archived = true. Delete: the author. No clause names the
// owner or admin role.
func PastoralRules(collection string) OrgRuleSet {
	read := PastoralReadRule(collection)
	m := OrgMemberRule("org")

	create := `@request.auth.id != "" && author = @request.auth.id && ` + m

	onlyArchive := `@request.body.archived:isset = true && @request.body.archived = true` +
		` && @request.body.body:isset = false && @request.body.visibility:isset = false` +
		` && @request.body.sealed:isset = false && @request.body.org:isset = false` +
		` && @request.body.author:isset = false && @request.body.subject:isset = false` +
		` && @request.body.expiresAt:isset = false`

	update := `@request.auth.id != "" && (` +
		`(author = @request.auth.id && ` + m + ` && @request.body.author:isset = false && @request.body.org:isset = false)` +
		` || (` + m + ` && ` + pastoralTagRule(PastoralTagModerator) + ` && author != @request.auth.id && ` + onlyArchive + `))`

	del := `@request.auth.id != "" && author = @request.auth.id`

	return OrgRuleSet{List: &read, View: &read, Create: &create, Update: &update, Delete: &del}
}

// PastoralRuleProblems lists what is wrong with the rules of a pastoral
// collection: anything other than the locked rule (or null, which means
// superusers only and is stricter) is reported as looser. It is conservative on
// purpose; an app that needs a different rule says so with AllowLooseRules.
func PastoralRuleProblems(collection string, rules OrgRuleSet) []string {
	want := PastoralRules(collection)

	check := func(name string, got, locked *string) string {
		if got == nil {
			return "" // superusers only: stricter
		}

		if locked != nil && *got == *locked {
			return ""
		}

		return name + " differs from the locked pastoral rule"
	}

	var out []string

	for _, p := range []struct {
		name      string
		got, lock *string
	}{
		{"listRule", rules.List, want.List}, {"viewRule", rules.View, want.View},
		{"createRule", rules.Create, want.Create}, {"updateRule", rules.Update, want.Update},
		{"deleteRule", rules.Delete, want.Delete},
	} {
		if s := check(p.name, p.got, p.lock); s != "" {
			out = append(out, s)
		}
	}

	return out
}

// ---- template ------------------------------------------------------------------

// PastoralNotesOpts configures CreatePastoralNotes.
type PastoralNotesOpts struct {
	// Name of the collection. Default "pastoral_notes".
	Name string
	// Rules replaces the locked rules. Anything but the locked rule (or nil,
	// superusers only) is refused unless AllowLooseRules is set.
	Rules *OrgRuleSet
	// AllowLooseRules is the explicit override that lets Rules through.
	AllowLooseRules bool
}

// CreatePastoralNotes creates the pastoral notes collection with the locked
// rules and registers it in the data map.
func CreatePastoralNotes(app App, opts PastoralNotesOpts) (*Collection, error) {
	var col *Collection

	err := app.RunInTransaction(func(txApp App) error {
		var err error

		col, err = createPastoralNotes(txApp, opts)

		return err
	})
	if err != nil {
		return nil, err
	}

	return col, nil
}

func createPastoralNotes(app App, opts PastoralNotesOpts) (*Collection, error) {
	if opts.Name == "" {
		opts.Name = DefaultPastoralNotesCollection
	}

	if !registerNameRegex.MatchString(opts.Name) {
		return nil, fmt.Errorf("invalid collection name %q", opts.Name)
	}

	// The body is an encryptedText field. Without a key-encryption key the
	// app fails closed at the next start, so refuse up front.
	if !FieldEncryptionAvailable(app) {
		return nil, errors.New("pastoral notes need field encryption: configure a key-encryption key (CLAVIGER_KEK) and restart first")
	}

	if _, err := app.FindCollectionByNameOrId(opts.Name); err == nil {
		return nil, fmt.Errorf("a collection named %q already exists", opts.Name)
	}

	rules := PastoralRules(opts.Name)

	if opts.Rules != nil {
		if problems := PastoralRuleProblems(opts.Name, *opts.Rules); len(problems) > 0 && !opts.AllowLooseRules {
			return nil, fmt.Errorf("refusing looser pastoral rules (%s); pass allowLooseRules to override", strings.Join(problems, "; "))
		}

		rules = *opts.Rules
	}

	if _, err := app.FindCachedCollectionByNameOrId(CollectionNameBreakGlass); err != nil {
		return nil, fmt.Errorf("the %s collection is not available", CollectionNameBreakGlass)
	}

	orgs, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgs)
	if err != nil {
		return nil, err
	}

	userCol, err := OrgUserCollection(app)
	if err != nil {
		return nil, fmt.Errorf("the user collection is not available")
	}

	col := NewBaseCollection(opts.Name)
	col.Fields.Add(
		&RelationField{Name: "org", Required: true, CollectionId: orgs.Id, MaxSelect: 1},
		&RelationField{Name: "author", Required: true, CollectionId: userCol.Id, MaxSelect: 1},
		// the person the note is about (optional); the data export holds these notes for review
		&RelationField{Name: "subject", CollectionId: userCol.Id, MaxSelect: 1},
		&SelectField{Name: "visibility", Required: true, MaxSelect: 1, Values: PastoralVisibilities},
		&BoolField{Name: "sealed"},
		&EncryptedTextField{Name: "body", Required: true, Max: pastoralBodyMax},
		&DateField{Name: "expiresAt"},
		&BoolField{Name: "archived"},
		&AutodateField{Name: "created", OnCreate: true},
		&AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)

	col.ListRule, col.ViewRule = rules.List, rules.View
	col.CreateRule, col.UpdateRule, col.DeleteRule = rules.Create, rules.Update, rules.Delete

	col.AddIndex("idx_"+opts.Name+"_org", false, "org, visibility", "")
	col.AddIndex("idx_"+opts.Name+"_author", false, "author", "")

	if err := app.Save(col); err != nil {
		return nil, err
	}

	if err := registerDataMapAnonymize(app, opts.Name, []string{"subject"}, []string{"subject"},
		"Pastoral notes (#86). The note stays for its author; erasure of the person it is about clears the subject link (UnlinkPastoralSubject). Notes about a person are never put in their automatic export: the export records them as held for review and an owner of the note's org releases them by hand."); err != nil {
		return nil, err
	}

	return col, nil
}

// EnsurePastoralTags creates the four tags in the org when they are missing.
func EnsurePastoralTags(app App, ctx context.Context, org *Record) ([]string, error) {
	var created []string

	if ctx == nil {
		ctx = context.Background()
	}

	for _, name := range PastoralTags {
		if _, err := app.FindFirstRecordByFilter(CollectionNameOrgRoles, "org={:org} && name={:name}",
			dbx.Params{"org": org.Id, "name": name}); err == nil {
			continue
		}

		col, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgRoles)
		if err != nil {
			return created, err
		}

		tag := NewRecord(col)
		tag.Set("org", org.Id)
		tag.Set("name", name)

		if err := saveOrgRecord(app, ctx, tag); err != nil {
			return created, err
		}

		created = append(created, name)
	}

	return created, nil
}
