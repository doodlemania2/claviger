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
