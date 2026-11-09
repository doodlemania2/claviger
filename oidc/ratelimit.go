package oidc

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
)

// Rate limits of the protocol endpoints (#114).
//
// They are always on and independent of Settings.RateLimits (like the WebAuthn
// login-begin throttle): a deployment that never turned the generic limiter on
// still cannot have its token endpoint hammered. The counters are the
// persistent ones of #91, so a restart does not refill a spent budget.
//
// All windows are fixed. The numbers are sized for a congregation: a few
// hundred people behind one NAT and a handful of relying parties whose
// servers each share one address.
//
//	endpoint     per IP / min   per client / min   notes
//	authorize        120             1200          a browser redirect; silent prompt=none checks count too
//	token            120              600          one call per login, one per refresh
//	introspect       600             1200          a resource server may call it per request
//	revoke            60              300
//	userinfo         300               60 / token  no client id in the request: the bearer token is the key
//	device_authorization 30           300          one call per device sign-in (#96)
//
// The per-client budget is a ceiling against a runaway client loop, not an
// abuse control: the client id of an unauthenticated request is public, so a
// third party could spend it. It is therefore generous, only counted for
// registered clients, and the IP budget is what holds an attacker.
//
// Failed client authentication (HTTP 401 from the token, introspection and
// revocation endpoints) is counted apart and is much stricter, in a longer
// window: 10 failures per 5 minutes per (IP, client) and 30 per 5 minutes per
// IP. Once a budget is spent the endpoint answers 429 before it looks at the
// credentials at all, so guessing stops. The pair key (not the client alone)
// is deliberate: a stranger who knows a client id must not be able to lock the
// real client out by failing on purpose.
//
// A refused request is HTTP 429 with Retry-After. RFC 6749 has no 429 and RFC
// 8628's slow_down belongs to device polling, so the JSON body uses the
// closest registered OAuth error code, temporarily_unavailable (RFC 6749
// 4.1.2.1: the server cannot handle the request right now). authorize is a
// browser navigation, so it gets the hosted error page instead.
const (
	rateWindow     = time.Minute
	failedWindow   = 5 * time.Minute
	failedPerPair  = 10
	failedPerIP    = 30
	limitersKey    = "__clavigerOIDCLimiters__"
	limitersClean  = time.Minute
	rateErrorCode  = "temporarily_unavailable"
	maxBodyForAuth = 64 << 10

	// Device user code entry (#96): 30 submissions a minute per IP, and a
	// brute-force stop of 10 wrong codes per 15 minutes per IP. The codes are
	// 20^8 (2.6e10) and live 10 minutes, so ten guesses a quarter hour is far
	// from a hit.
	deviceEntryPerIP = 30
	deviceFailPerIP  = 10
	deviceFailWindow = 15 * time.Minute
)

type budget struct{ ip, client int }

var endpointBudgets = map[string]budget{
	"authorize":  {120, 1200},
	"token":      {120, 600},
	"introspect": {600, 1200},
	"revoke":     {60, 300},
	"userinfo":   {300, 60},

	// device_authorization (#96) creates a row per call and needs no
	// credentials from a public client, so the budget is the tightest of the
	// protocol endpoints: a device asks once per sign-in.
	"device_authorization": {30, 300},
}

// endpointOf maps a request path to the limited endpoint ("" for the others).
func endpointOf(p string) string {
	p = strings.TrimSuffix(path.Clean("/"+strings.TrimPrefix(p, mountPath)), "/")

	switch p {
	case "/authorize":
		return "authorize"
	case "/oauth/token":
		return "token"
	case "/oauth/introspect":
		return "introspect"
	case "/revoke":
		return "revoke"
	case "/userinfo":
		return "userinfo"
	case "/device_authorization":
		return "device_authorization"
	}

	return ""
}

// clientAuthenticated endpoints authenticate the client, so a 401 is a failed
// client authentication.
func clientAuthenticated(endpoint string) bool {
	return endpoint == "token" || endpoint == "introspect" || endpoint == "revoke" || endpoint == "device_authorization"
}

type limiterSet struct {
	ip, client       map[string]*apis.FixedWindowLimiter
	failIP, failPair *apis.FixedWindowLimiter

	// device code entry (#96): every attempt, and the wrong ones apart
	deviceEntry, deviceFail *apis.FixedWindowLimiter

	mu        sync.Mutex
	lastClean time.Time
}

