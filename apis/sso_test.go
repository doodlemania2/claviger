package apis_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/sso"
)

func (e *ssoEnv) discover(email string) map[string]any {
	e.t.Helper()

	return e.must(e.browser().do(http.MethodPost, "/api/sso/discover", `{"email":"`+email+`"}`, nil), 200)
}

func (e *ssoEnv) userByEmail(email string) *core.Record {
	rec, err := e.app.FindAuthRecordByEmail("users", email)
	if err != nil {
		return nil
	}

	return rec
}

func (e *ssoEnv) seedUser(email string, verified bool) *core.Record {
	e.t.Helper()

	users, err := e.app.FindCollectionByNameOrId("users")
	orgMust(e.t, err)

	r := core.NewRecord(users)
	r.SetEmail(email)
	r.SetVerified(verified)
	r.SetPassword("password123")
	orgMust(e.t, e.app.Save(r))

	return r
}

func (e *ssoEnv) externalAuths(connID string) int {
	n, err := e.app.CountRecords(core.CollectionNameExternalAuths, nil)
	orgMust(e.t, err)

	if connID == "" {
		return int(n)
	}

	recs, err := e.app.FindAllRecords(core.CollectionNameExternalAuths)
	orgMust(e.t, err)

	c := 0

	for _, r := range recs {
		if r.GetString("provider") == sso.ExternalAuthProvider(connID) {
			c++
		}
	}

	return c
}

