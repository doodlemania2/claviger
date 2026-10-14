package core

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/tools/logger"
)

var errSinkFailed = errors.New("sink failed")

// recordingHandler is a minimal slog.Handler that captures what it receives.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
	enabled bool
	err     error
}

func (h *recordingHandler) Enabled(_ context.Context, _ slog.Level) bool { return h.enabled }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return h.err
}

func (h *recordingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *recordingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

func TestHasOTLPDestination(t *testing.T) {
	scenarios := []struct {
		name     string
		env      map[string]string
		expected bool
	}{
		{"no destination", map[string]string{}, false},
		{"generic endpoint", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example"}, true},
		{"logs endpoint", map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "https://otlp.example/v1/logs"}, true},
		// Regression guard for onboarding rule 5: telemetry must NOT be gated
		// on a single vendor's connection string. If this ever returns true,
		// removing the App Insights setting would silently kill OTLP export.
		{"app insights alone is not a destination", map[string]string{
			"APPLICATIONINSIGHTS_CONNECTION_STRING": "InstrumentationKey=00000000-0000-0000-0000-000000000000",
		}, false},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			for k, v := range s.env {
				t.Setenv(k, v)
			}

			if got := hasOTLPDestination(); got != s.expected {
				t.Fatalf("expected %v, got %v", s.expected, got)
			}
		})
	}
}

func TestOTLPMinLevel(t *testing.T) {
	scenarios := []struct {
		raw      string
		expected slog.Level
	}{
		{"", slog.LevelDebug - 100},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"  warn  ", slog.LevelWarn},
		{"WARNING", slog.LevelWarn},
		{"ERROR", slog.LevelError},
		{"nonsense", slog.LevelDebug - 100},
	}

	for _, s := range scenarios {
		t.Run("level_"+s.raw, func(t *testing.T) {
			t.Setenv("PB_OTEL_MIN_LEVEL", s.raw)

			if got := otlpMinLevel(); got != s.expected {
				t.Fatalf("expected %v, got %v", s.expected, got)
			}
		})
	}
}

// The no-destination path is what every consumer of this fork hits who is not
// deploying to Azure. It must return the original handler untouched, so
// PocketBase logs exactly as upstream does.
func TestInitOTelLoggerWithoutDestinationIsPassthrough(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")

	app := NewBaseApp(BaseAppConfig{DataDir: t.TempDir()})
	local := &recordingHandler{enabled: true}

	got, reporter := app.initOTelLogger(local)
	if got != slog.Handler(local) {
		t.Fatalf("expected the original handler back, got %T", got)
	}
	if reporter != nil {
		t.Fatal("expected a nil sink reporter when no collector is configured")
	}

	// the nil reporter must stay callable — initLogger calls it on every batch
	reporter.report(3, 10, errSinkFailed)
}

func TestFanoutHandlerWritesToBothSinks(t *testing.T) {
	local := &recordingHandler{enabled: true}
	remote := &recordingHandler{enabled: true}

	h := &fanoutHandler{local: local, remote: remote, minLevel: slog.LevelDebug - 100}

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0)
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if local.count() != 1 {
		t.Fatalf("expected 1 local record, got %d", local.count())
	}
	if remote.count() != 1 {
		t.Fatalf("expected 1 remote record, got %d", remote.count())
	}
}

// minLevel trims what crosses the wire without affecting local storage, so the
// dashboard keeps full detail while collector ingestion stays bounded.
func TestFanoutHandlerMinLevelOnlyFiltersRemote(t *testing.T) {
	local := &recordingHandler{enabled: true}
	remote := &recordingHandler{enabled: true}

	h := &fanoutHandler{local: local, remote: remote, minLevel: slog.LevelWarn}

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "below threshold", 0)
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if local.count() != 1 {
		t.Fatalf("expected the local sink to receive the record, got %d", local.count())
	}
	if remote.count() != 0 {
		t.Fatalf("expected the remote sink to be skipped, got %d", remote.count())
	}
}

// The whole point of the second sink: a failing local sink must not stop the
// remote one. This is the exact shape of the 2026-06-12 auxiliary.db failure.
func TestFanoutHandlerLocalFailureStillReachesRemote(t *testing.T) {
	local := &recordingHandler{enabled: true, err: errSinkFailed}
	remote := &recordingHandler{enabled: true}

	h := &fanoutHandler{local: local, remote: remote, minLevel: slog.LevelDebug - 100}

	rec := slog.NewRecord(time.Now(), slog.LevelError, "database disk image is malformed", 0)
	err := h.Handle(context.Background(), rec)

	if err == nil || !strings.Contains(err.Error(), "sink failed") {
		t.Fatalf("expected the local error to surface, got %v", err)
	}
	if remote.count() != 1 {
		t.Fatalf("expected the remote sink to receive the record anyway, got %d", remote.count())
	}
}

