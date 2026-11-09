package core

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
)

// Fork-local (Claviger #79a): the member-management operations behind
// /api/orgs. They are plain Go functions so that invitations (79b) and SCIM
// (79c) write memberships through the same guards.

// OrgActor is who performs an org operation.
type OrgActor struct {
	// Record is the authenticated record (a superuser or a member-collection record).
	Record *Record
	// Superuser bypasses the role hierarchy (the action is audited with the
	// superuser as actor).
	Superuser bool
	// Role is the actor's role in the org ("" when not an active member of a
	// live org).
	Role string
	// System marks internal callers (invitation accept, SCIM) that have
	// already authorized the change; they bypass the hierarchy like a superuser
	// but are not audited as one.
	System bool
}

// NewOrgActor resolves the actor's standing in the org.
func NewOrgActor(app App, org *Record, auth *Record) *OrgActor {
	a := &OrgActor{Record: auth}
	if auth == nil {
		return a
	}

	if auth.IsSuperuser() {
		a.Superuser = true

		return a
	}

	if m := ActiveOrgMember(app, org, auth.Id); m != nil {
		a.Role = m.GetString("role")
	}

	return a
}

// IsMember reports whether the actor is an active member (or bypasses).
func (a *OrgActor) IsMember() bool { return a.Superuser || a.System || a.Role != "" }

// CanManage reports whether the actor may manage the org's members.
func (a *OrgActor) CanManage() bool {
	return a.Superuser || a.System || a.Role == OrgRoleOwner || a.Role == OrgRoleAdmin
}

// rank is the actor's rank for hierarchy checks.
func (a *OrgActor) rank() int {
	if a.Superuser || a.System {
		return OrgRoleRank(OrgRoleOwner)
	}

	return OrgRoleRank(a.Role)
}

func (a *OrgActor) isSelf(member *Record) bool {
	return a.Record != nil && !a.Superuser && member.GetString("user") == a.Record.Id
}

// OrgMemberInput describes a new membership.
type OrgMemberInput struct {
	// User is a record id or an email of the member collection.
	User   string
	Role   string
	Status string
	Tags   []string
	Source string
	// ExternalId is the identity provider's key for the user (SCIM, #79c).
	ExternalId string
}

// OrgMemberPatch describes a change of a membership (nil = unchanged).
type OrgMemberPatch struct {
	Role   *string
	Status *string
	Tags   *[]string
	// ExternalId sets (or with "" clears) the identity provider's key (#79c).
	ExternalId *string
}

// FindOrgUser resolves a user id or email of the member collection. The error
// is the same for every miss.
func FindOrgUser(app App, ref string) (*Record, error) {
	col, err := OrgUserCollection(app)
	if err != nil {
		return nil, orgErr(500, "The organizations model is not initialized.")
	}

	ref = strings.TrimSpace(ref)
	if ref != "" {
		if rec, err := app.FindRecordById(col, ref); err == nil {
			return rec, nil
		}

		if strings.Contains(ref, "@") {
			if rec, err := app.FindAuthRecordByEmail(col, ref); err == nil {
				return rec, nil
			}
		}
	}

	return nil, orgErr(404, "User not found.")
}

// ResolveOrgTags returns the _orgRoles ids for the tag names of the org,
// creating the missing ones.
func ResolveOrgTags(app App, ctx context.Context, org *Record, names []string) ([]string, error) {
	cleaned := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || slices.Contains(cleaned, n) {
			continue
		}

		if !orgTagRegex.MatchString(n) {
			return nil, orgErr(400, "Invalid tag: "+n)
		}

		cleaned = append(cleaned, n)
	}

	if len(cleaned) > OrgMaxTags {
		return nil, orgErr(400, "Too many tags.")
	}

	ids := make([]string, 0, len(cleaned))
	for _, n := range cleaned {
		tag, err := app.FindFirstRecordByFilter(CollectionNameOrgRoles, "org={:org} && name={:name}",
			dbx.Params{"org": org.Id, "name": n})
		if err != nil {
			col, cerr := app.FindCachedCollectionByNameOrId(CollectionNameOrgRoles)
			if cerr != nil {
				return nil, cerr
			}

			tag = NewRecord(col)
			tag.Set("org", org.Id)
			tag.Set("name", n)

			if err := app.SaveWithContext(ctx, tag); err != nil {
				return nil, orgErr(400, "Failed to create the tag "+n+".")
			}
		}

		ids = append(ids, tag.Id)
	}

	return ids, nil
}

