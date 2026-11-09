package apis

import (
	"net/http"
	"strconv"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #79c): the SCIM Users resource.

func scimUserResource(r *scimReq, member, user *core.Record) map[string]any {
	email := user.Email()
	name := strings.TrimSpace(user.GetString("name"))

	out := map[string]any{
		"schemas":  []string{scimSchemaUser},
		"id":       user.Id,
		"userName": email,
		"emails":   []map[string]any{{"value": email, "type": "work", "primary": true}},
		"active":   member.GetString("status") == core.OrgStatusActive,
	}

	if ext := member.GetString("externalId"); ext != "" {
		out["externalId"] = ext
	}

	if name != "" {
		out["displayName"] = name
		out["name"] = map[string]any{"formatted": name}
	}

	userGroups := core.SCIMGroupsOfUser(r.e.App, r.org, user.Id)
	groups := make([]map[string]any, 0, len(userGroups))

	for _, g := range userGroups {
		groups = append(groups, map[string]any{
			"value": g.Id, "display": g.GetString("displayName"), "$ref": r.base() + "/Groups/" + g.Id,
		})
	}

	out["groups"] = groups

	meta := scimMeta(r, "User", "/Users/"+user.Id)
	meta["created"] = scimTime(member.GetDateTime("created"))
	meta["lastModified"] = scimTime(member.GetDateTime("updated"))
	out["meta"] = meta

	return out
}

// scimPaging reads startIndex and count.
func scimPaging(r *scimReq) (start, count int) {
	q := r.e.Request.URL.Query()

	start, _ = strconv.Atoi(q.Get("startIndex"))
	if start < 1 {
		start = 1
	}

	count = scimDefaultCount

	if v := q.Get("count"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			count = n
		}
	}

	if count < 0 {
		count = 0
	}

	if count > scimMaxResults {
		count = scimMaxResults
	}

	return start, count
}

func scimListResponse(r *scimReq, total, start int, items []map[string]any) error {
	if items == nil {
		items = []map[string]any{}
	}

	return scimWrite(r.e, http.StatusOK, map[string]any{
		"schemas": []string{scimSchemaList}, "totalResults": total, "startIndex": start,
		"itemsPerPage": len(items), "Resources": items,
	})
}

func scimUsersList(r *scimReq) error {
	preds, err := parseSCIMFilter(r.e.Request.URL.Query().Get("filter"),
		"id", "username", "externalid", "displayname", "active", "emails.value")
	if err != nil {
		return scimFail(r, err)
	}

	start, count := scimPaging(r)

	col, err := core.OrgUserCollection(r.e.App)
	if err != nil {
		return scimFail(r, err)
	}

	var members []*core.Record

	total := 0

	if len(preds) == 0 {
		total64, cerr := r.e.App.CountRecords(core.CollectionNameOrgMembers, orgHash(r.org.Id))
		if cerr != nil {
			return scimFail(r, cerr)
		}

		total = int(total64)

		if count > 0 {
			members, err = r.e.App.FindRecordsByFilter(core.CollectionNameOrgMembers, "org={:org}", "created,id",
				count, start-1, dbx.Params{"org": r.org.Id})
			if err != nil {
				return scimFail(r, err)
			}
		}
	} else {
		members, err = scimCandidates(r, col, preds)
		if err != nil {
			return scimFail(r, err)
		}
	}

	users, err := scimLoadUsers(r, col, members)
	if err != nil {
		return scimFail(r, err)
	}

	if len(preds) > 0 {
		kept := members[:0:0]

		for _, m := range members {
			if u := users[m.GetString("user")]; u != nil && scimUserMatches(m, u, preds) {
				kept = append(kept, m)
			}
		}

		total = len(kept)

		lo := min(start-1, len(kept))
		hi := min(lo+count, len(kept))
		members = kept[lo:hi]
	}

	items := make([]map[string]any, 0, len(members))

	for _, m := range members {
		if u := users[m.GetString("user")]; u != nil {
			items = append(items, scimUserResource(r, m, u))
		}
	}

	return scimListResponse(r, total, start, items)
}

