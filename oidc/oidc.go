// Package oidc makes Claviger an OpenID Connect provider (#71).
//
// Relying parties (Grafana, oauth2-proxy, your own apps) sign users of one
// auth collection in with an authorization code flow plus PKCE. The protocol
// endpoints come from github.com/zitadel/oidc/v3 (pkg/op), mounted under
// /oidc, with a Claviger-owned storage (store.go), signing keys (keys.go),
// hosted sign-in and consent pages (login.go) and an admin API (admin.go).
//
// Nothing here runs until a client is registered: the routes exist, but
// /oidc/authorize rejects unknown clients, and there is no background work.
// The provider refuses to run (a log line, no routes) outside dev mode when the
// settings encryption env is not set, because it would have to store its
// signing keys in clear text.
//
// Register is the only entry point the upstream-owned code calls.
package oidc

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

func envGet(name string) string { return os.Getenv(name) }

// Pathing. The issuer is <app url>/oidc.
const (
	mountPath     = "/oidc"
	discoveryPath = "/.well-known/openid-configuration"
)

// module is the provider attached to one app.
type module struct {
	app   core.App
	store *Store

	limMu    sync.Mutex // guards the creation of the rate limiters
	mu       sync.Mutex
	provider *op.Provider
	handler  http.Handler
	devKey   [32]byte // crypto key when there is no encryption env (dev mode), per process
}

// Register wires the provider into the app. It binds the hooks that keep the
// secrets out of the records API, and the routes (in OnServe).
func Register(app core.App) {
	m := &module{app: app, store: NewStore(app)}

	m.bindRecordHooks()

	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id:       "__clavigerOIDC__",
		Priority: 9990,
		Func: func(se *core.ServeEvent) error {
			m.bindRoutes(se)

			return se.Next()
		},
	})
}

// issuer is <app url>/oidc.
func (m *module) issuer() string {
	return strings.TrimRight(m.app.Settings().Meta.AppURL, "/") + mountPath
}

func (m *module) secureCookies() bool { return strings.HasPrefix(m.issuer(), "https://") }

// cryptoKey is the key of the library's own encryption (authorization codes,
// opaque access tokens). It is derived from the settings encryption key, so it
// survives restarts and is shared by replicas; without that key (dev mode) it
// is random per process.
func (m *module) cryptoKey() [32]byte {
	if key := encryptionKey(m.app); key != "" {
		return sha256.Sum256([]byte("claviger-oidc-crypto-v1\x00" + key))
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.devKey == ([32]byte{}) {
		_, _ = rand.Read(m.devKey[:])
	}

	return m.devKey
}

// loopbackURL reports whether the URL is http on a loopback host.
func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}

	h := u.Hostname()

	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// ready builds the provider on first use and reports why it cannot run.
func (m *module) ready() (http.Handler, string) {
	if ok, why := enabled(m.app); !ok {
		return nil, why
	}

	appURL := m.app.Settings().Meta.AppURL
	if strings.HasPrefix(appURL, "http://") && !loopbackURL(appURL) && !m.app.IsDev() {
		return nil, "the application URL (Settings, Application) must be https"
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.handler != nil {
		return m.handler, ""
	}

	// an http issuer got past the check above: loopback or dev mode
	opts := []op.Option{}
	if strings.HasPrefix(appURL, "http://") {
		opts = append(opts, op.WithAllowInsecure())
	}

	provider, err := op.NewProvider(&op.Config{
		CryptoKey:                m.cryptoKey(),
		DefaultLogoutRedirectURI: mountPath + "/logged-out",
		CodeMethodS256:           true,
		AuthMethodPost:           true,
		GrantTypeRefreshToken:    true,
		SupportedClaims:          SupportedClaims,
		SupportedScopes:          []string{oidc.ScopeOpenID, oidc.ScopeProfile, oidc.ScopeEmail, oidc.ScopeOfflineAccess, ScopeOrgs},
	}, m.store, func(bool) (op.IssuerFromRequest, error) {
		return func(*http.Request) string { return m.issuer() }, nil
	}, opts...)
	if err != nil {
		return nil, "failed to initialise the provider: " + err.Error()
	}

	m.provider = provider
	m.handler = http.StripPrefix(mountPath, provider)

	return m.handler, ""
}

// clientCount is the number of registered clients (0 when the table is unavailable).
func (m *module) clientCount() int {
	var n int
	err := m.app.ConcurrentDB().NewQuery("SELECT COUNT(*) FROM {{" + core.CollectionNameOAuthClients + "}}").Row(&n)
	if err != nil {
		return 0
	}

	return n
}

func (m *module) bindRoutes(se *core.ServeEvent) {
	// status and key/client management work even while the provider is off
	m.bindAdminRoutes(se)

	if ok, why := enabled(m.app); !ok {
		msg := "OpenID Connect provider is disabled: " + why
		if m.clientCount() > 0 {
			m.app.Logger().Warn(msg)
		} else {
			m.app.Logger().Info(msg)
		}

		return
	}

	if _, why := m.ready(); why != "" {
		m.app.Logger().Warn("OpenID Connect provider is disabled: " + why)

		return
	}

	// the root discovery URL clients try first
	se.Router.GET(discoveryPath, func(e *core.RequestEvent) error {
		return e.Redirect(http.StatusFound, mountPath+discoveryPath)
	})

	m.bindPages(se)

	se.Router.GET(mountPath+discoveryPath, m.serveDiscovery)

	// the callback only resumes the browser that started the sign-in
	callback := func(e *core.RequestEvent) error {
		req, err := m.store.requestByID(e.Request.FormValue("id"))
		if err != nil || !m.checkBinding(e, req) {
			return m.errorPage(e, http.StatusBadRequest, "This sign-in could not be resumed. Return to the app and start again.")
		}

		return m.serveLibrary(e)
	}

	// Explicit methods, never Router.Any: a pattern without a method conflicts
	// with the "GET /{path...}" catch-all that serves the public directory
	// ("matches more methods ... but has a more specific path") and makes the
	// mux panic when the server starts.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodOptions} {
		// the methods the library answers (a GET pattern also serves HEAD)
		se.Router.Route(method, mountPath+"/authorize/callback", callback)
		se.Router.Route(method, mountPath+"/{path...}", m.serveLibrary)
	}
}

