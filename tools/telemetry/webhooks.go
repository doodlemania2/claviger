package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/metric"
)

// RecordWebhookDelivery counts one webhook delivery attempt. outcome is one of
// success, failure (will retry) or dead (retries exhausted).
//
// Exported as claviger_webhook_deliveries_total{outcome} by the Prometheus handler.
func (t *Telemetry) RecordWebhookDelivery(outcome string) {
	if t == nil || t.webhookDeliveries == nil {
		return
	}
	t.webhookDeliveries.Add(context.Background(), 1, metric.WithAttributes(attrString("outcome", outcome)))
}