func TestFanoutHandlerEnabledIsUnionOfSinks(t *testing.T) {
	h := &fanoutHandler{
		local:    &recordingHandler{enabled: false},
		remote:   &recordingHandler{enabled: true},
		minLevel: slog.LevelDebug - 100,
	}

	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("expected enabled when only the remote sink is enabled")
	}

	h.minLevel = slog.LevelError
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("expected disabled when the remote sink is gated out by minLevel")
	}
}

// newTestSinkReporter builds a reporter over a recording handler with a clock
// the test drives by hand.
func newTestSinkReporter(remote slog.Handler, clock *time.Time) *otelSinkReporter {
	return &otelSinkReporter{
		remote: remote,
		now:    func() time.Time { return *clock },
	}
}

func attrsOf(r slog.Record) map[string]any {
	out := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		out[a.Key] = a.Value.Any()
		return true
	})
	return out
}

// The whole point of the reporter: a rejected local write has to reach the
// collector. Before this existed the only trace was a stderr log.Println.
func TestSinkReporterExportsFailure(t *testing.T) {
	remote := &recordingHandler{enabled: true}
	clock := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)
	r := newTestSinkReporter(remote, &clock)

	r.report(2, 5, errSinkFailed)

	if remote.count() != 1 {
		t.Fatalf("expected 1 exported record, got %d", remote.count())
	}

	rec := remote.records[0]
	if rec.Message != OTelSinkFailureMessage {
		t.Fatalf("expected message %q, got %q", OTelSinkFailureMessage, rec.Message)
	}
	if rec.Level != slog.LevelError {
		t.Fatalf("expected ERROR, got %v", rec.Level)
	}

	attrs := attrsOf(rec)
	if attrs["failedRecords"] != int64(2) || attrs["attemptedRecords"] != int64(5) {
		t.Fatalf("unexpected counts: %v", attrs)
	}
	if attrs["error"] != errSinkFailed.Error() {
		t.Fatalf("expected the cause on the record, got %v", attrs["error"])
	}
}

// A clean batch must not export anything, or a healthy app would pay for a
// record every 3 seconds.
func TestSinkReporterSilentWhenHealthy(t *testing.T) {
	remote := &recordingHandler{enabled: true}
	clock := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)
	r := newTestSinkReporter(remote, &clock)

	for i := 0; i < 5; i++ {
		r.report(0, 10, nil)
	}

	if remote.count() != 0 {
		t.Fatalf("expected no exported records, got %d", remote.count())
	}
}

// A fully corrupt auxiliary.db fails every record — ~8.6k a day from the health
// probe alone. The rate limit is what keeps that from becoming the ingestion
// bill, and the accumulated counts are what keep it honest.
func TestSinkReporterRateLimitsAndAccumulates(t *testing.T) {
	remote := &recordingHandler{enabled: true}
	clock := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)
	r := newTestSinkReporter(remote, &clock)

	// first failing batch exports immediately
	r.report(1, 1, errSinkFailed)

	// everything inside the interval is folded into the next record
	for i := 0; i < 100; i++ {
		clock = clock.Add(time.Second)
		r.report(2, 4, errSinkFailed)
	}

	if remote.count() != 1 {
		t.Fatalf("expected the interval to suppress the follow-ups, got %d records", remote.count())
	}

	clock = clock.Add(otelSinkReportInterval)
	r.report(3, 6, errSinkFailed)

	if remote.count() != 2 {
		t.Fatalf("expected a second record once the interval elapsed, got %d", remote.count())
	}

	attrs := attrsOf(remote.records[1])
	if got := attrs["failedRecords"]; got != int64(203) {
		t.Fatalf("expected the suppressed failures to be carried (203), got %v", got)
	}
	if got := attrs["attemptedRecords"]; got != int64(406) {
		t.Fatalf("expected attemptedRecords 406, got %v", got)
	}
	if got := attrs["failedBatches"]; got != int64(101) {
		t.Fatalf("expected failedBatches 101, got %v", got)
	}
}

