package apis_test

import (
	"testing"
	"time"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/tests"
)

func TestFixedWindowLimiter(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	l := apis.NewFixedWindowLimiter(app, "unit", 2, time.Hour)

	if blocked, _ := l.Blocked("k"); blocked {
		t.Fatal("an unseen key is not blocked")
	}

	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("event %d must be allowed", i+1)
		}
	}

	blocked, wait := l.Blocked("k")
	if !blocked || wait <= 0 || wait > time.Hour {
		t.Fatalf("a spent budget is blocked with a wait: %v %v", blocked, wait)
	}

	// Blocked does not consume
	if blocked, _ := l.Blocked("other"); blocked {
		t.Fatal("other keys are independent")
	}

	if ok, retry := l.Allow("k"); ok || retry <= 0 {
		t.Fatalf("the third event must be refused with a retry time: %v %v", ok, retry)
	}

	// survives a restart, and Blocked sees it before anything is consumed
	apis.RateLimitTestRestart(app)

	l = apis.NewFixedWindowLimiter(app, "unit", 2, time.Hour)
	if blocked, _ := l.Blocked("k"); !blocked {
		t.Fatal("the persisted budget must block after a restart")
	}

	// a Settings reload does not reset these counters
	apis.RateLimitTestForgetSettingsReload(app)

	l = apis.NewFixedWindowLimiter(app, "unit", 2, time.Hour)
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("a settings reload must not refill an always-on budget")
	}

	l.Clean()
}
