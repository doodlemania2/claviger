package core

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Fork-local (Claviger #84): rules generated before the tag's org was checked
// carry the old tag clause (`tags.name ?= "x"` alone). UpgradeOrgTagRules
// rewrites the rules that are exactly what a generator printed back then and
// leaves every other rule, edited or hand-written, alone. Migration
// 1780002600 calls it.

// OrgTagRuleChange is one rule rewritten by UpgradeOrgTagRules.
type OrgTagRuleChange struct {
	Collection string `json:"collection"`
	Rule       string `json:"rule"` // listRule, viewRule, ...
	Old        string `json:"old"`
	New        string `json:"new"`
}

var (
	legacyTagRuleRegex      = regexp.MustCompile(`^(.+?)\.archived = false && .*\._orgMembers_via_org\.tags\.name \?= "([^"\\]+)"$`)
	legacyAncestorTagRegex  = regexp.MustCompile(`^(.+?)\.archived = false && @collection\._orgClosure:k\.descendant .*@collection\._orgMembers:m\.tags\.name \?= "([^"\\]+)"$`)
	legacyAncestorRoleRegex = regexp.MustCompile(`@collection\._orgMembers:m\.role \?= "(\w+)"`)
	legacyAncestorDepthRe   = regexp.MustCompile(`@collection\._orgClosure:k\.depth \?<= (\d+)`)
	legacyStaffRegex        = regexp.MustCompile(`^\((.+?)\.archived = false && `)
)

// upgradeLegacyTagRule returns the current text of a rule when rule is
// exactly the legacy text of a generator, or "" when it is not.
func upgradeLegacyTagRule(rule string) string {
	if rule == "" || !strings.Contains(rule, `tags.name ?= "`) {
		return ""
	}

	// OrgTagRule(field, tag)
	if m := legacyTagRuleRegex.FindStringSubmatch(rule); m != nil {
		if old, err := orgTagRule(m[1], m[2], true); err == nil && old == rule {
			if cur, err := orgTagRule(m[1], m[2], false); err == nil {
				return cur
			}
		}
	}

	// OrgAncestorRule(field, {Roles, MaxDepth, Tag}) in its tag (alias) form
	if m := legacyAncestorTagRegex.FindStringSubmatch(rule); m != nil {
		opts := OrgAncestorOpts{Tag: m[2]}

		for _, r := range legacyAncestorRoleRegex.FindAllStringSubmatch(rule, -1) {
			opts.Roles = append(opts.Roles, r[1])
		}

		if d := legacyAncestorDepthRe.FindStringSubmatch(rule); d != nil {
			opts.MaxDepth, _ = strconv.Atoi(d[1])
		}

		if old, err := orgAncestorRule(m[1], opts, true); err == nil && old == rule {
			if cur, err := orgAncestorRule(m[1], opts, false); err == nil {
				return cur
			}
		}
	}

	// HouseholdStaffRule(orgField)
	if m := legacyStaffRegex.FindStringSubmatch(rule); m != nil && householdStaffRule(m[1], true) == rule {
		return householdStaffRule(m[1], false)
	}

	return ""
}

// UpgradeOrgTagRules rewrites the generated rules of every collection that
// still carry the legacy tag clause and returns what it changed. It matches
// whole rules against what the generators printed (the system household
// collections and the credentials collection are matched against their exact
// migration text), so a rule someone edited is never touched.
func UpgradeOrgTagRules(app App) ([]OrgTagRuleChange, error) {
	collections, err := app.FindAllCollections()
	if err != nil {
		return nil, err
	}

	oldH, oldM := householdRulesFor(true)
	newH, newM := householdRulesFor(false)

	credOld, credNew, err := credentialListRules()
	if err != nil {
		return nil, err
	}

	var changes []OrgTagRuleChange

	for _, col := range collections {
		type slot struct {
			name string
			rule **string
		}

		slots := []slot{
			{"listRule", &col.ListRule}, {"viewRule", &col.ViewRule}, {"createRule", &col.CreateRule},
			{"updateRule", &col.UpdateRule}, {"deleteRule", &col.DeleteRule},
		}

		if col.IsAuth() {
			slots = append(slots, slot{"authRule", &col.AuthRule}, slot{"manageRule", &col.ManageRule})
		}

		dirty := false

		for _, s := range slots {
			if *s.rule == nil {
				continue
			}

			old, cur := **s.rule, ""

			switch {
			case col.Name == CollectionNameHouseholds && old == oldH && (s.name == "listRule" || s.name == "viewRule"):
				cur = newH
			case col.Name == CollectionNameHouseholdMembers && old == oldM && (s.name == "listRule" || s.name == "viewRule"):
				cur = newM
			case col.Name == CollectionNameCredentials && old == credOld && (s.name == "listRule" || s.name == "viewRule"):
				cur = credNew
			default:
				cur = upgradeLegacyTagRule(old)
			}

			if cur == "" || cur == old {
				continue
			}

			next := cur
			*s.rule = &next
			dirty = true

			changes = append(changes, OrgTagRuleChange{Collection: col.Name, Rule: s.name, Old: old, New: cur})
		}

		if dirty {
			// the rules refer to other system collections, so skip the
			// cross-collection validation the way migration 1600 does
			if err := app.SaveNoValidate(col); err != nil {
				return changes, fmt.Errorf("collection %s: %w", col.Name, err)
			}
		}
	}

	return changes, nil
}

// credentialListRules renders the list/view rule of _credentials as migration
// 1780001600 wrote it (legacy) and as it is now.
func credentialListRules() (legacy, current string, err error) {
	build := func(old bool) (string, error) {
		admins, err := orgAncestorRule("org", OrgAncestorOpts{Roles: []string{OrgRoleOwner, OrgRoleAdmin}}, old)
		if err != nil {
			return "", err
		}

		tagged, err := orgAncestorRule("org", OrgAncestorOpts{
			Roles: []string{OrgRoleOwner, OrgRoleAdmin, OrgRoleMember}, Tag: CredentialCoordinatorTag,
		}, old)
		if err != nil {
			return "", err
		}

		return `@request.auth.id != "" && (user = @request.auth.id || (` + admins + `) || (` + tagged + `))`, nil
	}

	if legacy, err = build(true); err != nil {
		return "", "", err
	}

	current, err = build(false)

	return legacy, current, err
}
