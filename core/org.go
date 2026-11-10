package core

import (
	"context"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #79a): organizations, memberships and roles.
//
// Three hidden system collections hold the model:
//
//   - _orgs: the organization (name, unique slug, optional parent, archived).
//   - _orgMembers: one row per (org, user) with a built-in role, a status and
//     optional tags (rows of _orgRoles of the same org).
//   - _orgRoles: free-form labels such as "youth leader".
//
// Their list and view rules are fixed by the migration (members see their own
// orgs); nobody but a superuser can write them through the records API. Real
// member management goes through the functions in this package (and the
// /api/orgs routes built on them), which enforce what rules cannot: the last
// owner stays, nobody grants or edits above their own role. Model hooks back
// the invariants for every app-level write.
//
// With no org created, none of this runs and nothing changes.

const (
	// CollectionNameOrgs, CollectionNameOrgMembers and CollectionNameOrgRoles
	// are the hidden system collections of the organizations model.
	CollectionNameOrgs       = "_orgs"
	CollectionNameOrgMembers = "_orgMembers"
	CollectionNameOrgRoles   = "_orgRoles"

	OrgRoleOwner  = "owner"
	OrgRoleAdmin  = "admin"
	OrgRoleMember = "member"
	OrgRoleViewer = "viewer"

	OrgStatusActive    = "active"
	OrgStatusSuspended = "suspended"

	OrgSourceManual     = "manual"
	OrgSourceInvitation = "invitation"
	OrgSourceSCIM       = "scim"
	OrgSourceSSO        = "sso"

	// OrgUserCollectionEnv names the auth collection whose records are org
	// members (default "users"). It is read when the migration creates
	// _orgMembers; afterwards the relation field of _orgMembers is the source
	// of truth (see OrgUserCollection).
	OrgUserCollectionEnv = "CLAVIGER_ORGS_USER_COLLECTION"

	// OrgUsersCanCreateEnv, when "true" or "1", lets any authenticated record
	// of the member collection create an organization (it becomes the owner).
	// Off by default: only superusers create organizations.
	OrgUsersCanCreateEnv = "CLAVIGER_ORGS_USERS_CAN_CREATE"

	// OrgMaxTags caps the tags of one membership.
	OrgMaxTags = 20
)

// OrgRoles lists the built-in roles from the highest to the lowest rank.
var OrgRoles = []string{OrgRoleOwner, OrgRoleAdmin, OrgRoleMember, OrgRoleViewer}

var (
	orgSlugRegex = regexp.MustCompile(`^[a-z0-9-]{2,64}$`)
	orgTagRegex  = regexp.MustCompile(`^[^\x00-\x1f"'\\]{1,64}$`)
)

// OrgRoleRank ranks a role (owner 4 ... viewer 1, unknown 0).
func OrgRoleRank(role string) int {
	switch role {
	case OrgRoleOwner:
		return 4
	case OrgRoleAdmin:
		return 3
	case OrgRoleMember:
		return 2
	case OrgRoleViewer:
		return 1
	}

	return 0
}

// OrgError is a failure of an org operation that maps to an HTTP status.
type OrgError struct {
	Status  int
	Message string
}

func (e *OrgError) Error() string { return e.Message }

func orgErr(status int, msg string) error { return &OrgError{Status: status, Message: msg} }

// OrgUserCollectionName is the configured name of the member collection.
func OrgUserCollectionName() string {
	if v := strings.TrimSpace(os.Getenv(OrgUserCollectionEnv)); v != "" {
		return v
	}

	return "users"
}

// OrgUsersCanCreate reports whether regular users may create organizations.
func OrgUsersCanCreate() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(OrgUsersCanCreateEnv)))

	return v == "true" || v == "1"
}

// OrgUserCollection returns the auth collection that _orgMembers.user points at.
func OrgUserCollection(app App) (*Collection, error) {
	members, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgMembers)
	if err != nil {
		return nil, err
	}

	rel, _ := members.Fields.GetByName("user").(*RelationField)
	if rel == nil {
		return nil, errors.New("the _orgMembers collection has no user relation")
	}

	return app.FindCachedCollectionByNameOrId(rel.CollectionId)
}

