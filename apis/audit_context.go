package apis

import (
	"context"

	"github.com/doodlemania2/claviger/core"
)

// auditContext returns the request context decorated with the actor (the
// authenticated record, or "system" for anonymous requests) and the client IP,
// (deliberately not derived from the request context, to keep the previous
// non-cancellable save behavior) so that record changes saved with it are attributed in the audit log (#74).
func auditContext(e *core.RequestEvent) context.Context {
	return core.WithAuditContext(context.Background(), core.NewAuditActorFromRecord(e.Auth), e.RealIP())
}
