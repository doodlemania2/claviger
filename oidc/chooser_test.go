package oidc_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

// The account chooser (#94): several signed-in accounts per browser.

const (
	secondEmail = "second@example.com"
	pass2       = "1234567890"
)

// addUser creates another account of the users collection.
func (e *env) addUser(email string) *core.Record {
	e.t.Helper()

	return e.addUserTo("users", email)
}

func (e *env) addUserTo(collection, email string) *core.Record {
	e.t.Helper()

	col, err := e.app.FindCollectionByNameOrId(collection)
	if err != nil {
		e.t.Fatal(err)
	}

	r := core.NewRecord(col)
	r.SetEmail(email)
	r.SetPassword(pass2)
	r.SetVerified(true)

	if err := e.app.Save(r); err != nil {
		e.t.Fatal(err)
	}

	return r
}

// signInAs signs in on the hosted page of a forced (prompt=login) authorize
// request with another account, keeping the browser's cookie, and returns the
// result of the whole authorize flow.
func (b *browser) signInAs(clientID, collection, email, password string) loginResult {
	b.env.t.Helper()

	reqID, p := b.startAuthorize(clientID, authOpts{extra: url.Values{"prompt": {"login"}}})

	if rec := b.do("GET", "/oidc/login?authRequestID="+url.QueryEscape(reqID), nil, nil); rec.Code != 200 {
		b.env.t.Fatalf("login page: %d %s", rec.Code, rec.Body.String())
	}

	status, out := b.json("POST", "/api/collections/"+collection+"/auth-with-password", map[string]any{"identity": email, "password": password}, nil)
	if status != 200 {
		b.env.t.Fatalf("sign-in of %s: %d %v", email, status, out)
	}

	token := out["token"].(string)

	status, fin := b.finish(reqID, token)
	if status != 200 {
		b.env.t.Fatalf("finish: %d %v", status, fin)
	}

	u := b.resume(fin["redirect"].(string))

	return loginResult{code: u.Query().Get("code"), state: u.Query().Get("state"), pkce: p, token: token}
}

var accountIDs = regexp.MustCompile(`data-account="([^"]+)"`)

// chooserResult is where an authorize request ended up.
type chooserResult struct {
	kind string // "chooser", "login", "code", "error", "consent"
	req  string
	page string // the HTML of the page, if any
	to   *url.URL
	pkce pkce
	// every redirect the browser was sent through, for leak checks
	locations []string
}

func (b *browser) authorizeKind(clientID string, o authOpts) chooserResult {
	b.env.t.Helper()

	target, p := b.authorizeURL(clientID, o)
	rec := b.do("GET", target, nil, nil)
	res := chooserResult{pkce: p}
	res.locations = append(res.locations, rec.Header().Get("Location"))

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || rec.Code != http.StatusFound {
		b.env.t.Fatalf("authorize: %d %s", rec.Code, rec.Body.String())
	}

	if loc.Path != "/oidc/login" {
		res.kind, res.to = kindOf(loc), loc

		return res
	}

	res.req = loc.Query().Get("authRequestID")

	page := b.do("GET", loc.String(), nil, nil)
	if page.Code == http.StatusOK {
		res.page = page.Body.String()
		res.kind = "login"

		if strings.Contains(res.page, `data-page="chooser"`) {
			res.kind = "chooser"
		}

		return res
	}

	next, err := url.Parse(page.Header().Get("Location"))
	if err != nil || page.Code != http.StatusFound {
		b.env.t.Fatalf("login page: %d %s", page.Code, page.Body.String())
	}

	res.locations = append(res.locations, next.String())

	switch next.Path {
	case "/oidc/consent":
		res.kind, res.to = "consent", next
	case "/oidc/authorize/callback":
		final := b.resume(next.String())
		res.locations = append(res.locations, final.String())
		res.kind, res.to = kindOf(final), final
	default:
		res.kind, res.to = kindOf(next), next
	}

	return res
}

func (r chooserResult) errCode() string { return r.to.Query().Get("error") }

func (r chooserResult) login(token string) loginResult {
	return loginResult{code: r.to.Query().Get("code"), state: r.to.Query().Get("state"), pkce: r.pkce, token: token}
}

