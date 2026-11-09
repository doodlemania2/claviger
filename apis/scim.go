package apis

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #79c): SCIM 2.0 (RFC 7643 and 7644) under /scim/v2.
//
//	GET  /scim/v2/ServiceProviderConfig | ResourceTypes | Schemas
//	GET|POST        /scim/v2/Users            GET|PUT|PATCH|DELETE /scim/v2/Users/{id}
//	GET|POST        /scim/v2/Groups           GET|PUT|PATCH|DELETE /scim/v2/Groups/{id}
//
// Every route authenticates with a per-org bearer token (core/scim.go), never
// with a user session or an API key, and is scoped to that token's org. With
// no token configured nothing here is reachable: every request is a 401.
// The provisioning policy is in core/scim_users.go and core/scim_groups.go.
//
// Failed authentication is rate limited per client IP, like API keys.

const (
	scimContentType = "application/scim+json"

	scimSchemaError     = "urn:ietf:params:scim:api:messages:2.0:Error"
	scimSchemaList      = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimSchemaPatchOp   = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	scimSchemaUser      = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimSchemaGroup     = "urn:ietf:params:scim:schemas:core:2.0:Group"
	scimSchemaSPConfig  = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	scimSchemaResType   = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
	scimSchemaSchema    = "urn:ietf:params:scim:schemas:core:2.0:Schema"
	scimMaxResults      = 200
	scimDefaultCount    = 100
	scimMaxBody         = 1 << 20
	scimAuthFailMax     = 30
	scimAuthFailSeconds = 60
)

var scimAuthFailRule = core.RateLimitRule{MaxRequests: scimAuthFailMax, Duration: scimAuthFailSeconds}

const scimAuthFailRateRule = "@pb_scim_fail_"

func bindSCIMApi(_ core.App, rg *router.Router[*core.RequestEvent]) {
	g := rg.Group("/scim/v2")

	g.GET("/ServiceProviderConfig", scimRoute(scimServiceProviderConfig))
	g.GET("/ResourceTypes", scimRoute(scimResourceTypes))
	g.GET("/ResourceTypes/{id}", scimRoute(scimResourceTypes))
	g.GET("/Schemas", scimRoute(scimSchemas))
	g.GET("/Schemas/{id}", scimRoute(scimSchemas))

	g.GET("/Users", scimRoute(scimUsersList))
	g.POST("/Users", scimRoute(scimUsersCreate))
	g.GET("/Users/{id}", scimRoute(scimUsersGet))
	g.PUT("/Users/{id}", scimRoute(scimUsersPut))
	g.PATCH("/Users/{id}", scimRoute(scimUsersPatch))
	g.DELETE("/Users/{id}", scimRoute(scimUsersDelete))

	g.GET("/Groups", scimRoute(scimGroupsList))
	g.POST("/Groups", scimRoute(scimGroupsCreate))
	g.GET("/Groups/{id}", scimRoute(scimGroupsGet))
	g.PUT("/Groups/{id}", scimRoute(scimGroupsPut))
	g.PATCH("/Groups/{id}", scimRoute(scimGroupsPatch))
	g.DELETE("/Groups/{id}", scimRoute(scimGroupsDelete))
}

// scimReq is an authenticated SCIM request.
type scimReq struct {
	e     *core.RequestEvent
	org   *core.Record
	token *core.Record
	// ctx carries the audit actor scim:<tokenId> and the org.
	ctx context.Context
}

func (r *scimReq) base() string {
	scheme := "http"
	if r.e.Request.TLS != nil || strings.EqualFold(r.e.Request.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}

	return scheme + "://" + r.e.Request.Host + "/scim/v2"
}

func scimRoute(fn func(r *scimReq) error) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		raw := strings.TrimSpace(e.Request.Header.Get("Authorization"))
		if len(raw) > 7 && strings.EqualFold(raw[:7], "Bearer ") {
			raw = strings.TrimSpace(raw[7:])
		}

		var (
			tokenRec, org *core.Record
			err           error
		)

		if !core.IsSCIMToken(raw) {
			err = core.ErrInvalidSCIMToken
		} else {
			tokenRec, org, err = core.AuthenticateSCIMToken(e.App, raw)
		}

		if err != nil {
			if err == core.ErrSCIMOrgArchived {
				return scimWriteError(e, http.StatusForbidden, "", "The organization is archived.")
			}

			// bound brute force per client; only failures count
			if checkRateLimit(e, scimAuthFailRateRule, scimAuthFailRule) != nil {
				return scimWriteError(e, http.StatusTooManyRequests, "", "Too many requests, please try again later.")
			}

			e.Response.Header().Set("WWW-Authenticate", `Bearer realm="scim"`)

			return scimWriteError(e, http.StatusUnauthorized, "", "The SCIM token is missing, invalid, revoked or expired.")
		}

		actor := core.AuditActor{Type: core.AuditActorTypeSCIM, Id: "scim:" + tokenRec.Id, OrgId: org.Id}

		return fn(&scimReq{
			e: e, org: org, token: tokenRec,
			ctx: core.WithAuditContext(context.Background(), actor, e.RealIP()),
		})
	}
}

func scimWriteError(e *core.RequestEvent, status int, scimType, detail string) error {
	body := map[string]any{
		"schemas": []string{scimSchemaError},
		"status":  itoa(status),
		"detail":  detail,
	}
	if scimType != "" {
		body["scimType"] = scimType
	}

	return scimWrite(e, status, body)
}

func scimFail(r *scimReq, err error) error {
	se := core.AsSCIMError(err)
	if se.Status >= 500 {
		r.e.App.Logger().Error("SCIM request failed", "error", err)
	}

	return scimWriteError(r.e, se.Status, se.Type, se.Detail)
}

