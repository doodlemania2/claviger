package apis

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"

	"github.com/doodlemania2/claviger/core"
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
	if !key.CanProvision(collection.Name) {
		return apiKeyForbidden(e)
	}

	var update bool
	switch {
	case e.Request.Method == http.MethodPost && !hasRecordId:
	case e.Request.Method == http.MethodPatch && hasRecordId:
		update = true
	default:
		return apiKeyForbidden(e)
	}

	fields, ok := readKeyJSONBody(e)
	if !ok {
		return apiKeyForbidden(e)
	}

	for name := range fields {
		if update {
			if name != core.FieldNamePassword && name != core.FieldNamePassword+"Confirm" {
				return apiKeyForbidden(e)
			}
			continue
		}

		for _, denied := range apiKeyProvisionDeniedCreateFields {
			if name == denied {
				return apiKeyForbidden(e)
			}
		}
	}

	if update && fields[core.FieldNamePassword] == nil {
		return apiKeyForbidden(e)
	}

	return nil
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
