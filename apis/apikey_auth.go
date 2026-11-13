package apis

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/telemetry"
)

// Fork-local (Claviger #95): API key authentication and scope enforcement.
//
// loadAuthToken makes a single call into this file (loadAPIKeyAuth) for tokens
// that start with "clv_"; JWT handling is untouched.
//
// Scope enforcement lives here, in one route classifier, rather than in each
// handler or in the rule resolver: key-authenticated requests are checked
// before they reach any handler, and a route the classifier does not know is
// denied unless the key has the "*" scope. Rules still run afterwards as the
// owner, so scopes can only restrict.
//
// Not exposed as @request.apiKey in rules: that needs a change to the
// upstream-owned core/record_field_resolver.go, which the fork avoids.

const requestEventKeyAPIKey = "__clavigerAPIKey"

const apiKeyFailRateRule = "@pb_apikey_fail_"

// APIKeyFromRequest returns the API key that authenticated the request,
// or nil for JWT or guest requests.
func APIKeyFromRequest(e *core.RequestEvent) *core.APIKeyInfo {
	key, _ := e.Get(requestEventKeyAPIKey).(*core.APIKeyInfo)
	return key
}

// loadAPIKeyAuth authenticates the request with the API key in token.
// Unlike an invalid JWT (which silently leaves the request a guest) an
// invalid key fails the request with 401: it is never a JWT, so there is no
// legacy behavior to keep, and silently downgrading to guest would hide a
// revoked key behind public data.
func loadAPIKeyAuth(e *core.RequestEvent, token string) error {
	owner, key, err := core.AuthenticateAPIKey(e.App, token)

	if t := telemetry.Active(); t != nil {
		t.RecordAuth("apikey", err)
	}

	if err != nil {
		e.App.Logger().Debug("loadAuthToken api key failure")

		// bound brute-force attempts per client; only failures are counted,
		// so a valid key is never throttled by this
		if checkRateLimit(e, apiKeyFailRateRule, core.RateLimitRule{MaxRequests: 30, Duration: 60}) != nil {
			return e.TooManyRequestsError("Too many requests, please try again later.", nil)
		}

		return e.UnauthorizedError("The provided API key is invalid, revoked or expired.", nil)
	}

	e.Auth = owner
	e.Set(requestEventKeyAPIKey, key)

	if err := enforceAPIKeyScopes(e, key); err != nil {
		return err
	}

	return e.Next()
}

func apiKeyForbidden(e *core.RequestEvent) error {
	return e.ForbiddenError("The API key is not allowed to perform this action.", nil)
}

// enforceAPIKeyScopes applies the scope model to a key-authenticated request.
//
//   - /api/collections/{c}/records[/{id}]: needs collection:<c>:read for
//     GET/HEAD and collection:<c>:write otherwise. Writes to auth
//     collections are denied outright (that is how passwords and emails
//     change). Expanded relations need read on their collections too.
//   - /api/files/{c}/...: collection read. /api/files/token is denied.
//   - /api/health: always allowed.
//   - API key management, field key maintenance, the personal data export
//     (/api/me, /api/gdpr), auth flows (auth-*, request-*, confirm-*,
//     impersonate, oauth2-redirect): always denied, "*" included.
//   - realtime, batch and every other route: "*" only. Superuser-only routes
//     additionally need a superuser owner, so a key reaches them only when it
//     is owned by a superuser and scoped "*".
func enforceAPIKeyScopes(e *core.RequestEvent, key *core.APIKeyInfo) error {
	path := strings.Trim(e.Request.URL.Path, "/")
	segs := strings.Split(path, "/")

	if len(segs) < 2 || segs[0] != "api" {
		// not an /api route (custom routes, UI)
		if key.HasAll() {
			return nil
		}
		return apiKeyForbidden(e)
	}

	segs = segs[1:]
	read := e.Request.Method == http.MethodGet || e.Request.Method == http.MethodHead

	switch segs[0] {
	case "health":
		return nil
	case "apikeys", "sessions", "bans", "oauth2-redirect", "field-keys", "me", "gdpr", "consents", "consent-purposes": // me, gdpr: #88 data export
		return apiKeyForbidden(e)
	case "files":
		if len(segs) < 2 || segs[1] == "token" {
			return apiKeyForbidden(e)
		}
		if !read {
			return apiKeyForbidden(e)
		}
		return requireKeyCollectionAccess(e, key, segs[1], core.APIKeyScopeRead)
	case "collections":
		if len(segs) >= 3 {
			sub := segs[2]
			switch {
			case sub == "records":
				if len(segs) > 4 {
					return apiKeyForbidden(e) // sub-resources of a record
				}
				return enforceKeyRecordsRoute(e, key, segs[1], read)
			case strings.HasPrefix(sub, "auth-"),
				strings.HasPrefix(sub, "request-"),
				strings.HasPrefix(sub, "confirm-"),
				sub == "impersonate":
				return apiKeyForbidden(e)
			}
		}
	}

	// realtime, batch, collections/settings/logs/backups..., custom routes
	if key.HasAll() {
		return nil
	}

	return apiKeyForbidden(e)
}

