package oidc

import (
	"errors"
	"slices"

	"github.com/doodlemania2/claviger/core"
	"github.com/zitadel/oidc/v3/pkg/oidc"
)

// Fork-local (Claviger #79a): organizations in the provider.
//
//   - Scope "orgs" (opt-in per client, like any scope): the ID token, the
//     userinfo and JWT access tokens carry the user's active memberships.
//   - A client may be bound to one org (_oauthClients.org). Only active members
//     of it can sign in, and its tokens carry that org alone.
//
// Claims are read at token time, so a role change shows at the next refresh or
// userinfo call, and a suspended member stops working at once.

const (
	// ScopeOrgs requests the organization claims.
	ScopeOrgs = "orgs"

	// maxOrgClaims caps the orgs claim.
	maxOrgClaims = 50
)

var errNotOrgMember = errors.New("not a member of the organization this application belongs to")

// orgBoundOK reports whether the account may use the client: always, unless
// the client is bound to an org the account is not an active member of.
func (s *Store) orgBoundOK(c *Client, account *core.Record) error {
	if c == nil || c.OrgID == "" {
		return nil
	}

	org, err := s.app.FindRecordById(core.CollectionNameOrgs, c.OrgID)
	if err != nil || core.ActiveOrgMember(s.app, org, account.Id) == nil {
		return errNotOrgMember
	}

	return nil
}

// orgClaims returns the claims of the scope orgs for the account, or nil.
func (s *Store) orgClaims(clientID string, account *core.Record, scopes []string) map[string]any {
	if !slices.Contains(scopes, ScopeOrgs) {
		return nil
	}

	client, _ := s.findClient(clientID)

	if client != nil && client.OrgID != "" {
		org, err := s.app.FindRecordById(core.CollectionNameOrgs, client.OrgID)
		if err != nil {
			return nil
		}

		m := core.ActiveOrgMember(s.app, org, account.Id)
		if m == nil {
			return nil
		}

		return map[string]any{
			"org_id":   org.Id,
			"org_slug": org.GetString("slug"),
			"org_role": m.GetString("role"),
			"org_tags": core.OrgTagNames(s.app, m),
		}
	}

	members, err := core.ActiveOrgMemberships(s.app, account.Id)
	if err != nil {
		return nil
	}

	orgs := make([]map[string]any, 0, len(members))
	roles := map[string]string{}

	for _, m := range members {
		org, err := s.app.FindRecordById(core.CollectionNameOrgs, m.GetString("org"))
		if err != nil {
			continue
		}

		if len(orgs) >= maxOrgClaims {
			return map[string]any{"orgs": orgs, "org_roles": roles, "orgs_truncated": true}
		}

		orgs = append(orgs, map[string]any{
			"id": org.Id, "slug": org.GetString("slug"), "role": m.GetString("role"), "tags": core.OrgTagNames(s.app, m),
		})
		roles[org.Id] = m.GetString("role")
	}

	return map[string]any{"orgs": orgs, "org_roles": roles}
}

// appendOrgClaims adds the org claims to a userinfo / ID token.
func (s *Store) appendOrgClaims(ui *oidc.UserInfo, clientID string, account *core.Record, scopes []string) {
	for k, v := range s.orgClaims(clientID, account, scopes) {
		ui.AppendClaims(k, v)
	}
}

// clientOrgStillOK re-checks the org binding of a client for a live grant.
func (s *Store) clientOrgStillOK(clientID, collectionRef, subject string) error {
	client, err := s.findClient(clientID)
	if err != nil || client.OrgID == "" {
		return nil
	}

	account, err := s.accountOf(collectionRef, subject)
	if err != nil {
		return err
	}

	return s.orgBoundOK(client, account)
}