// ssoIDOf returns the opaque row id the chooser uses for an account.
func ssoIDOf(t *testing.T, e *env, subject string) string {
	t.Helper()

	var id string
	if err := e.app.ConcurrentDB().NewQuery("SELECT [[id]] FROM {{" + core.CollectionNameOAuthSso + "}} WHERE [[subject]] = {:s}").
		Bind(dbx.Params{"s": subject}).Row(&id); err != nil {
		t.Fatalf("no single sign-on row for %s: %v", subject, err)
	}

	return id
}

func (b *browser) selectAccount(req, account string) (int, map[string]any) {
	return b.json("POST", "/oidc/account/select", map[string]any{"flow": "authorize", "ref": req, "account": account}, nil)
}

func subOf(t *testing.T, e *env, clientID string, r loginResult) string {
	t.Helper()

	tok := e.mustExchange(clientID, "", r)
	_, claims := decodeJWT(t, tok["id_token"].(string))

	return claims["sub"].(string)
}

// twoAccounts signs a browser in to the two users and returns it.
func twoAccounts(t *testing.T, e *env, clientID string) (*browser, *core.Record, *core.Record) {
	t.Helper()

	first := e.user()
	second := e.addUser(secondEmail)

	b := e.browser()
	b.signInAs(clientID, "users", testEmail, testPass)
	b.signInAs(clientID, "users", secondEmail, pass2)

	if n := ssoRowCount(t, e); n != 2 {
		t.Fatalf("expected two single sign-on sessions, got %d", n)
	}

	return b, first, second
}

func TestChooserShownWithTwoAccountsAndSelectionWorks(t *testing.T) {
	e := newEnv(t)
	c, _ := e.createClient(map[string]any{"public": true, "firstParty": true})
	b, first, second := twoAccounts(t, e, c)

	if v := b.cookies[ssoCookie]; strings.Count(v, "~") != 1 {
		t.Fatalf("the cookie must hold both sessions: %q", v)
	}

	out := b.authorizeKind(c, authOpts{})
	if out.kind != "chooser" {
		t.Fatalf("two accounts and no hint must show the chooser, got %s", out.kind)
	}

	for _, want := range []string{testEmail, secondEmail, "Use another account", "Sign out of all accounts"} {
		if !strings.Contains(out.page, want) {
			t.Fatalf("the chooser must show %q:\n%s", want, out.page)
		}
	}

	if ids := accountIDs.FindAllStringSubmatch(out.page, -1); len(ids) != 2 {
		t.Fatalf("expected two accounts on the page, got %v", ids)
	}

	// the account ids on the page are opaque row ids, not the subjects
	if strings.Contains(out.page, first.Id) || strings.Contains(out.page, second.Id) {
		t.Fatal("the chooser must not expose record ids")
	}

	status, body := b.selectAccount(out.req, ssoIDOf(t, e, second.Id))
	if status != 200 {
		t.Fatalf("select: %d %v", status, body)
	}

	u := b.resume(body["redirect"].(string))
	if u.Query().Get("code") == "" {
		t.Fatalf("no code after the selection: %s", u)
	}

	if sub := subOf(t, e, c, loginResult{code: u.Query().Get("code"), pkce: out.pkce}); sub != second.Id {
		t.Fatalf("the chosen account must be the subject: %s vs %s", sub, second.Id)
	}

	// and the other one
	again := b.authorizeKind(c, authOpts{})
	if _, body = b.selectAccount(again.req, ssoIDOf(t, e, first.Id)); body["redirect"] == nil {
		t.Fatalf("select: %v", body)
	}

	u = b.resume(body["redirect"].(string))
	if sub := subOf(t, e, c, loginResult{code: u.Query().Get("code"), pkce: again.pkce}); sub != first.Id {
		t.Fatalf("the chosen account must be the subject: %s vs %s", sub, first.Id)
	}
}

