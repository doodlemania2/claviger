package core

import (
	"context"
	"slices"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
)

// Fork-local (Claviger #79c): SCIM groups.
//
// A SCIM Group is a row of _scimGroups (displayName, externalId, members) of
// one org. It never creates an org. What a group grants:
//
//   - a tag with the group's displayName on every member's membership
//     (created on demand; removed again when the user leaves the group);
//   - when the org setting scimGroupRoles maps the displayName to a role
//     (never owner), that role. The highest role of the groups a user is in
//     wins, and it is only ever raised by SCIM: an add never lowers a role.
//     When a user leaves a role-mapped group and still holds exactly that
//     role, the role falls back to the highest role of their remaining mapped
//     groups, or the org default role (scimDefaultRole). Owners are never
//     touched, and a role that was set by hand to something else is left.
//
// Removing a user from a group never removes the membership.

// FindSCIMGroup loads a group of the org.
func FindSCIMGroup(app App, org *Record, id string) (*Record, error) {
	g, err := app.FindRecordById(CollectionNameSCIMGroups, id)
	if err != nil || g.GetString("org") != org.Id {
		return nil, scimErr(404, "", "Resource "+id+" not found.")
	}

	return g, nil
}

// SCIMGroupMemberUsers returns the users of the group that are still members
// of the org.
func SCIMGroupMemberUsers(app App, org, group *Record) ([]*Record, error) {
	ids := group.GetStringSlice("members")
	if len(ids) == 0 {
		return nil, nil
	}

	live := map[string]bool{}

	for chunk := range slices.Chunk(ids, 400) {
		vals := make([]any, len(chunk))
		for i, id := range chunk {
			vals[i] = id
		}

		recs, err := app.FindAllRecords(CollectionNameOrgMembers, dbx.HashExp{"org": org.Id, "user": vals})
		if err != nil {
			return nil, err
		}

		for _, m := range recs {
			live[m.GetString("user")] = true
		}
	}

	var keep []string

	for _, id := range ids {
		if live[id] {
			keep = append(keep, id)
		}
	}

	if len(keep) == 0 {
		return nil, nil
	}

	col, err := OrgUserCollection(app)
	if err != nil {
		return nil, err
	}

	return app.FindRecordsByIds(col.Id, keep)
}

// SCIMGroupsOfUser lists the org's groups the user is in.
func SCIMGroupsOfUser(app App, org *Record, userId string) []*Record {
	groups, err := app.FindRecordsByFilter(CollectionNameSCIMGroups, "org={:o} && members:each ?= {:u}", "displayName", 0, 0,
		dbx.Params{"o": org.Id, "u": userId})
	if err != nil {
		return nil
	}

	return groups
}

// SCIMSaveGroup creates (group == nil) or updates a group. members, when not
// nil, is the complete new member list (user ids, all of them members of the
// org); nil leaves the members unchanged.
func SCIMSaveGroup(app App, ctx context.Context, org, group *Record, displayName, externalId *string, members *[]string) (*Record, error) {
	col, err := app.FindCachedCollectionByNameOrId(CollectionNameSCIMGroups)
	if err != nil {
		return nil, scimErr(500, "", "SCIM is not initialized.")
	}

	oldName := ""
	var oldMembers []string

	if group == nil {
		group = NewRecord(col)
		group.Set("org", org.Id)
	} else {
		oldName = group.GetString("displayName")
		oldMembers = group.GetStringSlice("members")
	}

	newName := oldName

	if displayName != nil {
		newName = strings.TrimSpace(*displayName)
	}

	if newName == "" {
		return nil, scimErr(400, "invalidValue", "displayName is required.")
	}

	if !orgTagRegex.MatchString(newName) {
		return nil, scimErr(400, "invalidValue",
			"displayName must be 1 to 64 characters without control characters, quotes or backslashes (it becomes a tag).")
	}

	if newName != oldName {
		if g, err := app.FindFirstRecordByFilter(CollectionNameSCIMGroups, "org={:o} && displayName={:n}",
			dbx.Params{"o": org.Id, "n": newName}); err == nil && g.Id != group.Id {
			return nil, scimErr(409, "uniqueness", "A group with that displayName already exists.")
		}
	}

	if externalId != nil {
		ext := strings.TrimSpace(*externalId)
		if ext != "" {
			if g, err := app.FindFirstRecordByFilter(CollectionNameSCIMGroups, "org={:o} && externalId={:e}",
				dbx.Params{"o": org.Id, "e": ext}); err == nil && g.Id != group.Id {
				return nil, scimErr(409, "uniqueness", "A group with that externalId already exists.")
			}
		}

		group.Set("externalId", ext)
	}

	finalMembers := oldMembers

	if members != nil {
		finalMembers = dedupe(*members)

		for _, id := range finalMembers {
			if !slices.Contains(oldMembers, id) {
				if _, err := FindOrgMember(app, org.Id, id); err != nil {
					return nil, scimErr(400, "invalidValue", "Unknown member "+id+": it is not a user of this organization.")
				}
			}
		}
	}

	group.Set("displayName", newName)
	group.Set("members", finalMembers)

	txErr := app.RunInTransaction(func(txApp App) error {
		if err := txApp.SaveWithContext(ctx, group); err != nil {
			return scimErr(400, "invalidValue", err.Error())
		}

		renamed := oldName != "" && oldName != newName

		var removed, added []string

		for _, id := range oldMembers {
			if !slices.Contains(finalMembers, id) || renamed {
				removed = append(removed, id)
			}
		}

		for _, id := range finalMembers {
			if !slices.Contains(oldMembers, id) || renamed {
				added = append(added, id)
			}
		}

		for _, id := range removed {
			if err := scimGroupLeave(txApp, ctx, org, group, oldName, id); err != nil {
				return err
			}
		}

		for _, id := range added {
			if err := scimGroupJoin(txApp, ctx, org, newName, id); err != nil {
				return err
			}
		}

		return nil
	})
	if txErr != nil {
		return nil, txErr
	}

	return group, nil
}