func scimWrite(e *core.RequestEvent, status int, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return e.InternalServerError("", err)
	}

	return e.Blob(status, scimContentType, raw)
}

func itoa(i int) string {
	b, _ := json.Marshal(i)

	return string(b)
}

// scimReadObject reads a JSON object body (any Content-Type).
func scimReadObject(r *scimReq) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.e.Request.Body, scimMaxBody+1))
	if err != nil || len(raw) > scimMaxBody {
		return nil, &core.SCIMError{Status: 400, Type: "invalidSyntax", Detail: "The request body is unreadable or too large."}
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return nil, &core.SCIMError{Status: 400, Type: "invalidSyntax", Detail: "The request body is not a valid JSON object."}
	}

	return out, nil
}

func scimTime(t interface{ Time() time.Time }) string {
	return t.Time().UTC().Format(time.RFC3339)
}

// ---- discovery -------------------------------------------------------------

func scimMeta(r *scimReq, resource, path string) map[string]any {
	return map[string]any{"resourceType": resource, "location": r.base() + path}
}

func scimServiceProviderConfig(r *scimReq) error {
	return scimWrite(r.e, 200, map[string]any{
		"schemas":          []string{scimSchemaSPConfig},
		"documentationUri": "https://github.com/doodlemania2/claviger/blob/master/docs/scim.md",
		"patch":            map[string]any{"supported": true},
		"bulk":             map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":           map[string]any{"supported": true, "maxResults": scimMaxResults},
		"changePassword":   map[string]any{"supported": false},
		"sort":             map[string]any{"supported": false},
		"etag":             map[string]any{"supported": false},
		"authenticationSchemes": []map[string]any{{
			"type":        "oauthbearertoken",
			"name":        "OAuth Bearer Token",
			"description": "A per-organization SCIM token sent as a bearer token.",
			"specUri":     "https://www.rfc-editor.org/info/rfc6750",
			"primary":     true,
		}},
		"meta": scimMeta(r, "ServiceProviderConfig", "/ServiceProviderConfig"),
	})
}

func scimResourceTypes(r *scimReq) error {
	types := []map[string]any{
		{
			"schemas": []string{scimSchemaResType}, "id": "User", "name": "User", "endpoint": "/Users",
			"description": "User Account", "schema": scimSchemaUser,
			"meta": scimMeta(r, "ResourceType", "/ResourceTypes/User"),
		},
		{
			"schemas": []string{scimSchemaResType}, "id": "Group", "name": "Group", "endpoint": "/Groups",
			"description": "Group", "schema": scimSchemaGroup,
			"meta": scimMeta(r, "ResourceType", "/ResourceTypes/Group"),
		},
	}

	return scimDiscoveryList(r, types)
}

func scimSchemas(r *scimReq) error {
	attr := func(name, typ string, multi bool, mutability, uniqueness string, required bool, sub ...map[string]any) map[string]any {
		a := map[string]any{
			"name": name, "type": typ, "multiValued": multi, "required": required, "caseExact": false,
			"mutability": mutability, "returned": "default", "uniqueness": uniqueness,
		}
		if len(sub) > 0 {
			a["subAttributes"] = sub
		}

		return a
	}

	schemas := []map[string]any{
		{
			"schemas": []string{scimSchemaSchema}, "id": scimSchemaUser, "name": "User", "description": "User Account",
			"attributes": []map[string]any{
				attr("userName", "string", false, "readWrite", "server", true),
				attr("externalId", "string", false, "readWrite", "none", false),
				attr("displayName", "string", false, "readWrite", "none", false),
				attr("name", "complex", false, "readWrite", "none", false,
					attr("formatted", "string", false, "readWrite", "none", false),
					attr("givenName", "string", false, "readWrite", "none", false),
					attr("familyName", "string", false, "readWrite", "none", false)),
				attr("emails", "complex", true, "readWrite", "none", false,
					attr("value", "string", false, "readWrite", "none", false),
					attr("type", "string", false, "readWrite", "none", false),
					attr("primary", "boolean", false, "readWrite", "none", false)),
				attr("active", "boolean", false, "readWrite", "none", false),
				attr("groups", "complex", true, "readOnly", "none", false),
			},
			"meta": scimMeta(r, "Schema", "/Schemas/"+scimSchemaUser),
		},
		{
			"schemas": []string{scimSchemaSchema}, "id": scimSchemaGroup, "name": "Group", "description": "Group",
			"attributes": []map[string]any{
				attr("displayName", "string", false, "readWrite", "server", true),
				attr("externalId", "string", false, "readWrite", "none", false),
				attr("members", "complex", true, "readWrite", "none", false,
					attr("value", "string", false, "immutable", "none", false),
					attr("display", "string", false, "readOnly", "none", false),
					attr("type", "string", false, "immutable", "none", false)),
			},
			"meta": scimMeta(r, "Schema", "/Schemas/"+scimSchemaGroup),
		},
	}

	return scimDiscoveryList(r, schemas)
}

// scimDiscoveryList serves a list or, with an {id}, one item of it.
func scimDiscoveryList(r *scimReq, items []map[string]any) error {
	if id := r.e.Request.PathValue("id"); id != "" {
		for _, it := range items {
			if it["id"] == id {
				return scimWrite(r.e, 200, it)
			}
		}

		return scimWriteError(r.e, 404, "", "Resource "+id+" not found.")
	}

	return scimWrite(r.e, 200, map[string]any{
		"schemas": []string{scimSchemaList}, "totalResults": len(items), "startIndex": 1,
		"itemsPerPage": len(items), "Resources": items,
	})
}
