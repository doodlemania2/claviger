package apis

import (
	"strings"
	"time"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local: an always-on, persistent fixed-window limiter for packages
// outside apis (the OpenID Connect provider, #114).
//
// It is the machinery of the WebAuthn login-begin throttle (rate_limit_store.go,
// record_auth_with_webauthn_login_begin_limit.go) behind a small exported type:
// the counters are written through to the _rateLimits table, so a restart does
// not hand an attacker a fresh budget, the client key is never stored (only
// sha256(rule, key)), and Settings.RateLimits neither enables nor resets it.

// fixedRulePrefixes are the rule name prefixes whose counters survive a
// Settings reload.
var fixedRulePrefixes = []string{rateLimitFixedRulePrefix, FixedWindowRulePrefix}

// FixedWindowRulePrefix is the required prefix of a FixedWindowLimiter name.
// NewFixedWindowLimiter adds it, so callers never see it.
const FixedWindowRulePrefix = "ext:"

func isFixedRule(rule string) bool {
	for _, p := range fixedRulePrefixes {
		if strings.HasPrefix(rule, p) {
			return true
		}
	}

	return false
}

// FixedWindowLimiter allows max events per client key in each fixed window.
type FixedWindowLimiter struct {
	rt     *rateLimiter
	window time.Duration
}

// NewFixedWindowLimiter returns a limiter named name (unique per budget).
// Call it after the app is bootstrapped (the counters live in auxiliary.db);
// when persistence is unavailable it silently keeps the counters in memory.
func NewFixedWindowLimiter(app core.App, name string, max int, window time.Duration) *FixedWindowLimiter {
	sec := int64(window / time.Second)
	if sec < 1 {
		sec = 1
	}

	return &FixedWindowLimiter{
		rt:     newPersistentRateLimiter(rateLimitStoreFor(app), FixedWindowRulePrefix+name, max, sec, sec),
		window: window,
	}
}

// Allow consumes one event of the key's budget. When the budget is spent it
// returns false and how long until the window resets.
func (l *FixedWindowLimiter) Allow(key string) (bool, time.Duration) {
	if l.rt.isAllowed(key) {
		return true, 0
	}

	return false, l.retryAfter(key)
}

// Blocked reports whether the key's budget is already spent, without
// consuming anything.
func (l *FixedWindowLimiter) Blocked(key string) (bool, time.Duration) {
	l.rt.RLock()
	client := l.rt.clients[key]
	l.rt.RUnlock()

	if client == nil {
		// after a restart the counter is only in the persisted rows: load it
		// (a key with no live row has nothing to be blocked by)
		if l.rt.persist == nil || l.rt.persist.lookup(rateLimitRecordId(l.rt.name, key)) == nil {
			return false, 0
		}

		l.rt.Lock()
		if client = l.rt.clients[key]; client == nil {
			client = newRateClient(l.rt.maxAllowed, l.rt.interval)
			client.attach(l.rt.persist, l.rt.name, key)
			l.rt.clients[key] = client
		}
		l.rt.Unlock()
	}

	client.Lock()
	defer client.Unlock()

	now := time.Now().Unix()
	if now-client.start >= client.interval || client.available > 0 {
		return false, 0
	}

	return true, time.Duration(client.start+client.interval-now) * time.Second
}

func (l *FixedWindowLimiter) retryAfter(key string) time.Duration {
	if blocked, d := l.Blocked(key); blocked {
		return d
	}

	return l.window
}

// Clean drops the clients whose window ended, so attacker-chosen keys cannot
// grow the map without bound. Call it now and then.
func (l *FixedWindowLimiter) Clean() { l.rt.clean() }
