package apis

import (
	"encoding/base64"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
	"github.com/go-webauthn/webauthn/webauthn"
)

// recordWebAuthnCredentialUse persists the outcome of a successful assertion on
// the stored credential: the new sign count, the refreshed flags and the
// lastUsed timestamp, all in one transaction so a credential never shows a
// bumped counter with a stale lastUsed (or the reverse).
//
// A failure here is logged and does not fail the sign-in: the signature has
// already been verified, and refusing a legitimate user because bookkeeping
// failed would be worse than a stale counter. It returns the stored credential
// (nil when it could not be found) for the auth event.
func recordWebAuthnCredentialUse(app core.App, collection *core.Collection, record *core.Record, cred *webauthn.Credential) *core.WebAuthnCredential {
	if cred == nil {
		return nil
	}

	credIdEncoded := base64.RawURLEncoding.EncodeToString(cred.ID)

	var stored *core.WebAuthnCredential
	txErr := app.RunInTransaction(func(txApp core.App) error {
		storedRecord, err := txApp.FindFirstRecordByFilter(
			core.CollectionNameWebAuthnCredentials,
			"credentialId = {:credId} && recordRef = {:recordRef} && collectionRef = {:colId}",
			dbx.Params{"credId": credIdEncoded, "recordRef": record.Id, "colId": collection.Id},
		)
		if err != nil {
			return err
		}

		proxy := &core.WebAuthnCredential{Record: storedRecord}
		proxy.SetSignCount(cred.Authenticator.SignCount)
		proxy.SetFlags(cred.Flags)
		proxy.SetLastUsed(types.NowDateTime())

		if err := txApp.Save(proxy); err != nil {
			return err
		}

		stored = proxy
		return nil
	})
	if txErr != nil {
		app.Logger().Error("Failed to update WebAuthn credential after sign-in",
			"error", txErr,
			"credentialId", credIdEncoded,
		)
	}

	return stored
}
