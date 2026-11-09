package apis

import (
	"net/http"
	"regexp"
	"slices"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #79c): the SCIM Groups resource. A group is a tag (and
// optionally a role) on its members' memberships, never an org; see
// core/scim_groups.go for the mapping.

func scimGroupResource(r *scimReq, group *core.Record, withMembers bool) (map[string]any, error) {
	out := map[string]any{
		"schemas":     []string{scimSchemaGroup},
		"id":          group.Id,
		"displayName": group.GetString("displayName"),
	}

	if ext := group.GetString("externalId"); ext != "" {
		out["externalId"] = ext
	}

	if withMembers {
		users, err := core.SCIMGroupMemberUsers(r.e.App, r.org, group)
		if err != nil {
			return nil, err
		}

		members := make([]map[string]any, 0, len(users))
		for _, u := range users {
			members = append(members, map[string]any{
				"value": u.Id, "display": u.Email(), "type": "User", "$ref": r.base() + "/Users/" + u.Id,
			})
		}

		out["members"] = members
	}

	meta := scimMeta(r, "Group", "/Groups/"+group.Id)
	meta["created"] = scimTime(group.GetDateTime("created"))
	meta["lastModified"] = scimTime(group.GetDateTime("updated"))
	out["meta"] = meta

	return out, nil
}

// scimWantMembers honours excludedAttributes=members (Entra asks for it).
func scimWantMembers(r *scimReq) bool {
	for _, a := range strings.Split(r.e.Request.URL.Query().Get("excludedAttributes"), ",") {
		if strings.EqualFold(strings.TrimSpace(a), "members") {
			return false
		}
	}

	return true
}

func scimGroupsList(r *scimReq) error {
	preds, err := parseSCIMFilter(r.e.Request.URL.Query().Get("filter"), "id", "displayname", "externalid")
	if err != nil {
		return scimFail(r, err)
	}

	start, count := scimPaging(r)

	all, err := r.e.App.FindRecordsByFilter(core.CollectionNameSCIMGroups, "org={:org}", "displayName,id", 0, 0,
		dbx.Params{"org": r.org.Id})
	if err != nil {
		return scimFail(r, err)
	}

	var kept []*core.Record

	for _, g := range all {
		ok := true

		for _, p := range preds {
			s, _ := p.value.(string)

			switch p.attr {
			case "id":
				ok = ok && s == g.Id
			case "displayname":
				ok = ok && strings.EqualFold(s, g.GetString("displayName"))
			case "externalid":
				ok = ok && s == g.GetString("externalId")
			}
		}

		if ok {
			kept = append(kept, g)
		}
	}

	lo := min(start-1, len(kept))
	hi := min(lo+count, len(kept))
	withMembers := scimWantMembers(r)

	items := make([]map[string]any, 0, hi-lo)

	for _, g := range kept[lo:hi] {
		res, err := scimGroupResource(r, g, withMembers)
		if err != nil {
			return scimFail(r, err)
		}

		items = append(items, res)
	}

	return scimListResponse(r, len(kept), start, items)
}

func scimGroupWrite(r *scimReq, status int, group *core.Record) error {
	res, err := scimGroupResource(r, group, scimWantMembers(r))
	if err != nil {
		return scimFail(r, err)
	}

	return scimWrite(r.e, status, res)
}

func scimGroupsGet(r *scimReq) error {
	group, err := core.FindSCIMGroup(r.e.App, r.org, r.e.Request.PathValue("id"))
	if err != nil {
		return scimFail(r, err)
	}

	return scimGroupWrite(r, http.StatusOK, group)
}

// scimMemberIds reads a members array ([{"value": "<user id>"}]).
func scimMemberIds(v any) ([]string, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, scimInvalidValue("members must be an array.")
	}

	ids := make([]string, 0, len(list))

	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, scimInvalidValue("A member must be an object with a value.")
		}

		id, _ := m["value"].(string)
		if id == "" {
			return nil, scimInvalidValue("A member needs a value (the user id).")
		}

		ids = append(ids, id)
	}

	return ids, nil
}

type scimGroupAttrs struct {
	displayName *string
	externalId  *string
	members     *[]string
}

func scimGroupFromObject(obj map[string]any) (scimGroupAttrs, error) {
	var a scimGroupAttrs

	if v, ok := obj["displayName"]; ok {
		s, err := scimString(v, "displayName")
		if err != nil {
			return a, err
		}

		a.displayName = &s
	}

	if v, ok := obj["externalId"]; ok {
		s, err := scimString(v, "externalId")
		if err != nil {
			return a, err
		}

		a.externalId = &s
	}

	if v, ok := obj["members"]; ok {
		ids, err := scimMemberIds(v)
		if err != nil {
			return a, err
		}

		a.members = &ids
	}

	return a, nil
}