// SCIMDeleteGroup deletes the group and takes back what it granted.
func SCIMDeleteGroup(app App, ctx context.Context, org, group *Record) error {
	name := group.GetString("displayName")
	members := group.GetStringSlice("members")

	return app.RunInTransaction(func(txApp App) error {
		if err := txApp.DeleteWithContext(ctx, group); err != nil {
			return scimErr(500, "", "Failed to delete the group.")
		}

		for _, id := range members {
			if err := scimGroupLeave(txApp, ctx, org, group, name, id); err != nil {
				return err
			}
		}

		return nil
	})
}

// scimStripUserFromGroups drops a user from the org's group member lists
// (after the membership is gone: nothing else to take back).
func scimStripUserFromGroups(app App, ctx context.Context, org *Record, userId string) error {
	for _, g := range SCIMGroupsOfUser(app, org, userId) {
		members := slices.DeleteFunc(slices.Clone(g.GetStringSlice("members")), func(id string) bool { return id == userId })
		g.Set("members", members)

		if err := app.SaveWithContext(ctx, g); err != nil {
			return scimErr(500, "", "Failed to update a group.")
		}
	}

	return nil
}

func dedupe(in []string) []string {
	out := make([]string, 0, len(in))

	for _, s := range in {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}

	return out
}

// scimGroupJoin applies what the group grants to a member.
func scimGroupJoin(app App, ctx context.Context, org *Record, groupName, userId string) error {
	member, err := FindOrgMember(app, org.Id, userId)
	if err != nil {
		return nil
	}

	tags := OrgTagNames(app, member)
	patch := OrgMemberPatch{}

	if !slices.Contains(tags, groupName) {
		tags = append(tags, groupName)
		patch.Tags = &tags
	}

	cur := member.GetString("role")
	if mapped, ok := GetSCIMSettings(org).GroupRoles[groupName]; ok && cur != OrgRoleOwner && OrgRoleRank(mapped) > OrgRoleRank(cur) {
		patch.Role = &mapped
	}

	if patch.Tags == nil && patch.Role == nil {
		return nil
	}

	_, err = UpdateOrgMember(app, ctx, scimSystemActor(), org, member, patch)

	return err
}

// scimGroupLeave takes back what the group granted to a member.
func scimGroupLeave(app App, ctx context.Context, org, group *Record, groupName, userId string) error {
	member, err := FindOrgMember(app, org.Id, userId)
	if err != nil {
		return nil
	}

	patch := OrgMemberPatch{}

	tags := OrgTagNames(app, member)
	if i := slices.Index(tags, groupName); i >= 0 {
		tags = slices.Delete(tags, i, i+1)
		patch.Tags = &tags
	}

	settings := GetSCIMSettings(org)
	cur := member.GetString("role")

	if mapped, ok := settings.GroupRoles[groupName]; ok && cur != OrgRoleOwner && cur == mapped {
		var others []string

		for _, g := range SCIMGroupsOfUser(app, org, userId) {
			if g.Id == group.Id {
				continue
			}

			if r, ok := settings.GroupRoles[g.GetString("displayName")]; ok {
				others = append(others, r)
			}
		}

		next := scimHighestRole(others)
		if next == "" {
			next = settings.DefaultRole
		}

		if next != cur {
			patch.Role = &next
		}
	}

	if patch.Tags == nil && patch.Role == nil {
		return nil
	}

	_, err = UpdateOrgMember(app, ctx, scimSystemActor(), org, member, patch)

	return err
}
