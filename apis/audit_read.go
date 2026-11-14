package apis

import (
	"context"
	"fmt"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #86, 86-3): read audit for flagged collections
// (Settings.Audit.ReadCollections, see core/audit_read.go).
//
// Covered read paths:
//   - GET /api/collections/{c}/records/{id}   (OnRecordViewRequest)
//   - GET /api/collections/{c}/records        (OnRecordsListRequest)
//   - records of a flagged collection returned through ?expand= on any
//     response (expandFetch)
//   - realtime create/update events delivered to a subscriber
//
// Not covered: see docs/audit.md.

const auditReadHookPriority = -99000 // run before user hooks, so a hook that skips e.Next() cannot dodge it

// bindAuditReadHooks registers the view and list request hooks. They are no-ops
// (one length check) while the flagged list is empty.
func bindAuditReadHooks(app core.App) {
	app.OnRecordViewRequest().Bind(&hook.Handler[*core.RecordRequestEvent]{
		Id:       "__pbAuditReadView__",
		Priority: auditReadHookPriority,
		Func: func(e *core.RecordRequestEvent) error {
			if !core.AuditReadsCollection(e.App, e.Collection.Name) {
				return e.Next()
			}

			if err := core.AuditRecordRead(e.App, auditContext(e.RequestEvent), e.Collection, []*core.Record{e.Record}, anonymousReadMeta(e.RequestEvent)); err != nil {
				return e.InternalServerError("Failed to write the read audit entry.", err)
			}

			return e.Next()
		},
	})

	app.OnRecordsListRequest().Bind(&hook.Handler[*core.RecordsListRequestEvent]{
		Id:       "__pbAuditReadList__",
		Priority: auditReadHookPriority,
		Func: func(e *core.RecordsListRequestEvent) error {
			if !core.AuditReadsCollection(e.App, e.Collection.Name) {
				return e.Next()
			}

			if err := core.AuditRecordRead(e.App, auditContext(e.RequestEvent), e.Collection, e.Records, anonymousReadMeta(e.RequestEvent)); err != nil {
				return e.InternalServerError("Failed to write the read audit entry.", err)
			}

			return e.Next()
		},
	})
}

// auditExpandRead writes the read entry for records of a flagged collection
// that are being returned as an expanded relation. The request IP is not
// available here (the expand fetch only has the request info).
func auditExpandRead(app core.App, info *core.RequestInfo, collection *core.Collection, records []*core.Record) error {
	if len(records) == 0 || !core.AuditReadsCollection(app, collection.Name) {
		return nil
	}

	var auth *core.Record
	if info != nil {
		auth = info.Auth
	}

	ctx := core.WithAuditContext(context.Background(), core.NewAuditActorFromRecord(auth), "")

	meta := map[string]any{"via": "expand"}
	if auth == nil {
		meta["anonymous"] = true
	}

	if err := core.AuditRecordRead(app, ctx, collection, records, meta); err != nil {
		return fmt.Errorf("failed to write the read audit entry: %w", err)
	}

	return nil
}

// auditRealtimeRead writes the read entry for a realtime event that is about
// to be delivered to the client.
func auditRealtimeRead(app core.App, action string, record *core.Record, clientAuth *core.Record, ip string) error {
	ctx := core.WithAuditContext(context.Background(), core.NewAuditActorFromRecord(clientAuth), ip)

	meta := map[string]any{"via": "realtime", "event": action}
	if clientAuth == nil {
		meta["anonymous"] = true
	}

	return core.AuditRecordRead(app, ctx, record.Collection(), []*core.Record{record}, meta)
}

// anonymousReadMeta marks a view or list read made without an auth record,
// like the expand and realtime entries do. The actor type stays "system".
func anonymousReadMeta(e *core.RequestEvent) map[string]any {
	if e.Auth != nil {
		return nil
	}

	return map[string]any{"anonymous": true}
}
