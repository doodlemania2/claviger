package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/metric"
)

// RecordCaptchaVerification counts one captcha verification decision.
//
// outcome is one of: success, failed (provider rejected the token), missing
// (no token sent), outage_open (provider unreachable, request let through),
// outage_closed (provider unreachable, request refused).
//
// Exported as claviger_captcha_verifications_total{provider,outcome} by the
// Prometheus handler.
func (t *Telemetry) RecordCaptchaVerification(provider, outcome string) {
	if t == nil || t.captchaVerifications == nil {
		return
	}
	t.captchaVerifications.Add(context.Background(), 1, metric.WithAttributes(
		attrString("provider", provider),
		attrString("outcome", outcome),
	))
}