func validRole(v string) bool   { return OrgRoleRank(v) > 0 }
func validStatus(v string) bool { return v == OrgStatusActive || v == OrgStatusSuspended }

func requireLiveOrg(org *Record, actor *OrgActor) error {
	if org.GetBool("archived") && !actor.Superuser && !actor.System {
		return orgErr(409, "The organization is archived.")
	}

	return nil
}

// AddOrgMember adds an existing user to the org.
//
// Guards: the actor must be an owner or admin (or a superuser) and cannot
// grant a role above their own.
func AddOrgMember(app App, ctx context.Context, actor *OrgActor, org *Record, in OrgMemberInput) (*Record, error) {
	if !actor.CanManage() {
		return nil, orgErr(403, "Only owners and admins can add members.")
	}

	if err := requireLiveOrg(org, actor); err != nil {
		return nil, err
	}

	if in.Role == "" {
		in.Role = OrgRoleMember
	}

	if in.Status == "" {
		in.Status = OrgStatusActive
	}

	if in.Source == "" {
		in.Source = OrgSourceManual
	}

	if !validRole(in.Role) {
		return nil, orgErr(400, "Invalid role.")
	}

	if !validStatus(in.Status) {
		return nil, orgErr(400, "Invalid status.")
	}

	if OrgRoleRank(in.Role) > actor.rank() {
		return nil, orgErr(403, "You cannot grant a role above your own.")
	}

	user, err := FindOrgUser(app, in.User)
	if err != nil {
		return nil, err
	}

	if _, err := FindOrgMember(app, org.Id, user.Id); err == nil {
		return nil, orgErr(409, "The user is already a member of the organization.")
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgMembers)
	if err != nil {
		return nil, err
	}

	var member *Record

	txErr := app.RunInTransaction(func(txApp App) error {
		tags, err := ResolveOrgTags(txApp, ctx, org, in.Tags)
		if err != nil {
			return err
		}

		member = NewRecord(col)
		member.Set("org", org.Id)
		member.Set("user", user.Id)
		member.Set("role", in.Role)
		member.Set("status", in.Status)
		member.Set("source", in.Source)
		member.Set("tags", tags)

		if in.ExternalId != "" {
			member.Set("externalId", in.ExternalId)
		}

		return saveOrgRecord(txApp, ctx, member)
	})
	if txErr != nil {
		return nil, txErr
	}

	return member, nil
}

