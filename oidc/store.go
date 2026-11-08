package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
	"github.com/go-jose/go-jose/v4"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// Store is the op.Storage of the provider. Clients and signing keys are
// records (and audited). Authorize requests, codes, access and refresh tokens
// are plain SQL rows: issuing a token must not fire model hooks or add audit
// rows, exactly like the _sessions table.
type Store struct {
	app core.App
}

// NewStore returns the storage for the app.
func NewStore(app core.App) *Store { return &Store{app: app} }

var _ op.Storage = (*Store)(nil)

// ---- helpers -------------------------------------------------------------

type ctxKey int

const (
	ctxKeyAlg ctxKey = iota
	ctxKeySSO
)

func withAlg(ctx context.Context, alg string) context.Context {
	return context.WithValue(ctx, ctxKeyAlg, alg)
}

func algFromContext(ctx context.Context) string {
	if alg, _ := ctx.Value(ctxKeyAlg).(string); validAlg(alg) {
		return alg
	}

	return AlgRS256
}

// detach keeps the values of ctx but drops its cancellation: a half written
// token or key must not be possible just because the client went away.
func detach(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}

	return context.WithoutCancel(ctx)
}

func nowString() string { return types.NowDateTime().String() }

func dateString(t time.Time) string {
	dt, _ := types.ParseDateTime(t.UTC())

	return dt.String()
}

func exec(app core.App, query string, params dbx.Params) (int64, error) {
	res, err := app.NonconcurrentDB().NewQuery(query).Bind(params).Execute()
	if err != nil {
		return 0, err
	}

	return res.RowsAffected()
}

func jsonList(v []string) string {
	if v == nil {
		v = []string{}
	}

	b, _ := json.Marshal(v)

	return string(b)
}

func parseList(raw string) []string {
	var out []string
	_ = json.Unmarshal([]byte(raw), &out)

	return out
}

var errNotFound = errors.New("not found")

// ---- authorize requests --------------------------------------------------

// request statuses
const (
	statusPending       = "pending"       // created by /authorize, waiting for the sign-in
	statusAuthenticated = "authenticated" // signed in, waiting for consent
	statusDone          = "done"          // the code may be issued
	statusDenied        = "denied"
)

type requestData struct {
	RedirectURI  string   `json:"redirectUri"`
	State        string   `json:"state"`
	Nonce        string   `json:"nonce"`
	Scopes       []string `json:"scopes"`
	ResponseType string   `json:"responseType"`
	ResponseMode string   `json:"responseMode"`
	Challenge    string   `json:"challenge"`
	Method       string   `json:"method"`
	MaxAge       *uint    `json:"maxAge,omitempty"`
	Prompt       []string `json:"prompt,omitempty"`
	ACRValues    []string `json:"acrValues,omitempty"`
}

type requestRow struct {
	Id            string         `db:"id"`
	RequestId     string         `db:"requestId"`
	ClientId      string         `db:"clientId"`
	Data          string         `db:"data"`
	BindingHash   string         `db:"bindingHash"`
	Status        string         `db:"status"`
	CollectionRef string         `db:"collectionRef"`
	Subject       string         `db:"subject"`
	Sid           string         `db:"sid"`
	Amr           string         `db:"amr"`
	Acr           string         `db:"acr"`
	AuthTime      types.DateTime `db:"authTime"`
	CodeHash      string         `db:"codeHash"`
	CodeUsed      bool           `db:"codeUsed"`
	CodeExpires   types.DateTime `db:"codeExpires"`
	FamilyId      string         `db:"familyId"`
	Expires       types.DateTime `db:"expires"`
}

const requestColumns = "[[id]], [[requestId]], [[clientId]], [[data]], [[bindingHash]], [[status]], [[collectionRef]], " +
	"[[subject]], [[sid]], [[amr]], [[acr]], [[authTime]], [[codeHash]], [[codeUsed]], [[codeExpires]], [[familyId]], [[expires]]"

// authRequest is an op.AuthRequest.
type authRequest struct {
	row requestRow
	d   requestData
}

