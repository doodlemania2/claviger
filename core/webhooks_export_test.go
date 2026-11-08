package core

import (
	"context"
	"time"
)

// Test seams for core_test (compiled only for tests).

type WebhookTestHooks struct{ m *webhookManager }

func WebhookTestSeams(app App) *WebhookTestHooks { return &WebhookTestHooks{m: webhookManagerFor(app)} }

func (h *WebhookTestHooks) SetClock(now func() time.Time)    { h.m.now = now }
func (h *WebhookTestHooks) SetRetryDelays(d []time.Duration) { h.m.retryDelays = d }
func (h *WebhookTestHooks) SetFailPolicy(n int, window time.Duration) {
	h.m.allowedFails, h.m.failWindow = n, window
}
func (h *WebhookTestHooks) SetServing(v bool) {
	h.m.mu.Lock()
	h.m.serving = v
	h.m.mu.Unlock()
}
func (h *WebhookTestHooks) SetPollInterval(d time.Duration) { h.m.pollInterval = d }
func (h *WebhookTestHooks) Reload()                         { h.m.reload() }
func (h *WebhookTestHooks) ProcessDue() int                 { return h.m.processDue(testCtx) }
func (h *WebhookTestHooks) Stop()                           { h.m.stop(true) }

var testCtx = context.Background()
