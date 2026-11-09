package core

import (
	"errors"
	"sync"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #93): server-side sessions and bans.
//
// Every successful authentication creates a row in the hidden _sessions
// collection and puts its id in the "sid" claim of the signed auth token.
// The auth-loading middleware accepts a token that carries a sid only while
// the session exists, is not revoked and has not expired; this is what makes
// revocation (one session, all sessions, a ban) take effect on the next
// request instead of at token expiry.
//
// Tokens without a sid (issued before the feature was deployed, impersonation
// and installer tokens minted elsewhere, non-auth token types) are not checked
// at all and behave exactly as upstream: they keep working until they expire.
//
// Sessions are written with plain SQL on purpose: a login must not fire the
// model/record hooks (and therefore must not add an audit row or break the
// event expectations of code that counts hooks). Revocations through the
// endpoints are record saves and are audited.

const (
	CollectionNameSessions = "_sessions"

	// TokenClaimSessionId is the JWT claim holding the session id.
	TokenClaimSessionId = "sid"

	// TokenClaimImpersonatedBy is set (to the superuser id) on impersonation tokens.
	TokenClaimImpersonatedBy = "impersonatedBy"

	SessionAuthMethodImpersonate = "impersonate"
	SessionAuthMethodMFA         = "mfa"
	SessionAuthMethodDiscover    = "discoverable"
	SessionAuthMethodRefresh     = "refresh"
	SessionAuthMethodOther       = "other"

	SessionRevokedPasswordChanged = "password changed"
	SessionRevokedTokenKeyRotated = "token key rotated"
	SessionRevokedBanned          = "banned"
	SessionRevokedByUser          = "revoked by user"
	SessionRevokedByAdmin         = "revoked by administrator"

	sessionMaxUserAgent    = 255
	sessionCacheStoreKey   = "__clavigerSessionCache__"
	sessionCacheMax        = 20000
	sessionCleanupCronId   = "__pbSessionCleanup__"
	sessionCleanupCronExpr = "47 4 * * *" // daily at 04:47 (off :00, :07, :37, :02 and 03:23)
)

var (
	// SessionCacheTTL is how long a positive session lookup is trusted
	// before the database is consulted again. Revocations made in this
	// process invalidate the cache immediately.
	SessionCacheTTL = 30 * time.Second

	// SessionLastSeenInterval is the minimum gap between lastSeen writes.
	SessionLastSeenInterval = 5 * time.Minute

	// SessionRetention is how long expired or revoked sessions are kept.
	SessionRetention = 30 * 24 * time.Hour

	ErrInvalidSession = errors.New("invalid, revoked or expired session")
)

type sessionCacheEntry struct {
	cachedAt      time.Time
	expires       time.Time
	lastSeen      time.Time
	collectionRef string
	recordRef     string
}

type sessionCache struct {
	mu sync.RWMutex
	m  map[string]sessionCacheEntry

	// fork-only (#106 M7): tells the other replicas, set in multi replica mode
	publish func(sid string)
}

func getSessionCache(app App) *sessionCache {
	return app.Store().GetOrSet(sessionCacheStoreKey, func() any {
		c := &sessionCache{m: map[string]sessionCacheEntry{}}
		if IsMultiReplica(app) {
			c.publish = func(sid string) {
				PublishReplicaEvent(app, ReplicaEventSession, map[string]string{"sid": sid})
			}
		}
		return c
	}).(*sessionCache)
}

func (c *sessionCache) get(sid string) (sessionCacheEntry, bool) {
	c.mu.RLock()
	v, ok := c.m[sid]
	c.mu.RUnlock()
	return v, ok
}

func (c *sessionCache) set(sid string, v sessionCacheEntry) {
	c.mu.Lock()
	if len(c.m) >= sessionCacheMax {
		c.m = map[string]sessionCacheEntry{}
	}
	c.m[sid] = v
	c.mu.Unlock()
}

// remove drops the entry here and on the other replicas.
func (c *sessionCache) remove(sid string) {
	c.removeLocal(sid)
	if c.publish != nil {
		c.publish(sid)
	}
}

// clear drops every entry here and on the other replicas.
func (c *sessionCache) clear() {
	c.clearLocal()
	if c.publish != nil {
		c.publish("")
	}
}

func (c *sessionCache) removeLocal(sid string) {
	c.mu.Lock()
	delete(c.m, sid)
	c.mu.Unlock()
}

func (c *sessionCache) clearLocal() {
	c.mu.Lock()
	c.m = map[string]sessionCacheEntry{}
	c.mu.Unlock()
}

// InvalidateSessionCache drops every cached session lookup of the app.
func InvalidateSessionCache(app App) {
	getSessionCache(app).clear()
}

// CreateSession inserts a session for the auth record and returns its id.
//
// expires is normally the expiration of the token the session backs.
func CreateSession(app App, authRecord *Record, authMethod, userAgent, ip string, expires time.Time) (string, error) {
	if authRecord == nil || !authRecord.Collection().IsAuth() {
		return "", ErrNotAuthRecord
	}

	if len(userAgent) > sessionMaxUserAgent {
		userAgent = userAgent[:sessionMaxUserAgent]
	}

	if authMethod == "" {
		authMethod = SessionAuthMethodOther
	}

	id := GenerateDefaultRandomId()
	now := types.NowDateTime().String()
	exp, _ := types.ParseDateTime(expires.UTC())

	_, err := app.NonconcurrentDB().Insert(CollectionNameSessions, dbx.Params{
		"id":            id,
		"collectionRef": authRecord.Collection().Id,
		"recordRef":     authRecord.Id,
		"created":       now,
		"lastSeen":      now,
		"expires":       exp.String(),
		"userAgent":     userAgent,
		"ip":            ip,
		"authMethod":    authMethod,
		"revoked":       false,
		"revokedReason": "",
	}).Execute()
	if err != nil {
		return "", err
	}

	return id, nil
}

// SetSessionExpires sets (without hooks) the session expiration, e.g. when
// the token is refreshed.
func SetSessionExpires(app App, sid string, expires time.Time) error {
	dt, _ := types.ParseDateTime(expires.UTC())

	_, err := app.NonconcurrentDB().Update(CollectionNameSessions,
		dbx.Params{"expires": dt.String()},
		dbx.HashExp{"id": sid},
	).Execute()

	getSessionCache(app).remove(sid)

	return err
}

// ValidateSession reports whether the session exists, belongs to the
// collection/record, is not revoked and has not expired.
//
// Positive results are cached for SessionCacheTTL. It also records the
// session activity (lastSeen), at most once per SessionLastSeenInterval.
func ValidateSession(app App, sid, collectionId, recordId string) error {
	if sid == "" {
		return ErrInvalidSession
	}

	cache := getSessionCache(app)
	now := time.Now()

	if v, ok := cache.get(sid); ok && now.Sub(v.cachedAt) < SessionCacheTTL {
		if v.collectionRef != collectionId || v.recordRef != recordId || !v.expires.After(now) {
			return ErrInvalidSession
		}

		if now.Sub(v.lastSeen) >= SessionLastSeenInterval {
			touchSessionLastSeen(app, cache, sid, v, now)
		}

		return nil
	}

	var row struct {
		CollectionRef string         `db:"collectionRef"`
		RecordRef     string         `db:"recordRef"`
		Expires       types.DateTime `db:"expires"`
		LastSeen      types.DateTime `db:"lastSeen"`
		Revoked       bool           `db:"revoked"`
	}
	err := app.ConcurrentDB().
		Select("collectionRef", "recordRef", "expires", "lastSeen", "revoked").
		From(CollectionNameSessions).
		Where(dbx.HashExp{"id": sid}).
		Limit(1).
		One(&row)
	if err != nil {
		cache.removeLocal(sid) // not published: a junk sid must not flood the bus
		return ErrInvalidSession
	}

	if row.Revoked ||
		row.CollectionRef != collectionId ||
		row.RecordRef != recordId ||
		row.Expires.IsZero() ||
		!row.Expires.Time().After(now) {
		cache.removeLocal(sid) // not published: a junk sid must not flood the bus
		return ErrInvalidSession
	}

	entry := sessionCacheEntry{
		cachedAt:      now,
		expires:       row.Expires.Time(),
		lastSeen:      row.LastSeen.Time(),
		collectionRef: row.CollectionRef,
		recordRef:     row.RecordRef,
	}
	cache.set(sid, entry)

	if now.Sub(entry.lastSeen) >= SessionLastSeenInterval {
		touchSessionLastSeen(app, cache, sid, entry, now)
	}

	return nil
}

// touchSessionLastSeen writes lastSeen with a conditional UPDATE so that
// concurrent requests cannot both write.
func touchSessionLastSeen(app App, cache *sessionCache, sid string, entry sessionCacheEntry, now time.Time) {
	cutoff := now.Add(-SessionLastSeenInterval)

	_, err := app.NonconcurrentDB().NewQuery(
		"UPDATE {{" + CollectionNameSessions + "}} SET [[lastSeen]] = {:now} " +
			"WHERE [[id]] = {:id} AND ([[lastSeen]] = '' OR [[lastSeen]] IS NULL OR [[lastSeen]] < {:cutoff})",
	).Bind(dbx.Params{
		"now":    now.UTC().Format(types.DefaultDateLayout),
		"cutoff": cutoff.UTC().Format(types.DefaultDateLayout),
		"id":     sid,
	}).Execute()
	if err != nil {
		app.Logger().Debug("Failed to update the session lastSeen", "id", sid, "error", err)
		return
	}

	entry.lastSeen = now
	cache.set(sid, entry)
}

// RevokeSessions revokes (without per-row hooks) the active sessions of the
// auth record, optionally keeping the one with id exceptSid, and returns how
// many were revoked.
func RevokeSessions(app App, collectionId, recordId, reason, exceptSid string) (int64, error) {
	conds := []dbx.Expression{
		dbx.HashExp{"collectionRef": collectionId, "recordRef": recordId, "revoked": false},
	}
	if exceptSid != "" {
		conds = append(conds, dbx.NewExp("[[id]] != {:except}", dbx.Params{"except": exceptSid}))
	}

	res, err := app.NonconcurrentDB().Update(CollectionNameSessions,
		dbx.Params{"revoked": true, "revokedReason": reason},
		dbx.And(conds...),
	).Execute()

	// always, even on error: a partial update must not leave stale cache entries
	InvalidateSessionCache(app)

	if err != nil {
		return 0, err
	}

	return res.RowsAffected()
}

// DeleteExpiredSessions deletes the sessions that expired or were revoked
// more than SessionRetention ago.
func DeleteExpiredSessions(app App, before time.Time) (int64, error) {
	cutoff := before.UTC().Format(types.DefaultDateLayout)

	res, err := app.NonconcurrentDB().Delete(CollectionNameSessions,
		dbx.NewExp("[[expires]] < {:cutoff} OR ([[revoked]] = TRUE AND [[lastSeen]] < {:cutoff})", dbx.Params{"cutoff": cutoff}),
	).Execute()
	if err != nil {
		return 0, err
	}

	return res.RowsAffected()
}

// -------------------------------------------------------------------

// sessionProxy is a minimal proxy so the shared ref hooks
// (validation and cascade delete) can be reused.
type sessionProxy struct{ *Record }

func (p *sessionProxy) ProxyRecord() *Record          { return p.Record }
func (p *sessionProxy) SetProxyRecord(record *Record) { p.Record = record }

func (app *BaseApp) registerSessionHooks() {
	// delete sessions and bans together with their auth record or collection
	recordRefHooks[*sessionProxy](app, CollectionNameSessions, CollectionTypeAuth)
	recordRefHooks[*sessionProxy](app, CollectionNameBans, CollectionTypeAuth)

	// admin edits of a session (or its deletion) must be visible at once
	invalidate := &hook.Handler[*RecordEvent]{
		Id: "__pbSessionCacheInvalidate__",
		Func: func(e *RecordEvent) error {
			err := e.Next()
			if err == nil {
				getSessionCache(e.App).remove(e.Record.Id)
			}
			return err
		},
	}
	app.OnRecordAfterUpdateSuccess(CollectionNameSessions).Bind(invalidate)
	app.OnRecordAfterDeleteSuccess(CollectionNameSessions).Bind(invalidate)

	// a password change or token key rotation kills every JWT of the
	// record (upstream behavior); mark its sessions to match
	app.OnRecordUpdate().Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbSessionTokenKeyRotated__",
		Func: func(e *RecordEvent) error {
			if !e.Record.Collection().IsAuth() || e.Record.Collection().Name == CollectionNameSessions {
				return e.Next()
			}

			err := e.Next()
			if err != nil {
				return err
			}

			orig := e.Record.Original()
			if orig == nil || orig.TokenKey() == e.Record.TokenKey() {
				return nil
			}

			reason := SessionRevokedTokenKeyRotated
			if orig.GetString(FieldNamePassword+":hash") != e.Record.GetString(FieldNamePassword+":hash") {
				reason = SessionRevokedPasswordChanged
			}

			if _, rerr := RevokeSessions(e.App, e.Record.Collection().Id, e.Record.Id, reason, ""); rerr != nil {
				e.App.Logger().Warn("Failed to revoke the sessions after a token key change", "error", rerr, "recordId", e.Record.Id)
			}

			return nil
		},
		Priority: 98,
	})

	app.Cron().Add(sessionCleanupCronId, sessionCleanupCronExpr, func() {
		if _, err := DeleteExpiredSessions(app, time.Now().Add(-SessionRetention)); err != nil {
			app.Logger().Warn("Failed to delete expired sessions", "error", err)
		}
		if _, err := DeleteExpiredBans(app, time.Now().Add(-SessionRetention)); err != nil {
			app.Logger().Warn("Failed to delete expired bans", "error", err)
		}
	})

	app.registerBanHooks()
}
