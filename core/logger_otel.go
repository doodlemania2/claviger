package core

// Fork-local addition — not upstream PocketBase.
//
// Exports records written through app.Logger() to an OTLP collector in addition
// to the SQLite `_logs` sink, so telemetry survives the auxiliary database.
// This matters concretely: auxiliary.db corrupted on 2026-06-12 and every
// request log was silently dropped until 2026-08-15, because the batch writer
// in initLogger swallows per-row write errors. A second, independent sink makes
// that failure visible the day it happens.
//
// Wire contract: docs/observability/otlp-onboarding.md in the STFoA-Church
// repo. Load-bearing points:
//
//   - OTLP/HTTP only. The collector sits behind a Cloudflare Tunnel carrying
//     HTTP; gRPC on 4317 never arrives. The deployment must set
//     OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf.
//   - Staging and production post to the same collector with the same token, so
//     resource attributes are the only thing telling them apart.
//   - Gate on the destination set, never on one vendor's connection string
//     (onboarding rule 5). Removing App Insights must not silence OTLP.
//
// Opt-in: with no OTLP endpoint configured this changes nothing.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/pocketbase/pocketbase/tools/hook"
)

const (
	// OTelLoggerShutdownHookId is the id of the OnTerminate hook that flushes
	// and shuts down the OTLP log pipeline.
	OTelLoggerShutdownHookId = "__pbOTelLoggerShutdown__"

	// defaultOTelServiceName applies only when the deployment sets neither
	// OTEL_SERVICE_NAME nor a service.name in OTEL_RESOURCE_ATTRIBUTES.
	defaultOTelServiceName = "stfoa-auth"

	// defaultOTelServiceNamespace groups the parish apps in SigNoz.
	defaultOTelServiceNamespace = "stfoa"

	// otelScopeName identifies the emitting library on every record.
	otelScopeName = "github.com/pocketbase/pocketbase/core"

	// OTelSinkFailureMessage is exported when the local `_logs` sink starts
	// rejecting writes, and OTelSinkRecoveryMessage when it starts accepting
	// them again. Alert on the first and use the second to clear the alert.
	OTelSinkFailureMessage  = "local log sink write failed"
	OTelSinkRecoveryMessage = "local log sink recovered"

	// otelSinkReportInterval is the minimum gap between two exported failure
	// reports. A fully corrupt auxiliary.db fails every record — ~8.6k a day
	// from the health probe alone — so the report is rate limited and carries
	// the accumulated counts rather than one record per dropped row.
	otelSinkReportInterval = 5 * time.Minute

	// otelShutdownTimeout bounds the final OTLP flush on termination.
	//
	// The single-writer handover in entrypoint.sh (#35, #54) waits for this
	// process to exit before the incoming replica may open /pb_data, so an
	// unbounded flush against a slow or unreachable collector (measured: 30s)
	// stretches the handover past the incoming replica's timeout. Losing the
	// last batch of telemetry is the right trade against two writers.
	otelShutdownTimeout = 5 * time.Second
)

// hasOTLPDestination reports whether an OTLP collector is configured.
//
// Checks the generic and logs-specific endpoints and nothing else. It does NOT
// consider APPLICATIONINSIGHTS_CONNECTION_STRING: gating telemetry on one
// vendor's setting is the trap the onboarding spec calls out, where removing
// that setting silently stops every export while the collector sits healthy.
func hasOTLPDestination() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") != ""
}

// otlpMinLevel returns the minimum level exported to OTLP.
//
// Request logging is high volume — a 10s health probe alone is ~8.6k records a
// day — and ingestion is not free. PB_OTEL_MIN_LEVEL trims what crosses the
// wire without touching what PocketBase stores locally, so the dashboard keeps
// full detail either way.
func otlpMinLevel() slog.Level {
	switch strings.ToUpper(strings.TrimSpace(os.Getenv("PB_OTEL_MIN_LEVEL"))) {
	case "DEBUG":
		return slog.LevelDebug
	case "INFO":
		return slog.LevelInfo
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelDebug - 100 // effectively unfiltered
	}
}

