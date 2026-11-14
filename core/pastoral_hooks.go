package core

import (
	"context"
	"slices"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #86): hooks of the pastoral notes.
//
//   - Sealed-note read logging (86-2): every list or view request that returns
//     a sealed note to anyone but its author writes ONE audit entry (action
//     "read") and raises the readCount of the grant that opened it. A
//     superuser's read is logged too. This is the self-contained piece the
//     generic read audit of #86-3 (audit.readCollections) will absorb; the
//     helper is called auditSealedNoteReads so it can be merged later.
//   - The response of an update by someone who is not the author (a moderator
//     archiving) never carries the body.
//   - A member who is suspended or removed loses their break-glass grants.

// pastoralReadAuditCap is the most record ids one read entry lists.
const pastoralReadAuditCap = 50

func (app *BaseApp) registerPastoralHooks() {
	// before everything else: the read is logged before any byte is sent, and
	// a failure to log stops the request
	const early = -90000

	app.OnRecordViewRequest().Bind(&hook.Handler[*RecordRequestEvent]{
		Id:       "__clvPastoralReadView__",
		Priority: early,
		Func: func(e *RecordRequestEvent) error {
			if err := auditSealedNoteReads(e.App, e.RequestEvent, e.Collection, []*Record{e.Record}); err != nil {
				return err
			}

			return e.Next()
		},
	})

	app.OnRecordsListRequest().Bind(&hook.Handler[*RecordsListRequestEvent]{
		Id:       "__clvPastoralReadList__",
		Priority: early,
		Func: func(e *RecordsListRequestEvent) error {
			if err := auditSealedNoteReads(e.App, e.RequestEvent, e.Collection, e.Records); err != nil {
				return err
			}

			return e.Next()
		},
	})

	app.OnRecordUpdateRequest().Bind(&hook.Handler[*RecordRequestEvent]{
		Id:       "__clvPastoralHideBody__",
		Priority: early,
		Func: func(e *RecordRequestEvent) error {
			if IsPastoralCollection(e.Collection) && (e.Auth == nil || e.Record.GetString("author") != e.Auth.Id) {
				e.Record.Hide("body")
			}

			return e.Next()
		},
	})

	revoke := func(e *RecordEvent) error {
		if e.Record.Collection().Name != CollectionNameOrgMembers {
			return e.Next()
		}

		if e.Type == ModelEventTypeUpdate && e.Record.GetString("status") == OrgStatusActive {
			return e.Next()
		}

		ctx := e.Context
		if ctx == nil {
			ctx = context.Background()
		}

		if err := RevokeBreakGlassForMember(e.App, ctx, e.Record.GetString("org"), e.Record.GetString("user"), "membership_ended"); err != nil {
			e.App.Logger().Warn("Failed to revoke the break-glass grants of a member", "error", err.Error())
		}

		return e.Next()
	}

	app.OnRecordAfterUpdateSuccess(CollectionNameOrgMembers).Bind(&hook.Handler[*RecordEvent]{Id: "__clvPastoralRevokeUpdate__", Func: revoke})
	app.OnRecordAfterDeleteSuccess(CollectionNameOrgMembers).Bind(&hook.Handler[*RecordEvent]{Id: "__clvPastoralRevokeDelete__", Func: revoke})
}

// auditSealedNoteReads writes the entry for the sealed notes among recs that
// someone other than the author is reading, and counts the reads on the grants.
func auditSealedNoteReads(app App, re *RequestEvent, col *Collection, recs []*Record) error {
	if re == nil || !IsPastoralCollection(col) || !auditAvailable(app) {
		return nil
	}

	var (
		ids, orgIds, grantIds []string
		reader                string
	)

	if re.Auth != nil {
		reader = re.Auth.Id
	}

	now := time.Now().UTC()
	perGrant := map[string]int{}
	superuser := re.Auth != nil && re.Auth.IsSuperuser()

	for _, r := range recs {
		if r == nil || !r.GetBool("sealed") || (reader != "" && r.GetString("author") == reader) {
			continue
		}

		if len(ids) < pastoralReadAuditCap {
			ids = append(ids, r.Id)
		}

		if o := r.GetString("org"); o != "" && !slices.Contains(orgIds, o) {
			orgIds = append(orgIds, o)
		}

		if reader != "" && !superuser {
			if g := LiveBreakGlass(app, col.Name, r.Id, reader, now); g != nil {
				perGrant[g.Id]++

				if !slices.Contains(grantIds, g.Id) {
					grantIds = append(grantIds, g.Id)
				}
			}
		}
	}

	if len(ids) == 0 {
		return nil
	}

	meta := map[string]any{
		"collection": col.Name,
		"recordIds":  ids,
		"orgIds":     orgIds,
		"grantIds":   nonNilStrings(grantIds),
		"grantId":    "",
	}

	if len(orgIds) > 0 {
		meta["orgId"] = orgIds[0]
	}

	if len(grantIds) > 0 {
		meta["grantId"] = grantIds[0]
	}

	if superuser {
		meta["superuser"] = true
	}

	ctx := WithAuditContext(context.Background(), NewAuditActorFromRecord(re.Auth), re.RealIP())

	entry := &AuditEntry{
		Action:         AuditActionRead,
		CollectionId:   col.Id,
		CollectionName: col.Name,
		RecordId:       ids[0],
		Meta:           meta,
	}

	applyAuditContext(ctx, entry)

	err := app.RunInTransaction(func(txApp App) error {
		if err := WriteAuditEntry(txApp, ctx, entry, nil); err != nil {
			return err
		}

		for id, n := range perGrant {
			if _, err := txApp.DB().NewQuery("UPDATE {{" + CollectionNameBreakGlass + "}} SET {{readCount}} = COALESCE({{readCount}}, 0) + {:n} WHERE {{id}} = {:id}").
				Bind(dbx.Params{"n": n, "id": id}).Execute(); err != nil {
				return err
			}
		}

		return nil
	})

	return err
}
