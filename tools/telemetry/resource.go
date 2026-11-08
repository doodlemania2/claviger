package telemetry

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
)

const (
	// DefaultServiceName applies only when the deployment sets neither
	// OTEL_SERVICE_NAME nor a service.name in OTEL_RESOURCE_ATTRIBUTES.
	DefaultServiceName = "claviger"

	// DefaultServiceNamespace applies only when OTEL_RESOURCE_ATTRIBUTES
	// carries no service.namespace.
	DefaultServiceNamespace = "claviger"
)

// NewResource builds the resource shared by logs, traces and metrics.
//
// resource.WithFromEnv() reads OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES,
// which is how the deployment supplies service.name, service.namespace and the
// deployment.environment spellings. The defaults only fill gaps, so an
// unconfigured app never lands in the backend as "unknown_service".
func NewResource(ctx context.Context) (*resource.Resource, error) {
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
		fallbacks = append(fallbacks, attribute.String("service.name", DefaultServiceName))
	}
	if !hasNamespace {
		fallbacks = append(fallbacks, attribute.String("service.namespace", DefaultServiceNamespace))
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