func (a *authRequest) GetID() string          { return a.row.RequestId }
func (a *authRequest) GetACR() string         { return a.row.Acr }
func (a *authRequest) GetAMR() []string       { return parseList(a.row.Amr) }
func (a *authRequest) GetAudience() []string  { return []string{a.row.ClientId} }
func (a *authRequest) GetAuthTime() time.Time { return a.row.AuthTime.Time() }
func (a *authRequest) GetClientID() string    { return a.row.ClientId }

func (a *authRequest) GetCodeChallenge() *oidc.CodeChallenge {
	if a.d.Challenge == "" {
		return nil
	}

	return &oidc.CodeChallenge{Challenge: a.d.Challenge, Method: oidc.CodeChallengeMethod(a.d.Method)}
}

func (a *authRequest) GetNonce() string                   { return a.d.Nonce }
func (a *authRequest) GetRedirectURI() string             { return a.d.RedirectURI }
func (a *authRequest) GetResponseType() oidc.ResponseType { return oidc.ResponseType(a.d.ResponseType) }
func (a *authRequest) GetResponseMode() oidc.ResponseMode { return oidc.ResponseMode(a.d.ResponseMode) }
func (a *authRequest) GetScopes() []string                { return a.d.Scopes }
func (a *authRequest) GetState() string                   { return a.d.State }
func (a *authRequest) GetSubject() string                 { return a.row.Subject }
func (a *authRequest) Done() bool                         { return a.row.Status == statusDone }

func (s *Store) scanRequest(row requestRow) (*authRequest, error) {
	a := &authRequest{row: row}
	if err := json.Unmarshal([]byte(row.Data), &a.d); err != nil {
		return nil, err
	}

	return a, nil
}

// requestByID loads an unexpired authorize request.
func (s *Store) requestByID(requestID string) (*authRequest, error) {
	if requestID == "" {
		return nil, errNotFound
	}

	var row requestRow
	err := s.app.ConcurrentDB().NewQuery(
		"SELECT " + requestColumns + " FROM {{" + core.CollectionNameOAuthRequests + "}} WHERE [[requestId]] = {:id} AND [[expires]] > {:now} LIMIT 1",
	).Bind(dbx.Params{"id": requestID, "now": nowString()}).One(&row)
	if err != nil {
		return nil, errNotFound
	}

	return s.scanRequest(row)
}

// CreateAuthRequest implements op.Storage. The library already validated the
// redirect URI (exact match), the response type and the PKCE method.
func (s *Store) CreateAuthRequest(ctx context.Context, r *oidc.AuthRequest, _ string) (op.AuthRequest, error) {
	c, err := s.findClient(r.ClientID)
	if err != nil {
		return nil, oidc.ErrInvalidRequest().WithDescription("unknown client")
	}

	if _, err := c.collection(); err != nil {
		return nil, oidc.ErrServerError().WithDescription("the client has no valid auth collection")
	}

	if r.CodeChallenge == "" {
		if c.RequirePKCE {
			return nil, oidc.ErrInvalidRequest().WithDescription("code_challenge is required (PKCE with S256)")
		}
	} else if r.CodeChallengeMethod != oidc.CodeChallengeMethodS256 {
		return nil, oidc.ErrInvalidRequest().WithDescription("only code_challenge_method S256 is supported")
	}

	scopes := make([]string, 0, len(r.Scopes))
	for _, sc := range r.Scopes {
		if c.allows(sc) && !slices.Contains(scopes, sc) {
			scopes = append(scopes, sc)
		}
	}

	if !slices.Contains(scopes, oidc.ScopeOpenID) {
		return nil, oidc.ErrInvalidScope().WithDescription("the openid scope is required")
	}

	if slices.Contains(r.Prompt, oidc.PromptNone) {
		if len(r.Prompt) > 1 {
			return nil, oidc.ErrInvalidRequest().WithDescription("prompt=none cannot be combined with other values")
		}

		// no user interface may be shown: the single sign-on session has to
		// carry the request as it is, or the client gets an error
		account, _ := s.ssoAccount(ssoFromContext(ctx), c, r.MaxAge, r.Prompt)
		if account == nil {
			return nil, oidc.ErrLoginRequired()
		}

		if !c.FirstParty && !s.hasConsent(account, c.ClientID, scopes) {
			return nil, errConsentRequired()
		}
	}

	d := requestData{
		RedirectURI:  r.RedirectURI,
		State:        r.State,
		Nonce:        r.Nonce,
		Scopes:       scopes,
		ResponseType: string(r.ResponseType),
		ResponseMode: string(r.ResponseMode),
		Challenge:    r.CodeChallenge,
		Method:       string(r.CodeChallengeMethod),
		MaxAge:       r.MaxAge,
		Prompt:       []string(r.Prompt),
		ACRValues:    r.ACRValues,
	}

	raw, _ := json.Marshal(d)
	requestID := randomToken(32)

	s.purgeExpired()

	_, err = s.app.NonconcurrentDB().Insert(core.CollectionNameOAuthRequests, dbx.Params{
		"id":        core.GenerateDefaultRandomId(),
		"requestId": requestID,
		"clientId":  c.ClientID,
		"data":      string(raw),
		"status":    statusPending,
		"created":   nowString(),
		"expires":   dateString(time.Now().Add(authRequestLifetime)),
	}).Execute()
	if err != nil {
		return nil, err
	}

	return s.requestByID(requestID)
}