// scimCandidates narrows the memberships to look at using the first
// selective predicate (every predicate is checked again afterwards).
func scimCandidates(r *scimReq, col *core.Collection, preds []scimPred) ([]*core.Record, error) {
	for _, p := range preds {
		s, isStr := p.value.(string)
		if !isStr {
			continue
		}

		switch p.attr {
		case "id":
			if m, err := core.FindOrgMember(r.e.App, r.org.Id, s); err == nil {
				return []*core.Record{m}, nil
			}

			return nil, nil
		case "username", "emails.value":
			u, err := r.e.App.FindAuthRecordByEmail(col, strings.ToLower(strings.TrimSpace(s)))
			if err != nil {
				return nil, nil
			}

			if m, err := core.FindOrgMember(r.e.App, r.org.Id, u.Id); err == nil {
				return []*core.Record{m}, nil
			}

			return nil, nil
		case "externalid":
			if s == "" {
				continue
			}

			if m, err := r.e.App.FindFirstRecordByFilter(core.CollectionNameOrgMembers, "org={:o} && externalId={:e}",
				dbx.Params{"o": r.org.Id, "e": s}); err == nil {
				return []*core.Record{m}, nil
			}

			return nil, nil
		}
	}

	return r.e.App.FindRecordsByFilter(core.CollectionNameOrgMembers, "org={:org}", "created,id", 0, 0,
		dbx.Params{"org": r.org.Id})
}

func scimLoadUsers(r *scimReq, col *core.Collection, members []*core.Record) (map[string]*core.Record, error) {
	out := make(map[string]*core.Record, len(members))
	if len(members) == 0 {
		return out, nil
	}

	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.GetString("user"))
	}

	recs, err := r.e.App.FindRecordsByIds(col.Id, ids)
	if err != nil {
		return nil, err
	}

	for _, u := range recs {
		out[u.Id] = u
	}

	return out, nil
}

func scimUserMatches(m, u *core.Record, preds []scimPred) bool {
	for _, p := range preds {
		switch p.attr {
		case "id":
			if p.value != u.Id {
				return false
			}
		case "username", "emails.value":
			if s, _ := p.value.(string); !strings.EqualFold(s, u.Email()) {
				return false
			}
		case "externalid":
			if p.value != m.GetString("externalId") {
				return false
			}
		case "displayname":
			if s, _ := p.value.(string); !strings.EqualFold(s, strings.TrimSpace(u.GetString("name"))) {
				return false
			}
		case "active":
			if p.value != (m.GetString("status") == core.OrgStatusActive) {
				return false
			}
		}
	}

	return true
}

func scimUsersGet(r *scimReq) error {
	member, user, err := core.FindSCIMMember(r.e.App, r.org, r.e.Request.PathValue("id"))
	if err != nil {
		return scimFail(r, err)
	}

	return scimWrite(r.e, http.StatusOK, scimUserResource(r, member, user))
}

func scimUsersCreate(r *scimReq) error {
	body, err := scimReadObject(r)
	if err != nil {
		return scimFail(r, err)
	}

	var attrs core.SCIMUserAttrs
	if err := scimApplyUserObject(&attrs, body); err != nil {
		return scimFail(r, err)
	}

	member, user, err := core.SCIMCreateUser(r.e.App, r.ctx, r.org, attrs)
	if err != nil {
		return scimFail(r, err)
	}

	r.e.Response.Header().Set("Location", r.base()+"/Users/"+user.Id)

	return scimWrite(r.e, http.StatusCreated, scimUserResource(r, member, user))
}

func scimUsersPut(r *scimReq) error {
	member, user, err := core.FindSCIMMember(r.e.App, r.org, r.e.Request.PathValue("id"))
	if err != nil {
		return scimFail(r, err)
	}

	body, err := scimReadObject(r)
	if err != nil {
		return scimFail(r, err)
	}

	var attrs core.SCIMUserAttrs
	if err := scimApplyUserObject(&attrs, body); err != nil {
		return scimFail(r, err)
	}

	return scimUserUpdate(r, member, user, attrs)
}

func scimUsersPatch(r *scimReq) error {
	member, user, err := core.FindSCIMMember(r.e.App, r.org, r.e.Request.PathValue("id"))
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

	var attrs core.SCIMUserAttrs
	if err := scimApplyUserPatch(&attrs, ops); err != nil {
		return scimFail(r, err)
	}

	return scimUserUpdate(r, member, user, attrs)
}

func scimUserUpdate(r *scimReq, member, user *core.Record, attrs core.SCIMUserAttrs) error {
	member, user, err := core.SCIMUpdateUser(r.e.App, r.ctx, r.org, member, user, attrs)
	if err != nil {
		return scimFail(r, err)
	}

	return scimWrite(r.e, http.StatusOK, scimUserResource(r, member, user))
}

func scimUsersDelete(r *scimReq) error {
	member, user, err := core.FindSCIMMember(r.e.App, r.org, r.e.Request.PathValue("id"))
	if err != nil {
		return scimFail(r, err)
	}

	if err := core.SCIMDeleteUser(r.e.App, r.ctx, r.org, member, user); err != nil {
		return scimFail(r, err)
	}

	return r.e.NoContent(http.StatusNoContent)
}

func orgHash(orgId string) dbx.Expression { return dbx.HashExp{"org": orgId} }
