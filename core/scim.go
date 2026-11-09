package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #79c): SCIM 2.0 provisioning, tokens and settings.
//
// A SCIM token belongs to one organization and is not an identity: it acts on
// no user's behalf, runs no collection rule and can only reach /scim/v2 for
// its own org. It is deliberately not an API key (an API key is its owner).
//
// Format: scim_<prefix>_<secret>, built like an API key (8 base32 chars for
// the lookup handle, 32 random bytes as 52 base32 chars for the secret, only
// the SHA-256 of the secret stored, constant time comparison). The full token
// is returned exactly once by the create function.
//
// The provisioning itself is in scim_users.go and scim_groups.go; the HTTP
// surface in apis/scim*.go.

const (
	CollectionNameSCIMTokens = "_scimTokens"
	CollectionNameSCIMGroups = "_scimGroups"

	// SCIMTokenPrefix starts every SCIM token.
	SCIMTokenPrefix = "scim_"

	// SCIMTokenMaxPerOrg caps the live tokens of an org (two are enough for a
	// rotation with overlap).
	SCIMTokenMaxPerOrg = 10

	// SCIMTokenLastUsedInterval is the minimum gap between lastUsed writes.
	SCIMTokenLastUsedInterval = time.Minute
)

// ErrInvalidSCIMToken is returned for every kind of token failure.
var ErrInvalidSCIMToken = errors.New("invalid scim token")

// ErrSCIMOrgArchived is returned for a valid token of an archived org.
var ErrSCIMOrgArchived = errors.New("the organization is archived")

// GenerateSCIMToken returns a new full token with its prefix and stored hash.
func GenerateSCIMToken() (token, prefix, secretHash string, err error) {
	pb := make([]byte, 5)
	sb := make([]byte, 32)

	if _, err = rand.Read(pb); err != nil {
		return "", "", "", err
	}

	if _, err = rand.Read(sb); err != nil {
		return "", "", "", err
	}

	prefix = strings.ToLower(apiKeyEncoding.EncodeToString(pb))
	secret := strings.ToLower(apiKeyEncoding.EncodeToString(sb))

	return SCIMTokenPrefix + prefix + "_" + secret, prefix, HashAPIKeySecret(secret), nil
}

// IsSCIMToken reports whether the bearer value looks like a SCIM token.
func IsSCIMToken(token string) bool { return strings.HasPrefix(token, SCIMTokenPrefix) }

// ParseSCIMToken splits a token into its prefix and secret.
func ParseSCIMToken(token string) (prefix, secret string, ok bool) {
	rest, found := strings.CutPrefix(token, SCIMTokenPrefix)
	if !found {
		return "", "", false
	}

	prefix, secret, found = strings.Cut(rest, "_")
	if !found ||
		len(prefix) != apiKeyPrefixLen || len(secret) != apiKeySecretLen ||
		!apiKeyPartRegex.MatchString(prefix) || !apiKeyPartRegex.MatchString(secret) {
		return "", "", false
	}

	return prefix, secret, true
}

// AuthenticateSCIMToken resolves a bearer token to its token row and org.
// Every failure but an archived org returns ErrInvalidSCIMToken, so neither
// the error nor the timing says which part was wrong.
func AuthenticateSCIMToken(app App, token string) (tokenRec, org *Record, err error) {
	prefix, secret, ok := ParseSCIMToken(token)
	if !ok {
		return nil, nil, ErrInvalidSCIMToken
	}

	given := HashAPIKeySecret(secret)

	tokenRec, err = app.FindFirstRecordByData(CollectionNameSCIMTokens, "prefix", prefix)
	if err != nil {
		subtle.ConstantTimeCompare([]byte(given), []byte(given))

		return nil, nil, ErrInvalidSCIMToken
	}

	if subtle.ConstantTimeCompare([]byte(given), []byte(tokenRec.GetString("tokenHash"))) != 1 {
		return nil, nil, ErrInvalidSCIMToken
	}

	if tokenRec.GetBool("revoked") {
		return nil, nil, ErrInvalidSCIMToken
	}

	if exp := tokenRec.GetDateTime("expires"); !exp.IsZero() && exp.Time().Before(time.Now()) {
		return nil, nil, ErrInvalidSCIMToken
	}

	org, err = app.FindRecordById(CollectionNameOrgs, tokenRec.GetString("org"))
	if err != nil {
		return nil, nil, ErrInvalidSCIMToken
	}

	if org.GetBool("archived") {
		return nil, nil, ErrSCIMOrgArchived
	}

	touchSCIMTokenLastUsed(app, tokenRec)

	return tokenRec, org, nil
}

