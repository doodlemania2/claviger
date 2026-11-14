package core

import (
	"context"
	"slices"
)

// Fork-local (Claviger #86, 86-3): read audit for flagged collections.
//
// The audit log records writes by default. Settings.Audit.ReadCollections
// flags collections whose reads must be recorded too. The records API (view,
// list, expand) and realtime delivery call the helpers below; another feature
// can call AuditRecordRead directly with extra meta (for example a grant id).
//
// With no flagged collection the cost is one length check per request. For an
// unflagged collection it is one scan of the (short) flagged list and no query.

const (
	// AuditActionRead is the action of a read entry.
	AuditActionRead = "read"

	// AuditReadMaxRecordIds caps meta.recordIds of one read entry. The real
	// number of records is always in meta.count and meta.truncated says
	// whether ids were dropped.
	AuditReadMaxRecordIds = 100
)

// AuditReadsCollection reports whether reads of the named collection are
// flagged for auditing (Settings.Audit.ReadCollections).
func AuditReadsCollection(app App, collectionName string) bool {
	flagged := app.Settings().Audit.ReadCollections
	if len(flagged) == 0 {
		return false
	}

	return slices.Contains(flagged, collectionName)
}

// AuditRecordRead writes ONE "read" audit entry for the records that were
// returned to a reader. It does not check the flagged list: the caller decides
// (see AuditReadsCollection), which lets a feature audit its own reads of a
// collection that is not flagged.
//
// The entry has the collection, the record ids (at most AuditReadMaxRecordIds
// in meta.recordIds, with meta.count and meta.truncated; targetId is set when
// exactly one record was read), the actor and IP carried by ctx
// (WithAuditContext; the "system" actor without one, superusers included as
// "superuser") and meta.orgId (or meta.orgIds when the records span several
// organizations) when the records have an "org" or "orgs" relation.
//
// extraMeta is merged into meta (it cannot override the keys above), for
// example {"grantId": "..."}. With no records nothing is written.
//
// When the audit log is not available (early migrations) it is a no-op.
// A returned error means the entry was NOT written: callers that must not
// serve unaudited reads should fail the request.
func AuditRecordRead(app App, ctx context.Context, collection *Collection, records []*Record, extraMeta map[string]any) error {
	if len(records) == 0 || collection == nil || !auditAvailable(app) {
		return nil
	}

	ids := make([]string, 0, min(len(records), AuditReadMaxRecordIds))
	orgs := map[string]struct{}{}
	orgOrder := []string{}

	addOrg := func(id string) {
		if id == "" {
			return
		}
		if _, ok := orgs[id]; !ok {
			orgs[id] = struct{}{}
			orgOrder = append(orgOrder, id)
		}
	}

	for _, r := range records {
		if len(ids) < AuditReadMaxRecordIds {
			ids = append(ids, r.Id)
		}

		switch {
		case collection.Name == CollectionNameOrgs:
			addOrg(r.Id)
		case collection.Fields.GetByName("org") != nil:
			addOrg(r.GetString("org"))
		case collection.Fields.GetByName("orgs") != nil:
			for _, id := range r.GetStringSlice("orgs") {
				addOrg(id)
			}
		}
	}

	entry := &AuditEntry{
		Action:         AuditActionRead,
		CollectionId:   collection.Id,
		CollectionName: collection.Name,
		ChangedFields:  []string{},
		Meta:           map[string]any{},
	}

	for k, v := range extraMeta {
		entry.Meta[k] = v
	}

	if len(records) == 1 {
		entry.RecordId = records[0].Id
	}

	entry.Meta["recordIds"] = ids
	entry.Meta["count"] = len(records)
	entry.Meta["truncated"] = len(records) > len(ids)

	switch len(orgOrder) {
	case 0:
	case 1:
		entry.Meta["orgId"] = orgOrder[0]
	default:
		entry.Meta["orgIds"] = orgOrder
	}

	applyAuditContext(ctx, entry)

	// the actor's org (SCIM tokens) must not hide the records' org
	if len(orgOrder) == 1 {
		entry.Meta["orgId"] = orgOrder[0]
	}

	return WriteAuditEntry(app, ctx, entry, nil)
}
