package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ScopeName identifies the instrumentation library on every span and metric.
const ScopeName = "github.com/doodlemania2/claviger/tools/telemetry"

var active atomic.Pointer[Telemetry]

// Active returns the running telemetry, or nil when it is not configured. This
// nil check is the whole cost of the feature for a deployment that opts out.
func Active() *Telemetry { return active.Load() }

// SetActive installs (or, with nil, removes) the process-wide instance.
func SetActive(t *Telemetry) { active.Store(t) }

// Options adjusts [Setup]. The zero value means "configure from the environment".
type Options struct {
	// SpanExporter, if set, replaces the OTLP trace exporter and turns tracing
	// on regardless of the environment. It is meant for tests.
	SpanExporter sdktrace.SpanExporter

	// MetricReaders are added to the readers derived from the environment.
	// They are meant for tests.
	MetricReaders []sdkmetric.Reader

	// Prometheus forces the Prometheus registry on even when
	// CLAVIGER_METRICS_TOKEN is empty. It is meant for tests.
	Prometheus bool
}

// Telemetry owns the providers and instruments.
type Telemetry struct {
	tracing bool
	tracer  trace.Tracer
	tp      *sdktrace.TracerProvider
	mp      *sdkmetric.MeterProvider
	prom    http.Handler
	prop    propagation.TextMapPropagator

	reqCount          metric.Int64Counter
	reqDuration       metric.Float64Histogram
	inflight          metric.Int64UpDownCounter
	dbDuration        metric.Float64Histogram
	authAttempts      metric.Int64Counter
	webhookDeliveries metric.Int64Counter

	realtime atomic.Pointer[func() int64]

	spans sync.Map // goroutine id -> context.Context holding the request span
}

var durationBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Setup builds the providers described by the environment and opts.
func Setup(ctx context.Context, opts Options) (*Telemetry, error) {
	res, err := NewResource(ctx)
	if err != nil {
		return nil, fmt.Errorf("resource: %w", err)
	}

	t := &Telemetry{
		prop:   propagation.TraceContext{},
		tracer: noop.NewTracerProvider().Tracer(ScopeName),
	}

	// Route SDK-internal errors to stderr, never to app.Logger(), which would
	// feed exporter failures straight back into the OTLP log pipeline.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		fmt.Fprintf(os.Stderr, "[otel] %v\n", err)
	}))

	// ---- traces ----
	if opts.SpanExporter != nil || TracesEnabled() {
		var tpOpts = []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
		// The sampler is deliberately not set: the SDK honours
		// OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG and defaults to
		// parentbased_always_on.
		if opts.SpanExporter != nil {
			tpOpts = append(tpOpts, sdktrace.WithSyncer(opts.SpanExporter))
		} else {
			exp, err := otlptracehttp.New(ctx)
			if err != nil {
				return nil, fmt.Errorf("trace exporter: %w", err)
			}
			tpOpts = append(tpOpts, sdktrace.WithBatcher(exp))
		}
		t.tp = sdktrace.NewTracerProvider(tpOpts...)
		t.tracer = t.tp.Tracer(ScopeName)
		t.tracing = true
	}

	// ---- metrics ----
	readers := append([]sdkmetric.Reader(nil), opts.MetricReaders...)

	if OTLPMetricsEnabled() {
		exp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("metric exporter: %w", err)
		}
		// interval: OTEL_METRIC_EXPORT_INTERVAL, read by the SDK.
		readers = append(readers, sdkmetric.NewPeriodicReader(exp))
	}

	if MetricsToken() != "" || opts.Prometheus {
		reg := promclient.NewRegistry()
		reader, err := otelprom.New(otelprom.WithRegisterer(reg))
		if err != nil {
			return nil, fmt.Errorf("prometheus exporter: %w", err)
		}
		readers = append(readers, reader)
		t.prom = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	}

	mpOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	for _, r := range readers {
		mpOpts = append(mpOpts, sdkmetric.WithReader(r))
	}
	t.mp = sdkmetric.NewMeterProvider(mpOpts...)

	if err := t.initInstruments(); err != nil {
		_ = t.Shutdown(ctx)
		return nil, err
	}

	return t, nil
}