// touchSCIMTokenLastUsed records the use at most once per interval. The
// throttle is evaluated by the database and bypasses the record hooks on
// purpose: it must not produce an audit entry per minute.
func touchSCIMTokenLastUsed(app App, tokenRec *Record) {
	now := time.Now().UTC()
	cutoff := now.Add(-SCIMTokenLastUsedInterval)

	if last := tokenRec.GetDateTime("lastUsed"); !last.IsZero() && last.Time().After(cutoff) {
		return
	}

	_, err := app.NonconcurrentDB().NewQuery(
		"UPDATE {{" + CollectionNameSCIMTokens + "}} SET [[lastUsed]] = {:now} " +
			"WHERE [[id]] = {:id} AND ([[lastUsed]] = '' OR [[lastUsed]] IS NULL OR [[lastUsed]] < {:cutoff})",
	).Bind(dbx.Params{
		"now":    now.Format(types.DefaultDateLayout),
		"cutoff": cutoff.Format(types.DefaultDateLayout),
		"id":     tokenRec.Id,
	}).Execute()
	if err != nil {
		app.Logger().Debug("Failed to update the SCIM token lastUsed", "id", tokenRec.Id, "error", err)
	}
}

// CreateSCIMToken creates a token for the org (owners and admins only) and
// returns the row with the full token, which is never retrievable again.
func CreateSCIMToken(app App, ctx context.Context, actor *OrgActor, org *Record, name string, expires time.Time) (*Record, string, error) {
	if !actor.CanManage() {
		return nil, "", orgErr(403, "Only owners and admins can manage SCIM tokens.")
	}

	if err := requireLiveOrg(org, actor); err != nil {
		return nil, "", err
	}

	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return nil, "", orgErr(400, "The name is required (at most 100 characters).")
	}

	if !expires.IsZero() && !expires.After(time.Now()) {
		return nil, "", orgErr(400, "The expiry must be in the future.")
	}

	live, err := ListSCIMTokens(app, org)
	if err != nil {
		return nil, "", err
	}

	active := 0

	for _, t := range live {
		if !SCIMTokenInactive(t) {
			active++
		}
	}

	if active >= SCIMTokenMaxPerOrg {
		return nil, "", orgErr(409, "The organization already has the maximum number of live SCIM tokens. Revoke one first.")
	}

	token, prefix, hash, err := GenerateSCIMToken()
	if err != nil {
		return nil, "", err
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameSCIMTokens)
	if err != nil {
		return nil, "", orgErr(500, "SCIM is not initialized.")
	}

	rec := NewRecord(col)
	rec.Set("org", org.Id)
	rec.Set("name", name)
	rec.Set("prefix", prefix)
	rec.Set("tokenHash", hash)

	if !expires.IsZero() {
		rec.Set("expires", expires.UTC().Format(types.DefaultDateLayout))
	}

	if actor.Record != nil && !actor.Superuser && !actor.System {
		rec.Set("createdBy", actor.Record.Id)
	}

	if err := saveOrgRecord(app, ctx, rec); err != nil {
		return nil, "", err
	}

	return rec, token, nil
}

// ListSCIMTokens lists the org's tokens, newest first.
func ListSCIMTokens(app App, org *Record) ([]*Record, error) {
	return app.FindRecordsByFilter(CollectionNameSCIMTokens, "org={:org}", "-created", 0, 0, dbx.Params{"org": org.Id})
}

// SCIMTokenInactive reports whether the token is revoked or expired.
func SCIMTokenInactive(t *Record) bool {
	if t.GetBool("revoked") {
		return true
	}

	exp := t.GetDateTime("expires")

	return !exp.IsZero() && exp.Time().Before(time.Now())
}

