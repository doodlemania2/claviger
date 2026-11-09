package oidc

import (
	"context"
	"net/http"
	"time"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
)

func auditCtx(e *core.RequestEvent) context.Context {
	return core.WithAuditContext(context.Background(), core.NewAuditActorFromRecord(e.Auth), e.RealIP())
}

// bindAdminRoutes registers the superuser-only management API used by the
// admin UI. Everything is superuser only (API keys are refused too, because
// these routes are unknown to the key scope check).
func (m *module) bindAdminRoutes(se *core.ServeEvent) {
	g := se.Router.Group("/api/oidc")
	g.Bind(apis.RequireSuperuserAuth())

	g.GET("/status", func(e *core.RequestEvent) error {
		ok, why := enabled(m.app)
		if ok {
			_, why = m.ready()
			ok = why == ""
		}

		return e.JSON(http.StatusOK, map[string]any{
			"enabled":    ok,
			"reason":     why,
			"issuer":     m.issuer(),
			"discovery":  m.issuer() + discoveryPath,
			"encrypted":  encryptionKey(m.app) != "",
			"algorithms": []string{AlgRS256, AlgES256},
		})
	})

	g.POST("/clients", m.createClient)
	g.POST("/clients/{id}/secret", m.rotateClientSecret)

	g.GET("/keys", func(e *core.RequestEvent) error {
		keys, err := m.store.Keys()
		if err != nil {
			return e.InternalServerError("Failed to list the keys.", err)
		}

		return e.JSON(http.StatusOK, keys)
	})

	g.POST("/keys/rotate", func(e *core.RequestEvent) error {
		if ok, why := enabled(m.app); !ok {
			return e.BadRequestError("The OpenID Connect provider is disabled: "+why, nil)
		}

		var body struct {
			Alg        string `json:"alg"`
			GraceHours int    `json:"graceHours"`
		}
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("Invalid request.", err)
		}

		var algs []string
		if body.Alg != "" {
			algs = []string{body.Alg}
		}

		ids, err := m.store.RotateKeys(auditCtx(e), algs, time.Duration(body.GraceHours)*time.Hour)
		if err != nil {
			return e.BadRequestError("Failed to rotate the keys: "+err.Error(), err)
		}

		return e.JSON(http.StatusOK, map[string]any{"created": ids})
	})
}

type clientBody struct {
	ClientID        string   `json:"clientId"`
	Name            string   `json:"name"`
	AuthCollection  string   `json:"authCollection"`
	Public          bool     `json:"public"`
	FirstParty      bool     `json:"firstParty"`
	RequirePKCE     *bool    `json:"requirePkce"`
	RedirectURIs    []string `json:"redirectUris"`
	PostLogoutURIs  []string `json:"postLogoutUris"`
	Scopes          []string `json:"scopes"`
	AccessTokenType string   `json:"accessTokenType"`
	SigningAlg      string   `json:"signingAlg"`
	Org             string   `json:"org"`
	AllowDeviceFlow bool     `json:"allowDeviceFlow"`
}

// createClient registers a client and returns its secret, once. The secret is
// only stored as a hash.
func (m *module) createClient(e *core.RequestEvent) error {
	var body clientBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid request.", err)
	}

	col, err := m.app.FindCachedCollectionByNameOrId(core.CollectionNameOAuthClients)
	if err != nil {
		return e.InternalServerError("", err)
	}

	rec := core.NewRecord(col)

	clientID := body.ClientID
	if clientID == "" {
		clientID = randomToken(15)
	}

	rec.Set("clientId", clientID)
	rec.Set("name", body.Name)
	rec.Set("authCollection", body.AuthCollection)
	rec.Set("public", body.Public)
	rec.Set("firstParty", body.FirstParty)
	rec.Set("requirePkce", body.RequirePKCE == nil || *body.RequirePKCE)
	rec.Set("redirectUris", body.RedirectURIs)
	rec.Set("postLogoutUris", body.PostLogoutURIs)
	rec.Set("scopes", body.Scopes)
	rec.Set("accessTokenType", body.AccessTokenType)
	rec.Set("signingAlg", body.SigningAlg)
	rec.Set("org", body.Org)
	rec.Set("allowDeviceFlow", body.AllowDeviceFlow)

	secret := ""
	if !body.Public {
		secret = randomToken(32)
		rec.Set("secretHash", hashToken(secret))
	}

	if err := m.app.SaveWithContext(auditCtx(e), rec); err != nil {
		return apis.NewBadRequestError("Failed to create the client.", err)
	}

	out := map[string]any{"id": rec.Id, "clientId": clientID, "name": body.Name}
	if secret != "" {
		out["clientSecret"] = secret
	}

	return e.JSON(http.StatusOK, out)
}

// rotateClientSecret replaces the secret of a confidential client and returns
// the new one, once.
func (m *module) rotateClientSecret(e *core.RequestEvent) error {
	rec, err := m.app.FindRecordById(core.CollectionNameOAuthClients, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("", err)
	}

	if rec.GetBool("public") {
		return e.BadRequestError("A public client has no secret.", nil)
	}

	secret := randomToken(32)
	rec.Set("secretHash", hashToken(secret))

	if err := m.app.SaveWithContext(auditCtx(e), rec); err != nil {
		return e.InternalServerError("Failed to save the secret.", err)
	}

	return e.JSON(http.StatusOK, map[string]any{"id": rec.Id, "clientId": rec.GetString("clientId"), "clientSecret": secret})
}