func TestChooserPromptSelectAccountAndUseAnotherAccount(t *testing.T) {
	e := newEnv(t)
	c, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	b := e.browser()
	b.login(c, authOpts{})

	// a single account is used silently...
	if out := b.authorizeKind(c, authOpts{}); out.kind != "code" {
		t.Fatalf("baseline: %s", out.kind)
	}

	// ...unless the client asks to choose
	out := b.authorizeKind(c, authOpts{extra: url.Values{"prompt": {"select_account"}}})
	if out.kind != "chooser" || !strings.Contains(out.page, testEmail) {
		t.Fatalf("prompt=select_account must show the chooser, got %s", out.kind)
	}

	// "use another account" is the sign-in page of the same request
	other := b.do("GET", "/oidc/login?authRequestID="+url.QueryEscape(out.req)+"&account=new", nil, nil)
	if other.Code != 200 || !strings.Contains(other.Body.String(), `data-page="login"`) {
		t.Fatalf("use another account: %d %s", other.Code, other.Body.String())
	}

	if !strings.Contains(out.page, "account=new") {
		t.Fatal("the chooser must link to the sign-in page")
	}

	// nothing signed in: select_account has nothing to choose from
	if out := e.browser().authorizeKind(c, authOpts{extra: url.Values{"prompt": {"select_account"}}}); out.kind != "login" {
		t.Fatalf("no accounts: %s", out.kind)
	}
}

func TestChooserLoginHint(t *testing.T) {
	e := newEnv(t)
	c, _ := e.createClient(map[string]any{"public": true, "firstParty": true})
	b, first, second := twoAccounts(t, e, c)

	hint := func(h string, extra ...string) authOpts {
		q := url.Values{"login_hint": {h}}
		if len(extra) > 0 {
			q.Set("prompt", extra[0])
		}

		return authOpts{extra: q}
	}

	// the hint picks the account without a page
	out := b.authorizeKind(c, hint(strings.ToUpper(secondEmail)))
	if out.kind != "code" {
		t.Fatalf("login_hint must resolve to the account, got %s", out.kind)
	}

	if sub := subOf(t, e, c, out.login("")); sub != second.Id {
		t.Fatalf("hinted account: %s vs %s", sub, second.Id)
	}

	if out = b.authorizeKind(c, hint(testEmail)); out.kind != "code" || subOf(t, e, c, out.login("")) != first.Id {
		t.Fatalf("hinted first account: %s", out.kind)
	}

	// with select_account the hint preselects instead
	out = b.authorizeKind(c, hint(secondEmail, "select_account"))
	if out.kind != "chooser" || strings.Count(out.page, `class="account pre"`) != 1 {
		t.Fatalf("the hinted account must be preselected: %s", out.kind)
	}

	// a hint that names nobody signed in goes to the sign-in page, prefilled
	out = b.authorizeKind(c, hint("nobody@example.com"))
	if out.kind != "login" || !strings.Contains(out.page, `data-login-hint="nobody@example.com"`) {
		t.Fatalf("an unknown hint must ask for credentials: %s", out.kind)
	}
}

func TestChooserPromptNone(t *testing.T) {
	e := newEnv(t)
	c, _ := e.createClient(map[string]any{"public": true, "firstParty": true})
	b, _, second := twoAccounts(t, e, c)

	none := func(hint string) authOpts {
		q := url.Values{"prompt": {"none"}}
		if hint != "" {
			q.Set("login_hint", hint)
		}

		return authOpts{extra: q}
	}

	out := b.authorizeKind(c, none(""))
	if out.kind != "error" || out.errCode() != "account_selection_required" {
		t.Fatalf("prompt=none with two accounts: %s %v", out.kind, out.to)
	}

	// the error carries nothing about the accounts
	if strings.Contains(out.to.String(), "example.com") {
		t.Fatalf("the error leaks an account: %s", out.to)
	}

	out = b.authorizeKind(c, none(secondEmail))
	if out.kind != "code" || subOf(t, e, c, out.login("")) != second.Id {
		t.Fatalf("a hint that resolves to one account satisfies prompt=none: %s %v", out.kind, out.to)
	}

	if out = b.authorizeKind(c, none("nobody@example.com")); out.kind != "error" || out.errCode() != "account_selection_required" {
		t.Fatalf("a hint that resolves to nobody: %s %v", out.kind, out.to)
	}

	// one account: silent success; none: login_required
	one := e.browser()
	one.login(c, authOpts{})

	if out = one.authorizeKind(c, none("")); out.kind != "code" {
		t.Fatalf("one account: %s", out.kind)
	}

	if out = e.browser().authorizeKind(c, none("")); out.kind != "error" || out.errCode() != "login_required" {
		t.Fatalf("no account: %s", out.kind)
	}
}