// purgeExpired drops what has expired. It runs when a new authorize request
// comes in, so an install with no OIDC traffic does no work at all.
func (s *Store) purgeExpired() {
	cutoff := dateString(time.Now().Add(-time.Hour))

	for _, table := range []string{core.CollectionNameOAuthRequests, core.CollectionNameOAuthTokens, core.CollectionNameOAuthRefreshTokens, core.CollectionNameOAuthSso} {
		if _, err := exec(s.app, "DELETE FROM {{"+table+"}} WHERE [[expires]] != '' AND [[expires]] < {:cutoff}", dbx.Params{"cutoff": cutoff}); err != nil {
			s.app.Logger().Debug("Failed to purge expired OIDC rows", "table", table, "error", err)
		}
	}
}

// AuthRequestByID implements op.Storage.
func (s *Store) AuthRequestByID(_ context.Context, id string) (op.AuthRequest, error) {
	a, err := s.requestByID(id)
	if err != nil {
		return nil, err
	}

	return a, nil
}

// SaveAuthCode implements op.Storage. A request yields one code, once.
func (s *Store) SaveAuthCode(ctx context.Context, id, code string) error {
	n, err := exec(s.app,
		"UPDATE {{"+core.CollectionNameOAuthRequests+"}} SET [[codeHash]] = {:hash}, [[codeExpires]] = {:exp}, [[codeUsed]] = FALSE, "+
			"[[familyId]] = {:family}, [[expires]] = {:keep} "+
			"WHERE [[requestId]] = {:id} AND [[status]] = {:done} AND [[codeHash]] = '' AND [[expires]] > {:now}",
		dbx.Params{
			"hash":   hashToken(code),
			"exp":    dateString(time.Now().Add(authCodeLifetime)),
			"family": randomToken(18),
			"keep":   dateString(time.Now().Add(authRequestLifetime)), // kept to detect a replayed code
			"id":     id,
			"done":   statusDone,
			"now":    nowString(),
		})
	if err != nil {
		return err
	}

	if n == 0 {
		return errors.New("the authorize request is not ready or its code was already issued")
	}

	return nil
}