// OrgReservedSlugs are the literal path segments under /api/orgs; an org with
// one of them as its slug could not be reached by slug.
var OrgReservedSlugs = []string{"config", "ancestor-rule", "rule-templates"}

// FindOrg loads an organization by id or slug.
func FindOrg(app App, idOrSlug string) (*Record, error) {
	if idOrSlug == "" {
		return nil, errors.New("missing org")
	}

	if rec, err := app.FindRecordById(CollectionNameOrgs, idOrSlug); err == nil {
		return rec, nil
	}

	return app.FindFirstRecordByData(CollectionNameOrgs, "slug", idOrSlug)
}

// FindOrgMember returns the membership of the user in the org (any status).
func FindOrgMember(app App, orgId, userId string) (*Record, error) {
	return app.FindFirstRecordByFilter(CollectionNameOrgMembers, "org={:org} && user={:user}",
		dbx.Params{"org": orgId, "user": userId})
}

// ActiveOrgMember returns the membership when it is active and the org is not
// archived; nil otherwise.
func ActiveOrgMember(app App, org *Record, userId string) *Record {
	if org == nil || org.GetBool("archived") {
		return nil
	}

	m, err := FindOrgMember(app, org.Id, userId)
	if err != nil || m.GetString("status") != OrgStatusActive {
		return nil
	}

	return m
}

// ActiveOrgMemberships lists the active memberships of a user in orgs that
// are not archived.
func ActiveOrgMemberships(app App, userId string) ([]*Record, error) {
	return app.FindRecordsByFilter(CollectionNameOrgMembers,
		"user={:user} && status='active' && org.archived=false", "created", 0, 0, dbx.Params{"user": userId})
}

// countOtherActiveOwners counts the active owners of the org except the member.
func countOtherActiveOwners(app App, orgId, exceptMemberId string) int {
	recs, err := app.FindRecordsByFilter(CollectionNameOrgMembers,
		"org={:org} && role='owner' && status='active' && id!={:id}", "", 1, 0,
		dbx.Params{"org": orgId, "id": exceptMemberId})
	if err != nil {
		return 0
	}

	return len(recs)
}

// OrgTagNames returns the tag names of a membership.
func OrgTagNames(app App, member *Record) []string {
	ids := member.GetStringSlice("tags")
	if len(ids) == 0 {
		return []string{}
	}

	recs, err := app.FindRecordsByIds(CollectionNameOrgRoles, ids)
	if err != nil {
		return []string{}
	}

	names := make([]string, 0, len(recs))
	for _, r := range recs {
		names = append(names, r.GetString("name"))
	}

	slices.Sort(names)

	return names
}

type orgDeletionCtxKey struct{}

// WithOrgDeletion marks ctx as part of deleting a whole organization, which
// lets the cascade remove the last owner. Only DeleteOrg uses it.
func withOrgDeletion(ctx context.Context) context.Context {
	return context.WithValue(ctx, orgDeletionCtxKey{}, true)
}

func inOrgDeletion(ctx context.Context) bool {
	v, _ := ctx.Value(orgDeletionCtxKey{}).(bool)

	return v
}

// DeleteOrg permanently deletes an organization with its memberships and tags.
// It refuses while the org has child orgs, while org-owned records still
// reference it (their required `org` relation blocks the delete) and while it
// is not archived.
func DeleteOrg(app App, ctx context.Context, org *Record) error {
	if !org.GetBool("archived") {
		return orgErr(409, "Archive the organization before deleting it.")
	}

	if err := app.DeleteWithContext(withOrgDeletion(ctx), org); err != nil {
		return orgErr(409, "The organization cannot be deleted: "+err.Error())
	}

	return nil
}

// ---- hooks ---------------------------------------------------------------

