package core

// Fork-local (Claviger #110): self-attributed audit actors.

// NewSelfAuditActor returns the actor for a record that acts on itself
// without an authenticated session (e.g. password reset confirm, where the
// emailed token is the proof of identity). It keeps the record's own
// type (record or superuser) and flags the entry with meta.self=true.
func NewSelfAuditActor(r *Record) AuditActor {
	a := NewAuditActorFromRecord(r)
	if r != nil {
		a.Self = true
	}

	return a
}

// NewPendingSelfAuditActor returns an actor to be resolved to the record
// that the audited change creates (anonymous sign-up of an auth record,
// where the id does not exist before the save).
func NewPendingSelfAuditActor() AuditActor {
	return AuditActor{Type: AuditActorTypeRecord, Self: true}
}

// resolveSelfAuditActor finalizes a self actor on the entry: a pending one is
// bound to the changed record when it is an auth record (otherwise it falls
// back to "system"), and meta.self is set.
func resolveSelfAuditActor(entry *AuditEntry, record *Record) {
	if !entry.Actor.Self {
		return
	}

	if entry.Actor.Id == "" {
		if record == nil || !record.Collection().IsAuth() {
			entry.Actor = AuditActor{Type: AuditActorTypeSystem}
			return
		}
		entry.Actor = NewSelfAuditActor(record)
	}

	entry.Meta["self"] = true
}