func TestChooserSignOutOneAndAll(t *testing.T) {
	e := newEnv(t)
	c, _ := e.createClient(map[string]any{"public": true, "firstParty": true})
	b, first, second := twoAccounts(t, e, c)

	// tokens of both accounts, to see which Claviger session dies
	tokenOf := func(sub string) map[string]any {
		out := b.authorizeKind(c, authOpts{})
		_, body := b.selectAccount(out.req, ssoIDOf(t, e, sub))
		u := b.resume(body["redirect"].(string))

		return e.mustExchange(c, "", loginResult{code: u.Query().Get("code"), pkce: out.pkce})
	}

	t1, t2 := tokenOf(first.Id), tokenOf(second.Id)

	out := b.authorizeKind(c, authOpts{})
	status, body := b.json("POST", "/oidc/account/signout", map[string]any{"flow": "authorize", "ref": out.req, "account": ssoIDOf(t, e, second.Id)}, nil)
	if status != 200 || body["redirect"] == nil {
		t.Fatalf("sign out one: %d %v", status, body)
	}

	if st, _ := e.refresh(c, "", t2["refresh_token"].(string)); st == 200 {
		t.Fatal("the signed-out account's Claviger session must be revoked")
	}

	if st, _ := e.refresh(c, "", t1["refresh_token"].(string)); st != 200 {
		t.Fatalf("the other account must stay signed in: %d", st)
	}

	if n := ssoRowCount(t, e); n != 1 {
		t.Fatalf("one account must be left, got %d", n)
	}

	if strings.Contains(b.cookies[ssoCookie], "~") {
		t.Fatalf("the cookie must forget the account: %q", b.cookies[ssoCookie])
	}

	// the one that is left carries the request without a page
	if out := b.authorizeKind(c, authOpts{}); out.kind != "code" || subOf(t, e, c, out.login("")) != first.Id {
		t.Fatalf("the remaining account: %s", out.kind)
	}

	// sign out of all
	second2 := e.addUser("third@example.com")
	b.signInAs(c, "users", second2.Email(), pass2)

	out = b.authorizeKind(c, authOpts{})
	if out.kind != "chooser" {
		t.Fatalf("two accounts again: %s", out.kind)
	}

	status, body = b.json("POST", "/oidc/account/signout", map[string]any{"flow": "authorize", "ref": out.req, "account": "all"}, nil)
	if status != 200 {
		t.Fatalf("sign out of all: %d %v", status, body)
	}

	if n := ssoRowCount(t, e); n != 0 {
		t.Fatalf("no account must be left, got %d", n)
	}

	if st, _ := e.refresh(c, "", t1["refresh_token"].(string)); st == 200 {
		t.Fatal("signing out of all revokes every Claviger session")
	}

	if out := b.authorizeKind(c, authOpts{}); out.kind != "login" {
		t.Fatalf("after signing out of all: %s", out.kind)
	}
}