// AuthRequestByCode implements op.Storage. The code is consumed by the first
// lookup, whatever happens next (wrong verifier, wrong client secret): a code
// is single use. A second use of a code that already produced tokens revokes
// those tokens.
func (s *Store) AuthRequestByCode(ctx context.Context, code string) (op.AuthRequest, error) {
	hash := hashToken(code)

	n, err := exec(s.app,
		"UPDATE {{"+core.CollectionNameOAuthRequests+"}} SET [[codeUsed]] = TRUE "+
			"WHERE [[codeHash]] = {:hash} AND [[codeUsed]] = FALSE AND [[codeExpires]] > {:now} AND [[status]] = {:done}",
		dbx.Params{"hash": hash, "now": nowString(), "done": statusDone})
	if err != nil {
		return nil, err
	}

	var row requestRow
	qerr := s.app.ConcurrentDB().NewQuery(
		"SELECT " + requestColumns + " FROM {{" + core.CollectionNameOAuthRequests + "}} WHERE [[codeHash]] = {:hash} AND [[codeHash]] != '' LIMIT 1",
	).Bind(dbx.Params{"hash": hash}).One(&row)
	if qerr != nil {
		return nil, errNotFound
	}

	if n == 0 {
		// replay (or an expired code)
		if row.CodeUsed && row.FamilyId != "" {
			s.revokeFamily(ctx, row.FamilyId, row.CollectionRef, row.Subject, "authorization code reuse")
		}

		return nil, errors.New("invalid or already used authorization code")
	}

	return s.scanRequest(row)
}

// DeleteAuthRequest implements op.Storage. The row stays until it expires so a
// replayed code can be told from an unknown one.
func (s *Store) DeleteAuthRequest(context.Context, string) error { return nil }

// ---- tokens --------------------------------------------------------------

type refreshRow struct {
	Id            string         `db:"id"`
	TokenHash     string         `db:"tokenHash"`
	FamilyId      string         `db:"familyId"`
	ClientId      string         `db:"clientId"`
	CollectionRef string         `db:"collectionRef"`
	Subject       string         `db:"subject"`
	Sid           string         `db:"sid"`
	Scopes        string         `db:"scopes"`
	Audience      string         `db:"audience"`
	Amr           string         `db:"amr"`
	Acr           string         `db:"acr"`
	AuthTime      types.DateTime `db:"authTime"`
	Expires       types.DateTime `db:"expires"`
	Used          bool           `db:"used"`
	Revoked       bool           `db:"revoked"`
}

const refreshColumns = "[[id]], [[tokenHash]], [[familyId]], [[clientId]], [[collectionRef]], [[subject]], [[sid]], [[scopes]], " +
	"[[audience]], [[amr]], [[acr]], [[authTime]], [[expires]], [[used]], [[revoked]]"

// refreshRequest is an op.RefreshTokenRequest.
type refreshRequest struct {
	row    refreshRow
	scopes []string
}

func (r *refreshRequest) GetAMR() []string            { return parseList(r.row.Amr) }
func (r *refreshRequest) GetAudience() []string       { return parseList(r.row.Audience) }
func (r *refreshRequest) GetAuthTime() time.Time      { return r.row.AuthTime.Time() }
func (r *refreshRequest) GetClientID() string         { return r.row.ClientId }
func (r *refreshRequest) GetScopes() []string         { return r.scopes }
func (r *refreshRequest) GetSubject() string          { return r.row.Subject }
func (r *refreshRequest) SetCurrentScopes(s []string) { r.scopes = s }

type tokenRow struct {
	Id            string         `db:"id"`
	TokenId       string         `db:"tokenId"`
	ClientId      string         `db:"clientId"`
	CollectionRef string         `db:"collectionRef"`
	Subject       string         `db:"subject"`
	Sid           string         `db:"sid"`
	Scopes        string         `db:"scopes"`
	FamilyId      string         `db:"familyId"`
	Expires       types.DateTime `db:"expires"`
	Revoked       bool           `db:"revoked"`
}

const tokenColumns = "[[id]], [[tokenId]], [[clientId]], [[collectionRef]], [[subject]], [[sid]], [[scopes]], [[familyId]], [[expires]], [[revoked]]"

// grant is what the token rows are derived from: an authorization code
// exchange or a refresh.
type grant struct {
	family, client, collection, subject, sid, acr string
	scopes, audience, amr                         []string
	authTime                                      time.Time
}

