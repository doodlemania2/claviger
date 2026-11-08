package apis_test

import (
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/tests"
)

func rateLimitRowCount(t *testing.T, app *tests.TestApp) int {
	t.Helper()
	var n int
	if err := app.AuxNonconcurrentDB().NewQuery("SELECT COUNT(*) FROM {{_rateLimits}}").Row(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPersistentRateLimitSurvivesRestart(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	allow := apis.RateLimitTestLimiter(app, "rule-a", 3, 3600)
	for i := 0; i < 3; i++ {
		if !allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	if allow("1.2.3.4") {
		t.Fatal("4th request should be blocked")
	}

	apis.RateLimitTestRestart(app)

	// brand new limiter + store, same aux DB
	allow = apis.RateLimitTestLimiter(app, "rule-a", 3, 3600)
	if allow("1.2.3.4") {
		t.Fatal("exhausted budget must survive the restart")
	}
	if !allow("5.6.7.8") {
		t.Fatal("other clients are unaffected")
	}
	// another rule has its own budget
	if !apis.RateLimitTestLimiter(app, "rule-b", 3, 3600)("1.2.3.4") {
		t.Fatal("other rules are unaffected")
	}
}

func TestPersistentRateLimitPartialBudgetSurvivesRestart(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	allow := apis.RateLimitTestLimiter(app, "rule-p", 3, 3600)
	allow("ip")
	apis.RateLimitTestRestart(app)

	allow = apis.RateLimitTestLimiter(app, "rule-p", 3, 3600)
	if !allow("ip") || !allow("ip") {
		t.Fatal("2 requests should remain")
	}
	if allow("ip") {
		t.Fatal("budget should be exhausted after 1+2 requests")
	}
}

func TestPersistentRateLimitWindowExpiryAfterRestart(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	allow := apis.RateLimitTestLimiter(app, "rule-w", 1, 1)
	if !allow("ip") || allow("ip") {
		t.Fatal("unexpected initial decisions")
	}
	apis.RateLimitTestRestart(app)
	time.Sleep(2100 * time.Millisecond)

	// the window ended while down: row is expired and not loaded
	if !apis.RateLimitTestLimiter(app, "rule-w", 1, 1)("ip") {
		t.Fatal("a new window should start")
	}
}

func TestPersistentRateLimitBatchedWrites(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	allow := apis.RateLimitTestLimiter(app, "rule-b", 100000, 3600)
	for i := 0; i < 500; i++ {
		allow("ip")
		allow("ip2")
	}

	if got := apis.RateLimitTestFlushes(app); got != 0 {
		t.Fatalf("no write is expected on the request path, got %d flushes", got)
	}
	if n := rateLimitRowCount(t, app); n != 0 {
		t.Fatalf("no row expected before the flush, got %d", n)
	}

	if err := apis.RateLimitTestFlush(app); err != nil {
		t.Fatal(err)
	}
	if got := apis.RateLimitTestFlushes(app); got != 1 {
		t.Fatalf("1000 requests must cost 1 write transaction, got %d", got)
	}
	if n := rateLimitRowCount(t, app); n != 2 {
		t.Fatalf("expected 2 rows, got %d", n)
	}

	// nothing changed: no further write
	if err := apis.RateLimitTestFlush(app); err != nil {
		t.Fatal(err)
	}
	if got := apis.RateLimitTestFlushes(app); got != 1 {
		t.Fatalf("an idle flush must not write, got %d", got)
	}
}

func TestPersistentRateLimitCleansExpiredRows(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	if !apis.RateLimitTestEnabled(app) {
		t.Fatal("store should be enabled")
	}

	now := time.Now().Unix()
	for id, exp := range map[string]int64{"old": now - 100, "live": now + 100} {
		_, err := app.AuxNonconcurrentDB().NewQuery(
			"INSERT INTO {{_rateLimits}} ([[id]], [[rule]], [[windowStart]], [[used]], [[expires]]) VALUES ({:id}, 'r', 0, 1, {:exp})",
		).Bind(dbx.Params{"id": id, "exp": exp}).Execute()
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := apis.RateLimitTestClean(app); err != nil {
		t.Fatal(err)
	}
	if n := rateLimitRowCount(t, app); n != 1 {
		t.Fatalf("expected only the live row, got %d", n)
	}

	// expired rows are also dropped at load
	_, _ = app.AuxNonconcurrentDB().NewQuery(
		"INSERT INTO {{_rateLimits}} ([[id]], [[rule]], [[windowStart]], [[used]], [[expires]]) VALUES ('old2', 'r', 0, 1, 1)",
	).Execute()
	apis.RateLimitTestRestart(app)
	apis.RateLimitTestEnabled(app)
	if n := rateLimitRowCount(t, app); n != 1 {
		t.Fatalf("expected the expired row to be removed at load, got %d", n)
	}
}

func TestPersistentRateLimitSettingsReloadResetsOnlyConfigurable(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	cfg := apis.RateLimitTestLimiter(app, "/api/x", 1, 3600)
	fixed := apis.RateLimitTestLimiter(app, "webauthn:loginbegin:ip", 1, 3600)
	cfg("ip")
	fixed("ip")
	if err := apis.RateLimitTestFlush(app); err != nil {
		t.Fatal(err)
	}

	apis.RateLimitTestForgetSettingsReload(app)
	if err := apis.RateLimitTestFlush(app); err != nil {
		t.Fatal(err)
	}
	if n := rateLimitRowCount(t, app); n != 1 {
		t.Fatalf("expected the always-on row to remain, got %d", n)
	}

	apis.RateLimitTestRestart(app)
	if !apis.RateLimitTestLimiter(app, "/api/x", 1, 3600)("ip") {
		t.Fatal("configurable limiter should have been reset")
	}
	if apis.RateLimitTestLimiter(app, "webauthn:loginbegin:ip", 1, 3600)("ip") {
		t.Fatal("always-on limiter must persist")
	}
}
