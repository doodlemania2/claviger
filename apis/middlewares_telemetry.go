package apis

// Fork-local addition — not upstream PocketBase. See tools/telemetry.

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/router"
	"github.com/doodlemania2/claviger/tools/telemetry"
)

const (
	// DefaultTelemetryMiddlewareId is the id of the request tracing/metrics middleware.
	DefaultTelemetryMiddlewareId = "claviger.telemetry"

	// DefaultTelemetryMiddlewarePriority runs before every other default
	// middleware (the panic recover included), so the span covers the whole
	// request and sees the final status.
	DefaultTelemetryMiddlewarePriority = DefaultWWWRedirectMiddlewarePriority - 1000

	// DefaultMetricsRoute is the Prometheus scrape endpoint. It is registered
	// only while CLAVIGER_METRICS_TOKEN is set.
	DefaultMetricsRoute = "/api/metrics"
)

// telemetryMiddleware records a server span and the HTTP metrics of each
// request. While telemetry is not active it costs one atomic load.
func telemetryMiddleware() *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id:       DefaultTelemetryMiddlewareId,
		Priority: DefaultTelemetryMiddlewarePriority,
		Func: func(e *core.RequestEvent) error {
			t := telemetry.Active()
			if t == nil {
				return e.Next()
			}

			var obs *telemetry.Request
			e.Request, obs = t.Begin(e.Request)

			err := e.Next()

			status := http.StatusOK
			switch {
			case err != nil:
				// the error handler has not written the response yet
				status = router.ToApiError(err).Status
			default:
				if st, ok := e.Response.(router.StatusTracker); ok && st.Status() != 0 {
					status = st.Status()
				}
			}
			obs.End(status)

			return err
		},
	}
}

// bindMetricsApi registers GET /api/metrics, a Prometheus text endpoint behind
// a static bearer token. Without CLAVIGER_METRICS_TOKEN no route is registered,
// so the path answers 404 like any unknown one.
func bindMetricsApi(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	token := telemetry.MetricsToken()
	if token == "" {
		return
	}

	// compare fixed-size digests so the comparison is constant time whatever
	// the length of the presented value
	want := sha256.Sum256([]byte(token))

	rg.GET("/metrics", func(e *core.RequestEvent) error {
		got := sha256.Sum256([]byte(bearerToken(e.Request)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			e.Response.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			return router.NewUnauthorizedError("Missing or invalid metrics token.", nil)
		}

		t := telemetry.Active()
		if t == nil || t.PrometheusHandler() == nil {
			return router.NewApiError(http.StatusServiceUnavailable, "Metrics are not available.", nil)
		}

		t.PrometheusHandler().ServeHTTP(e.Response, e.Request)
		return nil
	}).Bind(SkipSuccessActivityLog())
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return h[7:]
	}
	return ""
}
