package core

import (
	"fmt"
	"strings"
)

// Fork-local (Claviger #79a): ready-made API rules for org-owned collections.
//
// A collection is org-owned when it has a relation field named `org` that
// points at _orgs. Its rules then test the membership through the back
// relation `org._orgMembers_via_org`. All conditions on that path share one
// join, so they are evaluated on the same membership row: `user ?= me &&
// role ?= "admin"` means "I am an admin of this record's org", never "I am a
// member and somebody is an admin". The multi-valued `?` operators are
// required on those paths (a plain `>` on a multi-valued path silently
// denies everything).
//
// The rules need no resolver changes. docs/orgs.md explains each one; the
// admin UI and GET /api/orgs/rule-templates serve these exact strings, and
// the tests run them against real collections.

// OrgRuleSet is a complete set of rules for an org-owned collection.
//
// A nil rule means "superusers only" (null), the same as in a collection.
type OrgRuleSet struct {
	List   *string `json:"listRule"`
	View   *string `json:"viewRule"`
	Create *string `json:"createRule"`
	Update *string `json:"updateRule"`
	Delete *string `json:"deleteRule"`
}

// OrgRulePreset names a rule set.
type OrgRulePreset struct {
	Key         string     `json:"key"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Rules       OrgRuleSet `json:"rules"`
}

// OrgMemberRule returns the condition "the authenticated user is an active
// member of the record's org, with one of the roles" (no roles = any role).
// field is the name of the relation field (normally "org").
func OrgMemberRule(field string, roles ...string) string {
	p := field + "._orgMembers_via_org"

	rule := fmt.Sprintf(`%s.archived = false && %s.user ?= @request.auth.id && %s.status ?= "active"`, field, p, p)

	if len(roles) == 0 || len(roles) == len(OrgRoles) {
		return rule
	}

	parts := make([]string, len(roles))
	for i, r := range roles {
		parts[i] = fmt.Sprintf(`%s.role ?= "%s"`, p, r)
	}

	if len(parts) == 1 {
		return rule + " && " + parts[0]
	}

	return rule + " && (" + strings.Join(parts, " || ") + ")"
}

// OrgTagRule narrows a member rule to members who carry the tag. The tag is
// matched on the same membership row.
func OrgTagRule(field, tag string) (string, error) {
	if !orgTagRegex.MatchString(tag) {
		return "", fmt.Errorf("invalid tag %q", tag)
	}

	return OrgMemberRule(field) + fmt.Sprintf(` && %s._orgMembers_via_org.tags.name ?= "%s"`, field, tag), nil
}

// OrgParentRule grants access to active members (with the roles) of the
// record's org or of its parent org. One level only.
func OrgParentRule(field string, parentRoles ...string) string {
	pp := field + ".parent._orgMembers_via_org"

	parent := fmt.Sprintf(`%s.user ?= @request.auth.id && %s.status ?= "active"`, pp, pp)

	if len(parentRoles) > 0 {
		parts := make([]string, len(parentRoles))
		for i, r := range parentRoles {
			parts[i] = fmt.Sprintf(`%s.role ?= "%s"`, pp, r)
		}

		parent += " && (" + strings.Join(parts, " || ") + ")"
	}

	return fmt.Sprintf(`%s.archived = false && ((%s) || (%s))`,
		field,
		strings.TrimPrefix(OrgMemberRule(field), field+".archived = false && "),
		parent)
}

// OrgNoMoveRule is appended to update rules: a body that changes the org is
// only allowed into an org where the user has one of the roles. Uses its own
// join alias (n), distinct from the ones in the other rules.
func OrgNoMoveRule(field string, roles ...string) string {
	q := "@collection._orgMembers:n"

	rule := fmt.Sprintf(`%s.org ?= @request.body.%s && %s.user ?= @request.auth.id && %s.status ?= "active"`,
		q, field, q, q)

	if len(roles) > 0 && len(roles) != len(OrgRoles) {
		parts := make([]string, len(roles))
		for i, r := range roles {
			parts[i] = fmt.Sprintf(`%s.role ?= "%s"`, q, r)
		}

		rule += " && (" + strings.Join(parts, " || ") + ")"
	}

	return fmt.Sprintf(`(@request.body.%s:isset = false || (%s))`, field, rule)
}

// OrgRulePresets returns the ready-made rule sets for an org-owned collection
// whose relation field is called field.
func OrgRulePresets(field string) []OrgRulePreset {
	read := OrgMemberRule(field)
	writers := []string{OrgRoleOwner, OrgRoleAdmin, OrgRoleMember}
	admins := []string{OrgRoleOwner, OrgRoleAdmin}

	write := OrgMemberRule(field, writers...)
	adminOnly := OrgMemberRule(field, admins...)
	writeMove := write + " && " + OrgNoMoveRule(field, writers...)
	adminMove := adminOnly + " && " + OrgNoMoveRule(field, admins...)

	return []OrgRulePreset{
		{
			Key:         "members",
			Title:       "Members read and write, admins delete",
			Description: "Every active member (viewers included) reads. Owners, admins and members create and edit. Owners and admins delete. Records cannot be moved to an org where the user lacks write access.",
			Rules: OrgRuleSet{
				List: &read, View: &read, Create: &write,
				Update: &writeMove,
				Delete: &adminOnly,
			},
		},
		{
			Key:         "members-all",
			Title:       "Members read and write, members delete",
			Description: "Like the first preset, but owners, admins and members delete too.",
			Rules: OrgRuleSet{
				List: &read, View: &read, Create: &write,
				Update: &writeMove,
				Delete: &write,
			},
		},
		{
			Key:         "admins",
			Title:       "Members read, admins write",
			Description: "Every active member reads. Only owners and admins create, edit and delete.",
			Rules: OrgRuleSet{
				List: &read, View: &read, Create: &adminOnly,
				Update: &adminMove,
				Delete: &adminOnly,
			},
		},
		{
			Key:         "readonly",
			Title:       "Members read, superusers write",
			Description: "Every active member reads. Writes are left to superusers (null rules).",
			Rules:       OrgRuleSet{List: &read, View: &read},
		},
	}
}
