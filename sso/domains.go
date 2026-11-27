package sso

import (
	"context"
	"net/http"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/security"
	"github.com/doodlemania2/claviger/tools/types"
)

// ChallengeLabel is the extra DNS name the verification record may be put
// under, for DNS hosts that cannot hold more than one TXT record at the apex:
// "_claviger.<domain>". The apex of the domain works as well.
const ChallengeLabel = "_claviger."

// VerificationValue is the TXT value that proves control of a claim.
func VerificationValue(token string) string { return core.SSOVerificationPrefix + token }

// CreateDomain records a pending claim of domain by org, routed to connection.
func CreateDomain(app core.App, ctx context.Context, org *core.Record, connectionId, domain string) (*core.Record, error) {
	d, ok := core.NormalizeSSODomain(domain)
	if !ok {
		return nil, newErr(http.StatusBadRequest, "invalid_domain", "That is not a valid domain name.")
	}

	conn, err := app.FindRecordById(core.CollectionNameSSOConnections, connectionId)
	if err != nil || conn.GetString("org") != org.Id {
		return nil, newErr(http.StatusBadRequest, "invalid_connection", "The connection does not belong to this organization.")
	}

	if _, err := app.FindFirstRecordByFilter(core.CollectionNameSSODomains, "org={:org} && domain={:d}",
		dbx.Params{"org": org.Id, "d": d}); err == nil {
		return nil, newErr(http.StatusConflict, "domain_exists", "The organization already claims this domain.")
	}

	col, err := app.FindCachedCollectionByNameOrId(core.CollectionNameSSODomains)
	if err != nil {
		return nil, err
	}

	rec := core.NewRecord(col)
	rec.Set("org", org.Id)
	rec.Set("connection", connectionId)
	rec.Set("domain", d)
	rec.Set("token", security.RandomString(32))
	rec.Set("status", core.SSODomainPending)

	if err := app.SaveWithContext(ctx, rec); err != nil {
		return nil, newErr(http.StatusBadRequest, "invalid_domain", err.Error())
	}

	return rec, nil
}

// DomainMatches reports whether the TXT values contain the claim's record.
func DomainMatches(values []string, token string) bool {
	want := VerificationValue(token)

	for _, v := range values {
		if strings.TrimSpace(strings.Trim(strings.TrimSpace(v), `"`)) == want {
			return true
		}
	}

	return false
}

// CheckDomain looks for the claim's TXT record (at the domain and at
// _claviger.<domain>) and, when it is there, marks the claim verified. It
// returns whether the claim is verified afterwards. A claim that is already
// verified is returned as is, without a lookup.
//
// A domain is verified for one organization only: when another one already
// holds it, the answer is a 409 and nothing changes.
func CheckDomain(app core.App, ctx context.Context, rec *core.Record) (bool, error) {
	if rec.GetString("status") == core.SSODomainVerified {
		return true, nil
	}

	domain := rec.GetString("domain")
	token := rec.GetString("token")

	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	found := false

	for _, name := range []string{domain, ChallengeLabel + domain} {
		values, err := resolver().LookupTXT(lctx, name)
		if err != nil {
			continue // not found, or the lookup failed: both mean "not proven"
		}

		if DomainMatches(values, token) {
			found = true

			break
		}
	}

	rec.Set("lastCheckedAt", types.NowDateTime())

	if !found {
		if err := app.SaveWithContext(ctx, rec); err != nil {
			return false, err
		}

		return false, nil
	}

	if other, err := app.FindFirstRecordByFilter(core.CollectionNameSSODomains,
		"domain={:d} && status='verified' && id!={:id}", dbx.Params{"d": domain, "id": rec.Id}); err == nil && other != nil {
		return false, newErr(http.StatusConflict, "domain_taken", "Another organization has already verified this domain.")
	}

	err := app.RunInTransaction(func(tx core.App) error {
		rec.Set("status", core.SSODomainVerified)
		rec.Set("verifiedAt", types.NowDateTime())

		if err := tx.SaveWithContext(ctx, rec); err != nil {
			return err
		}

		return core.WriteSSOAudit(tx, ctx, core.AuditActionSSODomainVerify, core.CollectionNameSSODomains, rec.Id, map[string]any{
			"orgId": rec.GetString("org"), "domain": domain, "connection": rec.GetString("connection"),
		})
	})
	if err != nil {
		return false, err
	}

	return true, nil
}

// Route is where a verified domain sends its people.
type Route struct {
	Connection *Connection
	Domain     *core.Record
}

// RouteForEmail returns the enabled connection of the verified claim of the
// email's domain. A domain that is unclaimed, or claimed but not verified, has
// no route: the person signs in the ordinary way.
func RouteForEmail(app core.App, email string) *Route {
	domain := core.SSOEmailDomain(email)
	if domain == "" {
		return nil
	}

	return routeForDomain(app, domain)
}

func routeForDomain(app core.App, domain string) *Route {
	claim, err := app.FindFirstRecordByFilter(core.CollectionNameSSODomains, "domain={:d} && status='verified'",
		dbx.Params{"d": domain})
	if err != nil {
		return nil
	}

	rec, err := app.FindRecordById(core.CollectionNameSSOConnections, claim.GetString("connection"))
	if err != nil || !rec.GetBool("enabled") || rec.GetString("org") != claim.GetString("org") {
		return nil
	}

	if org, err := app.FindRecordById(core.CollectionNameOrgs, rec.GetString("org")); err != nil || org.GetBool("archived") {
		return nil
	}

	if _, ok := ProviderFor(rec.GetString("type")); !ok {
		return nil
	}

	conn, err := LoadConnection(app, rec)
	if err != nil {
		return nil
	}

	return &Route{Connection: conn, Domain: claim}
}

// RequiredFor returns the route of the email's domain when its connection
// requires single sign-on (password and OTP sign-in are then refused).
func RequiredFor(app core.App, email string) *Route {
	r := RouteForEmail(app, email)
	if r == nil || !r.Connection.SSORequired {
		return nil
	}

	return r
}
