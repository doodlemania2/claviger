package oidc

import (
	"context"
	"errors"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

const (
	// ACRPassword, ACRMFA and ACRPasskey are the authentication context
	// class references the provider asserts.
	ACRPassword = "urn:claviger:acr:password"
	ACRMFA      = "urn:claviger:acr:mfa"
	ACRPasskey  = "urn:claviger:acr:passkey"

	claimCollection = "collection"
	claimSid        = "sid"
	claimACR        = "acr"
)

// SupportedClaims is what the discovery document advertises.
var SupportedClaims = []string{
	"sub", "iss", "aud", "exp", "iat", "auth_time", "nonce", "acr", "amr", "sid", "azp", "at_hash", "c_hash",
	claimCollection, "email", "email_verified", "name", "preferred_username", "updated_at",
}

// accountOf loads the end user behind a grant.
func (s *Store) accountOf(collectionRef, subject string) (*core.Record, error) {
	rec, err := s.app.FindRecordById(collectionRef, subject)
	if err != nil {
		return nil, errors.New("account not found")
	}

	return rec, nil
}

// fillUserInfo sets the identity claims allowed by the scopes.
func (s *Store) fillUserInfo(ui *oidc.UserInfo, rec *core.Record, scopes []string) {
	has := func(scope string) bool {
		for _, sc := range scopes {
			if sc == scope {
				return true
			}
		}

		return false
	}

	if has(oidc.ScopeOpenID) {
		ui.Subject = rec.Id
		ui.AppendClaims(claimCollection, rec.Collection().Name)
	}

	if has(oidc.ScopeEmail) {
		ui.Email = rec.Email()
		ui.EmailVerified = oidc.Bool(rec.Verified())
	}

	if has(oidc.ScopeProfile) {
		fields := rec.Collection().Fields

		if fields.GetByName("name") != nil {
			ui.Name = rec.GetString("name")
		}

		if fields.GetByName("username") != nil {
			ui.PreferredUsername = rec.GetString("username")
		}

		if updated := rec.GetDateTime("updated"); !updated.IsZero() {
			ui.UpdatedAt = oidc.FromTime(updated.Time())
		}
	}
}

// SetUserinfoFromRequest implements op.CanSetUserinfoFromRequest: the claims
// of the ID token. The private ones (sid, collection, acr) can only travel
// through UserInfo.AppendClaims in this library; acr is added here for the
// refresh grant too, which would otherwise lose it.
func (s *Store) SetUserinfoFromRequest(_ context.Context, ui *oidc.UserInfo, r op.IDTokenRequest, scopes []string) error {
	g, err := grantFrom(r)
	if err != nil {
		return err
	}

	rec, err := s.accountOf(g.collection, g.subject)
	if err != nil {
		return err
	}

	s.fillUserInfo(ui, rec, scopes)

	if g.sid != "" {
		ui.AppendClaims(claimSid, g.sid)
	}

	if g.acr != "" {
		ui.AppendClaims(claimACR, g.acr)
	}

	return nil
}

// GetPrivateClaimsFromRequest implements op.CanGetPrivateClaimsFromRequest:
// the extra claims of a JWT access token.
func (s *Store) GetPrivateClaimsFromRequest(_ context.Context, r op.TokenRequest, _ []string) (map[string]any, error) {
	g, err := grantFrom(r)
	if err != nil {
		return nil, err
	}

	claims := map[string]any{
		"scope": joinScopes(g.scopes),
	}

	if col, err := s.app.FindCachedCollectionByNameOrId(g.collection); err == nil {
		claims[claimCollection] = col.Name
	}

	if g.sid != "" {
		claims[claimSid] = g.sid
	}

	return claims, nil
}

// liveToken loads an access token row that is neither revoked nor expired and
// whose session and account are still alive.
func (s *Store) liveToken(tokenID string) (*tokenRow, error) {
	row, err := s.tokenByID(tokenID)
	if err != nil || row.Revoked || !row.Expires.Time().After(time.Now()) {
		return nil, errors.New("token is invalid or has expired")
	}

	if err := s.accountAlive(row.Sid, row.CollectionRef, row.Subject); err != nil {
		return nil, errors.New("token is invalid or has expired")
	}

	return row, nil
}

// SetUserinfoFromToken implements op.Storage (the userinfo endpoint).
func (s *Store) SetUserinfoFromToken(_ context.Context, ui *oidc.UserInfo, tokenID, _, _ string) error {
	row, err := s.liveToken(tokenID)
	if err != nil {
		return err
	}

	rec, err := s.accountOf(row.CollectionRef, row.Subject)
	if err != nil {
		return err
	}

	s.fillUserInfo(ui, rec, parseList(row.Scopes))

	if row.Sid != "" {
		ui.AppendClaims(claimSid, row.Sid)
	}

	return nil
}

// SetIntrospectionFromToken implements op.Storage (RFC 7662). Unlike a JWT
// checked offline, introspection also sees revocation and the session.
func (s *Store) SetIntrospectionFromToken(ctx context.Context, in *oidc.IntrospectionResponse, tokenID, _, _ string) error {
	row, err := s.liveToken(tokenID)
	if err != nil {
		return err
	}

	rec, err := s.accountOf(row.CollectionRef, row.Subject)
	if err != nil {
		return err
	}

	scopes := parseList(row.Scopes)

	ui := new(oidc.UserInfo)
	s.fillUserInfo(ui, rec, scopes)
	in.SetUserInfo(ui)

	in.Scope = scopes
	in.ClientID = row.ClientId
	in.Expiration = oidc.FromTime(row.Expires.Time())
	in.TokenType = oidc.BearerToken
	in.Subject = row.Subject

	if row.Sid != "" {
		if in.Claims == nil {
			in.Claims = map[string]any{}
		}

		in.Claims[claimSid] = row.Sid
	}

	return nil
}