func grantFrom(req op.TokenRequest) (*grant, error) {
	switch r := req.(type) {
	case *authRequest:
		return &grant{
			family: r.row.FamilyId, client: r.row.ClientId, collection: r.row.CollectionRef, subject: r.row.Subject,
			sid: r.row.Sid, acr: r.row.Acr, scopes: r.d.Scopes, audience: r.GetAudience(), amr: r.GetAMR(),
			authTime: r.GetAuthTime(),
		}, nil
	case *refreshRequest:
		return &grant{
			family: r.row.FamilyId, client: r.row.ClientId, collection: r.row.CollectionRef, subject: r.row.Subject,
			sid: r.row.Sid, acr: r.row.Acr, scopes: r.scopes, audience: r.GetAudience(), amr: r.GetAMR(),
			authTime: r.GetAuthTime(),
		}, nil
	}

	return nil, errors.New("unsupported grant")
}

func insertAccessToken(app core.App, g *grant) (string, time.Time, error) {
	id := randomToken(18)
	exp := time.Now().Add(AccessTokenLifetime)

	_, err := app.NonconcurrentDB().Insert(core.CollectionNameOAuthTokens, dbx.Params{
		"id":            core.GenerateDefaultRandomId(),
		"tokenId":       id,
		"clientId":      g.client,
		"collectionRef": g.collection,
		"subject":       g.subject,
		"sid":           g.sid,
		"scopes":        jsonList(g.scopes),
		"audience":      jsonList(g.audience),
		"familyId":      g.family,
		"expires":       dateString(exp),
		"revoked":       false,
		"created":       nowString(),
	}).Execute()

	return id, exp, err
}

// CreateAccessToken implements op.Storage (an access token without refresh token).
func (s *Store) CreateAccessToken(ctx context.Context, req op.TokenRequest) (string, time.Time, error) {
	g, err := grantFrom(req)
	if err != nil {
		return "", time.Time{}, err
	}

	return insertAccessToken(s.app, g)
}

// CreateAccessAndRefreshTokens implements op.Storage. With a current refresh
// token this is a rotation: the old token is marked used in the same
// transaction that creates the new one.
func (s *Store) CreateAccessAndRefreshTokens(ctx context.Context, req op.TokenRequest, current string) (string, string, time.Time, error) {
	g, err := grantFrom(req)
	if err != nil {
		return "", "", time.Time{}, err
	}

	var (
		accessID string
		exp      time.Time
		refresh  = randomToken(32)
		reuse    bool
	)

	err = s.app.RunInTransaction(func(txApp core.App) error {
		if current != "" {
			n, err := exec(txApp,
				"UPDATE {{"+core.CollectionNameOAuthRefreshTokens+"}} SET [[used]] = TRUE "+
					"WHERE [[tokenHash]] = {:hash} AND [[used]] = FALSE AND [[revoked]] = FALSE",
				dbx.Params{"hash": hashToken(current)})
			if err != nil {
				return err
			}

			if n == 0 {
				reuse = true

				return errors.New("refresh token already used")
			}
		}

		_, err := txApp.NonconcurrentDB().Insert(core.CollectionNameOAuthRefreshTokens, dbx.Params{
			"id":            core.GenerateDefaultRandomId(),
			"tokenHash":     hashToken(refresh),
			"familyId":      g.family,
			"clientId":      g.client,
			"collectionRef": g.collection,
			"subject":       g.subject,
			"sid":           g.sid,
			"scopes":        jsonList(g.scopes),
			"audience":      jsonList(g.audience),
			"amr":           jsonList(g.amr),
			"acr":           g.acr,
			"authTime":      dateString(g.authTime),
			"expires":       dateString(time.Now().Add(RefreshTokenLifetime)),
			"used":          false,
			"revoked":       false,
			"created":       nowString(),
		}).Execute()
		if err != nil {
			return err
		}

		accessID, exp, err = insertAccessToken(txApp, g)

		return err
	})
	if err != nil {
		if reuse {
			s.revokeFamily(ctx, g.family, g.collection, g.subject, "refresh token reuse")
		}

		return "", "", time.Time{}, err
	}

	if current != "" {
		s.extendSession(g.sid, g.collection)
	}

	return accessID, refresh, exp, nil
}

