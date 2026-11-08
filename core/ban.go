package core

import (
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #93): bans.
//
// A ban is a row in the hidden _bans collection (at most one per auth
// record), so user collections need no schema change. While a ban is active
// (no "until", or "until" in the future) the account cannot sign in, its API
// keys stop working and no reset email is sent.
//
// Creating or changing a ban revokes the account's sessions and rotates its
// tokenKey. The rotation is what kills tokens issued before sessions existed
// (they carry no sid and are not checked per request), plus any outstanding
// file or reset tokens.

const CollectionNameBans = "_bans"

// FindActiveBan returns the active ban of the auth record, or nil.
func FindActiveBan(app App, collectionId, recordId string) *Record {
	bans, err := app.FindAllRecords(CollectionNameBans, dbx.HashExp{
		"collectionRef": collectionId,
		"recordRef":     recordId,
	})
	if err != nil {
		return nil
	}

	now := time.Now()
	for _, b := range bans {
		until := b.GetDateTime("until")
		if until.IsZero() || until.Time().After(now) {
			return b
		}
	}

	return nil
}

// IsBanned reports whether the auth record has an active ban.
func IsBanned(app App, authRecord *Record) bool {
	if authRecord == nil {
		return false
	}

	return FindActiveBan(app, authRecord.Collection().Id, authRecord.Id) != nil
}

// DeleteExpiredBans deletes the bans whose "until" passed before the cutoff.
func DeleteExpiredBans(app App, before time.Time) (int64, error) {
	res, err := app.NonconcurrentDB().Delete(CollectionNameBans,
		dbx.NewExp("[[until]] != '' AND [[until]] < {:cutoff}", dbx.Params{
			"cutoff": before.UTC().Format(types.DefaultDateLayout),
		}),
	).Execute()
	if err != nil {
		return 0, err
	}

	return res.RowsAffected()
}

func (app *BaseApp) registerBanHooks() {
	enforce := &hook.Handler[*RecordEvent]{
		Id: "__pbBanEnforce__",
		Func: func(e *RecordEvent) error {
			if err := e.Next(); err != nil {
				return err
			}

			collectionId := e.Record.GetString("collectionRef")
			recordId := e.Record.GetString("recordRef")

			if _, err := RevokeSessions(e.App, collectionId, recordId, SessionRevokedBanned, ""); err != nil {
				e.App.Logger().Error("Failed to revoke the sessions of a banned account", "error", err, "recordId", recordId)
			}

			target, err := e.App.FindRecordById(collectionId, recordId)
			if err != nil {
				return nil
			}

			target.RefreshTokenKey()
			if err := e.App.SaveNoValidateWithContext(e.Context, target); err != nil {
				e.App.Logger().Error("Failed to rotate the token key of a banned account", "error", err, "recordId", recordId)
			}

			return nil
		},
	}

	app.OnRecordAfterCreateSuccess(CollectionNameBans).Bind(enforce)
	app.OnRecordAfterUpdateSuccess(CollectionNameBans).Bind(enforce)
}
