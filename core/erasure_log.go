package core

import (
	"context"
	"errors"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, 88-4): the erasure log and its replay after a
// restore (spec 10.6).
//
// Backups and replicas are not rewritten: erased data stays in them until they
// rotate. So every erasure writes one row to _erasureLog: a one-way hash of the
// collection name and the subject id, the date, and the collections touched.
// Nothing in it can be turned back into a person. After a restore the app goes
// through the ids of its auth collections and household members, finds the ones
// whose hash is in the log and that are no longer anonymized, and erases them
// again (ReplayErasureLog, at every start of the server). The replay makes an
// ordinary erase request of mode "replay": no grace period, no email, and the
// same checks (legal holds, the last guardian) as any erasure. It is idempotent:
// a subject that is erased is not found again.

// writeErasureLog creates or updates the row of the subject. collections, when
// not nil, replaces the list of collections touched.
func writeErasureLog(app App, collection, id string, collections []string) error {
	col, err := app.FindCachedCollectionByNameOrId(CollectionNameErasureLog)
	if err != nil {
		return err
	}

	hash := ErasureSubjectHash(app, collection, id)

	rec, err := app.FindFirstRecordByData(col, "subjectHash", hash)
	if err != nil {
		rec = NewRecord(col)
		rec.Id = GenerateDefaultRandomId() // saved without hooks: nothing else sets it
		rec.SetRaw("created", types.NowDateTime())
		rec.Set("subjectHash", hash)
		rec.Set("erasedAt", types.NowDateTime())
		rec.Set("collections", []string{})
	}

	if collections != nil {
		rec.Set("collections", collections)
		rec.Set("erasedAt", types.NowDateTime())
	}

	rec.SetRaw("updated", types.NowDateTime())

	return app.UnsafeWithoutHooks().Save(rec)
}

func markErasureReplayed(app App, collection, id string) error {
	rec, err := app.FindFirstRecordByData(CollectionNameErasureLog, "subjectHash", ErasureSubjectHash(app, collection, id))
	if err != nil {
		return err
	}

	rec.Set("replays", rec.GetInt("replays")+1)
	rec.Set("lastReplayAt", types.NowDateTime())
	rec.SetRaw("updated", types.NowDateTime())

	return app.UnsafeWithoutHooks().Save(rec)
}

// ReplayErasureLog finds the people in the log who are back (a restore of an
// older backup) and erases them again. It returns how many it found.
func ReplayErasureLog(app App) (int, error) {
	if !ErasureAvailable(app) {
		return 0, nil
	}

	logRows, err := app.FindAllRecords(CollectionNameErasureLog)
	if err != nil || len(logRows) == 0 {
		return 0, err
	}

	// the hashes are keyed: without the install's secret nothing can be
	// matched, and a different key would erase the wrong people or nobody
	if _, err := erasureSecret(app, false); err != nil {
		if errors.Is(err, ErrErasureSecretMissing) {
			app.Logger().Warn("The erasure log has rows but the erasure secret is missing; the replay is skipped. Restore the _params row \"erasureSecret\" from the backup of this database.")

			return 0, nil
		}

		return 0, err
	}

	logged := make(map[string]bool, len(logRows))
	for _, r := range logRows {
		logged[r.GetString("subjectHash")] = true
	}

	var candidates []*Record

	// the accounts
	cols, err := app.FindAllCollections(CollectionTypeAuth)
	if err != nil {
		return 0, err
	}

	for _, c := range cols {
		if c.Name == CollectionNameSuperusers {
			continue
		}

		ids, err := erasureAllIds(app, c)
		if err != nil {
			return 0, err
		}

		for _, id := range ids {
			if !logged[ErasureSubjectHash(app, c.Name, id)] {
				continue
			}

			rec, err := app.FindRecordById(c, id)
			if err != nil || IsErasedAccount(rec) {
				continue
			}

			candidates = append(candidates, rec)
		}
	}

	// the household rows of people without an account
	if mc, err := app.FindCachedCollectionByNameOrId(CollectionNameHouseholdMembers); err == nil {
		ids, err := erasureAllIds(app, mc)
		if err != nil {
			return 0, err
		}

		for _, id := range ids {
			if !logged[ErasureSubjectHash(app, mc.Name, id)] {
				continue
			}

			rec, err := app.FindRecordById(mc, id)
			if err != nil || erasedMember(rec) {
				continue
			}

			candidates = append(candidates, rec)
		}
	}

	found := 0

	for _, rec := range candidates {
		if err := enqueueErasureReplay(app, rec); err != nil {
			app.Logger().Warn("Failed to queue an erasure replay", "collection", rec.Collection().Name, "error", err.Error())

			continue
		}

		found++
	}

	if found > 0 {
		app.Logger().Warn("The erasure log found people who are back after a restore; erasing them again", "count", found)
	}

	return found, nil
}

func erasureAllIds(app App, col *Collection) ([]string, error) {
	var ids []string

	err := app.DB().Select("id").From(col.Name).Column(&ids)

	return ids, err
}

func enqueueErasureReplay(app App, subject *Record) error {
	dr, err := app.FindCachedCollectionByNameOrId(CollectionNameDataRequests)
	if err != nil {
		return err
	}

	active, err := app.CountRecords(CollectionNameDataRequests, dbx.And(
		dbx.HashExp{"type": DataRequestTypeErase, "collectionRef": subject.Collection().Id, "recordRef": subject.Id},
		dbx.In("state", DataRequestStateAwaitingConfirmation, DataRequestStateGrace, DataRequestStateQueued, DataRequestStateRunning),
	))
	if err != nil || active > 0 {
		return err
	}

	req := NewRecord(dr)
	req.Id = GenerateDefaultRandomId()
	req.SetRaw("created", types.NowDateTime())
	req.SetRaw("updated", types.NowDateTime())
	req.Set("type", DataRequestTypeErase)
	req.Set("state", DataRequestStateQueued)
	req.Set("collectionRef", subject.Collection().Id)
	req.Set("recordRef", subject.Id)
	req.Set("selfService", false)
	req.Set("dueAt", types.NowDateTime())
	req.Set("meta", map[string]any{"mode": ErasureModeReplay})

	if err := app.UnsafeWithoutHooks().Save(req); err != nil {
		return err
	}

	// the entry says only that a replay was queued
	if auditAvailable(app) {
		return writeErasureAudit(app, context.Background(), AuditActionErasureRequest, req, nil, "", map[string]any{
			"subjectCollection": subject.Collection().Name,
			"subjectId":         subject.Id,
			"mode":              ErasureModeReplay,
		})
	}

	return nil
}
