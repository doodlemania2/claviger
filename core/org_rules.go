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
	return orgTagRule(field, tag, false)
}

// orgTagRule renders OrgTagRule; legacy renders the text generated before the
// tag's org was checked (kept only so the migration can recognise it).
func orgTagRule(field, tag string, legacy bool) (string, error) {
	if !orgTagRegex.MatchString(tag) {
		return "", fmt.Errorf("invalid tag %q", tag)
	}

	return OrgMemberRule(field) + orgTagCondition(field+"._orgMembers_via_org", tag, legacy), nil
}

// orgTagCondition is the condition "the membership row m carries a tag called
// tag that belongs to the membership's own org". Both tests are read through
// the one `tags` join of the membership, so they hold for the SAME tag row: a
// tag of another org named tag, next to a tag of this org with another name,
// grants nothing. The org check is the rule-side twin of OrgTagNames and
// validateOrgMemberTags (a role tag is owned by one org); it matters for rows
// written around the hooks (a raw database write, an import). Every generated
// rule that tests a tag name goes through here.
func orgTagCondition(m, tag string, legacy bool) string {
	if legacy {
		return fmt.Sprintf(` && %s.tags.name ?= "%s"`, m, tag)
	}

	return fmt.Sprintf(` && %s.tags.name ?= "%s" && %s.tags.org ?= %s.org`, m, tag, m, m)
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

// OrgAncestorOpts narrows OrgAncestorRule. The zero value means "an active
// member of the record's org or of any ancestor org, any role".
type OrgAncestorOpts struct {
	// Roles limits the membership at the ancestor (empty = any role).
	Roles []string
	// MaxDepth, when > 0, only counts ancestors at most this many levels above
	// the record's org (0 is the org itself, 1 its parent, ...).
	MaxDepth int
	// Tag, when set, requires the membership to carry this tag.
	Tag string
}

// OrgAncestorRule (#84) grants access to active members of the record's org
// **or of any of its ancestors** through the closure table `_orgClosure`.
// Oversight is opt-in: use it only on collections that should be visible to
// higher orgs; the presets stay org-only (OrgMemberRule). Pastoral data
// never gets an ancestor rule.
//
// Every condition on `ancestor._orgMembers_via_org` and on `depth` shares one
// join, so role, tag and depth are read from the same row: "admin at an
// ancestor within two levels" means that one ancestor, not an admin somewhere
// and a membership elsewhere. Members of a lower org never see upward: the
// rule only walks from the record's org to its ancestors.
func OrgAncestorRule(field string, opts OrgAncestorOpts) (string, error) {
	return orgAncestorRule(field, opts, false)
}

// orgAncestorRule renders OrgAncestorRule; legacy renders the text generated
// before the tag's org was checked (see orgTagRule).
func orgAncestorRule(field string, opts OrgAncestorOpts, legacy bool) (string, error) {
	if opts.Tag != "" && !orgTagRegex.MatchString(opts.Tag) {
		return "", fmt.Errorf("invalid tag %q", opts.Tag)
	}

	if opts.MaxDepth < 0 || opts.MaxDepth > OrgHardMaxDepth {
		return "", fmt.Errorf("invalid depth %d", opts.MaxDepth)
	}

	for _, r := range opts.Roles {
		if OrgRoleRank(r) == 0 {
			return "", fmt.Errorf("invalid role %q", r)
		}
	}

	// A tag has to be matched through the tags relation of the membership,
	// which makes the join alias of the back-relation path longer than the 63
	// characters of a Postgres identifier (two aliases then truncate to the
	// same name and the query fails). So a tag rule is written with explicit,
	// short @collection aliases (k = closure row, m = membership), which keeps
	// the conditions on one row each and works on both engines.
	var (
		member, depth, ancestors string
		prefix                   string
	)

	if opts.Tag != "" {
		k, m := "@collection."+CollectionNameOrgClosure+":k", "@collection."+CollectionNameOrgMembers+":m"
		prefix = fmt.Sprintf(`%s.archived = false && %s.descendant ?= %s && %s.org ?= %s.ancestor`, field, k, field, m, k)
		member = m
		depth = k + ".depth"
		ancestors = orgTagCondition(m, opts.Tag, legacy)
	} else {
		cp := field + "." + CollectionNameOrgClosure + "_via_descendant"
		member = cp + ".ancestor._orgMembers_via_org"
		depth = cp + ".depth"
		prefix = field + ".archived = false"
	}

	rule := fmt.Sprintf(`%s && %s.user ?= @request.auth.id && %s.status ?= "active"`, prefix, member, member)

	if len(opts.Roles) > 0 {
		parts := make([]string, len(opts.Roles))
		for i, r := range opts.Roles {
			parts[i] = fmt.Sprintf(`%s.role ?= "%s"`, member, r)
		}

		if len(parts) == 1 {
			rule += " && " + parts[0]
		} else {
			rule += " && (" + strings.Join(parts, " || ") + ")"
		}
	}

	if opts.MaxDepth > 0 {
		rule += fmt.Sprintf(` && %s ?<= %d`, depth, opts.MaxDepth)
	}

	return rule + ancestors, nil
}

// OrgOversightRuleSet (#84) is the "members read, ancestors read too" rule
// set: list and view use OrgAncestorRule, writes stay with the record's own
// org (the "members" preset).
func OrgOversightRuleSet(field string, opts OrgAncestorOpts) (OrgRuleSet, error) {
	read, err := OrgAncestorRule(field, opts)
	if err != nil {
		return OrgRuleSet{}, err
	}

	set := OrgRulePresets(field)[0].Rules
	set.List, set.View = &read, &read

	return set, nil
}
