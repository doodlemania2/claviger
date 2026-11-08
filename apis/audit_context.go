package apis

import (
	"context"

	"github.com/doodlemania2/claviger/core"
)

// auditContext returns the request context decorated with the actor (the
// authenticated record, or "system" for anonymous requests) and the client IP,
// (deliberately not derived from the request context, to keep the previous
// non-cancellable save behavior) so that record changes saved with it are attributed in the audit log (#74).
//
// When the request was authenticated with an API key (#95) the actor also
// carries the key id (written to meta.apiKeyId).
func auditContext(e *core.RequestEvent) context.Context {
	actor := core.NewAuditActorFromRecord(e.Auth)
	if key := APIKeyFromRequest(e); key != nil {
		actor.APIKeyId = key.Id
	}

	return core.WithAuditContext(context.Background(), actor, e.RealIP())
}

// auditContextAs is auditContext for flows whose subject is a record that may
// act without an authenticated session (confirm flows authorized by an
// emailed token, OTP, OAuth2, ...). An authenticated request keeps its own
// actor; otherwise the subject record is the actor, flagged meta.self=true.
// (#110)
func auditContextAs(e *core.RequestEvent, subject *core.Record) context.Context {
	if e.Auth != nil || subject == nil {
		return auditContext(e)
	}

	return core.WithAuditContext(context.Background(), core.NewSelfAuditActor(subject), e.RealIP())
}

// auditContextForCreate is auditContext for record creates: an anonymous
// create of an auth record (sign-up) is attributed to the record being
// created once it has an id.
func auditContextForCreate(e *core.RequestEvent, collection *core.Collection) context.Context {
	if e.Auth != nil || collection == nil || !collection.IsAuth() {
		return auditContext(e)
	}

	return core.WithAuditContext(context.Background(), core.NewPendingSelfAuditActor(), e.RealIP())
}