// extendSession keeps the Claviger session behind a refreshed grant alive: it
// now ends the collection's auth token duration from this refresh, but never
// later than MaxSessionLifetime after the sign-in, and it is never shortened.
// Only a refresh does this, and only after the refresh token was accepted, so
// the session was alive, the account not banned and the chain not revoked.
func (s *Store) extendSession(sid, collectionRef string) {
	if sid == "" {
		return
	}

	col, err := s.app.FindCachedCollectionByNameOrId(collectionRef)
	if err != nil || !col.IsAuth() || col.AuthToken.Duration <= 0 {
		return
	}

	var row struct {
		Created types.DateTime `db:"created"`
		Expires types.DateTime `db:"expires"`
	}

	if err := s.app.ConcurrentDB().NewQuery(
		"SELECT [[created]], [[expires]] FROM {{" + core.CollectionNameSessions + "}} WHERE [[id]] = {:sid} AND [[revoked]] = FALSE",
	).Bind(dbx.Params{"sid": sid}).One(&row); err != nil {
		return
	}

	target := time.Now().Add(time.Duration(col.AuthToken.Duration) * time.Second)
	if limit := row.Created.Time().Add(MaxSessionLifetime); target.After(limit) {
		target = limit
	}

	if !target.After(row.Expires.Time()) {
		return
	}

	if err := core.SetSessionExpires(s.app, sid, target); err != nil {
		s.app.Logger().Warn("Failed to extend the session on an OpenID Connect refresh", "error", err)
	}
}

// TokenRequestByRefreshToken implements op.Storage.
func (s *Store) TokenRequestByRefreshToken(ctx context.Context, token string) (op.RefreshTokenRequest, error) {
	row, err := s.refreshByHash(hashToken(token))
	if err != nil {
		return nil, op.ErrInvalidRefreshToken
	}

	if row.Revoked || !row.Expires.Time().After(time.Now()) {
		return nil, op.ErrInvalidRefreshToken
	}

	if row.Used {
		// a rotated token came back: somebody holds a stale copy, and it is
		// not known which side. Kill the whole chain.
		s.revokeFamily(ctx, row.FamilyId, row.CollectionRef, row.Subject, "refresh token reuse")

		return nil, op.ErrInvalidRefreshToken
	}

	if err := s.accountAlive(row.Sid, row.CollectionRef, row.Subject); err != nil {
		// the Claviger session was revoked, expired or the account is banned or gone
		s.revokeFamily(ctx, row.FamilyId, "", "", "")

		return nil, op.ErrInvalidRefreshToken
	}

	return &refreshRequest{row: *row, scopes: parseList(row.Scopes)}, nil
}

func (s *Store) refreshByHash(hash string) (*refreshRow, error) {
	var row refreshRow
	err := s.app.ConcurrentDB().NewQuery(
		"SELECT " + refreshColumns + " FROM {{" + core.CollectionNameOAuthRefreshTokens + "}} WHERE [[tokenHash]] = {:hash} LIMIT 1",
	).Bind(dbx.Params{"hash": hash}).One(&row)
	if err != nil {
		return nil, errNotFound
	}

	return &row, nil
}

func (s *Store) refreshByID(id string) (*refreshRow, error) {
	var row refreshRow
	err := s.app.ConcurrentDB().NewQuery(
		"SELECT " + refreshColumns + " FROM {{" + core.CollectionNameOAuthRefreshTokens + "}} WHERE [[id]] = {:id} LIMIT 1",
	).Bind(dbx.Params{"id": id}).One(&row)
	if err != nil {
		return nil, errNotFound
	}

	return &row, nil
}

func (s *Store) tokenByID(tokenID string) (*tokenRow, error) {
	var row tokenRow
	err := s.app.ConcurrentDB().NewQuery(
		"SELECT " + tokenColumns + " FROM {{" + core.CollectionNameOAuthTokens + "}} WHERE [[tokenId]] = {:id} LIMIT 1",
	).Bind(dbx.Params{"id": tokenID}).One(&row)
	if err != nil {
		return nil, errNotFound
	}

	return &row, nil
}

