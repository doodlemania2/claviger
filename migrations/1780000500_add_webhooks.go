package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #77): creates the hidden _webhooks (endpoints) and
// _webhookDeliveries (durable outbox + delivery log) system collections.
//
// Both live in data.db so that a delivery is enqueued in the very same
// transaction as the record change it reports. All API rules are null
// (superusers only); the secret is a hidden field and the management API in
// apis/webhooks.go is the only intended writer.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		endpoints, err := txApp.FindCollectionByNameOrId(core.CollectionNameWebhooks)
		if err != nil {
			endpoints = core.NewBaseCollection(core.CollectionNameWebhooks)
			endpoints.System = true
			endpoints.Fields.Add(
				&core.URLField{Name: "url", System: true, Required: true},
				&core.TextField{Name: "description", System: true, Max: 500},
				&core.TextField{Name: "secret", System: true, Hidden: true},
				&core.TextField{Name: "prevSecret", System: true, Hidden: true},
				&core.DateField{Name: "prevSecretExpires", System: true},
				&core.JSONField{Name: "events", System: true, MaxSize: 64 << 10},
				&core.BoolField{Name: "enabled", System: true},
				&core.NumberField{Name: "consecutiveFailures", System: true, OnlyInt: true},
				&core.DateField{Name: "failingSince", System: true},
				&core.TextField{Name: "disabledReason", System: true, Max: 500},
				&core.AutodateField{Name: "created", System: true, OnCreate: true},
				&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
			)
			if err := txApp.Save(endpoints); err != nil {
				return err
			}
		}

		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameWebhookDeliveries); err == nil {
			return nil
		}

		deliveries := core.NewBaseCollection(core.CollectionNameWebhookDeliveries)
		deliveries.System = true
		deliveries.Fields.Add(
			&core.RelationField{Name: "webhook", System: true, Required: true, CollectionId: endpoints.Id, CascadeDelete: true, MaxSelect: 1},
			&core.TextField{Name: "eventType", System: true, Required: true},
			&core.JSONField{Name: "payload", System: true, MaxSize: 4 << 20},
			// pending | success | dead
			&core.TextField{Name: "status", System: true, Required: true},
			&core.NumberField{Name: "attempts", System: true, OnlyInt: true},
			&core.DateField{Name: "nextAttempt", System: true},
			&core.NumberField{Name: "lastStatusCode", System: true, OnlyInt: true},
			&core.TextField{Name: "lastError", System: true, Max: 2000},
			// one entry per attempt: at, statusCode, durationMs, response, error
			&core.JSONField{Name: "attemptLog", System: true, MaxSize: 256 << 10},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		deliveries.AddIndex("idx_webhookDeliveries_due", false, "status, nextAttempt", "")
		deliveries.AddIndex("idx_webhookDeliveries_webhook", false, "webhook, created", "")
		deliveries.AddIndex("idx_webhookDeliveries_created", false, "created", "")

		return txApp.Save(deliveries)
	}, func(txApp core.App) error {
		return nil
	})
}