func (m *module) limiters() *limiterSet {
	// built outside Store.GetOrSet: the limiters resolve the persistent counter
	// store from the same app store, which is not reentrant
	m.limMu.Lock()

	s, _ := m.app.Store().Get(limitersKey).(*limiterSet)
	if s == nil {
		s = &limiterSet{
			ip:       map[string]*apis.FixedWindowLimiter{},
			client:   map[string]*apis.FixedWindowLimiter{},
			failIP:   apis.NewFixedWindowLimiter(m.app, "oidc:failauth:ip", failedPerIP, failedWindow),
			failPair: apis.NewFixedWindowLimiter(m.app, "oidc:failauth:pair", failedPerPair, failedWindow),

			deviceEntry: apis.NewFixedWindowLimiter(m.app, "oidc:device:entry:ip", deviceEntryPerIP, rateWindow),
			deviceFail:  apis.NewFixedWindowLimiter(m.app, "oidc:device:fail:ip", deviceFailPerIP, deviceFailWindow),
			lastClean:   time.Now(),
		}

		for name, b := range endpointBudgets {
			s.ip[name] = apis.NewFixedWindowLimiter(m.app, "oidc:"+name+":ip", b.ip, rateWindow)
			s.client[name] = apis.NewFixedWindowLimiter(m.app, "oidc:"+name+":client", b.client, rateWindow)
		}

		m.app.Store().Set(limitersKey, s)
	}

	m.limMu.Unlock()

	// drop the expired clients now and then so chosen keys cannot grow the maps
	s.mu.Lock()
	due := time.Since(s.lastClean) >= limitersClean
	if due {
		s.lastClean = time.Now()
	}
	s.mu.Unlock()

	if due {
		for _, l := range s.ip {
			l.Clean()
		}

		for _, l := range s.client {
			l.Clean()
		}

		s.failIP.Clean()
		s.failPair.Clean()
		s.deviceEntry.Clean()
		s.deviceFail.Clean()
	}

	return s
}

// clientIDOf returns the client id a protocol request names (Basic
// credentials, else the client_id parameter), leaving the body readable.
func clientIDOf(r *http.Request) string {
	if user, _, ok := r.BasicAuth(); ok {
		id, err := url.QueryUnescape(user)
		if err != nil {
			return user
		}

		return id
	}

	if id := r.URL.Query().Get("client_id"); id != "" {
		return id
	}

	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Body == nil {
		return ""
	}

	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		return ""
	}

	body, err := readAndRestore(r)
	if err != nil {
		return ""
	}

	form, err := url.ParseQuery(string(body))
	if err != nil {
		return ""
	}

	return form.Get("client_id")
}

func bearerKey(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < 8 || !strings.EqualFold(h[:7], "bearer ") {
		// no bearer: the library answers 401 and the IP budget holds
		return ""
	}

	sum := sha256.Sum256([]byte(strings.TrimSpace(h[7:])))

	return hex.EncodeToString(sum[:16])
}

// statusRecorder remembers the status the library answered with.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}

	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}

	return s.ResponseWriter.Write(p)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// limited is the gate in front of the library. It reports whether it already
// answered the request (with a 429); otherwise the returned func (nil for the
// endpoints that are not limited) must be called with the final status.
func (m *module) limited(e *core.RequestEvent) (done bool, after func(status int)) {
	endpoint := endpointOf(e.Request.URL.Path)
	if endpoint == "" {
		return false, nil
	}

	// nothing to protect, and no counters written, until a client is registered
	if m.clientCount() == 0 {
		return false, nil
	}

	set := m.limiters()
	ip := e.RealIP()
	clientID := clientIDOf(e.Request)

	refuse := func(which string, retry time.Duration, level func(string, ...any)) bool {
		level("OpenID Connect rate limit", "endpoint", endpoint, "budget", which, "retryAfter", retry.Round(time.Second).String())
		m.tooManyRequests(e, endpoint, retry)

		return true
	}

	// the client key: the registered client id; userinfo has none, so the token
	var clientKey string

	switch {
	case endpoint == "userinfo":
		clientKey = bearerKey(e.Request)
	case clientID != "":
		if _, err := m.store.findClient(clientID); err == nil {
			clientKey = clientID
		}
	}

	// a spent failed-authentication budget refuses before credentials are looked at
	if clientAuthenticated(endpoint) && ip != "" {
		if blocked, retry := set.failIP.Blocked(ip); blocked {
			return refuse("failed client authentication (per IP)", retry, m.app.Logger().Debug), nil
		}

		if blocked, retry := set.failPair.Blocked(ip + "|" + clientID); blocked {
			return refuse("failed client authentication (per IP and client)", retry, m.app.Logger().Debug), nil
		}
	}

	if ip != "" {
		if ok, retry := set.ip[endpoint].Allow(ip); !ok {
			return refuse("per IP", retry, m.app.Logger().Info), nil
		}
	}

	if clientKey != "" {
		if ok, retry := set.client[endpoint].Allow(clientKey); !ok {
			return refuse("per client", retry, m.app.Logger().Info), nil
		}
	}

	return false, func(status int) {
		if status != http.StatusUnauthorized || !clientAuthenticated(endpoint) || ip == "" {
			return
		}

		set.failIP.Allow(ip)
		set.failPair.Allow(ip + "|" + clientID)

		if blocked, _ := set.failPair.Blocked(ip + "|" + clientID); blocked {
			m.app.Logger().Warn("OpenID Connect client authentication keeps failing, refusing further attempts",
				"endpoint", endpoint, "clientId", clientID, "ip", ip)
		}
	}
}

// tooManyRequests writes the 429.
func (m *module) tooManyRequests(e *core.RequestEvent, endpoint string, retry time.Duration) {
	secs := int(retry.Seconds() + 0.999)
	if secs < 1 {
		secs = 1
	}

	h := e.Response.Header()
	h.Set("Retry-After", strconv.Itoa(secs))
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")

	if endpoint == "authorize" {
		_ = m.errorPage(e, http.StatusTooManyRequests, "Too many sign-in requests. Wait a minute and try again.")

		return
	}

	_ = e.JSON(http.StatusTooManyRequests, map[string]any{
		"error":             rateErrorCode,
		"error_description": "Too many requests. Retry after " + strconv.Itoa(secs) + " seconds.",
	})
}