func scimGroupsCreate(r *scimReq) error {
	body, err := scimReadObject(r)
	if err != nil {
		return scimFail(r, err)
	}

	a, err := scimGroupFromObject(body)
	if err != nil {
		return scimFail(r, err)
	}

	if a.displayName == nil {
		return scimFail(r, scimInvalidValue("displayName is required."))
	}

	group, err := core.SCIMSaveGroup(r.e.App, r.ctx, r.org, nil, a.displayName, a.externalId, a.members)
	if err != nil {
		return scimFail(r, err)
	}

	r.e.Response.Header().Set("Location", r.base()+"/Groups/"+group.Id)

	return scimGroupWrite(r, http.StatusCreated, group)
}

func scimGroupsPut(r *scimReq) error {
	group, err := core.FindSCIMGroup(r.e.App, r.org, r.e.Request.PathValue("id"))
	if err != nil {
		return scimFail(r, err)
	}

	body, err := scimReadObject(r)
	if err != nil {
		return scimFail(r, err)
	}

	a, err := scimGroupFromObject(body)
	if err != nil {
		return scimFail(r, err)
	}

	// PUT replaces the member list, an omitted one empties it
	if a.members == nil {
		empty := []string{}
		a.members = &empty
	}

	group, err = core.SCIMSaveGroup(r.e.App, r.ctx, r.org, group, a.displayName, a.externalId, a.members)
	if err != nil {
		return scimFail(r, err)
	}

	return scimGroupWrite(r, http.StatusOK, group)
}

var scimMemberPathRegex = regexp.MustCompile(`(?i)^members\[\s*value\s+eq\s+"([^"]+)"\s*\]$`)

func scimGroupsPatch(r *scimReq) error {
	group, err := core.FindSCIMGroup(r.e.App, r.org, r.e.Request.PathValue("id"))
	if err != nil {
		return scimFail(r, err)
	}

	body, err := scimReadObject(r)
	if err != nil {
		return scimFail(r, err)
	}

	ops, err := scimParsePatch(body)
	if err != nil {
		return scimFail(r, err)
	}

	members := slices.Clone(group.GetStringSlice("members"))

	var a scimGroupAttrs

	apply := func(path string, value any, op string) error {
		low := strings.ToLower(path)

		switch {
		case low == "members":
			switch op {
			case "remove":
				if value == nil {
					members = nil

					return nil
				}

				ids, err := scimMemberIds(value)
				if err != nil {
					return err
				}

				members = slices.DeleteFunc(members, func(id string) bool { return slices.Contains(ids, id) })
			case "replace":
				ids, err := scimMemberIds(value)
				if err != nil {
					return err
				}

				members = ids
			default:
				ids, err := scimMemberIds(value)
				if err != nil {
					return err
				}

				members = append(members, ids...)
			}
		case scimMemberPathRegex.MatchString(path):
			id := scimMemberPathRegex.FindStringSubmatch(path)[1]
			if op == "remove" {
				members = slices.DeleteFunc(members, func(m string) bool { return m == id })
			} else {
				members = append(members, id)
			}
		case low == "displayname":
			if op == "remove" {
				return nil
			}

			s, err := scimString(value, "displayName")
			if err != nil {
				return err
			}

			a.displayName = &s
		case low == "externalid":
			s := ""
			if op != "remove" {
				var err error
				if s, err = scimString(value, "externalId"); err != nil {
					return err
				}
			}

			a.externalId = &s
		}

		return nil
	}

	for _, op := range ops {
		if op.Path == "" {
			obj, ok := op.Value.(map[string]any)
			if !ok {
				return scimFail(r, scimInvalidValue("A path-less operation needs an object value."))
			}

			for k, v := range obj {
				if err := apply(k, v, op.Op); err != nil {
					return scimFail(r, err)
				}
			}

			continue
		}

		if err := apply(op.Path, op.Value, op.Op); err != nil {
			return scimFail(r, err)
		}
	}

	group, err = core.SCIMSaveGroup(r.e.App, r.ctx, r.org, group, a.displayName, a.externalId, &members)
	if err != nil {
		return scimFail(r, err)
	}

	return scimGroupWrite(r, http.StatusOK, group)
}

func scimGroupsDelete(r *scimReq) error {
	group, err := core.FindSCIMGroup(r.e.App, r.org, r.e.Request.PathValue("id"))
	if err != nil {
		return scimFail(r, err)
	}

	if err := core.SCIMDeleteGroup(r.e.App, r.ctx, r.org, group); err != nil {
		return scimFail(r, err)
	}

	return r.e.NoContent(http.StatusNoContent)
}