func TestChooserEndSessionWithHintEndsOnlyThatAccount(t *testing.T) {
	e := newEnv(t)
	c, _ := e.createClient(map[string]any{"public": true, "firstParty": true, "postLogoutUris": []string{"https://app.example.net/bye"}})
	b, first, second := twoAccounts(t, e, c)

	out := b.authorizeKind(c, authOpts{})
	_, body := b.selectAccount(out.req, ssoIDOf(t, e, second.Id))
	u := b.resume(body["redirect"].(string))
	tok := e.mustExchange(c, "", loginResult{code: u.Query().Get("code"), pkce: out.pkce})

	q := url.Values{"id_token_hint": {tok["id_token"].(string)}, "post_logout_redirect_uri": {"https://app.example.net/bye"}}

	rec := b.do("GET", "/oidc/end_session?"+q.Encode(), nil, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("end_session: %d %s", rec.Code, rec.Body.String())
	}

	for _, ck := range rec.Result().Cookies() {
		if ck.Name == ssoCookie && ck.MaxAge < 0 {
			t.Fatal("the other account is still signed in: the cookie must not be cleared")
		}
	}

	if n := ssoRowCount(t, e); n != 1 {
		t.Fatalf("one account must be left, got %d", n)
	}

	if out := b.authorizeKind(c, authOpts{}); out.kind != "code" || subOf(t, e, c, out.login("")) != first.Id {
		t.Fatalf("the other account carries on: %s", out.kind)
	}

	// a bare logout ends everything the browser holds
	b.signInAs(c, "users", secondEmail, pass2)

	if rec := b.do("GET", "/oidc/end_session", nil, nil); rec.Code >= 500 {
		t.Fatalf("bare end_session: %d", rec.Code)
	}

	if n := ssoRowCount(t, e); n != 0 {
		t.Fatalf("a bare logout signs out of all accounts, %d left", n)
	}
}

func TestChooserRevokedAccountDisappears(t *testing.T) {
	cases := map[string]func(t *testing.T, e *env, u *core.Record){
		"session revoked": func(t *testing.T, e *env, u *core.Record) {
			if _, err := core.RevokeSessions(e.app, u.Collection().Id, u.Id, "revoked by user", ""); err != nil {
				t.Fatal(err)
			}
		},
		"ban": func(t *testing.T, e *env, u *core.Record) {
			bans, _ := e.app.FindCollectionByNameOrId(core.CollectionNameBans)
			ban := core.NewRecord(bans)
			ban.Set("collectionRef", u.Collection().Id)
			ban.Set("recordRef", u.Id)
			ban.Set("reason", "test ban")

			if err := e.app.Save(ban); err != nil {
				t.Fatal(err)
			}
		},
		"password change": func(t *testing.T, e *env, u *core.Record) {
			fresh, _ := e.app.FindRecordById(u.Collection().Id, u.Id)
			fresh.SetPassword("brand-new-password-1")

			if err := e.app.Save(fresh); err != nil {
				t.Fatal(err)
			}
		},
		"account deleted": func(t *testing.T, e *env, u *core.Record) {
			fresh, _ := e.app.FindRecordById(u.Collection().Id, u.Id)
			if err := e.app.Delete(fresh); err != nil {
				t.Fatal(err)
			}
		},
		"session expired": func(t *testing.T, e *env, u *core.Record) {
			if _, err := e.app.NonconcurrentDB().NewQuery("UPDATE {{" + core.CollectionNameSessions + "}} SET [[expires]] = {:t} WHERE [[recordRef]] = {:r}").
				Bind(dbx.Params{"t": time.Now().Add(-time.Minute).UTC().Format("2006-01-02 15:04:05.000Z"), "r": u.Id}).Execute(); err != nil {
				t.Fatal(err)
			}

			core.InvalidateSessionCache(e.app)
		},
	}

	for name, end := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			c, _ := e.createClient(map[string]any{"public": true, "firstParty": true})
			b, first, second := twoAccounts(t, e, c)

			if out := b.authorizeKind(c, authOpts{}); out.kind != "chooser" {
				t.Fatalf("baseline: %s", out.kind)
			}

			end(t, e, second)

			// the survivor now carries the request alone...
			out := b.authorizeKind(c, authOpts{})
			if out.kind != "code" || subOf(t, e, c, out.login("")) != first.Id {
				t.Fatalf("the remaining account must carry the request: %s", out.kind)
			}

			// ...and even the chooser no longer lists the ended one
			ch := b.authorizeKind(c, authOpts{extra: url.Values{"prompt": {"select_account"}}})
			if ch.kind != "chooser" || strings.Contains(ch.page, secondEmail) || !strings.Contains(ch.page, testEmail) {
				t.Fatalf("the ended account must be gone from the chooser:\n%s", ch.page)
			}

			if n := ssoRowCount(t, e); n != 1 {
				t.Fatalf("its row must be dropped, %d rows", n)
			}
		})
	}
}

