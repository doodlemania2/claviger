package oidc_test

import (
	"testing"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #79a): org claims and org-bound clients.

var orgScopes = []string{"openid", "profile", "email", "offline_access", "orgs"}

type orgOIDC struct {
	org, archived *core.Record
	member        *core.Record
}

func seedOIDCOrgs(t testing.TB, e *env) *orgOIDC {
	t.Helper()

	orgs, _ := e.app.FindCollectionByNameOrId(core.CollectionNameOrgs)
	members, _ := e.app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)
	roles, _ := e.app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)

	mk := func(name, slug string, archived bool) *core.Record {
		r := core.NewRecord(orgs)
		r.Set("name", name)
		r.Set("slug", slug)
		r.Set("archived", archived)
		if err := e.app.Save(r); err != nil {
			t.Fatal(err)
		}

		return r
	}

	o := &orgOIDC{org: mk("Parish", "parish", false), archived: mk("Closed", "closed", true)}

	tag := core.NewRecord(roles)
	tag.Set("org", o.org.Id)
	tag.Set("name", "youth")
	if err := e.app.Save(tag); err != nil {
		t.Fatal(err)
	}

	add := func(org *core.Record, role string, tags ...string) *core.Record {
		m := core.NewRecord(members)
		m.Set("org", org.Id)
		m.Set("user", e.user().Id)
		m.Set("role", role)
		m.Set("status", "active")
		m.Set("tags", tags)
		if err := e.app.Save(m); err != nil {
			t.Fatal(err)
		}

		return m
	}

	o.member = add(o.org, "admin", tag.Id)
	add(o.archived, "owner")

	return o
}

func TestOrgClaimsWithScopeOrgs(t *testing.T) {
	e := newEnv(t)
	o := seedOIDCOrgs(t, e)

	clientID, secret := e.createClient(map[string]any{"firstParty": true, "scopes": orgScopes})
	b := e.browser()

	tok := e.mustExchange(clientID, secret, b.login(clientID, authOpts{scope: "openid email orgs"}))

	_, idClaims := decodeJWT(t, tok["id_token"].(string))
	_, accessClaims := decodeJWT(t, tok["access_token"].(string))
	status, info := e.userinfo(tok["access_token"].(string))

	if status != 200 {
		t.Fatalf("userinfo: %d", status)
	}

	for name, claims := range map[string]map[string]any{"id token": idClaims, "access token": accessClaims, "userinfo": info} {
		list, _ := claims["orgs"].([]any)
		if len(list) != 1 {
			t.Fatalf("%s: expected exactly the live org, got %v", name, claims["orgs"])
		}

		item := list[0].(map[string]any)
		if item["id"] != o.org.Id || item["slug"] != "parish" || item["role"] != "admin" {
			t.Fatalf("%s: unexpected org %v", name, item)
		}

		if tags, _ := item["tags"].([]any); len(tags) != 1 || tags[0] != "youth" {
			t.Fatalf("%s: unexpected tags %v", name, item["tags"])
		}

		if roles, _ := claims["org_roles"].(map[string]any); roles[o.org.Id] != "admin" {
			t.Fatalf("%s: unexpected org_roles %v", name, claims["org_roles"])
		}

		if _, bound := claims["org_id"]; bound {
			t.Fatalf("%s: an unbound client gets no org_id", name)
		}
	}

	// a role change shows at the next userinfo call
	o.member.Set("role", "viewer")
	if err := e.app.Save(o.member); err != nil {
		t.Fatal(err)
	}

	_, info = e.userinfo(tok["access_token"].(string))
	if list, _ := info["orgs"].([]any); len(list) != 1 || list[0].(map[string]any)["role"] != "viewer" {
		t.Fatalf("expected the new role, got %v", info["orgs"])
	}
}

