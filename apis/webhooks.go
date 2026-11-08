package apis

import (
	"errors"
	"net/http"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #77): webhook management API (superusers only).
//
// Listing, reading, updating and deleting endpoints and reading the delivery
// log use the regular records API on the hidden _webhooks and
// _webhookDeliveries collections (superuser-only rules; the secret is a
// hidden field and is never returned). The routes below cover what the
// records API cannot: creating an endpoint with a server-generated secret
// that is shown exactly once, rotating it, sending a test event and
// redelivering.
func bindWebhooksApi(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	sub := rg.Group("/webhooks")
	sub.POST("", webhookCreate).Bind(RequireSuperuserAuth())
	sub.POST("/{id}/rotate-secret", webhookRotateSecret).Bind(RequireSuperuserAuth())
	sub.POST("/{id}/test", webhookTest).Bind(RequireSuperuserAuth())
	rg.POST("/webhook-deliveries/{id}/redeliver", webhookRedeliver).Bind(RequireSuperuserAuth())

	// Keep the records API from bypassing the server-generated secret or
	// forging outbox rows.
	app.OnRecordCreateRequest(core.CollectionNameWebhooks).Bind(&hook.Handler[*core.RecordRequestEvent]{
		Id: "__pbWebhookNoGenericCreate__",
		Func: func(e *core.RecordRequestEvent) error {
			return e.BadRequestError("Use POST /api/webhooks to create a webhook endpoint.", nil)
		},
	})
	app.OnRecordUpdateRequest(core.CollectionNameWebhooks).Bind(&hook.Handler[*core.RecordRequestEvent]{
		Id: "__pbWebhookKeepSecret__",
		Func: func(e *core.RecordRequestEvent) error {
			orig := e.Record.Original()
			for _, f := range []string{"secret", "prevSecret", "prevSecretExpires"} {
				e.Record.Set(f, orig.GetRaw(f))
			}
			// re-enabling clears the failure streak
			if e.Record.GetBool("enabled") && !orig.GetBool("enabled") {
				e.Record.Set("consecutiveFailures", 0)
				e.Record.Set("failingSince", "")
				e.Record.Set("disabledReason", "")
			}
			return e.Next()
		},
	})
	for _, bindFn := range []func(...string) *hook.TaggedHook[*core.RecordRequestEvent]{
		app.OnRecordCreateRequest, app.OnRecordUpdateRequest,
	} {
		bindFn(core.CollectionNameWebhookDeliveries).Bind(&hook.Handler[*core.RecordRequestEvent]{
			Id: "__pbWebhookDeliveriesReadOnly__",
			Func: func(e *core.RecordRequestEvent) error {
				return e.ForbiddenError("Webhook deliveries are read-only.", nil)
			},
		})
	}
}

type webhookCreateForm struct {
	URL         string   `json:"url" form:"url"`
	Description string   `json:"description" form:"description"`
	Events      []string `json:"events" form:"events"`
	Enabled     *bool    `json:"enabled" form:"enabled"`
}

func webhookCreate(e *core.RequestEvent) error {
	form := &webhookCreateForm{}
	if err := e.BindBody(form); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	collection, err := e.App.FindCachedCollectionByNameOrId(core.CollectionNameWebhooks)
	if err != nil {
		return e.InternalServerError("Webhooks are not available.", err)
	}

	secret := core.GenerateWebhookSecret()

	record := core.NewRecord(collection)
	record.Set("url", form.URL)
	record.Set("description", form.Description)
	record.Set("events", form.Events)
	record.Set("enabled", form.Enabled == nil || *form.Enabled)
	if err := core.SetWebhookSecret(e.App, record, secret); err != nil {
		return e.InternalServerError("Failed to store the webhook secret.", err)
	}

	if err := e.App.SaveWithContext(auditContext(e), record); err != nil {
		var vErr validation.Errors
		if errors.As(err, &vErr) {
			return e.BadRequestError("Failed to create the webhook endpoint.", vErr)
		}
		return e.InternalServerError("Failed to create the webhook endpoint.", err)
	}

	// the only time the secret is ever returned
	return e.JSON(http.StatusOK, map[string]any{
		"endpoint": record.PublicExport(),
		"secret":   secret,
	})
}

func webhookRotateSecret(e *core.RequestEvent) error {
	record, err := e.App.FindRecordById(core.CollectionNameWebhooks, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("", err)
	}

	secret, err := core.RotateWebhookSecret(e.App, record)
	if err != nil {
		return e.InternalServerError("Failed to rotate the webhook secret.", err)
	}

	if err := e.App.SaveNoValidateWithContext(auditContext(e), record); err != nil {
		return e.InternalServerError("Failed to rotate the webhook secret.", err)
	}

	return e.JSON(http.StatusOK, map[string]any{
		"endpoint": record.PublicExport(),
		"secret":   secret,
	})
}

func webhookTest(e *core.RequestEvent) error {
	record, err := e.App.FindRecordById(core.CollectionNameWebhooks, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("", err)
	}

	delivery, err := core.SendWebhookTest(e.App, record)
	if err != nil {
		return e.InternalServerError("Failed to send the test event.", err)
	}

	return e.JSON(http.StatusOK, delivery.PublicExport())
}

func webhookRedeliver(e *core.RequestEvent) error {
	delivery, err := e.App.FindRecordById(core.CollectionNameWebhookDeliveries, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("", err)
	}

	if err := core.RedeliverWebhook(e.App, delivery); err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	return e.JSON(http.StatusOK, delivery.PublicExport())
}