// newOTelResource builds the resource every exported record carries.
//
// resource.WithFromEnv() reads OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES,
// which is how the deployment supplies service.name, service.namespace and both
// deployment.environment spellings. The defaults only fill gaps, so an
// unconfigured app never lands in SigNoz as "unknown_service".
func newOTelResource(ctx context.Context) (*resource.Resource, error) {
	base, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, err
	}

	var hasServiceName, hasNamespace bool
	for _, attr := range base.Attributes() {
		switch string(attr.Key) {
		case "service.name":
			// WithFromEnv substitutes a synthetic "unknown_service:<binary>"
			// when nothing is configured; treat that as absent.
			hasServiceName = !strings.HasPrefix(attr.Value.AsString(), "unknown_service")
		case "service.namespace":
			hasNamespace = true
		}
	}

	fallbacks := []attribute.KeyValue{}
	if !hasServiceName {
		fallbacks = append(fallbacks, attribute.String("service.name", defaultOTelServiceName))
	}
	if !hasNamespace {
		fallbacks = append(fallbacks, attribute.String("service.namespace", defaultOTelServiceNamespace))
	}
	if len(fallbacks) == 0 {
		return base, nil
	}

	defaults, err := resource.New(ctx, resource.WithAttributes(fallbacks...))
	if err != nil {
		return nil, err
	}

	// base wins on conflict — a configured value is never overridden.
	return resource.Merge(defaults, base)
}

// initOTelLogger wraps the local slog handler with an OTLP exporter when a
// collector is configured, and registers the flush-on-terminate hook.
//
// Returns the handler unchanged and a nil reporter when no destination is set,
// so the non-exporting path costs nothing and cannot fail.
//
// The returned reporter is how the local sink reports its own failures — see
// otelSinkReporter. It is separate from the returned handler because the caller
// must not route those reports through app.Logger().
func (app *BaseApp) initOTelLogger(local slog.Handler) (slog.Handler, *otelSinkReporter) {
	if !hasOTLPDestination() {
		return local, nil
	}

	ctx := context.Background()

	res, err := newOTelResource(ctx)
	if err != nil {
		// Never let telemetry setup take down the app.
		fmt.Fprintf(os.Stderr, "[otel] resource init failed, OTLP export disabled: %v\n", err)
		return local, nil
	}

	// The exporter reads OTEL_EXPORTER_OTLP_{ENDPOINT,PROTOCOL,HEADERS} itself,
	// which is why repointing the collector needs no code change.
	exporter, err := otlploghttp.New(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[otel] exporter init failed, OTLP export disabled: %v\n", err)
		return local, nil
	}

	provider := otellog.NewLoggerProvider(
		otellog.WithResource(res),
		otellog.WithProcessor(otellog.NewBatchProcessor(exporter)),
	)

	// Route SDK-internal errors to stderr. Container Apps already ships
	// stdout/stderr to Log Analytics, so a broken exporter stays visible there
	// — and this keeps exporter failures OFF app.Logger(), which would
	// otherwise feed them straight back into this handler.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		fmt.Fprintf(os.Stderr, "[otel] %v\n", err)
	}))

	app.OnTerminate().Bind(&hook.Handler[*TerminateEvent]{
		Id: OTelLoggerShutdownHookId,
		Func: func(e *TerminateEvent) error {
			// Flush before the process goes away, otherwise the last batch —
			// often the one explaining the shutdown — is lost.
			ctx, cancel := context.WithTimeout(context.Background(), otelShutdownTimeout)
			defer cancel()

			if err := provider.Shutdown(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "[otel] shutdown: %v\n", err)
			}

			return e.Next()
		},
	})

	remote := otelslog.NewHandler(otelScopeName, otelslog.WithLoggerProvider(provider))

	fanout := &fanoutHandler{
		local:    local,
		remote:   remote,
		minLevel: otlpMinLevel(),
	}

	reporter := &otelSinkReporter{
		remote: remote,
		now:    time.Now,
	}

	return fanout, reporter
}

// fanoutHandler writes every record to the local sink and, when it clears
// minLevel, to the OTLP sink as well.
//
// The sinks are independent on purpose: a failure in either must not suppress
// the other. That independence is the point — the local sink going silent is
// precisely the failure this file exists to surface.
type fanoutHandler struct {
	local    slog.Handler
	remote   slog.Handler
	minLevel slog.Level
}

func (h *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.local.Enabled(ctx, level) || (level >= h.minLevel && h.remote.Enabled(ctx, level))
}