// An alert on the failure message needs an all-clear, and it must fire once —
// not on every healthy batch that follows.
func TestSinkReporterExportsRecoveryOnce(t *testing.T) {
	remote := &recordingHandler{enabled: true}
	clock := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)
	r := newTestSinkReporter(remote, &clock)

	r.report(4, 8, errSinkFailed)
	r.report(0, 8, nil)
	r.report(0, 8, nil)
	r.report(0, 8, nil)

	if remote.count() != 2 {
		t.Fatalf("expected exactly one failure and one recovery record, got %d", remote.count())
	}

	rec := remote.records[1]
	if rec.Message != OTelSinkRecoveryMessage {
		t.Fatalf("expected message %q, got %q", OTelSinkRecoveryMessage, rec.Message)
	}
	if rec.Level != slog.LevelWarn {
		t.Fatalf("expected WARN, got %v", rec.Level)
	}

	// a fresh failure after recovery reports immediately rather than waiting
	// out the interval left over from the previous run
	r.report(1, 8, errSinkFailed)
	if remote.count() != 3 {
		t.Fatalf("expected a new failure record after recovery, got %d", remote.count())
	}
}

// PB_OTEL_MIN_LEVEL exists to trim request-log volume. It must not trim the one
// record that says the local sink is dead.
func TestSinkReporterIgnoresMinLevelGate(t *testing.T) {
	t.Setenv("PB_OTEL_MIN_LEVEL", "ERROR")

	remote := &recordingHandler{enabled: true}
	clock := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)
	r := newTestSinkReporter(remote, &clock)

	// the recovery record is WARN, below the configured ERROR gate
	r.report(1, 1, errSinkFailed)
	r.report(0, 1, nil)

	if remote.count() != 2 {
		t.Fatalf("expected both records to bypass the min-level gate, got %d", remote.count())
	}
	if remote.records[1].Level != slog.LevelWarn {
		t.Fatalf("expected the WARN recovery record to survive, got %v", remote.records[1].Level)
	}
}

// The reporter is called from the batch-flush goroutine while the app is
// running; it must not be the thing that introduces a race.
func TestSinkReporterConcurrentReports(t *testing.T) {
	remote := &recordingHandler{enabled: true}
	clock := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)
	r := newTestSinkReporter(remote, &clock)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.report(1, 2, errSinkFailed)
		}()
	}
	wg.Wait()

	if remote.count() != 1 {
		t.Fatalf("expected the rate limit to hold under concurrency, got %d records", remote.count())
	}
}

// Regression for #53: with OTLP export on, app.Logger() is a fanoutHandler, and
// the settings-reload hook must still reach the local BatchHandler's level.
func TestSettingsReloadUpdatesLocalLevelWithOTLP(t *testing.T) {
	// nothing listens here; the exporter only dials when a batch is flushed
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")

	app := NewBaseApp(BaseAppConfig{DataDir: t.TempDir()})
	defer app.ClearBootstrap()

	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}

	if _, ok := app.Logger().Handler().(*fanoutHandler); !ok {
		t.Fatalf("expected the OTLP fanout handler, got %T", app.Logger().Handler())
	}

	local := app.Logger().Handler().(*fanoutHandler).local
	ctx := context.Background()

	app.Settings().Logs.MinLevel = int(slog.LevelError)
	if err := app.Save(app.Settings()); err != nil {
		t.Fatalf("failed to save settings: %v", err)
	}

	if local.Enabled(ctx, slog.LevelWarn) {
		t.Fatal("expected WARN to be disabled on the local sink after raising the min level to ERROR")
	}
	if !local.Enabled(ctx, slog.LevelError) {
		t.Fatal("expected ERROR to stay enabled on the local sink")
	}
}

func TestFanoutHandlerSetLevelForwardsToLocal(t *testing.T) {
	local := logger.NewBatchHandler(logger.BatchOptions{
		Level:     slog.LevelInfo,
		WriteFunc: func(context.Context, []*logger.Log) error { return nil },
	})
	remote := &recordingHandler{enabled: true}
	h := &fanoutHandler{local: local, remote: remote, minLevel: slog.LevelWarn}

	h.SetLevel(slog.LevelError)

	if local.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("expected the local level to be raised to ERROR")
	}
	if h.minLevel != slog.LevelWarn {
		t.Fatalf("expected the OTLP minLevel to be untouched, got %v", h.minLevel)
	}

	// a local sink without SetLevel must not panic
	(&fanoutHandler{local: &recordingHandler{}, remote: remote}).SetLevel(slog.LevelError)
}
