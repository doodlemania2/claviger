package apis

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/doodlemania2/claviger/core"
)

// login-begin is unauthenticated and, before issue #40, unthrottled unless the
// operator had turned on Settings.RateLimits (default off). It is the endpoint
// an attacker would use to probe which addresses are registered, so it carries
// its own always-on throttle with two independent budgets:
//
//   - per client IP, so one host cannot sweep a list of addresses;
//   - per account identity (normalized submitted identity), so a botnet cannot
//     hammer one account either.
//
// The windows are fixed 60s windows. 20/min per IP leaves room for several
// people behind one church NAT signing in at the same time (a login is one
// begin call), while capping an enumeration sweep to ~20 addresses a minute.
// 10/min per identity is far above a human retrying their own passkey prompt
// (login-finish itself allows 5 per 3 minutes per account) yet bounds
// targeted probing.
//
// Known tradeoff: the identity budget is shared by all callers, so an attacker
// can spend a victim's 10/min and make that victim's passkey sign-in return 429
// for the rest of the window. Doing so costs the attacker IP budget (at most 20
// requests/min per IP, and requests refused by the IP budget do not touch the
// identity budget). Password and OTP sign-in are unaffected.
const (
	webauthnLoginBeginIPMax       = 20
	webauthnLoginBeginIdentityMax = 10
	webauthnLoginBeginWindowSec   = 60

	webauthnLoginBeginLimitersKey = "webauthn:loginbegin:limiters"
	webauthnLoginBeginCleanEvery  = time.Minute
)

type webauthnLoginBeginLimiters struct {
	ip       *rateLimiter
	identity *rateLimiter

	mu        sync.Mutex
	lastClean time.Time
}

func newWebAuthnLoginBeginLimiters() *webauthnLoginBeginLimiters {
	return &webauthnLoginBeginLimiters{
		ip:        newRateLimiter(webauthnLoginBeginIPMax, webauthnLoginBeginWindowSec, webauthnLoginBeginWindowSec),
		identity:  newRateLimiter(webauthnLoginBeginIdentityMax, webauthnLoginBeginWindowSec, webauthnLoginBeginWindowSec),
		lastClean: time.Now(),
	}
}

// maybeClean drops expired clients so attacker-chosen keys cannot grow the maps
// without bound.
func (l *webauthnLoginBeginLimiters) maybeClean() {
	l.mu.Lock()
	due := time.Since(l.lastClean) >= webauthnLoginBeginCleanEvery
	if due {
		l.lastClean = time.Now()
	}
	l.mu.Unlock()

	if due {
		l.ip.clean()
		l.identity.clean()
	}
}

// webauthnLoginBeginLimitersFor returns the process-wide limiters.
func webauthnLoginBeginLimitersFor(e *core.RequestEvent) *webauthnLoginBeginLimiters {
	limiters := e.App.Store().GetOrSet(webauthnLoginBeginLimitersKey, func() any {
		return newWebAuthnLoginBeginLimiters()
	}).(*webauthnLoginBeginLimiters)

	limiters.maybeClean()

	return limiters
}

// checkWebAuthnLoginBeginIPLimit enforces only the per-IP budget. The
// discoverable (usernameless) login-begin has no identity in the request, so
// it spends from the very same per-IP budget as identity-bound login-begin:
// the two cannot be combined to exceed the cap.
func checkWebAuthnLoginBeginIPLimit(e *core.RequestEvent) error {
	limiters := webauthnLoginBeginLimitersFor(e)

	// same client IP resolution as upstream's checkRateLimit (honors the
	// trusted proxy settings); like upstream, an empty key is not throttled
	if ip := e.RealIP(); ip != "" {
		if !limiters.ip.isAllowed(ip) {
			return e.TooManyRequestsError("", errors.New("triggered rate limit rule: webauthn login-begin per IP"))
		}
	}

	return nil
}

// checkWebAuthnLoginBeginLimits enforces both budgets and returns the same 429
// ApiError shape the upstream rate limiter produces. It ignores
// Settings.RateLimits on purpose.
func checkWebAuthnLoginBeginLimits(e *core.RequestEvent, identity string) error {
	if err := checkWebAuthnLoginBeginIPLimit(e); err != nil {
		return err
	}

	limiters := webauthnLoginBeginLimitersFor(e)

	// hash the key to bound its size regardless of what is submitted
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(identity))))
	if !limiters.identity.isAllowed(hex.EncodeToString(sum[:16])) {
		return e.TooManyRequestsError("", errors.New("triggered rate limit rule: webauthn login-begin per identity"))
	}

	return nil
}
