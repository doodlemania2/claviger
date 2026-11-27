package sso

import (
	"context"
	"net/http"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

// ExternalAuthProvider is the _externalAuths.provider value of the accounts
// linked to a connection: "sso:<connection id>". The links are the ordinary
// _externalAuths rows, so a person sees them like any other linked sign-in and
// the data export and erasure engines already cover them.
func ExternalAuthProvider(connectionId string) string { return "sso:" + connectionId }

// Outcome is the result of ProvisionLogin.
type Outcome struct {
	User    *core.Record
	Member  *core.Record
	Created bool // the account was created just now (JIT)
	Linked  bool // an existing account was linked just now
}

// MapRole returns the highest org role that the groups map to ("" when none
// of them is in the mapping).
func MapRole(c *Connection, groups []string) string {
	best := ""

	for _, g := range groups {
		role, ok := c.RoleMapping[g]
		if !ok {
			continue
		}

		if core.OrgRoleRank(role) > core.OrgRoleRank(best) {
			best = role
		}
	}

	return best
}

// ProvisionLogin turns a verified identity into an account and an org
// membership, and returns the account to sign in. The rules, in order:
//
//  1. The email must belong to a domain this connection holds a VERIFIED claim
//     for. Without that, an identity provider could assert any address (the
//     owner's of another organization) and take its account over.
//  2. An account already linked to (connection, subject) is the person; the
//     email is not consulted, so a changed address does not lock anyone out.
//  3. Otherwise an existing account with that email is linked, but only when
//     the identity provider vouches for the address (email_verified, or
//     Entra's xms_edov). Otherwise the sign-in is refused: the person must not
//     be given someone else's account on an unconfirmed address. An account
//     whose own email was never verified is taken over for its owner: it is
//     marked verified and its password and tokens are replaced.
//  4. Otherwise an account is created (just-in-time) when the connection allows
//     it.
//  5. The person becomes a member of the org: role from the mapped groups, else
//     the connection's default role, else "member". A member whose source is
//     "sso" follows the mapping on every sign-in; a member added another way
//     keeps their role. Nobody is made owner, a suspended member stays
//     suspended.
func ProvisionLogin(app core.App, base context.Context, ip string, c *Connection, id *Identity) (*Outcome, error) {
	if id == nil || id.Subject == "" {
		return nil, newErr(http.StatusBadGateway, "invalid_response", "The identity provider did not identify the person.")
	}

	email := strings.ToLower(strings.TrimSpace(id.Email))

	domain := core.SSOEmailDomain(email)
	if domain == "" {
		return nil, newErr(http.StatusForbidden, "no_email", "The identity provider did not send an email address.")
	}

	if _, err := app.FindFirstRecordByFilter(core.CollectionNameSSODomains,
		"org={:org} && connection={:c} && domain={:d} && status='verified'",
		dbx.Params{"org": c.OrgId, "c": c.Id, "d": domain}); err != nil {
		return nil, newErr(http.StatusForbidden, "domain_not_verified", "The email domain is not verified for this connection.")
	}

	org, err := app.FindRecordById(core.CollectionNameOrgs, c.OrgId)
	if err != nil || org.GetBool("archived") {
		return nil, newErr(http.StatusForbidden, "org_unavailable", "The organization is not available.")
	}

	col, err := core.OrgUserCollection(app)
	if err != nil {
		return nil, newErr(http.StatusInternalServerError, "unavailable", "The organizations model is not initialized.")
	}

	out := &Outcome{}
	provider := ExternalAuthProvider(c.Id)

	txErr := app.RunInTransaction(func(tx core.App) error {
		// 2. already linked
		if ea, err := tx.FindFirstExternalAuthByExpr(dbx.HashExp{
			"collectionRef": col.Id, "provider": provider, "providerId": id.Subject,
		}); err == nil {
			if u, err := tx.FindRecordById(col, ea.RecordRef()); err == nil {
				out.User = u
			} else if err := tx.DeleteWithContext(system(base, ip), ea); err != nil { // the account is gone
				return err
			}
		}

		// 3. an existing account by email
		if out.User == nil {
			if u, err := tx.FindAuthRecordByEmail(col, email); err == nil {
				if !id.EmailVerified {
					return newErr(http.StatusForbidden, "email_unverified",
						"An account with this address exists, but the identity provider does not confirm the address.")
				}

				if !u.Verified() {
					u.SetVerified(true)
					u.SetRandomPassword()
					u.RefreshTokenKey()

					if err := tx.SaveWithContext(core.WithAuditContext(base, core.NewSelfAuditActor(u), ip), u); err != nil {
						return err
					}
				}

				out.User = u
				out.Linked = true
			}
		}

		// 4. just-in-time creation
		if out.User == nil {
			if !c.JIT {
				return newErr(http.StatusForbidden, "no_account", "There is no account for this person, and this connection does not create accounts.")
			}

			u := core.NewRecord(col)
			u.SetEmail(email)
			u.SetVerified(id.EmailVerified)
			u.SetRandomPassword()

			if id.Name != "" && col.Fields.GetByName("name") != nil {
				u.Set("name", id.Name)
			}

			pctx := core.WithAuditContext(base, core.NewPendingSelfAuditActor(), ip)
			if err := tx.SaveWithContext(pctx, u); err != nil {
				return newErr(http.StatusUnprocessableEntity, "provision_failed", "The account could not be created: "+err.Error())
			}

			out.User = u
			out.Created = true

			if err := core.WriteSSOAudit(tx, core.WithAuditContext(base, core.NewSelfAuditActor(u), ip), core.AuditActionSSOJIT,
				col.Name, u.Id, map[string]any{"orgId": c.OrgId, "connection": c.Id}); err != nil {
				return err
			}
		}

		if out.Created || out.Linked {
			if err := linkExternalAuth(tx, base, ip, c, col, out.User, id.Subject); err != nil {
				return err
			}

			if out.Linked {
				if err := core.WriteSSOAudit(tx, core.WithAuditContext(base, core.NewSelfAuditActor(out.User), ip), core.AuditActionSSOLink,
					col.Name, out.User.Id, map[string]any{"orgId": c.OrgId, "connection": c.Id, "by": "verified_email"}); err != nil {
					return err
				}
			}
		}

		// 5. membership and role
		m, err := syncMembership(tx, base, ip, c, org, out.User, id)
		if err != nil {
			return err
		}

		out.Member = m

		return nil
	})
	if txErr != nil {
		return nil, txErr
	}

	return out, nil
}

func system(base context.Context, ip string) context.Context {
	return core.WithAuditContext(base, core.AuditActor{Type: core.AuditActorTypeSystem}, ip)
}

func linkExternalAuth(tx core.App, base context.Context, ip string, c *Connection, col *core.Collection, user *core.Record, subject string) error {
	// Saved without validation: the provider name is not one of the OAuth2
	// providers that the validator of _externalAuths knows. The write is audited
	// by the sso_jit_create / sso_link entry.
	raw := tx.UnsafeWithoutHooks()

	ea := core.NewExternalAuth(raw)
	ea.Id = core.GenerateDefaultRandomId()
	ea.SetCollectionRef(col.Id)
	ea.SetRecordRef(user.Id)
	ea.SetProvider(ExternalAuthProvider(c.Id))
	ea.SetProviderId(subject)

	return raw.SaveNoValidateWithContext(system(base, ip), ea)
}

func syncMembership(tx core.App, base context.Context, ip string, c *Connection, org, user *core.Record, id *Identity) (*core.Record, error) {
	ctx := core.WithAuditContext(base, core.NewSelfAuditActor(user), ip)
	actor := &core.OrgActor{Record: user, System: true}

	mapped := MapRole(c, id.Groups)

	want := mapped
	if want == "" {
		want = c.DefaultRole
	}

	if want == "" {
		want = core.OrgRoleMember
	}

	// never owner, whatever a stored mapping says
	if want == core.OrgRoleOwner {
		want = core.OrgRoleAdmin
	}

	member, err := core.FindOrgMember(tx, org.Id, user.Id)
	if err != nil {
		in := core.OrgMemberInput{User: user.Id, Role: want, Source: core.OrgSourceSSO}

		m, err := core.AddOrgMember(tx, ctx, actor, org, in)
		if err != nil {
			return nil, newErr(http.StatusForbidden, "membership_failed", err.Error())
		}

		return m, nil
	}

	if member.GetString("source") == core.OrgSourceSSO &&
		member.GetString("role") != core.OrgRoleOwner &&
		member.GetString("role") != want {
		from := member.GetString("role")

		if _, err := core.UpdateOrgMember(tx, ctx, actor, org, member, core.OrgMemberPatch{Role: &want}); err != nil {
			return nil, newErr(http.StatusForbidden, "membership_failed", err.Error())
		}

		if err := core.WriteSSOAudit(tx, ctx, core.AuditActionSSORoleSync, core.CollectionNameOrgMembers, member.Id,
			map[string]any{"orgId": org.Id, "connection": c.Id, "from": from, "to": want}); err != nil {
			return nil, err
		}
	}

	return member, nil
}
