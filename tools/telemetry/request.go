package telemetry

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func attrString(k, v string) attribute.KeyValue { return attribute.String(k, v) }

// untracedRoutes never get a span. A 10s health probe is ~8.6k spans a day and
// a realtime stream stays open for hours; both still count in the metrics.
var untracedRoutes = map[string]bool{
	"/api/health":   true,
	"/api/realtime": true,
	"/api/metrics":  true,
}

var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodConnect: true,
	http.MethodOptions: true, http.MethodTrace: true,
}

// routeOf returns the route template of a ServeMux pattern ("GET /a/{id}" ->
// "/a/{id}"). The template, never the raw path, keeps cardinality bounded.
func routeOf(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = pattern[i+1:]
	}
	if pattern == "" {
		return "unmatched"
	}
	return pattern
}

// Request is the observation of one in-flight HTTP request.
type Request struct {
	t      *Telemetry
	span   trace.Span
	gid    uint64
	start  time.Time
	method string
	route  string
}

// Begin starts observing r, which must already carry its route pattern (that
// is, be called from inside the mux handler). It returns the request to hand
// to the rest of the chain.
//
// Only the method, route template, scheme, protocol version and status code
// ever become attributes. Query strings, headers (Authorization included),
// bodies and the raw path are never read.
func (t *Telemetry) Begin(r *http.Request) (*http.Request, *Request) {
	obs := &Request{t: t, start: time.Now(), route: routeOf(r.Pattern), method: "_OTHER"}
	if knownMethods[r.Method] {
		obs.method = r.Method
	}

	t.inflight.Add(r.Context(), 1)

	if t.tracing && !untracedRoutes[obs.route] {
		ctx := t.prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		ctx, obs.span = t.tracer.Start(ctx, obs.method+" "+obs.route,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", obs.method),
				attribute.String("http.route", obs.route),
				attribute.String("url.scheme", scheme),
				attribute.String("network.protocol.version", strings.TrimPrefix(r.Proto, "HTTP/")),
			))

		obs.gid = goroutineID()
		if obs.gid != 0 {
			t.spans.Store(obs.gid, ctx)
		}
		r = r.WithContext(ctx)
	}

	return r, obs
}

// End records the finished request.
func (q *Request) End(status int) {
	t := q.t
	ctx := context.Background()

	attrs := metric.WithAttributes(
		attribute.String("http.request.method", q.method),
		attribute.String("http.route", q.route),
		attribute.String("http.response.status_class", statusClass(status)),
	)
	t.inflight.Add(ctx, -1)
	t.reqCount.Add(ctx, 1, attrs)
	t.reqDuration.Record(ctx, time.Since(q.start).Seconds(), attrs)

	if q.span != nil {
		if q.gid != 0 {
			t.spans.Delete(q.gid)
		}
		q.span.SetAttributes(attribute.Int("http.response.status_code", status))
		if status >= 500 {
			q.span.SetStatus(codes.Error, "")
		}
		q.span.End()
	}
}

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "unknown"
	}
	return strconv.Itoa(status/100) + "xx"
}

// requestContext returns the context of the request being served by the
// calling goroutine, if any.
func (t *Telemetry) requestContext() (context.Context, bool) {
	if !t.tracing {
		return nil, false
	}
	v, ok := t.spans.Load(goroutineID())
	if !ok {
		return nil, false
	}
	return v.(context.Context), true
}