// UpdateOrgMember changes the role, status or tags of a membership.
//
// Guards: owners and admins only; nobody edits a member ranked above them and
// nobody assigns a role above their own; nobody changes their own status;
// the last active owner cannot be demoted or suspended (model hook).
func UpdateOrgMember(app App, ctx context.Context, actor *OrgActor, org, member *Record, patch OrgMemberPatch) (*Record, error) {
	if member.GetString("org") != org.Id {
		return nil, orgErr(404, "Member not found.")
	}

	if !actor.CanManage() {
		return nil, orgErr(403, "Only owners and admins can change members.")
	}

	if err := requireLiveOrg(org, actor); err != nil {
		return nil, err
	}

	if OrgRoleRank(member.GetString("role")) > actor.rank() {
		return nil, orgErr(403, "You cannot change a member ranked above you.")
	}

	if patch.Role != nil {
		if !validRole(*patch.Role) {
			return nil, orgErr(400, "Invalid role.")
		}

		if OrgRoleRank(*patch.Role) > actor.rank() {
			return nil, orgErr(403, "You cannot grant a role above your own.")
		}
	}

	if patch.Status != nil {
		if !validStatus(*patch.Status) {
			return nil, orgErr(400, "Invalid status.")
		}

		if actor.isSelf(member) && *patch.Status != member.GetString("status") {
			return nil, orgErr(403, "You cannot change your own status.")
		}
	}

	txErr := app.RunInTransaction(func(txApp App) error {
		fresh, err := txApp.FindRecordById(CollectionNameOrgMembers, member.Id)
		if err != nil {
			return orgErr(404, "Member not found.")
		}

		if patch.Role != nil {
			fresh.Set("role", *patch.Role)
		}

		if patch.Status != nil {
			fresh.Set("status", *patch.Status)
		}

		if patch.ExternalId != nil {
			fresh.Set("externalId", *patch.ExternalId)
		}

		if patch.Tags != nil {
			tags, err := ResolveOrgTags(txApp, ctx, org, *patch.Tags)
			if err != nil {
				return err
			}

			fresh.Set("tags", tags)
		}

		if err := saveOrgRecord(txApp, ctx, fresh); err != nil {
			return err
		}

		member = fresh

		return nil
	})
	if txErr != nil {
		return nil, txErr
	}

	return member, nil
}

// RemoveOrgMember removes a membership. Any member may remove themselves
// (leave the org); removing someone else needs an owner or admin who ranks at
// least as high as the member. The last active owner cannot be removed (model
// hook).
func RemoveOrgMember(app App, ctx context.Context, actor *OrgActor, org, member *Record) error {
	if member.GetString("org") != org.Id {
		return orgErr(404, "Member not found.")
	}

	if actor.isSelf(member) && actor.Role != "" {
		return deleteOrgRecord(app, ctx, member)
	}

	if !actor.CanManage() {
		return orgErr(403, "Only owners and admins can remove members.")
	}

	if err := requireLiveOrg(org, actor); err != nil {
		return err
	}

	if OrgRoleRank(member.GetString("role")) > actor.rank() {
		return orgErr(403, "You cannot remove a member ranked above you.")
	}

	return deleteOrgRecord(app, ctx, member)
}

var orgSlugCleanRegex = regexp.MustCompile(`[^a-z0-9]+`)

// OrgSlugFromName derives a slug candidate from a name.
func OrgSlugFromName(name string) string {
	s := strings.Trim(orgSlugCleanRegex.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 64 {
		s = strings.Trim(s[:64], "-")
	}

	return s
}

// OrgInput describes a new organization.
type OrgInput struct {
	Name   string
	Slug   string
	Parent string
	// Owner is the user (id or email) who becomes the owner. For a regular
	// user creating an org it is always the user themselves.
	Owner string
}

// CreateOrg creates an organization. Superusers always may (an owner is
// optional); a record of the member collection may when OrgUsersCanCreate()
// is on, and becomes the owner. Only superusers set a parent.
func CreateOrg(app App, ctx context.Context, actor *OrgActor, in OrgInput) (*Record, error) {
	if actor.Record == nil {
		return nil, orgErr(401, "Authentication required.")
	}

	if !actor.Superuser && !actor.System {
		if !OrgUsersCanCreate() {
			return nil, orgErr(403, "Only superusers can create organizations.")
		}

		col, err := OrgUserCollection(app)
		if err != nil || actor.Record.Collection().Id != col.Id {
			return nil, orgErr(403, "Your account type cannot create organizations.")
		}

		if in.Parent != "" {
			return nil, orgErr(403, "Only superusers can set a parent organization.")
		}

		in.Owner = actor.Record.Id
	}

	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return nil, orgErr(400, "The name is required.")
	}

	if in.Slug == "" {
		in.Slug = OrgSlugFromName(in.Name)
	}

	if !orgSlugRegex.MatchString(in.Slug) {
		return nil, orgErr(400, "The slug must be 2 to 64 characters: lowercase letters, digits and dashes.")
	}

	if _, err := app.FindFirstRecordByData(CollectionNameOrgs, "slug", in.Slug); err == nil {
		return nil, orgErr(409, "The slug is already in use.")
	}

	var owner *Record

	if in.Owner != "" {
		var err error
		if owner, err = FindOrgUser(app, in.Owner); err != nil {
			return nil, err
		}
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgs)
	if err != nil {
		return nil, orgErr(500, "The organizations model is not initialized.")
	}

	org := NewRecord(col)
	org.Set("name", in.Name)
	org.Set("slug", in.Slug)
	org.Set("parent", in.Parent)

	txErr := app.RunInTransaction(func(txApp App) error {
		if err := saveOrgRecord(txApp, ctx, org); err != nil {
			return err
		}

		if owner == nil {
			return nil
		}

		members, err := txApp.FindCachedCollectionByNameOrId(CollectionNameOrgMembers)
		if err != nil {
			return err
		}

		m := NewRecord(members)
		m.Set("org", org.Id)
		m.Set("user", owner.Id)
		m.Set("role", OrgRoleOwner)
		m.Set("status", OrgStatusActive)
		m.Set("source", OrgSourceManual)

		return saveOrgRecord(txApp, ctx, m)
	})
	if txErr != nil {
		return nil, txErr
	}

	return org, nil
}