func (h *fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	// Errors are collected rather than short-circuited, so one failing sink
	// still lets the other receive the record.
	var firstErr error

	if h.local.Enabled(ctx, record.Level) {
		if err := h.local.Handle(ctx, record.Clone()); err != nil {
			firstErr = err
		}
	}

	if record.Level >= h.minLevel && h.remote.Enabled(ctx, record.Level) {
		if err := h.remote.Handle(ctx, record.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

// SetLevel forwards a settings-driven level change to the local sink, which is
// what initLogger's OnSettingsReload hook means to adjust. The OTLP minLevel is
// env-driven (PB_OTEL_MIN_LEVEL) and deliberately left alone.
func (h *fanoutHandler) SetLevel(level slog.Level) {
	if l, ok := h.local.(levelSetter); ok {
		l.SetLevel(level)
	}
}

// levelSetter is the part of *logger.BatchHandler the reload hook relies on,
// so that the hook also reaches the local sink through a fanoutHandler.
type levelSetter interface {
	SetLevel(level slog.Level)
}

func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &fanoutHandler{
		local:    h.local.WithAttrs(attrs),
		remote:   h.remote.WithAttrs(attrs),
		minLevel: h.minLevel,
	}
}

func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	return &fanoutHandler{
		local:    h.local.WithGroup(name),
		remote:   h.remote.WithGroup(name),
		minLevel: h.minLevel,
	}
}

// otelSinkReporter surfaces failures of the *local* `_logs` sink on the OTLP
// sink.
//
// This closes the gap that made the 2026-06-12 corruption invisible for two
// months. The batch writer in initLogger reports a rejected row with a stdlib
// log.Println to stderr, which never crosses app.Logger() and so never reaches
// a collector; and with PB_OTEL_MIN_LEVEL=WARN the INFO request logs that would
// otherwise thin out are filtered too, leaving SigNoz unable to tell a dead
// local sink from an idle app. The reporter writes straight to the OTLP handler
// instead: the local sink is the thing that just failed, so routing the report
// through it would either lose the report or amplify the failure.
//
// The PB_OTEL_MIN_LEVEL gate is deliberately bypassed. This record is the
// reason the second sink exists, and a deployment trimming request-log volume
// must still receive it.
//
// A nil *otelSinkReporter is a working no-op, which is what every deployment
// without a configured collector gets.
type otelSinkReporter struct {
	remote slog.Handler

	// now is injectable so the rate limiter is testable without sleeping.
	now func() time.Time

	mu sync.Mutex
	// failing is true while the sink is known to be rejecting writes; it is
	// what turns the next clean batch into a recovery record.
	failing bool
	// lastReportAt is the timestamp of the last exported failure record.
	lastReportAt time.Time
	// counts accumulated since the last exported record.
	failedRecords    int
	attemptedRecords int
	failedBatches    int
}

// report takes the outcome of one batch write of the local sink.
//
// It exports at most one failure record per otelSinkReportInterval, carrying
// the counts accumulated in between so the true magnitude survives the rate
// limit, and exactly one recovery record on the first clean batch after a
// failing run.
func (r *otelSinkReporter) report(failedRecords, attemptedRecords int, cause error) {
	if r == nil {
		return
	}

	r.mu.Lock()

	if failedRecords <= 0 {
		if !r.failing {
			r.mu.Unlock()
			return
		}

		// First clean batch after a failing run — emit once and reset, so an
		// alert on the failure message has a matching all-clear.
		r.failing = false
		r.lastReportAt = time.Time{}
		r.failedRecords, r.attemptedRecords, r.failedBatches = 0, 0, 0
		r.mu.Unlock()

		r.emit(slog.LevelWarn, OTelSinkRecoveryMessage, nil,
			slog.Int("recoveredAfterRecords", attemptedRecords),
		)

		return
	}

	r.failing = true
	r.failedRecords += failedRecords
	r.attemptedRecords += attemptedRecords
	r.failedBatches++

	now := r.now()
	if !r.lastReportAt.IsZero() && now.Sub(r.lastReportAt) < otelSinkReportInterval {
		r.mu.Unlock()
		return
	}

	r.lastReportAt = now
	failed, attempted, batches := r.failedRecords, r.attemptedRecords, r.failedBatches
	r.failedRecords, r.attemptedRecords, r.failedBatches = 0, 0, 0
	r.mu.Unlock()

	r.emit(slog.LevelError, OTelSinkFailureMessage, cause,
		slog.String("sink", "auxiliary.db"),
		slog.Int("failedRecords", failed),
		slog.Int("attemptedRecords", attempted),
		slog.Int("failedBatches", batches),
	)
}

// emit writes one record to the OTLP handler, bypassing both the local sink and
// the PB_OTEL_MIN_LEVEL gate. Failures here go to stderr, never to
// app.Logger(), for the same reason the exporter's own errors do.
func (r *otelSinkReporter) emit(level slog.Level, msg string, cause error, attrs ...slog.Attr) {
	record := slog.NewRecord(r.now(), level, msg, 0)
	record.AddAttrs(attrs...)
	if cause != nil {
		record.AddAttrs(slog.String("error", cause.Error()))
	}

	if err := r.remote.Handle(context.Background(), record); err != nil {
		fmt.Fprintf(os.Stderr, "[otel] could not report local log sink state: %v\n", err)
	}
}