func TestChooserClientCollectionBinding(t *testing.T) {
	e := newEnv(t)

	members := core.NewAuthCollection("members")
	if err := e.app.Save(members); err != nil {
		t.Fatal(err)
	}

	member := e.addUserTo("members", "member@example.com")
	userClient, _ := e.createClient(map[string]any{"name": "Users app", "public": true, "firstParty": true})
	memberClient, _ := e.createClient(map[string]any{"name": "Members app", "authCollection": "members", "public": true, "firstParty": true})

	b := e.browser()
	b.login(userClient, authOpts{})
	b.signInAs(memberClient, "members", member.Email(), pass2)

	if n := ssoRowCount(t, e); n != 2 {
		t.Fatalf("accounts of two collections now coexist, got %d", n)
	}

	// each client sees only the account it can use, and no chooser is needed
	out := b.authorizeKind(userClient, authOpts{})
	if out.kind != "code" || subOf(t, e, userClient, out.login("")) != e.user().Id {
		t.Fatalf("the users client uses the users account: %s", out.kind)
	}

	out = b.authorizeKind(memberClient, authOpts{})
	if out.kind != "code" || subOf(t, e, memberClient, out.login("")) != member.Id {
		t.Fatalf("the members client uses the member account: %s", out.kind)
	}

	// the chooser lists both, the unusable one greyed with the reason
	ch := b.authorizeKind(userClient, authOpts{extra: url.Values{"prompt": {"select_account"}}})
	if ch.kind != "chooser" || !strings.Contains(ch.page, member.Email()) ||
		!strings.Contains(ch.page, "This account cannot be used with this application.") || strings.Count(ch.page, " disabled>") != 1 {
		t.Fatalf("the chooser must grey out the other kind of account:\n%s", ch.page)
	}

	if status, body := b.selectAccount(ch.req, ssoIDOf(t, e, member.Id)); status != http.StatusBadRequest {
		t.Fatalf("an unusable account cannot be selected: %d %v", status, body)
	}

	// prompt=none with a usable account among unusable ones is fine
	if out := b.authorizeKind(userClient, authOpts{extra: url.Values{"prompt": {"none"}}}); out.kind != "code" {
		t.Fatalf("prompt=none: %s", out.kind)
	}
}

func TestChooserOrgBoundClient(t *testing.T) {
	e := newEnv(t)
	o := seedOIDCOrgs(t, e) // the first user is an active member of o.org

	other := e.addUser(secondEmail) // not a member

	open, _ := e.createClient(map[string]any{"name": "Open", "public": true, "firstParty": true})
	bound, _ := e.createClient(map[string]any{"name": "Bound", "public": true, "firstParty": true, "org": o.org.Id})

	b := e.browser()
	b.signInAs(open, "users", testEmail, testPass)
	b.signInAs(open, "users", secondEmail, pass2)

	// only the member can use the org's application: no chooser, no guess
	out := b.authorizeKind(bound, authOpts{})
	if out.kind != "code" || subOf(t, e, bound, out.login("")) != e.user().Id {
		t.Fatalf("the org-bound client must use the member: %s", out.kind)
	}

	ch := b.authorizeKind(bound, authOpts{extra: url.Values{"prompt": {"select_account"}}})
	if ch.kind != "chooser" || !strings.Contains(ch.page, "not a member of the organization") {
		t.Fatalf("the non-member must be greyed out with the reason:\n%s", ch.page)
	}

	if status, _ := b.selectAccount(ch.req, ssoIDOf(t, e, other.Id)); status != http.StatusBadRequest {
		t.Fatalf("a non-member cannot be selected: %d", status)
	}

	// the open client sees both
	if out := b.authorizeKind(open, authOpts{}); out.kind != "chooser" {
		t.Fatalf("the open client lets the user choose: %s", out.kind)
	}
}

