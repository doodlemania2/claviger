package apis

// Fork-local addition (issue #106, M7): the request state that upstream and
// the first fork versions kept in process memory, made shared when several
// replicas run on one PostgreSQL database. Every function here is inert (and
// the callers behave exactly as before) when the mode is off.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/replica"
)

const replicaStateTimeout = 5 * time.Second

// putSharedWebAuthnSession stores a WebAuthn ceremony session in the database,
// so the finish request can reach any replica.
func putSharedWebAuthnSession(app core.App, token string, entry *webauthnSessionEntry) {
	bus := core.ReplicaBusOf(app)
	if bus == nil {
		return
	}

	raw, err := json.Marshal(entry)
	if err != nil {
		app.Logger().Warn("Failed to encode a WebAuthn session for the other replicas", "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), replicaStateTimeout)
	defer cancel()

	if err := replica.PutCeremony(ctx, bus.DB(), token, raw, entry.ExpiresAt.Unix()+1); err != nil {
		app.Logger().Warn("Failed to share a WebAuthn session with the other replicas", "error", err)
	}
}

// getSharedWebAuthnSession reads a ceremony session from the database. shared
// is false when the mode is off (the caller uses its process memory); entry is
// nil when the token is unknown, expired or already used.
func getSharedWebAuthnSession(app core.App, token string) (entry *webauthnSessionEntry, shared bool) {
	bus := core.ReplicaBusOf(app)
	if bus == nil {
		return nil, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), replicaStateTimeout)
	defer cancel()

	raw, err := replica.GetCeremony(ctx, bus.DB(), token)
	if err != nil {
		app.Logger().Warn("Failed to read a shared WebAuthn session", "error", err)
		return nil, true
	}
	if raw == nil {
		return nil, true
	}

	e := &webauthnSessionEntry{}
	if err := json.Unmarshal(raw, e); err != nil || e.Session == nil || time.Now().After(e.ExpiresAt) {
		return nil, true
	}

	return e, true
}

func deleteSharedWebAuthnSession(app core.App, token string) {
	bus := core.ReplicaBusOf(app)
	if bus == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), replicaStateTimeout)
	defer cancel()

	if err := replica.DeleteCeremony(ctx, bus.DB(), token); err != nil {
		app.Logger().Warn("Failed to delete a shared WebAuthn session", "error", err)
	}
}

// ---------------------------------------------------------------------------
// shared rate limit counters
// ---------------------------------------------------------------------------

// sharedRateLimitMarkers pick the limiters whose counters are shared between
// the replicas: everything that guards a credential, a code or a token. The
// limiters of the generic API budget (reads, writes, files...) stay per
// replica because counting them in the database would add a round trip to every
// request. A rule is shared when its name (the configured label, or the
// internal rule id) contains one of these.
var sharedRateLimitMarkers = []string{
	"auth", "otp", "totp", "password", "passkey", "webauthn", "verification",
	"login", "oidc:", "ext:", "@pb_", "token", "email-change", "reset",
}

func isSharedRateLimitRule(name string) bool {
	n := strings.ToLower(name)
	for _, m := range sharedRateLimitMarkers {
		if strings.Contains(n, m) {
			return true
		}
	}
	return false
}

// sharedRateLimits counts the requests of the shared limiters in the database.
type sharedRateLimits struct {
	app core.App
	bus *replica.Bus
}

func sharedRateLimitsFor(app core.App, rule string) *sharedRateLimits {
	if app == nil || !isSharedRateLimitRule(rule) {
		return nil
	}

	bus := core.ReplicaBusOf(app)
	if bus == nil {
		return nil
	}

	return &sharedRateLimits{app: app, bus: bus}
}

// hit counts one request. ok is false when the database could not be asked, and
// the caller falls back to its replica local counter (a limiter must not take
// the login endpoints down with the database).
func (s *sharedRateLimits) hit(rule, key string, max int, windowSec int64) (allowed, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), replicaStateTimeout)
	defer cancel()

	used, _, err := replica.RateHit(ctx, s.bus.DB(), rateLimitRecordId(rule, key), rule, windowSec)
	if err != nil {
		s.app.Logger().Warn("Shared rate limit unavailable, counting on this replica only", "error", err)
		return false, false
	}

	return used <= max, true
}

// blocked reports, without counting, whether the budget is spent and for how
// long.
func (s *sharedRateLimits) blocked(rule, key string, max int) (blocked bool, retry time.Duration, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), replicaStateTimeout)
	defer cancel()

	used, expires, err := replica.RatePeek(ctx, s.bus.DB(), rateLimitRecordId(rule, key))
	if err != nil {
		return false, 0, false
	}
	if used < max {
		return false, 0, true
	}

	return true, time.Until(time.Unix(expires, 0)), true
}