// accountAlive reports whether the Claviger session behind a grant is still
// valid and its account exists and is not banned.
func (s *Store) accountAlive(sid, collectionRef, subject string) error {
	rec, err := s.app.FindRecordById(collectionRef, subject)
	if err != nil {
		return errors.New("account not found")
	}

	if core.IsBanned(s.app, rec) {
		return errors.New("account is banned")
	}

	if sid == "" {
		return errors.New("no session")
	}

	return core.ValidateSession(s.app, sid, rec.Collection().Id, rec.Id)
}

// revokeFamily revokes every refresh and access token of an authorization.
// A non-empty subject also writes an audit entry (a security event).
func (s *Store) revokeFamily(ctx context.Context, family, collectionRef, subject, reason string) {
	if family == "" {
		return
	}

	for _, table := range []string{core.CollectionNameOAuthRefreshTokens, core.CollectionNameOAuthTokens} {
		if _, err := exec(s.app, "UPDATE {{"+table+"}} SET [[revoked]] = TRUE WHERE [[familyId]] = {:family}", dbx.Params{"family": family}); err != nil {
			s.app.Logger().Warn("Failed to revoke OIDC tokens", "table", table, "error", err)
		}
	}

	if subject != "" {
		s.audit(ctx, "oidc_revoke_family", collectionRef, subject, map[string]any{"reason": reason, "family": family})
	}
}

// audit writes a non-record audit entry.
func (s *Store) audit(ctx context.Context, action, collectionRef, subject string, meta map[string]any) {
	name := collectionRef
	if col, err := s.app.FindCachedCollectionByNameOrId(collectionRef); err == nil {
		name = col.Name
	}

	err := core.WriteAuditEntry(s.app, detach(ctx), &core.AuditEntry{
		Action:         action,
		Actor:          core.AuditActor{Type: core.AuditActorTypeSystem},
		CollectionId:   collectionRef,
		CollectionName: name,
		RecordId:       subject,
		Meta:           meta,
	}, nil)
	if err != nil {
		s.app.Logger().Warn("Failed to write the OIDC audit entry", "action", action, "error", err)
	}
}

// TerminateSession implements op.Storage. It is only reached when
// TerminateSessionFromRequest is not (it always is); kept for the interface.
func (s *Store) TerminateSession(context.Context, string, string) error { return nil }

// TerminateSessionFromRequest implements op.CanTerminateSessionFromRequest
// (RP-initiated logout). The id_token_hint was already verified by the library;
// its sid names the Claviger session to revoke. The single sign-on session of
// the browser (its cookie travels with the request) ends too, together with the
// Claviger session it is bound to.
func (s *Store) TerminateSessionFromRequest(ctx context.Context, req *op.EndSessionRequest) (string, error) {
	if sso := s.ssoRow(ssoFromContext(ctx)); sso != nil {
		if err := s.endSession(ctx, sso.Sid, sso.Subject, "signed out of the sign-in page", req.ClientID); err != nil {
			return "", err
		}
	}

	if req.IDTokenHintClaims == nil || req.UserID == "" {
		// nothing identifies the session: only the redirect is honoured
		return req.RedirectURI, nil
	}

	if sid := req.IDTokenHintClaims.SessionID; sid != "" {
		if err := s.endSession(ctx, sid, req.UserID, "signed out by "+req.ClientID, req.ClientID); err != nil {
			return "", err
		}
	} else {
		s.audit(ctx, "oidc_logout", "", req.UserID, map[string]any{"clientId": req.ClientID, "sid": ""})
	}

	return req.RedirectURI, nil
}