// OrgUpdate describes a change of an organization (nil = unchanged).
type OrgUpdate struct {
	Name     *string
	Slug     *string
	Parent   *string
	Archived *bool
}

// UpdateOrg changes an organization. Name and slug need an owner or admin,
// archiving and unarchiving an owner, the parent a superuser.
func UpdateOrg(app App, ctx context.Context, actor *OrgActor, org *Record, in OrgUpdate) error {
	archivedNow := org.GetBool("archived")

	if !actor.CanManage() {
		return orgErr(403, "Only owners and admins can change the organization.")
	}

	if archivedNow && !actor.Superuser && !actor.System && (in.Archived == nil || *in.Archived) {
		return orgErr(409, "The organization is archived.")
	}

	if in.Archived != nil && *in.Archived != archivedNow && actor.Role != OrgRoleOwner && !actor.Superuser && !actor.System {
		return orgErr(403, "Only owners can archive or restore the organization.")
	}

	if in.Parent != nil && !actor.Superuser && !actor.System {
		return orgErr(403, "Only superusers can change the parent organization.")
	}

	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" {
			return orgErr(400, "The name is required.")
		}

		org.Set("name", n)
	}

	if in.Slug != nil && *in.Slug != org.GetString("slug") {
		if !orgSlugRegex.MatchString(*in.Slug) {
			return orgErr(400, "The slug must be 2 to 64 characters: lowercase letters, digits and dashes.")
		}

		if _, err := app.FindFirstRecordByData(CollectionNameOrgs, "slug", *in.Slug); err == nil {
			return orgErr(409, "The slug is already in use.")
		}

		org.Set("slug", *in.Slug)
	}

	if in.Parent != nil {
		org.Set("parent", *in.Parent)
	}

	if in.Archived != nil {
		org.Set("archived", *in.Archived)
	}

	return saveOrgRecord(app, ctx, org)
}

// saveOrgRecord saves and turns validation failures into a 400.
func saveOrgRecord(app App, ctx context.Context, rec *Record) error {
	if err := app.SaveWithContext(ctx, rec); err != nil {
		return wrapOrgSaveErr(err)
	}

	return nil
}

func deleteOrgRecord(app App, ctx context.Context, rec *Record) error {
	if err := app.DeleteWithContext(ctx, rec); err != nil {
		return wrapOrgSaveErr(err)
	}

	return nil
}

func wrapOrgSaveErr(err error) error {
	var oe *OrgError
	if errors.As(err, &oe) {
		return oe
	}

	return orgErr(400, err.Error())
}