func requireKeyCollectionAccess(e *core.RequestEvent, key *core.APIKeyInfo, collectionNameOrId, access string) error {
	collection, err := e.App.FindCachedCollectionByNameOrId(collectionNameOrId)
	if err != nil || !key.Allows(collection.Name, access) {
		return apiKeyForbidden(e)
	}

	return nil
}

func enforceKeyRecordsRoute(e *core.RequestEvent, key *core.APIKeyInfo, collectionNameOrId string, read bool) error {
	collection, err := e.App.FindCachedCollectionByNameOrId(collectionNameOrId)
	if err != nil {
		return apiKeyForbidden(e)
	}

	access := core.APIKeyScopeRead
	if !read {
		access = core.APIKeyScopeWrite

		// auth records carry the password, email and token key: never writable
		// with a key
		if collection.IsAuth() {
			return apiKeyForbidden(e)
		}
	}

	if !key.Allows(collection.Name, access) {
		return apiKeyForbidden(e)
	}

	// filter and sort can reach other collections (relation paths, back
	// relations, @collection) and turn a match into a yes/no oracle about
	// data the key has no scope for
	if !key.HasAll() {
		q := e.Request.URL.Query()
		for _, expr := range append(q["filter"], q["sort"]...) {
			if !keyCanReference(e.App, key, collection, e.Auth.Collection(), expr) {
				return apiKeyForbidden(e)
			}
		}
	}

	// a relation expand returns records of other collections: they need
	// their own read scope, otherwise expand would bypass the scopes
	if !key.HasAll() {
		for _, expand := range e.Request.URL.Query()["expand"] {
			if !keyCanExpand(e.App, key, collection, expand) {
				return apiKeyForbidden(e)
			}
		}
	}

	return nil
}

// keyCanExpand reports whether every collection reachable through the expand
// expression is readable with the key. Unresolvable segments are ignored
// (the records API ignores them too).
func keyCanExpand(app core.App, key *core.APIKeyInfo, base *core.Collection, expand string) bool {
	for _, path := range strings.Split(expand, ",") {
		current := base

		for _, name := range strings.Split(strings.TrimSpace(path), ".") {
			if current == nil || name == "" {
				break
			}

			next := resolveExpandTarget(app, current, name)
			if next == nil {
				break
			}
			if !key.Allows(next.Name, core.APIKeyScopeRead) {
				return false
			}
			current = next
		}
	}

	return true
}

func resolveExpandTarget(app core.App, from *core.Collection, name string) *core.Collection {
	if f, ok := from.Fields.GetByName(name).(*core.RelationField); ok {
		c, _ := app.FindCachedCollectionByNameOrId(f.CollectionId)
		return c
	}

	// back-relation: <collection>_via_<field>
	if idx := strings.Index(name, "_via_"); idx > 0 {
		c, _ := app.FindCachedCollectionByNameOrId(name[:idx])
		return c
	}

	return nil
}

var (
	keyExprQuotedRegex = regexp.MustCompile(`'(?:\\.|[^'\\])*'|"(?:\\.|[^"\\])*"`)
	keyExprTokenRegex  = regexp.MustCompile(`@?[A-Za-z_][A-Za-z0-9_]*(?::[A-Za-z]+)?(?:\.[A-Za-z0-9_@]+(?::[A-Za-z0-9_]+)?)*`)
)

// keyCanReference reports whether every collection that a filter or sort
// expression reaches (through relation paths walked against the real schema,
// "<collection>_via_<field>" back-relations, @collection.* and
// @request.auth.* paths) is readable with the key. Identifiers that do not
// resolve are ignored: the records API rejects them itself.
func keyCanReference(app core.App, key *core.APIKeyInfo, base, authCollection *core.Collection, expr string) bool {
	expr = keyExprQuotedRegex.ReplaceAllString(expr, " ")

	for _, tok := range keyExprTokenRegex.FindAllString(expr, -1) {
		segs := strings.Split(tok, ".")
		for i := range segs {
			segs[i], _, _ = strings.Cut(segs[i], ":") // modifiers and aliases
		}

		current := base

		switch {
		case segs[0] == "@collection":
			if len(segs) < 2 {
				continue
			}
			c, err := app.FindCachedCollectionByNameOrId(segs[1])
			if err != nil {
				continue // an alias of an earlier @collection reference
			}
			if !key.Allows(c.Name, core.APIKeyScopeRead) {
				return false
			}
			current, segs = c, segs[2:]
		case segs[0] == "@request":
			if len(segs) < 3 || segs[1] != "auth" || authCollection == nil {
				continue
			}
			current, segs = authCollection, segs[2:]
		case strings.HasPrefix(segs[0], "@"):
			continue
		}

		for i, name := range segs {
			// a trailing relation field is just the stored ids; only
			// following it (or a back-relation) reads the other collection
			if i == len(segs)-1 && current.Fields.GetByName(name) != nil {
				break
			}

			next := resolveExpandTarget(app, current, name)
			if next == nil {
				break
			}
			if !key.Allows(next.Name, core.APIKeyScopeRead) {
				return false
			}
			current = next
		}
	}

	return true
}
