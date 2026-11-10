package core

import (
	"fmt"
	"strings"
)

// Fork-local (Claviger #81): rule snippets for the credential gate.
//
// There is no `hasCredential` helper in the rule engine (the resolver is
// upstream's and stays untouched). The gate is plain filter syntax that joins
// _credentials with an `@collection` alias, the same way the org presets do.
// The prototype of docs/specs/80-88-congregations.md 2.2 ran these forms.
//
// Every condition on the alias `c` is evaluated on the SAME credential row, so
// "my credential of this type, not expired, not revoked, verified" means one
// credential. `expires ?> @now` is evaluated at query time: access lapses at
// the expiry with no job. Aliases used: c (credentials) and q (closure); they
// avoid n (OrgNoMoveRule), and k and m (OrgAncestorRule with a tag).

// Credential scopes for CredentialRuleOpts.Scope.
const (
	// CredentialScopeAny accepts a credential issued by any org.
	CredentialScopeAny = "any"
	// CredentialScopeOrg accepts only a credential issued by the record's org.
	CredentialScopeOrg = "org"
	// CredentialScopeAncestors accepts a credential issued by the record's org
	// or by any of its ancestors (a diocese-issued credential counts in a
	// parish), through _orgClosure.
	CredentialScopeAncestors = "ancestors"
)

// CredentialRuleOpts configures CredentialGateRule.
type CredentialRuleOpts struct {
	// Type is the credential type key (required), for example "safe_environment".
	Type string
	// Scope is one of the CredentialScope constants (default: org, the strictest
	// useful form, so that a credential of another parish does not open this one).
	Scope string
	// MaxDepth, for the ancestors scope, limits how many levels above the
	// record's org the issuing org may be (0 = no limit).
	MaxDepth int
}

// CredentialGateRule returns the condition "the authenticated user holds a
// current credential of the type" for a collection whose relation to the org
// is field. AND it with a member rule (OrgMemberRule / OrgAncestorRule); see
// CredentialRuleSet for a full set.
//
// Do not write the ancestor test as a path on the right of a comparison; a
// multi-valued path there silently matches nothing. This form uses a second
// alias, as the spec requires.
func CredentialGateRule(field string, opts CredentialRuleOpts) (string, error) {
	if !ValidCredentialKey(opts.Type) {
		return "", fmt.Errorf("invalid credential type %q", opts.Type)
	}

	if opts.MaxDepth < 0 || opts.MaxDepth > OrgHardMaxDepth {
		return "", fmt.Errorf("invalid depth %d", opts.MaxDepth)
	}

	c := "@collection." + CollectionNameCredentials + ":c"

	parts := []string{
		c + `.user ?= @request.auth.id`,
		fmt.Sprintf(`%s.type ?= "%s"`, c, opts.Type),
		c + `.expires ?> @now`,
		c + `.revoked ?= false`,
		c + `.verified ?= true`,
	}

	switch opts.Scope {
	case "", CredentialScopeOrg:
		parts = append(parts, fmt.Sprintf(`%s.org ?= %s`, c, field))
	case CredentialScopeAny:
	case CredentialScopeAncestors:
		q := "@collection." + CollectionNameOrgClosure + ":q"

		parts = append(parts,
			fmt.Sprintf(`%s.descendant ?= %s`, q, field),
			fmt.Sprintf(`%s.ancestor ?= %s.org`, q, c))

		if opts.MaxDepth > 0 {
			parts = append(parts, fmt.Sprintf(`%s.depth ?<= %d`, q, opts.MaxDepth))
		}
	default:
		return "", fmt.Errorf("invalid scope %q", opts.Scope)
	}

	return strings.Join(parts, " && "), nil
}

// CredentialGatedMemberRule is OrgMemberRule(field, roles...) AND the gate.
func CredentialGatedMemberRule(field string, opts CredentialRuleOpts, roles ...string) (string, error) {
	gate, err := CredentialGateRule(field, opts)
	if err != nil {
		return "", err
	}

	return OrgMemberRule(field, roles...) + " && " + gate, nil
}

// CredentialRuleSet is the "members" preset of OrgRulePresets with the gate
// added to list, view, create and update: members read and write only while
// they hold a current credential. Owners and admins still delete, without the
// gate, so a coordinator can clean up. Records cannot be moved to an org
// where the user lacks write access.
func CredentialRuleSet(field string, opts CredentialRuleOpts) (OrgRuleSet, error) {
	gate, err := CredentialGateRule(field, opts)
	if err != nil {
		return OrgRuleSet{}, err
	}

	set := OrgRulePresets(field)[0].Rules

	gated := func(rule *string) *string {
		s := *rule + " && " + gate

		return &s
	}

	set.List, set.View, set.Create, set.Update = gated(set.List), gated(set.View), gated(set.Create), gated(set.Update)

	return set, nil
}
