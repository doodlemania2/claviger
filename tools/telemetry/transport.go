package telemetry

import (
	"net/http"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// InstallOutbound makes http.DefaultClient (used by the OAuth2/JWKS fetchers,
// S3 and the JS hooks) carry the W3C traceparent of the request being served.
// It never replaces http.DefaultTransport, which third-party code may
// type-assert or clone, and it leaves a client with its own transport alone.
func (t *Telemetry) InstallOutbound() {
	if t.tracing && http.DefaultClient.Transport == nil {
		http.DefaultClient.Transport = &propagatingTransport{t: t, base: http.DefaultTransport}
	}
}

// UninstallOutbound reverts [Telemetry.InstallOutbound].
func (t *Telemetry) UninstallOutbound() {
	if p, ok := http.DefaultClient.Transport.(*propagatingTransport); ok && p.t == t {
		http.DefaultClient.Transport = nil
	}
}

type propagatingTransport struct {
	t    *Telemetry
	base http.RoundTripper
}

func (p *propagatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if !trace.SpanContextFromContext(ctx).IsValid() {
		if rc, ok := p.t.requestContext(); ok {
			ctx = rc
		}
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		req = req.Clone(req.Context()) // RoundTrippers must not mutate the caller's request
		p.t.prop.Inject(trace.ContextWithSpanContext(req.Context(), sc), propagation.HeaderCarrier(req.Header))
	}
	return p.base.RoundTrip(req)
}
