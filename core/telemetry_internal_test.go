package core

import (
	"runtime"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/tools/telemetry"
)

// With nothing configured, initTelemetry must return before doing anything: no
// goroutine, no global instance, no hook bound.
func TestInitTelemetryDisabledByDefault(t *testing.T) {
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", telemetry.MetricsTokenEnv,
	} {
		t.Setenv(k, "")
	}

	app := NewBaseApp(BaseAppConfig{DataDir: t.TempDir()})

	hooks := func() int {
		return app.OnTerminate().Length() + app.OnRecordAuthWithPasswordRequest().Length() +
			app.OnRecordAuthWithOAuth2Request().Length() + app.OnRecordAuthWithOTPRequest().Length() +
			app.OnRecordAuthWithWebAuthnRequest().Length()
	}

	before, hooksBefore := runtime.NumGoroutine(), hooks()
	app.initTelemetry()
	time.Sleep(20 * time.Millisecond)

	if got := runtime.NumGoroutine(); got != before {
		t.Fatalf("goroutines %d -> %d", before, got)
	}
	if telemetry.Active() != nil {
		t.Fatal("telemetry active without configuration")
	}
	if n := hooks(); n != hooksBefore {
		t.Fatalf("%d hook handlers bound while disabled", n-hooksBefore)
	}
}

// With a collector configured, traces and metrics start and are flushed on
// terminate; with =none for both, nothing starts even though the endpoint is set.
func TestInitTelemetryFromEnv(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")

	t.Run("none for both signals", func(t *testing.T) {
		t.Setenv("OTEL_TRACES_EXPORTER", "none")
		t.Setenv("OTEL_METRICS_EXPORTER", "none")
		t.Setenv(telemetry.MetricsTokenEnv, "")
		if telemetry.Configured() {
			t.Fatal("Configured() must be false when both signals are none")
		}
	})

	t.Run("endpoint enables both", func(t *testing.T) {
		t.Setenv("OTEL_TRACES_EXPORTER", "")
		t.Setenv("OTEL_METRICS_EXPORTER", "")
		t.Setenv(telemetry.MetricsTokenEnv, "")
		if !telemetry.TracesEnabled() || !telemetry.OTLPMetricsEnabled() {
			t.Fatal("endpoint must enable traces and metrics")
		}

		app := NewBaseApp(BaseAppConfig{DataDir: t.TempDir()})
		app.initTelemetry()
		if telemetry.Active() == nil || !telemetry.Active().TracingEnabled() {
			t.Fatal("telemetry not started")
		}
		if telemetry.Active().PrometheusHandler() != nil {
			t.Fatal("prometheus registry without a token")
		}

		if err := app.OnTerminate().Trigger(&TerminateEvent{App: app}); err != nil {
			t.Fatal(err)
		}
		if telemetry.Active() != nil {
			t.Fatal("terminate must stop the instance it created")
		}
	})
}
