package apis

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/doodlemania2/claviger/core"
)

// Per-account lockout of password sign-in (#139).
//
// Every failed auth-with-password spends one of MaxFailures from a fixed window
// keyed by (collection, submitted identity). Once they are spent the identity is
// refused (429, before the password is looked at) until the window ends, so the
// correct secret does not get through either. A success clears the count.
//
// The key does not depend on whether the account exists, so the answer never
// says which identities are registered. The counters are the persistent fixed
// window limiter (#91), so a restart does not hand back the budget. The tradeoff
// is the usual one: someone who knows an identity can lock it for one window;
// a superuser clears it by changing nothing but waiting, or by turning the
// feature off. It is off by default (Settings.AccountLockout).
type accountLockout struct {
	mu      sync.Mutex
	limiter *FixedWindowLimiter
	max     int
	window  int
	cleaned time.Time
}

const accountLockoutStoreKey = "__clvAccountLockout__"

func accountLockoutFor(app core.App) *accountLockout {
	cfg := app.Settings().AccountLockout
	if !cfg.Enabled() {
		return nil
	}

	// the callback does not touch the app store; the limiter below does, so it is built outside it
	al := app.Store().GetOrSet(accountLockoutStoreKey, func() any {
		return &accountLockout{cleaned: time.Now()}
	}).(*accountLockout)

	al.mu.Lock()
	defer al.mu.Unlock()
	// a changed setting starts a new budget (the old counters expire on their own)
	if al.limiter == nil || al.max != cfg.MaxFailures || al.window != cfg.Window() {
		al.max = cfg.MaxFailures
		al.window = cfg.Window()
		al.limiter = NewFixedWindowLimiter(app, "accountlockout:"+strconv.Itoa(al.max)+":"+strconv.Itoa(al.window), al.max, time.Duration(al.window)*time.Second)
	}
	if time.Since(al.cleaned) >= time.Minute {
		al.cleaned = time.Now()
		al.limiter.Clean()
	}

	return al
}

func accountLockoutKey(collection *core.Collection, identity string) string {
	sum := sha256.Sum256([]byte(collection.Id + "\x00" + strings.ToLower(strings.TrimSpace(identity))))
	return hex.EncodeToString(sum[:16])
}

// check refuses a locked identity. It consumes nothing.
func (al *accountLockout) check(e *core.RequestEvent, key string) error {
	if blocked, wait := al.limiter.Blocked(key); blocked {
		secs := int(wait.Round(time.Second) / time.Second)
		if secs < 1 {
			secs = 1
		}
		e.Response.Header().Set("Retry-After", strconv.Itoa(secs))

		return e.TooManyRequestsError("Too many failed sign-in attempts. Try again later.", errors.New("account lockout"))
	}

	return nil
}

// failed spends one failure of the identity.
func (al *accountLockout) failed(key string) { al.limiter.Allow(key) }

// succeeded clears the identity's failures.
func (al *accountLockout) succeeded(key string) { al.limiter.Reset(key) }
