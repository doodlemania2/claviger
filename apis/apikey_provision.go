package apis

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #141): the provisioning scope of an API key.
//
// A backend that signs people up on its own (creating the account of a child, a
// guest or an employee and choosing the first password or PIN) used to need a
// superuser token, because a key may never write an auth collection. The scope
// "provision:<auth collection>" opens exactly two requests on that collection
// and nothing else:
//
//	POST  /api/collections/{c}/records        create a record
//	PATCH /api/collections/{c}/records/{id}   body with only password and passwordConfirm
//
// "manage:<auth collection>" is the narrower companion: PATCH with only an
// email, and DELETE of a record. A changed email clears verified, rotates the
// token key and removes the OAuth2 links (bindKeyEmailChange). It does not
// include a password or anything else.
//
// Not allowed even with the scope: changing an email, `verified` or the token
// key, deleting, a multipart body (so the whole body can be checked here), and
// the superusers collection. The key reads nothing it has no read scope for.
// The collection's rules still run as the key's owner afterwards, so give the
// owner an account those rules allow (a superuser owner bypasses them, which is
// fine: the scope is what bounds the key).

const apiKeyProvisionMaxBody = 1 << 20

// create may set any field except these.
var apiKeyProvisionDeniedCreateFields = []string{"verified", "tokenKey", "id"}

func enforceKeyProvisioning(e *core.RequestEvent, key *core.APIKeyInfo, collection *core.Collection, hasRecordId bool) error {
	switch {
	case e.Request.Method == http.MethodDelete && hasRecordId:
		// manage:<collection>: delete a record
		if !key.CanManage(collection.Name) {
			return apiKeyForbidden(e)
		}

		return nil
	case e.Request.Method == http.MethodPost && !hasRecordId:
		if !key.CanProvision(collection.Name) {
			return apiKeyForbidden(e)
		}
	case e.Request.Method == http.MethodPatch && hasRecordId:
	default:
		return apiKeyForbidden(e)
	}

	fields, ok := readKeyJSONBody(e)
	if !ok {
		return apiKeyForbidden(e)
	}

	if e.Request.Method == http.MethodPatch {
		return enforceKeyPatch(e, key, collection, fields)
	}

	for name := range fields {
		for _, denied := range apiKeyProvisionDeniedCreateFields {
			if name == denied {
				return apiKeyForbidden(e)
			}
		}
	}

	return nil
}

// enforceKeyPatch allows exactly two bodies: {password, passwordConfirm}
// (provision scope) and {email} (manage scope).
func enforceKeyPatch(e *core.RequestEvent, key *core.APIKeyInfo, collection *core.Collection, fields map[string]json.RawMessage) error {
	only := func(names ...string) bool {
		if len(fields) != len(names) {
			return false
		}
		for _, n := range names {
			if _, ok := fields[n]; !ok {
				return false
			}
		}

		return true
	}

	switch {
	case key.CanProvision(collection.Name) && (only(core.FieldNamePassword) || only(core.FieldNamePassword, core.FieldNamePassword+"Confirm")) &&
		string(fields[core.FieldNamePassword]) != "null":
		return nil
	case key.CanManage(collection.Name) && only(core.FieldNameEmail):
		var email string
		if json.Unmarshal(fields[core.FieldNameEmail], &email) != nil || strings.TrimSpace(email) == "" {
			return apiKeyForbidden(e)
		}

		return nil
	}

	return apiKeyForbidden(e)
}

// bindKeyEmailChange makes an email change made with a manage key safe: the
// record is no longer verified, its token key is rotated (every token it holds
// stops working) and its OAuth2 links are removed, so the new address has to be
// proven again and the previous linked account cannot sign in as it.
func bindKeyEmailChange(app core.App) {
	app.OnRecordUpdateRequest().Bind(&hook.Handler[*core.RecordRequestEvent]{
		Id:       "__clvKeyEmailChange__",
		Priority: -1000,
		Func: func(e *core.RecordRequestEvent) error {
			if APIKeyFromRequest(e.RequestEvent) == nil || !e.Collection.IsAuth() ||
				e.Record.Email() == e.Record.Original().Email() {
				return e.Next()
			}

			e.Record.SetVerified(false)
			e.Record.RefreshTokenKey()

			if err := e.Next(); err != nil {
				return err
			}

			links, err := e.App.FindAllExternalAuthsByRecord(e.Record)
			if err != nil {
				return err
			}
			for _, l := range links {
				if err := e.App.Delete(l); err != nil {
					return err
				}
			}

			return nil
		},
	})
}

// readKeyJSONBody reads a JSON object body and puts the bytes back for the handler.
func readKeyJSONBody(e *core.RequestEvent) (map[string]json.RawMessage, bool) {
	if mt, _, err := mime.ParseMediaType(e.Request.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return nil, false
	}

	raw, err := io.ReadAll(io.LimitReader(e.Request.Body, apiKeyProvisionMaxBody+1))
	if err != nil || len(raw) > apiKeyProvisionMaxBody {
		return nil, false
	}
	e.Request.Body = io.NopCloser(bytes.NewReader(raw))

	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false
	}

	return fields, true
}