func (app *BaseApp) registerOrgHooks() {
	app.OnRecordValidate(CollectionNameOrgs).Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbOrgSlugReserved__",
		Func: func(e *RecordEvent) error {
			if slug := e.Record.GetString("slug"); slices.Contains(OrgReservedSlugs, slug) {
				return validation.Errors{"slug": validation.NewError("validation_slug_reserved",
					"This slug is reserved by an /api/orgs route; choose another.")}
			}

			return e.Next()
		},
	})

	app.OnRecordValidate(CollectionNameOrgMembers).Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbOrgMemberValidate__",
		Func: func(e *RecordEvent) error {
			if err := validateOrgMemberTags(e.App, e.Record); err != nil {
				return err
			}

			return e.Next()
		},
	})

	app.OnRecordUpdate(CollectionNameOrgMembers).Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbOrgLastOwnerUpdate__",
		Func: func(e *RecordEvent) error {
			// the stored state: Original() can be stale for a record instance
			// that was saved before (same reasoning as the audit hook)
			orig := e.Record.Original()
			if stored, err := e.App.FindRecordById(e.Record.Collection(), e.Record.Id); err == nil {
				orig = stored
			}

			if isActiveOrgOwner(orig) && !isActiveOrgOwner(e.Record) {
				if err := guardLastOwner(e.App, e.Context, orig); err != nil {
					return err
				}
			}

			return e.Next()
		},
	})

	app.OnRecordDelete(CollectionNameOrgMembers).Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbOrgLastOwnerDelete__",
		Func: func(e *RecordEvent) error {
			if isActiveOrgOwner(e.Record) {
				if err := guardLastOwner(e.App, e.Context, e.Record); err != nil {
					return err
				}
			}

			return e.Next()
		},
	})

	app.OnRecordValidate(CollectionNameOrgs).Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbOrgValidate__",
		Func: func(e *RecordEvent) error {
			if err := validateOrg(e.App, e.Record); err != nil {
				return err
			}

			return e.Next()
		},
	})

	app.OnRecordDelete(CollectionNameOrgs).Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbOrgDeleteChildren__",
		Func: func(e *RecordEvent) error {
			children, _ := e.App.FindRecordsByFilter(CollectionNameOrgs, "parent={:id}", "", 1, 0, dbx.Params{"id": e.Record.Id})
			if len(children) > 0 {
				return orgErr(409, "The organization has child organizations.")
			}

			return e.Next()
		},
	})

	app.registerOrgClosureHooks() // #84

	// roles of a deleted tag row are dropped from memberships by the relation
	// cleanup of the records engine; nothing to do here.
}

func isActiveOrgOwner(m *Record) bool {
	return m != nil && m.GetString("role") == OrgRoleOwner && m.GetString("status") == OrgStatusActive
}

func guardLastOwner(app App, ctx context.Context, member *Record) error {
	if inOrgDeletion(ctx) {
		return nil
	}

	if countOtherActiveOwners(app, member.GetString("org"), member.Id) > 0 {
		return nil
	}

	return orgErr(409, "The last owner of an organization cannot be removed, demoted or suspended. "+
		"Make someone else an owner first, or archive and delete the organization (DELETE /api/orgs/{id}).")
}

func validateOrgMemberTags(app App, m *Record) error {
	ids := m.GetStringSlice("tags")
	if len(ids) == 0 {
		return nil
	}

	if len(ids) > OrgMaxTags {
		return orgErr(400, "Too many tags.")
	}

	tags, err := app.FindRecordsByIds(CollectionNameOrgRoles, ids)
	if err != nil || len(tags) != len(ids) {
		return orgErr(400, "Unknown tag.")
	}

	for _, t := range tags {
		if t.GetString("org") != m.GetString("org") {
			return orgErr(400, "A tag belongs to a different organization.")
		}
	}

	return nil
}

// auditOrgMeta stamps meta.orgId (or meta.orgIds) on audit entries of org
// related records and flags actions of superusers on organizations (#79a).
func auditOrgMeta(entry *AuditEntry, record *Record) {
	col := record.Collection()

	switch {
	case col.Name == CollectionNameOrgs:
		entry.Meta["orgId"] = record.Id
	case col.Fields.GetByName("org") != nil:
		if id := record.GetString("org"); id != "" {
			entry.Meta["orgId"] = id
		}
	case col.Fields.GetByName("orgs") != nil:
		if ids := record.GetStringSlice("orgs"); len(ids) > 0 {
			entry.Meta["orgIds"] = ids
		}
	}
}