// serveLibrary hands the request to the zitadel provider.
func (m *module) serveLibrary(e *core.RequestEvent) error {
	handler, why := m.ready()
	if handler == nil {
		return e.NotFoundError(why, nil)
	}

	done, after := m.limited(e)
	if done {
		return nil
	}

	r := m.withSSO(m.withClientAlg(e.Request))

	if strings.TrimSuffix(r.URL.Path, "/") == mountPath+"/end_session" && ssoCookieValue(r) != "" {
		// logging out of the sign-in page forgets the browser's session
		m.clearSSOCookie(e)
	}

	if after == nil {
		handler.ServeHTTP(e.Response, r)

		return nil
	}

	rec := &statusRecorder{ResponseWriter: e.Response}
	handler.ServeHTTP(rec, r)
	after(rec.status)

	return nil
}

// withSSO hands the single sign-on cookie of the browser to the storage (the
// library calls it with the request context only).
func (m *module) withSSO(r *http.Request) *http.Request {
	if v := ssoCookieValue(r); v != "" {
		return r.WithContext(withSSO(r.Context(), v))
	}

	return r
}

// withClientAlg tells the token endpoint which signing algorithm the calling
// client wants (the library asks the storage for "the" signing key without
// saying which client it signs for).
func (m *module) withClientAlg(r *http.Request) *http.Request {
	if r.Method != http.MethodPost || r.URL.Path != mountPath+"/oauth/token" || r.Body == nil {
		return r
	}

	body, err := readAndRestore(r)
	if err != nil {
		return r
	}

	clientID, _, ok := r.BasicAuth()
	if ok {
		clientID, _ = url.QueryUnescape(clientID)
	} else if form, perr := url.ParseQuery(string(body)); perr == nil {
		clientID = form.Get("client_id")
	}

	if c, err := m.store.findClient(clientID); err == nil {
		return r.WithContext(withAlg(r.Context(), c.Alg))
	}

	return r
}

// readAndRestore reads (at most 64 KiB of) the request body and puts it back.
func readAndRestore(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyForAuth))
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))

	return body, err
}

// bufferedResponse captures a handler's response so the discovery document
// can be edited.
type bufferedResponse struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufferedResponse) WriteHeader(code int)        { b.code = code }

// serveDiscovery serves the library's discovery document minus the things
// this provider does not do: the implicit flow (the library advertises it
// unconditionally), id_token response types and the device flow.
func (m *module) serveDiscovery(e *core.RequestEvent) error {
	handler, why := m.ready()
	if handler == nil {
		return e.NotFoundError(why, nil)
	}

	buf := &bufferedResponse{header: http.Header{}, code: http.StatusOK}
	handler.ServeHTTP(buf, e.Request)

	doc := map[string]any{}
	if buf.code != http.StatusOK || json.Unmarshal(buf.body.Bytes(), &doc) != nil {
		return e.InternalServerError("Failed to build the discovery document.", nil)
	}

	doc["response_types_supported"] = []string{"code"}
	doc["grant_types_supported"] = []string{"authorization_code", "refresh_token"}
	doc["id_token_signing_alg_values_supported"] = []string{"RS256", "ES256"}
	doc["claims_parameter_supported"] = false
	doc["request_uri_parameter_supported"] = false

	delete(doc, "device_authorization_endpoint")
	delete(doc, "check_session_iframe")

	e.Response.Header().Set("Cache-Control", "public, max-age=300")

	return e.JSON(http.StatusOK, doc)
}