func (t *Telemetry) initInstruments() error {
	m := t.mp.Meter(ScopeName)
	var err error
	var errs []error
	keep := func(e error) {
		if e != nil {
			errs = append(errs, e)
		}
	}

	t.reqCount, err = m.Int64Counter("claviger.http.server.requests",
		metric.WithUnit("{request}"),
		metric.WithDescription("HTTP requests handled, by route template, method and status class."))
	keep(err)

	t.reqDuration, err = m.Float64Histogram("http.server.request.duration",
		metric.WithUnit("s"),
		metric.WithDescription("HTTP server request duration, by route template, method and status class."),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	keep(err)

	t.inflight, err = m.Int64UpDownCounter("http.server.active_requests",
		metric.WithUnit("{request}"),
		metric.WithDescription("HTTP requests currently being handled."))
	keep(err)

	t.dbDuration, err = m.Float64Histogram("claviger.db.query.duration",
		metric.WithUnit("s"),
		metric.WithDescription("SQLite statement duration, by database file and operation."),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	keep(err)

	t.authAttempts, err = m.Int64Counter("claviger.auth.attempts",
		metric.WithUnit("{attempt}"),
		metric.WithDescription("Record auth attempts, by method and outcome."))
	keep(err)

	t.webhookDeliveries, err = m.Int64Counter("claviger.webhook.deliveries",
		metric.WithUnit("{delivery}"),
		metric.WithDescription("Webhook delivery attempts, by outcome."))
	keep(err)

	// ---- realtime ----
	subs, err := m.Int64ObservableGauge("claviger.realtime.subscribers",
		metric.WithUnit("{client}"),
		metric.WithDescription("Connected realtime (SSE) clients."))
	keep(err)

	// ---- go runtime / process ----
	goroutines, err := m.Int64ObservableGauge("go.goroutines", metric.WithUnit("{goroutine}"),
		metric.WithDescription("Number of goroutines."))
	keep(err)
	heapAlloc, err := m.Int64ObservableGauge("go.memory.heap_alloc", metric.WithUnit("By"),
		metric.WithDescription("Bytes of allocated heap objects."))
	keep(err)
	sysBytes, err := m.Int64ObservableGauge("go.memory.sys", metric.WithUnit("By"),
		metric.WithDescription("Bytes of memory obtained from the OS."))
	keep(err)
	gcCycles, err := m.Int64ObservableCounter("go.gc.cycles", metric.WithUnit("{cycle}"),
		metric.WithDescription("Completed GC cycles."))
	keep(err)
	gcPause, err := m.Float64ObservableCounter("go.gc.pause.total", metric.WithUnit("s"),
		metric.WithDescription("Cumulative GC stop-the-world pause time."))
	keep(err)
	uptime, err := m.Float64ObservableGauge("process.uptime", metric.WithUnit("s"),
		metric.WithDescription("Seconds since the process started."))
	keep(err)

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("instruments: %w", err)
	}

	started := time.Now()
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		o.ObserveInt64(goroutines, int64(runtime.NumGoroutine()))
		o.ObserveInt64(heapAlloc, int64(ms.HeapAlloc))
		o.ObserveInt64(sysBytes, int64(ms.Sys))
		o.ObserveInt64(gcCycles, int64(ms.NumGC))
		o.ObserveFloat64(gcPause, float64(ms.PauseTotalNs)/1e9)
		o.ObserveFloat64(uptime, time.Since(started).Seconds())
		if fn := t.realtime.Load(); fn != nil {
			o.ObserveInt64(subs, (*fn)())
		}
		return nil
	}, goroutines, heapAlloc, sysBytes, gcCycles, gcPause, uptime, subs)

	return err
}

// SetRealtimeSource sets the function reporting the connected realtime clients.
func (t *Telemetry) SetRealtimeSource(fn func() int64) { t.realtime.Store(&fn) }

// TracingEnabled reports whether spans are recorded.
func (t *Telemetry) TracingEnabled() bool { return t.tracing }

// PrometheusHandler returns the scrape handler, or nil when the Prometheus
// registry is not enabled.
func (t *Telemetry) PrometheusHandler() http.Handler { return t.prom }

// RecordAuth counts one auth attempt. method is password, oauth2, otp or
// webauthn; a nil err is a success.
func (t *Telemetry) RecordAuth(method string, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	t.authAttempts.Add(context.Background(), 1, metric.WithAttributes(
		attrString("auth.method", method),
		attrString("auth.outcome", outcome),
	))
}

// Shutdown flushes and stops the providers.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	if t.tp != nil {
		errs = append(errs, t.tp.Shutdown(ctx))
	}
	if t.mp != nil {
		errs = append(errs, t.mp.Shutdown(ctx))
	}
	return errors.Join(errs...)
}