func TestSSODomainVerificationAndRouting(t *testing.T) {
	env := newSSOEnv(t)
	connID, domainID := env.setupConnection(ssoSetup{Enabled: true, JIT: true})

	t.Run("an unverified domain is never routed", func(t *testing.T) {
		if got := env.discover("sam@" + ssoDomain); got["sso"] != false {
			t.Fatalf("a pending claim must not route: %v", got)
		}
	})

	t.Run("the claim shows the record to publish", func(t *testing.T) {
		list := env.must(env.admin(http.MethodGet, "/api/orgs/"+orgP1+"/sso/domains", ""), 200)
		items, _ := list["items"].([]any)

		if len(items) != 1 {
			t.Fatalf("expected one claim: %v", list)
		}

		ver, _ := items[0].(map[string]any)["verification"].(map[string]any)
		if !strings.HasPrefix(ver["value"].(string), "claviger-verification=") || ver["name"] != ssoDomain || items[0].(map[string]any)["domain"] != ssoDomain {
			t.Fatalf("unexpected verification block: %v", items[0])
		}
	})

	t.Run("a wrong or missing TXT record does not verify", func(t *testing.T) {
		got := env.must(env.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/domains/"+domainID+"/verify", ""), 200)
		if got["verified"] != false || got["status"] != "pending" {
			t.Fatalf("no record yet: %v", got)
		}

		env.dns[ssoDomain] = []string{"claviger-verification=not-the-token"}

		got = env.must(env.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/domains/"+domainID+"/verify", ""), 200)
		if got["verified"] != false {
			t.Fatalf("a wrong token must not verify: %v", got)
		}

		if got := env.discover("sam@" + ssoDomain); got["sso"] != false {
			t.Fatalf("still not routed: %v", got)
		}
	})

	t.Run("another organization cannot verify with someone else's record, nor take a verified domain", func(t *testing.T) {
		// carol is admin of P2: her claim has its own token
		conn := env.must(env.as("carol", http.MethodPost, "/api/orgs/"+orgP2+"/sso/connections",
			`{"name":"P2 IdP","issuer":"`+env.idp.srv.URL+`","clientId":"c","clientSecret":"s"}`), 200)
		dom := env.must(env.as("carol", http.MethodPost, "/api/orgs/"+orgP2+"/sso/domains",
			`{"domain":"`+ssoDomain+`","connection":"`+conn["id"].(string)+`"}`), 200)

		got := env.must(env.as("carol", http.MethodPost, "/api/orgs/"+orgP2+"/sso/domains/"+dom["id"].(string)+"/verify", ""), 200)
		if got["verified"] != false {
			t.Fatalf("P1's token is not P2's: %v", got)
		}
	})

	t.Run("the TXT record at the apex verifies, and the domain then routes", func(t *testing.T) {
		env.publishTXT(domainID, ssoDomain)

		got := env.must(env.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/domains/"+domainID+"/verify", ""), 200)
		if got["verified"] != true || got["status"] != "verified" {
			t.Fatalf("expected verified: %v", got)
		}

		route := env.discover("Someone@" + strings.ToUpper(ssoDomain))
		if route["sso"] != true || route["connection"] != connID || route["collection"] != "users" ||
			route["url"] != "/api/sso/start?connection="+connID {
			t.Fatalf("expected a route: %v", route)
		}

		// another domain still goes the normal way
		if got := env.discover("sam@other.example"); got["sso"] != false {
			t.Fatalf("other domains are not routed: %v", got)
		}

		if got := env.discover("not-an-address"); got["sso"] != false {
			t.Fatalf("not an address: %v", got)
		}
	})

	t.Run("a second organization cannot verify a domain that is already verified", func(t *testing.T) {
		recs, err := env.app.FindRecordsByFilter(core.CollectionNameSSODomains, "org={:o}", "", 0, 0, map[string]any{"o": orgP2})
		orgMust(t, err)

		env.dns[ssoDomain] = append(env.dns[ssoDomain], core.SSOVerificationPrefix+recs[0].GetString("token"))

		rec := env.as("carol", http.MethodPost, "/api/orgs/"+orgP2+"/sso/domains/"+recs[0].Id+"/verify", "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("expected 409 domain_taken, got %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("the verification is audited", func(t *testing.T) {
		if got := auditEntries(t, env.app, "action='sso_domain_verify'"); len(got) != 1 {
			t.Fatalf("expected one sso_domain_verify entry, got %d", len(got))
		}
	})

	t.Run("a disabled connection does not route", func(t *testing.T) {
		env.must(env.admin(http.MethodPatch, "/api/orgs/"+orgP1+"/sso/connections/"+connID, `{"enabled":false}`), 200)

		if got := env.discover("sam@" + ssoDomain); got["sso"] != false {
			t.Fatalf("disabled connections do not route: %v", got)
		}
	})
}

func TestSSODomainChallengeLabelVerifies(t *testing.T) {
	env := newSSOEnv(t)
	_, domainID := env.setupConnection(ssoSetup{Enabled: true})

	env.publishTXT(domainID, "_claviger."+ssoDomain)

	got := env.must(env.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/domains/"+domainID+"/verify", ""), 200)
	if got["verified"] != true {
		t.Fatalf("the _claviger label must verify: %v", got)
	}
}

func TestSSOSignInFlows(t *testing.T) {
	cases := []struct {
		name  string
		setup ssoSetup
		seed  func(e *ssoEnv)
		// the identity the provider asserts
		claims map[string]any
		// expected
		wantErr    string
		wantCreate bool
		wantLink   bool
		wantRole   string
		check      func(t *testing.T, e *ssoEnv, connID string)
	}{
		{
			name:   "first sign-in creates the account and the membership (JIT)",
			setup:  ssoSetup{Enabled: true, JIT: true, Verified: true},
			claims: idClaims("sub-new", "new@"+ssoDomain, true, nil), wantCreate: true, wantRole: core.OrgRoleMember,
			check: func(t *testing.T, e *ssoEnv, connID string) {
				u := e.userByEmail("new@" + ssoDomain)
				if u == nil || !u.Verified() || u.GetString("name") != "Test sub-new" && u.Collection().Fields.GetByName("name") != nil {
					t.Fatalf("unexpected account: %v", u)
				}

				m, err := core.FindOrgMember(e.app, orgP1, u.Id)
				orgMust(t, err)

				if m.GetString("source") != "sso" || m.GetString("status") != "active" {
					t.Fatalf("unexpected membership: %v", m.PublicExport())
				}

				if e.externalAuths(connID) != 1 {
					t.Fatalf("expected one link")
				}

				if len(auditEntries(t, e.app, "action='sso_jit_create'")) != 1 || len(auditEntries(t, e.app, "action='sso_login'")) != 1 {
					t.Fatalf("expected sso_jit_create and sso_login entries")
				}
			},
		},
		{
			name:   "JIT off: no account is created",
			setup:  ssoSetup{Enabled: true, JIT: false, Verified: true},
			claims: idClaims("sub-x", "nobody@"+ssoDomain, true, nil), wantErr: "no_account",
			check: func(t *testing.T, e *ssoEnv, _ string) {
				if e.userByEmail("nobody@"+ssoDomain) != nil {
					t.Fatalf("no account may be created")
				}
			},
		},
		{
			name:   "an existing account is linked by a verified email",
			setup:  ssoSetup{Enabled: true, JIT: true, Verified: true},
			seed:   func(e *ssoEnv) { e.seedUser("ann@"+ssoDomain, true) },
			claims: idClaims("sub-ann", "ann@"+ssoDomain, true, nil), wantLink: true, wantRole: core.OrgRoleMember,
			check: func(t *testing.T, e *ssoEnv, connID string) {
				if e.externalAuths(connID) != 1 || len(auditEntries(t, e.app, "action='sso_link'")) != 1 {
					t.Fatalf("expected one link and one sso_link entry")
				}
			},
		},
		{
			name:   "an unverified email is not linked to an existing account",
			setup:  ssoSetup{Enabled: true, JIT: true, Verified: true},
			seed:   func(e *ssoEnv) { e.seedUser("vic@"+ssoDomain, true) },
			claims: idClaims("sub-vic", "vic@"+ssoDomain, false, nil), wantErr: "email_unverified",
			check: func(t *testing.T, e *ssoEnv, connID string) {
				if e.externalAuths(connID) != 0 {
					t.Fatalf("nothing may be linked")
				}

				if _, err := core.FindOrgMember(e.app, orgP1, e.userByEmail("vic@"+ssoDomain).Id); err == nil {
					t.Fatalf("no membership may be created for a refused sign-in")
				}

				if len(auditEntries(t, e.app, "action='sso_login_failed'")) != 1 {
					t.Fatalf("a refused sign-in is audited")
				}
			},
		},
		{
			name:     "Entra's xms_edov counts as a verified address",
			setup:    ssoSetup{Enabled: true, JIT: true, Verified: true},
			seed:     func(e *ssoEnv) { e.seedUser("eve@"+ssoDomain, true) },
			claims:   idClaims("sub-eve", "eve@"+ssoDomain, false, map[string]any{"xms_edov": true, "email_verified": nil}),
			wantLink: true, wantRole: core.OrgRoleMember,
		},
		{
			name:   "an email at a domain the connection does not hold is refused",
			setup:  ssoSetup{Enabled: true, JIT: true, Verified: true},
			seed:   func(e *ssoEnv) { e.seedUser("ceo@other.org", true) },
			claims: idClaims("sub-ceo", "ceo@other.org", true, nil), wantErr: "domain_not_verified",
			check: func(t *testing.T, e *ssoEnv, connID string) {
				if e.externalAuths(connID) != 0 {
					t.Fatalf("nothing may be linked")
				}
			},
		},
		{
			name: "groups map to the highest org role",
			setup: ssoSetup{Enabled: true, JIT: true, Verified: true, GroupsClaim: "groups",
				RoleMapping: map[string]string{"staff": "member", "leads": "admin", "readers": "viewer"}},
			claims:     idClaims("sub-grp", "grp@"+ssoDomain, true, map[string]any{"groups": []any{"readers", "leads", "unrelated"}}),
			wantCreate: true, wantRole: core.OrgRoleAdmin,
		},
		{
			name: "no matching group falls back to the default role",
			setup: ssoSetup{Enabled: true, JIT: true, Verified: true, DefaultRole: "viewer",
				RoleMapping: map[string]string{"leads": "admin"}},
			claims:     idClaims("sub-def", "def@"+ssoDomain, true, map[string]any{"groups": []any{"other"}}),
			wantCreate: true, wantRole: core.OrgRoleViewer,
		},
		{
			name: "a nested claim path works as the groups claim",
			setup: ssoSetup{Enabled: true, JIT: true, Verified: true, GroupsClaim: "realm_access.roles",
				RoleMapping: map[string]string{"admins": "admin"}},
			claims:     idClaims("sub-nest", "nest@"+ssoDomain, true, map[string]any{"realm_access": map[string]any{"roles": []any{"admins"}}}),
			wantCreate: true, wantRole: core.OrgRoleAdmin,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newSSOEnv(t)
			connID, _ := env.setupConnection(c.setup)

			if c.seed != nil {
				c.seed(env)
			}

			b := env.browser()
			back := env.signIn(b, connID, c.claims)

			if c.wantErr != "" {
				if back.Query().Get("sso_error") != c.wantErr || back.Query().Get("sso_code") != "" {
					t.Fatalf("expected sso_error=%s, got %s", c.wantErr, back)
				}
			} else {
				if back.Path != "/oidc/login" || back.Query().Get("request") != "abc" || back.Query().Get("sso_code") == "" {
					t.Fatalf("expected a return with a code, got %s", back)
				}

				ex := b.do(http.MethodPost, "/api/sso/exchange", `{"code":"`+back.Query().Get("sso_code")+`"}`, nil)
				got := env.must(ex, 200)

				rec, _ := got["record"].(map[string]any)
				email, _ := c.claims["email"].(string)

				u := env.userByEmail(email)
				if u == nil || got["token"] == "" || rec["id"] != u.Id {
					t.Fatalf("expected the auth response of %s: %v", email, got)
				}

				m, err := core.FindOrgMember(env.app, orgP1, u.Id)
				orgMust(t, err)

				if m.GetString("role") != c.wantRole {
					t.Fatalf("expected role %s, got %s", c.wantRole, m.GetString("role"))
				}

				if c.wantCreate && u.Verified() != (c.claims["email_verified"] == true) {
					t.Fatalf("the verified flag follows the provider")
				}
			}

			if c.check != nil {
				c.check(t, env, connID)
			}
		})
	}
}

func TestSSORoleFollowsTheMappingOnlyForSSOMembers(t *testing.T) {
	env := newSSOEnv(t)
	connID, _ := env.setupConnection(ssoSetup{Enabled: true, JIT: true, Verified: true,
		RoleMapping: map[string]string{"leads": "admin"}})

	login := func(sub, email string, groups []any) {
		b := env.browser()
		back := env.signIn(b, connID, idClaims(sub, email, true, map[string]any{"groups": groups}))

		if back.Query().Get("sso_code") == "" {
			t.Fatalf("expected a code: %s", back)
		}
	}

	role := func(email string) string {
		m, err := core.FindOrgMember(env.app, orgP1, env.userByEmail(email).Id)
		orgMust(t, err)

		return m.GetString("role")
	}

	login("s1", "rolly@"+ssoDomain, []any{"leads"})
	if role("rolly@"+ssoDomain) != "admin" {
		t.Fatalf("expected admin")
	}

	login("s1", "rolly@"+ssoDomain, []any{})
	if role("rolly@"+ssoDomain) != "member" {
		t.Fatalf("an SSO member loses a role the mapping no longer gives")
	}

	if len(auditEntries(t, env.app, "action='sso_role_sync'")) != 1 {
		t.Fatalf("the demotion is audited")
	}

	// a member added by hand keeps the role the admins gave
	u := env.seedUser("manual@"+ssoDomain, true)
	_, err := core.AddOrgMember(env.app, context.Background(), &core.OrgActor{System: true}, ssoOrgP1(t, env), core.OrgMemberInput{User: u.Id, Role: "viewer"})
	orgMust(t, err)

	login("s2", "manual@"+ssoDomain, []any{"leads"})
	if role("manual@"+ssoDomain) != "viewer" {
		t.Fatalf("a manual member keeps their role")
	}
}

func ssoOrgP1(t testing.TB, env *ssoEnv) *core.Record {
	org, err := core.FindOrg(env.app, orgP1)
	orgMust(t, err)

	return org
}

func TestSSOLinkedAccountSurvivesAnEmailChange(t *testing.T) {
	env := newSSOEnv(t)
	connID, _ := env.setupConnection(ssoSetup{Enabled: true, JIT: true, Verified: true})

	first := env.signIn(env.browser(), connID, idClaims("same-sub", "first@"+ssoDomain, true, nil))
	if first.Query().Get("sso_code") == "" {
		t.Fatalf("expected a code")
	}

	// the provider now says another address for the same subject: still the same account
	second := env.signIn(env.browser(), connID, idClaims("same-sub", "renamed@"+ssoDomain, true, nil))
	if second.Query().Get("sso_code") == "" {
		t.Fatalf("expected a code: %s", second)
	}

	if env.userByEmail("renamed@"+ssoDomain) != nil || env.externalAuths(connID) != 1 {
		t.Fatalf("the subject identifies the person, not the address")
	}
}

func TestSSOTakesOverAnUnverifiedPreRegisteredAccount(t *testing.T) {
	env := newSSOEnv(t)
	connID, _ := env.setupConnection(ssoSetup{Enabled: true, JIT: true, Verified: true})

	squatter := env.seedUser("squat@"+ssoDomain, false) // someone registered the address before its owner
	before := squatter.TokenKey()

	back := env.signIn(env.browser(), connID, idClaims("sub-sq", "squat@"+ssoDomain, true, nil))
	if back.Query().Get("sso_code") == "" {
		t.Fatalf("expected a code: %s", back)
	}

	after, err := env.app.FindRecordById("users", squatter.Id)
	orgMust(t, err)

	if !after.Verified() || after.TokenKey() == before || after.ValidatePassword("password123") {
		t.Fatalf("the squatter's password and tokens must be gone")
	}
}

func TestSSOCallbackSecurity(t *testing.T) {
	startAt := func(e *ssoEnv, b *ssoBrowser, connID string) (code, state string) {
		start := b.do(http.MethodGet, "/api/sso/start?connection="+connID+"&returnTo=/oidc/login", "", nil)
		if start.Code != http.StatusFound {
			t.Fatalf("start: %d", start.Code)
		}

		return e.idp.approve(start.Header().Get("Location"), idClaims("sub-sec", "sec@"+ssoDomain, true, nil), false)
	}

	t.Run("another browser cannot complete a sign-in", func(t *testing.T) {
		env := newSSOEnv(t)
		connID, _ := env.setupConnection(ssoSetup{Enabled: true, JIT: true, Verified: true})

		code, state := startAt(env, env.browser(), connID)

		back := env.callback(env.browser(), code, state) // no binding cookie
		if back.Query().Get("sso_error") != "other_browser" {
			t.Fatalf("expected other_browser: %s", back)
		}

		if env.userByEmail("sec@"+ssoDomain) != nil {
			t.Fatalf("no account may be created")
		}
	})

	t.Run("a state is used once", func(t *testing.T) {
		env := newSSOEnv(t)
		connID, _ := env.setupConnection(ssoSetup{Enabled: true, JIT: true, Verified: true})

		b := env.browser()
		code, state := startAt(env, b, connID)
		saved := b.cookies["claviger_sso_bind"]

		if env.callback(b, code, state).Query().Get("sso_code") == "" {
			t.Fatalf("the first callback works")
		}

		b.cookies["claviger_sso_bind"] = saved

		rec := b.do(http.MethodGet, "/api/sso/callback?code="+code+"&state="+url.QueryEscape(state), "", nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("a replayed state has nowhere to go: %d", rec.Code)
		}
	})

	t.Run("a made-up state is refused", func(t *testing.T) {
		env := newSSOEnv(t)
		rec := env.browser().do(http.MethodGet, "/api/sso/callback?code=x&state=nope", "", nil)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("the exchange code works once and only with the binding cookie", func(t *testing.T) {
		env := newSSOEnv(t)
		connID, _ := env.setupConnection(ssoSetup{Enabled: true, JIT: true, Verified: true})

		// a code carried to another browser is useless (and spent)
		b := env.browser()
		code, state := startAt(env, b, connID)
		sc := env.callback(b, code, state).Query().Get("sso_code")

		if rec := env.browser().do(http.MethodPost, "/api/sso/exchange", `{"code":"`+sc+`"}`, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("a code without the binding cookie must fail: %d", rec.Code)
		}

		// the right browser exchanges once
		b2 := env.browser()
		code, state = startAt(env, b2, connID)
		sc = env.callback(b2, code, state).Query().Get("sso_code")

		if rec := b2.do(http.MethodPost, "/api/sso/exchange", `{"code":"`+sc+`"}`, nil); rec.Code != 200 {
			t.Fatalf("the right browser exchanges once: %d %s", rec.Code, rec.Body.String())
		}

		if rec := b2.do(http.MethodPost, "/api/sso/exchange", `{"code":"`+sc+`"}`, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("a second exchange must fail: %d", rec.Code)
		}

		if rec := b2.do(http.MethodPost, "/api/sso/exchange", `{"code":"made-up"}`, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("a made-up code must fail: %d", rec.Code)
		}
	})

	t.Run("an ID token that is not signed by the provider's key is refused", func(t *testing.T) {
		env := newSSOEnv(t)
		connID, _ := env.setupConnection(ssoSetup{Enabled: true, JIT: true, Verified: true})

		b := env.browser()
		start := b.do(http.MethodGet, "/api/sso/start?connection="+connID+"&returnTo=/oidc/login", "", nil)
		code, state := env.idp.approve(start.Header().Get("Location"), idClaims("s", "forged@"+ssoDomain, true, nil), true)

		back := env.callback(b, code, state)
		if back.Query().Get("sso_error") != "invalid_token" || env.userByEmail("forged@"+ssoDomain) != nil {
			t.Fatalf("a forged token must be refused: %s", back)
		}
	})

	t.Run("claims that do not hold are refused", func(t *testing.T) {
		for name, claims := range map[string]map[string]any{
			"wrong audience": idClaims("s", "a@"+ssoDomain, true, map[string]any{"aud": "someone-else"}),
			"wrong issuer":   idClaims("s", "a@"+ssoDomain, true, map[string]any{"iss": "https://evil.example"}),
			"expired":        idClaims("s", "a@"+ssoDomain, true, map[string]any{"exp": 1000}),
			"wrong nonce":    idClaims("s", "a@"+ssoDomain, true, map[string]any{"nonce": "replayed"}),
			"no subject":     idClaims("s", "a@"+ssoDomain, true, map[string]any{"sub": nil}),
		} {
			t.Run(name, func(t *testing.T) {
				env := newSSOEnv(t)
				connID, _ := env.setupConnection(ssoSetup{Enabled: true, JIT: true, Verified: true})

				back := env.signIn(env.browser(), connID, claims)
				if back.Query().Get("sso_error") != "invalid_token" || env.userByEmail("a@"+ssoDomain) != nil {
					t.Fatalf("expected invalid_token: %s", back)
				}
			})
		}
	})

	t.Run("a connection that is disabled or unknown cannot start", func(t *testing.T) {
		env := newSSOEnv(t)
		connID, _ := env.setupConnection(ssoSetup{Enabled: false, JIT: true, Verified: true})

		for _, id := range []string{connID, "nonexistent"} {
			rec := env.browser().do(http.MethodGet, "/api/sso/start?connection="+id+"&returnTo=/oidc/login", "", nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected 404 for %s, got %d", id, rec.Code)
			}
		}
	})

	t.Run("returnTo must stay on this server", func(t *testing.T) {
		env := newSSOEnv(t)
		connID, _ := env.setupConnection(ssoSetup{Enabled: true, JIT: true, Verified: true})

		for _, bad := range []string{"", "https://evil.example/x", "//evil.example/x", "javascript:alert(1)", "/\\evil.example"} {
			rec := env.browser().do(http.MethodGet, "/api/sso/start?connection="+connID+"&returnTo="+url.QueryEscape(bad), "", nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("returnTo %q must be refused, got %d", bad, rec.Code)
			}
		}

		ok := env.browser().do(http.MethodGet, "/api/sso/start?connection="+connID+"&returnTo="+url.QueryEscape(ssoAppURL+"/passkeys"), "", nil)
		if ok.Code != http.StatusFound {
			t.Fatalf("an absolute URL on the app origin is fine: %d", ok.Code)
		}
	})

	t.Run("a login hint of another domain is not forwarded", func(t *testing.T) {
		env := newSSOEnv(t)
		connID, _ := env.setupConnection(ssoSetup{Enabled: true, JIT: true, Verified: true})

		rec := env.browser().do(http.MethodGet, "/api/sso/start?connection="+connID+"&returnTo=/oidc/login&login_hint=ceo@other.org", "", nil)
		if strings.Contains(rec.Header().Get("Location"), "login_hint") {
			t.Fatalf("the hint must be dropped: %s", rec.Header().Get("Location"))
		}

		rec = env.browser().do(http.MethodGet, "/api/sso/start?connection="+connID+"&returnTo=/oidc/login&login_hint=me@"+ssoDomain, "", nil)
		if !strings.Contains(rec.Header().Get("Location"), "login_hint=me%40"+ssoDomain) {
			t.Fatalf("a hint of the connection's domain is forwarded: %s", rec.Header().Get("Location"))
		}
	})
}

func TestSSORequiredBlocksPasswordSignInForThatDomainOnly(t *testing.T) {
	passwordLogin := func(env *ssoEnv, email string) (int, map[string]any) {
		rec := env.browser().do(http.MethodPost, "/api/collections/users/auth-with-password",
			`{"identity":"`+email+`","password":"password123"}`, nil)

		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)

		return rec.Code, out
	}

	env := newSSOEnv(t)
	connID, _ := env.setupConnection(ssoSetup{Enabled: true, JIT: true, Verified: true, SSORequired: true})
	env.seedUser("pat@"+ssoDomain, true)

	if code, body := passwordLogin(env, "pat@"+ssoDomain); code != http.StatusForbidden || body["data"].(map[string]any)["sso"].(map[string]any)["code"] != "sso_required" {
		t.Fatalf("the password sign-in of the claimed domain must be refused: %d %v", code, body)
	}

	if code, _ := passwordLogin(env, "alice@orgs.test"); code != 200 {
		t.Fatalf("another domain signs in with a password as before: %d", code)
	}

	if len(auditEntries(t, env.app, "action='sso_password_blocked'")) != 1 {
		t.Fatalf("the refusal is audited")
	}

	// the discovery tells the page to hide the password
	if got := env.discover("pat@" + ssoDomain); got["required"] != true {
		t.Fatalf("expected required: %v", got)
	}

	// a connection that is turned off does not lock anyone out
	env.must(env.admin(http.MethodPatch, "/api/orgs/"+orgP1+"/sso/connections/"+connID, `{"enabled":false}`), 200)

	if code, _ := passwordLogin(env, "pat@"+ssoDomain); code != 200 {
		t.Fatalf("a disabled connection must not require SSO: %d", code)
	}

	// and so does one that does not require it
	env.must(env.admin(http.MethodPatch, "/api/orgs/"+orgP1+"/sso/connections/"+connID, `{"enabled":true,"ssoRequired":false}`), 200)

	if code, _ := passwordLogin(env, "pat@"+ssoDomain); code != 200 {
		t.Fatalf("expected the password to work when SSO is optional: %d", code)
	}

	// SSO itself still signs the person in while it is required
	env.must(env.admin(http.MethodPatch, "/api/orgs/"+orgP1+"/sso/connections/"+connID, `{"ssoRequired":true}`), 200)

	back := env.signIn(env.browser(), connID, idClaims("sub-pat", "pat@"+ssoDomain, true, nil))
	if back.Query().Get("sso_code") == "" {
		t.Fatalf("SSO sign-in works for a required domain: %s", back)
	}
}

func TestSSOManagementRoutes(t *testing.T) {
	env := newSSOEnv(t)

	conn := `{"name":"Okta","issuer":"` + "https://dev-1.okta.example/oauth2/default/.well-known/openid-configuration" + `","clientId":"abc","clientSecret":"shh-secret","scopes":["openid","email","groups"]}`

	t.Run("a plain member cannot manage", func(t *testing.T) {
		env.must(env.as("carol", http.MethodGet, "/api/orgs/"+orgP1+"/sso/connections", ""), 403)
		env.must(env.as("carol", http.MethodPost, "/api/orgs/"+orgP1+"/sso/connections", conn), 403)
	})

	t.Run("a stranger sees nothing", func(t *testing.T) {
		env.must(env.as("frank", http.MethodGet, "/api/orgs/"+orgP1+"/sso/connections", ""), 404)
	})

	t.Run("an owner creates a connection; the secret is never returned", func(t *testing.T) {
		rec := env.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/connections", conn)
		got := env.must(rec, 200)

		if got["issuer"] != "https://dev-1.okta.example/oauth2/default" || got["hasSecret"] != true || got["jit"] != true || got["enabled"] != false {
			t.Fatalf("unexpected connection: %v", got)
		}

		if strings.Contains(rec.Body.String(), "shh-secret") || strings.Contains(env.admin(http.MethodGet, "/api/orgs/"+orgP1+"/sso/connections", "").Body.String(), "shh-secret") {
			t.Fatalf("the secret must never be returned")
		}

		rows, err := env.app.FindRecordsByFilter(core.CollectionNameSSOConnections, "name='Okta'", "", 1, 0)
		orgMust(t, err)

		if rows[0].GetString("clientSecret") != "shh-secret" {
			t.Fatalf("without an encryption key the secret is stored as given: %q", rows[0].GetString("clientSecret"))
		}

		// the audit trail records the change without the secret
		for _, e := range auditEntries(t, env.app, "targetCollection='_ssoConnections'") {
			if strings.Contains(e.GetString("after"), "shh-secret") {
				t.Fatalf("the audit log must not hold the secret")
			}
		}
	})

	t.Run("invalid settings are refused", func(t *testing.T) {
		for name, body := range map[string]string{
			"http issuer":        `{"name":"x","issuer":"http://idp.example.org","clientId":"a","clientSecret":"b"}`,
			"no client id":       `{"name":"x","issuer":"https://idp.example.org","clientSecret":"b"}`,
			"no secret":          `{"name":"x","issuer":"https://idp.example.org","clientId":"a"}`,
			"no openid scope":    `{"name":"x","issuer":"https://idp.example.org","clientId":"a","clientSecret":"b","scopes":["email"]}`,
			"owner via mapping":  `{"name":"x","issuer":"https://idp.example.org","clientId":"a","clientSecret":"b","roleMapping":{"g":"owner"}}`,
			"unknown role":       `{"name":"x","issuer":"https://idp.example.org","clientId":"a","clientSecret":"b","defaultRole":"king"}`,
			"saml not available": `{"name":"x","type":"saml"}`,
			"no name":            `{"issuer":"https://idp.example.org","clientId":"a","clientSecret":"b"}`,
		} {
			if rec := env.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/connections", body); rec.Code != http.StatusBadRequest {
				t.Errorf("%s: expected 400, got %d %s", name, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("a domain claim needs a valid name and a connection of the same org", func(t *testing.T) {
		list := env.must(env.admin(http.MethodGet, "/api/orgs/"+orgP1+"/sso/connections", ""), 200)
		id := list["items"].([]any)[0].(map[string]any)["id"].(string)

		for _, d := range []string{"", "localhost", "-bad.example", "a b.example", "*.example.org"} {
			if rec := env.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/domains", `{"domain":"`+d+`","connection":"`+id+`"}`); rec.Code != 400 {
				t.Errorf("domain %q: expected 400, got %d", d, rec.Code)
			}
		}

		// the connection of another org
		other := env.must(env.as("carol", http.MethodPost, "/api/orgs/"+orgP2+"/sso/connections",
			`{"name":"P2","issuer":"https://idp.example.org","clientId":"a","clientSecret":"b"}`), 200)
		if rec := env.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/domains", `{"domain":"x.example.org","connection":"`+other["id"].(string)+`"}`); rec.Code != 400 {
			t.Errorf("a foreign connection must be refused: %d", rec.Code)
		}

		env.must(env.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/domains", `{"domain":"Mixed.Example.org.","connection":"`+id+`"}`), 200)

		if rec := env.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/domains", `{"domain":"mixed.example.org","connection":"`+id+`"}`); rec.Code != 409 {
			t.Errorf("a duplicate claim: %d", rec.Code)
		}
	})

	t.Run("the other org cannot see or use these rows", func(t *testing.T) {
		list := env.must(env.admin(http.MethodGet, "/api/orgs/"+orgP1+"/sso/connections", ""), 200)
		id := list["items"].([]any)[0].(map[string]any)["id"].(string)

		env.must(env.as("carol", http.MethodGet, "/api/orgs/"+orgP2+"/sso/connections/"+id, ""), 404)
		env.must(env.as("carol", http.MethodDelete, "/api/orgs/"+orgP2+"/sso/connections/"+id, ""), 404)
	})

	t.Run("presets", func(t *testing.T) {
		got := env.must(env.admin(http.MethodGet, "/api/orgs/"+orgP1+"/sso/presets", ""), 200)

		if got["callback"] != ssoAppURL+"/api/sso/callback" || len(got["presets"].([]any)) < 3 {
			t.Fatalf("unexpected presets: %v", got)
		}
	})

	t.Run("the provider test checks the discovery document", func(t *testing.T) {
		env2 := newSSOEnv(t)
		connID, _ := env2.setupConnection(ssoSetup{Enabled: true})

		got := env2.must(env2.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/connections/"+connID+"/test", ""), 200)
		if got["ok"] != true {
			t.Fatalf("expected ok: %v", got)
		}
	})
}

func TestSSOClientSecretIsEncryptedWhenTheAppKeyIsSet(t *testing.T) {
	t.Setenv("pb_test_env", "0123456789abcdef0123456789abcdef")

	env := newSSOEnv(t)
	connID, _ := env.setupConnection(ssoSetup{Enabled: true, JIT: true, Verified: true})

	rec, err := env.app.FindRecordById(core.CollectionNameSSOConnections, connID)
	orgMust(t, err)

	stored := rec.GetString("clientSecret")
	if !strings.HasPrefix(stored, "sso:enc:v1:") || strings.Contains(stored, ssoClientSecret) {
		t.Fatalf("the secret must be stored encrypted: %q", stored)
	}

	// and the flow still works: the engine decrypts it to call the token endpoint
	back := env.signIn(env.browser(), connID, idClaims("sub-enc", "enc@"+ssoDomain, true, nil))
	if back.Query().Get("sso_code") == "" {
		t.Fatalf("expected a code: %s", back)
	}
}