func TestNoOrgClaimsWithoutTheScope(t *testing.T) {
	e := newEnv(t)
	seedOIDCOrgs(t, e)

	// the client may use the scope, the request does not ask for it
	clientID, secret := e.createClient(map[string]any{"firstParty": true, "scopes": orgScopes})
	tok := e.mustExchange(clientID, secret, e.browser().login(clientID, authOpts{scope: "openid email"}))

	_, idClaims := decodeJWT(t, tok["id_token"].(string))
	_, info := func() (int, map[string]any) { return e.userinfo(tok["access_token"].(string)) }()

	for _, claims := range []map[string]any{idClaims, info} {
		if _, ok := claims["orgs"]; ok {
			t.Fatalf("orgs claim without the scope: %v", claims)
		}
	}

	// a client that does not list the scope cannot get it either
	plainID, plainSecret := e.createClient(map[string]any{"firstParty": true})
	tok = e.mustExchange(plainID, plainSecret, e.browser().login(plainID, authOpts{scope: "openid email orgs"}))
	_, idClaims = decodeJWT(t, tok["id_token"].(string))

	if _, ok := idClaims["orgs"]; ok {
		t.Fatalf("orgs claim for a client without the scope: %v", idClaims)
	}
}

func TestOrgBoundClient(t *testing.T) {
	e := newEnv(t)
	o := seedOIDCOrgs(t, e)

	clientID, secret := e.createClient(map[string]any{"firstParty": true, "scopes": orgScopes, "org": o.org.Id})
	b := e.browser()

	tok := e.mustExchange(clientID, secret, b.login(clientID, authOpts{scope: "openid orgs offline_access"}))
	_, idClaims := decodeJWT(t, tok["id_token"].(string))

	if idClaims["org_id"] != o.org.Id || idClaims["org_slug"] != "parish" || idClaims["org_role"] != "admin" {
		t.Fatalf("unexpected org claims %v", idClaims)
	}

	if tags, _ := idClaims["org_tags"].([]any); len(tags) != 1 || tags[0] != "youth" {
		t.Fatalf("unexpected org_tags %v", idClaims["org_tags"])
	}

	if _, ok := idClaims["orgs"]; ok {
		t.Fatalf("a bound client carries its org only: %v", idClaims)
	}

	// suspended: the access token and the refresh token stop working at once
	o.member.Set("status", "suspended")
	if err := e.app.Save(o.member); err != nil {
		t.Fatal(err)
	}

	if status, _ := e.userinfo(tok["access_token"].(string)); status == 200 {
		t.Fatal("userinfo still works for a suspended member")
	}

	if status, _ := e.refresh(clientID, secret, tok["refresh_token"].(string)); status == 200 {
		t.Fatal("refresh still works for a suspended member")
	}

	// and a new sign-in is refused
	reqID, _ := e.browser().startAuthorize(clientID, authOpts{})
	bb := e.browser()
	token := bb.signInToken(reqID)

	if status, _ := bb.finish(reqID, token); status != 403 {
		t.Fatalf("expected 403 for a suspended member, got %d", status)
	}
}

func TestOrgBoundClientRefusesNonMembers(t *testing.T) {
	e := newEnv(t)
	seedOIDCOrgs(t, e)

	// bound to an org the user does not belong to
	orgs, _ := e.app.FindCollectionByNameOrId(core.CollectionNameOrgs)
	other := core.NewRecord(orgs)
	other.Set("name", "Other")
	other.Set("slug", "other")
	if err := e.app.Save(other); err != nil {
		t.Fatal(err)
	}

	clientID, _ := e.createClient(map[string]any{"firstParty": true, "scopes": orgScopes, "org": other.Id})

	b := e.browser()
	reqID, _ := b.startAuthorize(clientID, authOpts{})
	token := b.signInToken(reqID)

	if status, _ := b.finish(reqID, token); status != 403 {
		t.Fatalf("expected 403, got %d", status)
	}

	// a client bound to an archived org refuses everybody
	closed, _ := e.app.FindRecordById(core.CollectionNameOrgs, other.Id)
	closed.Set("archived", true)
	if err := e.app.Save(closed); err != nil {
		t.Fatal(err)
	}
}

func TestUnboundClientsIgnoreOrgs(t *testing.T) {
	e := newEnv(t)

	// no orgs at all: sign-in and claims are untouched
	clientID, secret := e.createClient(map[string]any{"firstParty": true, "scopes": orgScopes})
	tok := e.mustExchange(clientID, secret, e.browser().login(clientID, authOpts{scope: "openid orgs"}))
	_, idClaims := decodeJWT(t, tok["id_token"].(string))

	if list, ok := idClaims["orgs"].([]any); !ok || len(list) != 0 {
		t.Fatalf("expected an empty orgs list, got %v", idClaims["orgs"])
	}
}