// RevokeSCIMToken revokes a token of the org (owners and admins only).
func RevokeSCIMToken(app App, ctx context.Context, actor *OrgActor, org, token *Record) error {
	if token.GetString("org") != org.Id {
		return orgErr(404, "Token not found.")
	}

	if !actor.CanManage() {
		return orgErr(403, "Only owners and admins can manage SCIM tokens.")
	}

	if token.GetBool("revoked") {
		return nil
	}

	token.Set("revoked", true)

	return saveOrgRecord(app, ctx, token)
}

// ---- settings ------------------------------------------------------------

// SCIMSettings are the per-org SCIM options, kept in the org's settings JSON:
//
//	scimDefaultRole        role of new members (default "member"; not owner)
//	scimGroupRoles         {"<group displayName>": "<role>"}; never owner
//	scimDeactivationBans   also ban the account on active=false (see below)
type SCIMSettings struct {
	DefaultRole      string            `json:"defaultRole"`
	GroupRoles       map[string]string `json:"groupRoles"`
	DeactivationBans bool              `json:"deactivationBans"`
}

// GetSCIMSettings reads the org's SCIM options (defaults when unset).
func GetSCIMSettings(org *Record) SCIMSettings {
	raw := orgSettings(org)
	out := SCIMSettings{DefaultRole: OrgRoleMember, GroupRoles: map[string]string{}}

	if r, _ := raw["scimDefaultRole"].(string); validSCIMRole(r) {
		out.DefaultRole = r
	}

	if m, ok := raw["scimGroupRoles"].(map[string]any); ok {
		for k, v := range m {
			if r, _ := v.(string); validSCIMRole(r) {
				out.GroupRoles[k] = r
			}
		}
	}

	out.DeactivationBans, _ = raw["scimDeactivationBans"].(bool)

	return out
}

// validSCIMRole: SCIM never hands out ownership.
func validSCIMRole(r string) bool {
	return r != OrgRoleOwner && validRole(r)
}

// UpdateSCIMSettings replaces the org's SCIM options (owners and admins; an
// admin cannot set a role above their own).
func UpdateSCIMSettings(app App, ctx context.Context, actor *OrgActor, org *Record, in SCIMSettings) error {
	if !actor.CanManage() {
		return orgErr(403, "Only owners and admins can change the SCIM settings.")
	}

	if err := requireLiveOrg(org, actor); err != nil {
		return err
	}

	if in.DefaultRole == "" {
		in.DefaultRole = OrgRoleMember
	}

	if !validSCIMRole(in.DefaultRole) {
		return orgErr(400, "Invalid default role (owner cannot be provisioned by SCIM).")
	}

	if OrgRoleRank(in.DefaultRole) > actor.rank() {
		return orgErr(403, "You cannot grant a role above your own.")
	}

	if len(in.GroupRoles) > 200 {
		return orgErr(400, "Too many group mappings.")
	}

	for name, role := range in.GroupRoles {
		if !orgTagRegex.MatchString(name) {
			return orgErr(400, "Invalid group name: "+name)
		}

		if !validSCIMRole(role) {
			return orgErr(400, "Invalid role for the group "+name+" (owner cannot be provisioned by SCIM).")
		}

		if OrgRoleRank(role) > actor.rank() {
			return orgErr(403, "You cannot grant a role above your own.")
		}
	}

	settings := orgSettings(org)
	settings["scimDefaultRole"] = in.DefaultRole
	settings["scimGroupRoles"] = in.GroupRoles
	settings["scimDeactivationBans"] = in.DeactivationBans

	raw, err := json.Marshal(settings)
	if err != nil {
		return err
	}

	org.Set("settings", string(raw))

	return saveOrgRecord(app, ctx, org)
}

// scimHighestRole returns the highest role of the list ("" when empty).
func scimHighestRole(roles []string) string {
	best := ""

	for _, r := range roles {
		if OrgRoleRank(r) > OrgRoleRank(best) {
			best = r
		}
	}

	return best
}