func TestChooserNeverLeaksOtherAccountsToTheClient(t *testing.T) {
	e := newEnv(t)
	c, _ := e.createClient(map[string]any{"public": true, "firstParty": true})
	b, _, second := twoAccounts(t, e, c)

	out := b.authorizeKind(c, authOpts{})
	_, body := b.selectAccount(out.req, ssoIDOf(t, e, second.Id))
	redirect := body["redirect"].(string)

	u := b.resume(redirect)
	tok := e.mustExchange(c, "", loginResult{code: u.Query().Get("code"), pkce: out.pkce})

	everything := strings.Join(append(out.locations, redirect, u.String()), " ")
	for _, v := range tok {
		if s, ok := v.(string); ok {
			everything += " " + s
			if strings.Count(s, ".") == 2 {
				_, claims := decodeJWT(t, s)
				everything += " " + jsonString(claims)
			}
		}
	}

	status, info := e.userinfo(tok["access_token"].(string))
	everything += " " + jsonString(info)

	if status != 200 {
		t.Fatalf("userinfo: %d", status)
	}

	for _, secret := range []string{testEmail, e.user().Id} {
		if strings.Contains(everything, secret) {
			t.Fatalf("the other account (%s) reached the client:\n%s", secret, everything)
		}
	}

	// the chooser's endpoints need the browser binding: another browser gets nothing
	stranger := e.browser()
	stranger.cookies[ssoCookie] = b.cookies[ssoCookie]

	again := b.authorizeKind(c, authOpts{})
	if status, _ := stranger.selectAccount(again.req, ssoIDOf(t, e, second.Id)); status != http.StatusForbidden {
		t.Fatalf("selecting from another browser must fail, got %d", status)
	}

	if status, _ := stranger.json("POST", "/oidc/account/signout", map[string]any{"flow": "authorize", "ref": again.req, "account": "all"}, nil); status != http.StatusForbidden {
		t.Fatalf("signing out from another browser must fail, got %d", status)
	}

	if rec := stranger.do("GET", "/oidc/login?authRequestID="+url.QueryEscape(again.req), nil, nil); rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), testEmail) {
		t.Fatal("another browser must not see the chooser")
	}

	// nor can the endpoints be used without a request
	if status, _ := b.json("POST", "/oidc/account/select", map[string]any{"flow": "authorize", "ref": "nope", "account": "x"}, nil); status != http.StatusBadRequest {
		t.Fatalf("unknown request: %d", status)
	}
}

func TestChooserCapsTheAccountsOfABrowser(t *testing.T) {
	e := newEnv(t)
	c, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	b := e.browser()
	b.signInAs(c, "users", testEmail, testPass)

	for i := 0; i < 5; i++ {
		email := "extra" + string(rune('a'+i)) + "@example.com"
		e.addUser(email)
		b.signInAs(c, "users", email, pass2)
	}

	if n := ssoRowCount(t, e); n != 5 {
		t.Fatalf("a browser keeps at most 5 accounts, got %d rows", n)
	}

	if n := len(strings.Split(b.cookies[ssoCookie], "~")); n != 5 {
		t.Fatalf("the cookie must hold 5 sessions, got %d", n)
	}

	ch := b.authorizeKind(c, authOpts{})
	if ch.kind != "chooser" || strings.Contains(ch.page, testEmail) || strings.Count(ch.page, "data-account=") != 5 {
		t.Fatalf("the oldest account must have been dropped:\n%s", ch.page)
	}

	// signing in to an account the browser already has replaces its session, it does not add one
	b.signInAs(c, "users", "extrab@example.com", pass2)

	if n := ssoRowCount(t, e); n != 5 {
		t.Fatalf("a repeated sign-in replaces the session, got %d rows", n)
	}
}

func TestChooserStillAsksForConsent(t *testing.T) {
	e := newEnv(t)
	first, _ := e.createClient(map[string]any{"name": "Mine", "public": true, "firstParty": true})
	third, _ := e.createClient(map[string]any{"name": "Third", "public": true})

	b, _, second := twoAccounts(t, e, first)

	ch := b.authorizeKind(third, authOpts{})
	if ch.kind != "chooser" {
		t.Fatalf("%s", ch.kind)
	}

	status, body := b.selectAccount(ch.req, ssoIDOf(t, e, second.Id))
	if status != 200 || !strings.HasPrefix(body["redirect"].(string), "/oidc/consent") {
		t.Fatalf("a third-party client still needs consent after the choice: %d %v", status, body)
	}
}
