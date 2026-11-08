package core

import (
	"context"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
)

// Fork-local (Claviger #79b): tag management for the hosted org page. Tags
// are the _orgRoles rows of one org; assigning them to members stays in
// UpdateOrgMember/AddOrgMember.

// OrgMaxTagRows caps the tags of one org.
const OrgMaxTagRows = 200

// CreateOrgTag creates a tag (owners and admins only).
func CreateOrgTag(app App, ctx context.Context, actor *OrgActor, org *Record, name, description string) (*Record, error) {
	if !actor.CanManage() {
		return nil, orgErr(403, "Only owners and admins can manage tags.")
	}

	if err := requireLiveOrg(org, actor); err != nil {
		return nil, err
	}

	name = strings.TrimSpace(name)
	if !orgTagRegex.MatchString(name) {
		return nil, orgErr(400, "Invalid tag name.")
	}

	if len(description) > 500 {
		return nil, orgErr(400, "The description is too long.")
	}

	if _, err := app.FindFirstRecordByFilter(CollectionNameOrgRoles, "org={:org} && name={:name}",
		dbx.Params{"org": org.Id, "name": name}); err == nil {
		return nil, orgErr(409, "That tag already exists.")
	}

	if all, _ := app.FindRecordsByFilter(CollectionNameOrgRoles, "org={:org}", "", OrgMaxTagRows, 0, dbx.Params{"org": org.Id}); len(all) >= OrgMaxTagRows {
		return nil, orgErr(400, "Too many tags.")
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgRoles)
	if err != nil {
		return nil, err
	}

	tag := NewRecord(col)
	tag.Set("org", org.Id)
	tag.Set("name", name)
	tag.Set("description", strings.TrimSpace(description))

	if err := saveOrgRecord(app, ctx, tag); err != nil {
		return nil, err
	}

	return tag, nil
}

// DeleteOrgTag deletes a tag; the records engine removes it from every
// membership that carried it.
func DeleteOrgTag(app App, ctx context.Context, actor *OrgActor, org, tag *Record) error {
	if tag.GetString("org") != org.Id {
		return orgErr(404, "Tag not found.")
	}

	if !actor.CanManage() {
		return orgErr(403, "Only owners and admins can manage tags.")
	}

	if err := requireLiveOrg(org, actor); err != nil {
		return err
	}

	return deleteOrgRecord(app, ctx, tag)
}