// endSession revokes the Claviger session sid of the user, every OIDC token
// issued under it and the single sign-on sessions bound to it. A session that
// is not the user's (or is already gone) is left alone.
func (s *Store) endSession(ctx context.Context, sid, userID, reason, clientID string) error {
	var collectionRef string

	err := s.app.ConcurrentDB().NewQuery("SELECT [[collectionRef]] FROM {{" + core.CollectionNameSessions + "}} WHERE [[id]] = {:sid} AND [[recordRef]] = {:sub}").
		Bind(dbx.Params{"sid": sid, "sub": userID}).Row(&collectionRef)
	if err != nil {
		// not this user's session (or already gone)
		return nil //nolint:nilerr
	}

	if _, err := exec(s.app,
		"UPDATE {{"+core.CollectionNameSessions+"}} SET [[revoked]] = TRUE, [[revokedReason]] = {:reason} WHERE [[id]] = {:sid} AND [[revoked]] = FALSE",
		dbx.Params{"sid": sid, "reason": reason}); err != nil {
		return err
	}

	core.InvalidateSessionCache(s.app)

	for _, table := range []string{core.CollectionNameOAuthRefreshTokens, core.CollectionNameOAuthTokens} {
		if _, err := exec(s.app, "UPDATE {{"+table+"}} SET [[revoked]] = TRUE WHERE [[sid]] = {:sid}", dbx.Params{"sid": sid}); err != nil {
			return err
		}
	}

	if _, err := exec(s.app, "DELETE FROM {{"+core.CollectionNameOAuthSso+"}} WHERE [[sid]] = {:sid}", dbx.Params{"sid": sid}); err != nil {
		return err
	}

	s.audit(ctx, "oidc_logout", collectionRef, userID, map[string]any{"clientId": clientID, "sid": sid})

	return nil
}

// RevokeToken implements op.Storage (RFC 7009).
func (s *Store) RevokeToken(ctx context.Context, tokenOrID, userID, clientID string) *oidc.Error {
	if row, err := s.refreshByID(tokenOrID); err == nil {
		if row.ClientId != clientID {
			return oidc.ErrInvalidClient().WithDescription("the token was not issued to this client")
		}

		s.revokeFamily(ctx, row.FamilyId, "", "", "")

		return nil
	}

	if row, err := s.tokenByID(tokenOrID); err == nil {
		if row.ClientId != clientID {
			return oidc.ErrInvalidClient().WithDescription("the token was not issued to this client")
		}

		if _, err := exec(s.app, "UPDATE {{"+core.CollectionNameOAuthTokens+"}} SET [[revoked]] = TRUE WHERE [[tokenId]] = {:id}", dbx.Params{"id": tokenOrID}); err != nil {
			return oidc.ErrServerError().WithParent(err)
		}
	}

	// RFC 7009: an unknown token is not an error
	return nil
}

// GetRefreshTokenInfo implements op.Storage.
func (s *Store) GetRefreshTokenInfo(_ context.Context, clientID, token string) (string, string, error) {
	row, err := s.refreshByHash(hashToken(token))
	if err != nil || row.ClientId != clientID {
		return "", "", op.ErrInvalidRefreshToken
	}

	return row.Subject, row.Id, nil
}

// ---- unsupported parts of op.Storage ------------------------------------

func (s *Store) GetKeyByIDAndClientID(context.Context, string, string) (*jose.JSONWebKey, error) {
	return nil, errors.New("the JWT profile is not supported")
}

func (s *Store) ValidateJWTProfileScopes(context.Context, string, []string) ([]string, error) {
	return nil, errors.New("the JWT profile is not supported")
}

func (s *Store) SetUserinfoFromScopes(context.Context, *oidc.UserInfo, string, string, []string) error {
	return nil // deprecated hook, SetUserinfoFromRequest is used
}

func (s *Store) GetPrivateClaimsFromScopes(context.Context, string, string, []string) (map[string]any, error) {
	return nil, nil
}

// Health implements op.Storage.
func (s *Store) Health(context.Context) error {
	_, err := s.app.FindCachedCollectionByNameOrId(core.CollectionNameOAuthClients)

	return err
}

func joinScopes(scopes []string) string { return strings.Join(scopes, " ") }
