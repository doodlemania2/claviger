package apis

import (
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/captcha"
)

// RateLimitTestLimiter returns a persistent limiter decision func.
func RateLimitTestLimiter(app core.App, name string, maxAllowed int, interval int64) func(key string) bool {
	rt := newPersistentRateLimiter(rateLimitStoreFor(app), name, maxAllowed, interval, 1800)
	return rt.isAllowed
}

func RateLimitTestEnabled(app core.App) bool { return rateLimitStoreFor(app) != nil }

func RateLimitTestFlush(app core.App) error { return rateLimitStoreFor(app).flush() }

func RateLimitTestFlushes(app core.App) int {
	s := rateLimitStoreFor(app)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushes
}

func RateLimitTestClean(app core.App) error { return rateLimitStoreFor(app).cleanExpired() }

// RateLimitTestRestart simulates a process restart: pending counters are
// flushed (OnTerminate) and the in-memory store is dropped, so the next
// limiter reloads from auxiliary.db.
func RateLimitTestRestart(app core.App) {
	if s, ok := app.Store().Get(rateLimitStoreKey).(*rateLimitStore); ok {
		s.close()
	}
	app.Store().Remove(rateLimitStoreKey)
}

// RateLimitTestForgetSettingsReload mimics the Settings reload reset.
func RateLimitTestForgetSettingsReload(app core.App) { forgetRateLimitStoreEntries(app) }

// SetCaptchaVerifierForTest replaces the provider verifier.
func SetCaptchaVerifierForTest(app core.App, v captcha.Verifier) {
	app.Store().Set(captchaVerifierStoreKey, v)
}

func PasskeysPageCSPForTest(cfg core.CaptchaConfig) string { return passkeysPageCSP(cfg) }

func AdminUICSPForTest(app core.App) string { return adminUICSP(app) }

const DefaultCSPForTest = defaultCSP
