// Package telemetry is a fork-local addition (not upstream PocketBase): OTLP
// traces and metrics plus an optional Prometheus scrape endpoint, next to the
// OTLP log export in core/logger_otel.go.
//
// Opt-in. With none of the variables below set, Setup is never called, no
// provider, exporter or goroutine exists, and the only cost left in the request
// path is a nil check on [Active].
//
//   - OTEL_EXPORTER_OTLP_ENDPOINT (or the _TRACES_/_METRICS_ specific one)
//     turns on OTLP/HTTP export. Headers, protocol, timeout, resource
//     (OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES) and the trace sampler
//     (OTEL_TRACES_SAMPLER[_ARG], default parentbased_always_on) are the
//     standard OTEL_* variables, read by the SDK and exporters themselves.
//   - OTEL_TRACES_EXPORTER=none / OTEL_METRICS_EXPORTER=none switch one signal
//     off while leaving the others (and the log export) alone.
//   - OTEL_SDK_DISABLED=true switches traces and metrics off.
//   - CLAVIGER_METRICS_TOKEN enables the Prometheus endpoint GET /api/metrics.
//
// Like the log exporter, nothing here is gated on a vendor connection string.
package telemetry

import (
	"os"
	"strings"
)

// MetricsTokenEnv is the variable holding the static bearer token of the
// Prometheus endpoint. The endpoint does not exist while it is empty.
const MetricsTokenEnv = "CLAVIGER_METRICS_TOKEN"

func envSet(name string) bool { return strings.TrimSpace(os.Getenv(name)) != "" }

func sdkDisabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true")
}

func exporterNone(name string) bool {
	for _, v := range strings.Split(os.Getenv(name), ",") {
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			return true
		}
	}
	return false
}

// MetricsToken returns the configured Prometheus endpoint token ("" = off).
func MetricsToken() string { return os.Getenv(MetricsTokenEnv) }

// TracesEnabled reports whether OTLP trace export is configured.
func TracesEnabled() bool {
	return !sdkDisabled() &&
		(envSet("OTEL_EXPORTER_OTLP_ENDPOINT") || envSet("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")) &&
		!exporterNone("OTEL_TRACES_EXPORTER")
}

// OTLPMetricsEnabled reports whether OTLP metric export is configured.
func OTLPMetricsEnabled() bool {
	return !sdkDisabled() &&
		(envSet("OTEL_EXPORTER_OTLP_ENDPOINT") || envSet("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")) &&
		!exporterNone("OTEL_METRICS_EXPORTER")
}

// Configured reports whether anything at all asks for traces or metrics.
// When false the app must not touch the package further.
func Configured() bool {
	return TracesEnabled() || OTLPMetricsEnabled() || MetricsToken() != ""
}
