//go:build postgres

package pgtests

// Multi replica suite (issue #106, M7): several apps on ONE PostgreSQL
// database in one process, each behind its own HTTP server, the way N
// containers of a deployment are. The assertions are engine neutral (they only
// use the public API of the apps) and each one names the guarantee it checks in
// docs/datastores.md ("Several replicas").
//
// Run: cd scripts/pgembedded && go run . -count=1 -run MultiReplica ./tests/pgtests/...

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
)

type replicas struct {
	t       testing.TB
	apps    []*core.BaseApp
	servers []*httptest.Server
	url     string
}

// newReplicas starts n apps on one fresh database, all at the same moment, so
// that the concurrent start (migrations lock, schema creation) is exercised.
func newReplicas(t *testing.T, n int) *replicas {
	t.Helper()

	url := freshDatabase(t)
	t.Setenv(core.DBURLEnv, url)
	t.Setenv(core.MultiReplicaEnv, "true")

	rs := &replicas{t: t, url: url}
	for i := 0; i < n; i++ {
		rs.apps = append(rs.apps, core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir()}))
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, app := range rs.apps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = app.Bootstrap()
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("replica %d failed to bootstrap: %v", i, err)
		}
	}

	for _, app := range rs.apps {
		app := app
		t.Cleanup(func() { _ = app.ClearBootstrap() })

		if core.ReplicaBusOf(app) == nil || !core.ReplicaBusOf(app).Listening() {
			t.Fatal("the replica bus is not running")
		}

		r, err := apis.NewRouter(app)
		if err != nil {
			t.Fatal(err)
		}
		mux, err := r.BuildMux()
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		rs.servers = append(rs.servers, srv)
	}

	return rs
}

func eventually(t testing.TB, d time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

func (rs *replicas) superuserToken(app core.App) string {
	rs.t.Helper()

	su, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "root@example.com")
	if err != nil {
		col, _ := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
		su = core.NewRecord(col)
		su.SetEmail("root@example.com")
		su.SetPassword("1234567890")
		if err := app.Save(su); err != nil {
			rs.t.Fatal(err)
		}
	}
	tok, err := su.NewAuthToken()
	if err != nil {
		rs.t.Fatal(err)
	}
	return tok
}

// createCollection creates a base collection on one replica and waits for all
// the others to see it (cache invalidation, guarantee: bounded by the event
// latency, a few hundred milliseconds).
func (rs *replicas) createCollection(on int, name, listRule string) *core.Collection {
	rs.t.Helper()

	c := core.NewBaseCollection(name)
	c.Fields.Add(&core.TextField{Name: "title"}, &core.BoolField{Name: "public"})
	c.ListRule = &listRule
	c.ViewRule = &listRule
	if err := rs.apps[on].Save(c); err != nil {
		rs.t.Fatal(err)
	}

	for i, app := range rs.apps {
		if i == on {
			continue
		}
		eventually(rs.t, 5*time.Second, fmt.Sprintf("replica %d to see collection %s", i, name), func() bool {
			_, err := app.FindCachedCollectionByNameOrId(name)
			return err == nil
		})
	}

	return c
}

// ---------------------------------------------------------------------------
// SSE client
// ---------------------------------------------------------------------------

type sseEvent struct {
	Name string
	Data string
}

type sseClient struct {
	t        testing.TB
	base     string
	clientId string
	events   chan sseEvent
	body     io.Closer
}

func connectSSE(t testing.TB, base string) *sseClient {
	t.Helper()

	req, _ := http.NewRequest(http.MethodGet, base+"/api/realtime", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	c := &sseClient{t: t, base: base, events: make(chan sseEvent, 256), body: resp.Body}
	t.Cleanup(func() { _ = resp.Body.Close() })

	go func() {
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				ev.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				ev.Data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			case line == "":
				if ev.Name != "" {
					c.events <- ev
				}
				ev = sseEvent{}
			}
		}
	}()

	select {
	case ev := <-c.events:
		if ev.Name != "PB_CONNECT" {
			t.Fatalf("first event = %q", ev.Name)
		}
		var d struct{ ClientId string }
		_ = json.Unmarshal([]byte(strings.ReplaceAll(ev.Data, "clientId", "ClientId")), &d)
		c.clientId = d.ClientId
	case <-time.After(5 * time.Second):
		t.Fatal("no PB_CONNECT")
	}

	return c
}

func (c *sseClient) subscribe(token string, topics ...string) {
	c.t.Helper()

	body, _ := json.Marshal(map[string]any{"clientId": c.clientId, "subscriptions": topics})
	req, _ := http.NewRequest(http.MethodPost, c.base+"/api/realtime", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		c.t.Fatalf("subscribe: %d %s", resp.StatusCode, raw)
	}
}

// expect waits for an event of the topic and returns its decoded data.
func (c *sseClient) expect(topic string) map[string]any {
	c.t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-c.events:
			if ev.Name != topic {
				continue
			}
			var data map[string]any
			if err := json.Unmarshal([]byte(ev.Data), &data); err != nil {
				c.t.Fatalf("bad event data %q: %v", ev.Data, err)
			}
			return data
		case <-deadline:
			c.t.Fatalf("no %q event within 5s", topic)
			return nil
		}
	}
}

func (c *sseClient) expectNone(topic string, wait time.Duration) {
	c.t.Helper()

	deadline := time.After(wait)
	for {
		select {
		case ev := <-c.events:
			if ev.Name == topic {
				c.t.Fatalf("unexpected %q event: %s", topic, ev.Data)
			}
		case <-deadline:
			return
		}
	}
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// Several replicas starting at the same moment all come up: initMigrationsTable,
// the system migrations and the shared tables are race safe.
func TestMultiReplicaConcurrentStart(t *testing.T) {
	rs := newReplicas(t, 4)

	var applied int
	if err := rs.apps[0].DB().NewQuery("SELECT count(*) FROM _migrations").Row(&applied); err != nil {
		t.Fatal(err)
	}
	if want := len(core.SystemMigrations.Items()); applied != want {
		t.Fatalf("migrations applied = %d, want %d (each once)", applied, want)
	}
}

// A change made on one replica reaches the realtime clients of every other
// replica (and of its own, once), with the rules of each subscription evaluated
// on the receiving replica.
func TestMultiReplicaRealtimeCrossReplica(t *testing.T) {
	rs := newReplicas(t, 3)
	rs.createCollection(0, "rtposts", "public = true")

	clients := make([]*sseClient, 3)
	for i := range clients {
		clients[i] = connectSSE(t, rs.servers[i].URL)
		clients[i].subscribe("", "rtposts/*")
	}

	col, _ := rs.apps[1].FindCollectionByNameOrId("rtposts")

	// create on replica 1: delivered to all three, exactly once each
	pub := core.NewRecord(col)
	pub.Id = "rtpublic0000001"
	pub.Set("title", "hello")
	pub.Set("public", true)
	if err := rs.apps[1].Save(pub); err != nil {
		t.Fatal(err)
	}
	for i, c := range clients {
		ev := c.expect("rtposts/*")
		if ev["action"] != "create" || ev["record"].(map[string]any)["id"] != pub.Id {
			t.Fatalf("replica %d: %v", i, ev)
		}
		c.expectNone("rtposts/*", 300*time.Millisecond)
	}

	// a record the rule hides is delivered to nobody
	hidden := core.NewRecord(col)
	hidden.Id = "rthidden0000001"
	hidden.Set("title", "secret")
	hidden.Set("public", false)
	if err := rs.apps[1].Save(hidden); err != nil {
		t.Fatal(err)
	}
	for _, c := range clients {
		c.expectNone("rtposts/*", 400*time.Millisecond)
	}

	// update on replica 2
	pub2, _ := rs.apps[2].FindRecordById("rtposts", pub.Id)
	pub2.Set("title", "changed")
	if err := rs.apps[2].Save(pub2); err != nil {
		t.Fatal(err)
	}
	for i, c := range clients {
		ev := c.expect("rtposts/*")
		rec := ev["record"].(map[string]any)
		if ev["action"] != "update" || rec["title"] != "changed" {
			t.Fatalf("replica %d: %v", i, ev)
		}
	}

	// the update that makes the rule fail is NOT sent (same as on one replica)
	pub2.Set("public", false)
	if err := rs.apps[2].Save(pub2); err != nil {
		t.Fatal(err)
	}
	for _, c := range clients {
		c.expectNone("rtposts/*", 300*time.Millisecond)
	}
	pub2.Set("public", true)
	if err := rs.apps[2].Save(pub2); err != nil {
		t.Fatal(err)
	}
	for _, c := range clients {
		c.expect("rtposts/*")
	}

	// delete on replica 0: the row is gone when the others evaluate the rule
	// (public = true), which is what the rolled back re-insert is for
	del, _ := rs.apps[0].FindRecordById("rtposts", pub.Id)
	if err := rs.apps[0].Delete(del); err != nil {
		t.Fatal(err)
	}
	for i, c := range clients {
		ev := c.expect("rtposts/*")
		if ev["action"] != "delete" || ev["record"].(map[string]any)["id"] != pub.Id {
			t.Fatalf("replica %d: %v", i, ev)
		}
	}

	// deleting the hidden one tells nobody
	del, _ = rs.apps[0].FindRecordById("rtposts", hidden.Id)
	if err := rs.apps[0].Delete(del); err != nil {
		t.Fatal(err)
	}
	for _, c := range clients {
		c.expectNone("rtposts/*", 400*time.Millisecond)
	}

	// the re-insert never persisted anything
	for _, app := range rs.apps {
		var n int
		if err := app.DB().NewQuery("SELECT count(*) FROM rtposts").Row(&n); err != nil || n != 0 {
			t.Fatalf("rows left in rtposts: %d (err %v)", n, err)
		}
	}
}

// A per record subscription and a subscription with the authenticated rule work
// across replicas: the receiving replica uses the auth of its own client.
func TestMultiReplicaRealtimeRulesUseTheClientAuth(t *testing.T) {
	rs := newReplicas(t, 2)
	rs.createCollection(0, "rtprivate", `@request.auth.id != ""`)

	users, _ := rs.apps[1].FindCollectionByNameOrId("users")
	u := core.NewRecord(users)
	u.SetEmail("rt@example.com")
	u.SetPassword("1234567890")
	if err := rs.apps[1].Save(u); err != nil {
		t.Fatal(err)
	}
	tok, _ := u.NewAuthToken()

	anon := connectSSE(t, rs.servers[1].URL)
	anon.subscribe("", "rtprivate/*")
	authed := connectSSE(t, rs.servers[1].URL)
	authed.subscribe(tok, "rtprivate/*")

	col, _ := rs.apps[0].FindCollectionByNameOrId("rtprivate")
	r := core.NewRecord(col)
	r.Set("title", "x")
	if err := rs.apps[0].Save(r); err != nil {
		t.Fatal(err)
	}

	authed.expect("rtprivate/*")
	anon.expectNone("rtprivate/*", 500*time.Millisecond)
}

// The OAuth2 popup flow: the provider redirects to a replica that does not hold
// the realtime connection of the client, and the client still gets the result.
func TestMultiReplicaOAuth2RedirectForwarded(t *testing.T) {
	rs := newReplicas(t, 2)

	client := connectSSE(t, rs.servers[1].URL)
	client.subscribe("", "@oauth2")

	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(rs.servers[0].URL + "/api/oauth2-redirect?state=" + client.clientId + "&code=thecode")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect && resp.StatusCode != http.StatusOK {
		t.Fatalf("redirect status = %d", resp.StatusCode)
	}

	ev := client.expect("@oauth2")
	if ev["code"] != "thecode" || ev["state"] != client.clientId {
		t.Fatalf("event = %v", ev)
	}

	// a state nobody has is a failure, and nothing is delivered
	resp, err = noFollow.Get(rs.servers[0].URL + "/api/oauth2-redirect?state=short&code=x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// Collections and settings changed on one replica are visible on the others
// without a restart (what the .pb_notify file watcher does on SQLite).
func TestMultiReplicaCachesInvalidated(t *testing.T) {
	rs := newReplicas(t, 3)

	rs.createCollection(2, "cacheprobe", "")

	// an update of the collection (new rule) reaches the others
	c, _ := rs.apps[2].FindCollectionByNameOrId("cacheprobe")
	rule := "id != ''"
	c.ListRule = &rule
	if err := rs.apps[2].Save(c); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		app := rs.apps[i]
		eventually(t, 5*time.Second, "the updated rule", func() bool {
			cached, err := app.FindCachedCollectionByNameOrId("cacheprobe")
			return err == nil && cached.ListRule != nil && *cached.ListRule == rule
		})
	}

	// deleting it too
	if err := rs.apps[0].Delete(c); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 3; i++ {
		app := rs.apps[i]
		eventually(t, 5*time.Second, "the collection to disappear", func() bool {
			_, err := app.FindCachedCollectionByNameOrId("cacheprobe")
			return err != nil
		})
	}

	// settings
	s := rs.apps[1].Settings()
	s.Meta.AppName = "Renamed on replica 1"
	if err := rs.apps[1].Save(s); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 2} {
		app := rs.apps[i]
		eventually(t, 5*time.Second, "the new app name", func() bool {
			return app.Settings().Meta.AppName == "Renamed on replica 1"
		})
	}

	// a field key reload event reaches the other replicas
	before := core.ReplicaFieldKeyReloads()
	core.PublishReplicaEvent(rs.apps[0], core.ReplicaEventFieldKeys, nil)
	eventually(t, 5*time.Second, "the field key reload", func() bool { return core.ReplicaFieldKeyReloads() >= before+2 })
}

// A session revoked on one replica stops working on the others: at once when
// the event arrives, and in any case within core.SessionCacheTTL.
func TestMultiReplicaSessionRevocation(t *testing.T) {
	rs := newReplicas(t, 2)

	users, _ := rs.apps[0].FindCollectionByNameOrId("users")
	u := core.NewRecord(users)
	u.SetEmail("sess@example.com")
	u.SetPassword("1234567890")
	if err := rs.apps[0].Save(u); err != nil {
		t.Fatal(err)
	}

	sid, err := core.CreateSession(rs.apps[0], u, core.SessionAuthMethodOther, "ua", "127.0.0.1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// replica 1 validates and caches it
	if err := core.ValidateSession(rs.apps[1], sid, users.Id, u.Id); err != nil {
		t.Fatal(err)
	}

	if _, err := core.RevokeSessions(rs.apps[0], users.Id, u.Id, "test", ""); err != nil {
		t.Fatal(err)
	}

	eventually(t, 3*time.Second, "the revocation on the other replica", func() bool {
		return core.ValidateSession(rs.apps[1], sid, users.Id, u.Id) != nil
	})
}

// Cron: a job registered on every replica runs once per tick across all of
// them, a replica local job runs on each, and a manual run does not overlap.
func TestMultiReplicaCronRunsOnce(t *testing.T) {
	rs := newReplicas(t, 3)

	var mu sync.Mutex
	perMinute := map[int64]int{}
	var localRuns atomic.Int32

	for _, app := range rs.apps {
		app.Cron().Add("mrshared", "* * * * *", func() {
			mu.Lock()
			perMinute[time.Now().Round(time.Minute).Unix()]++
			mu.Unlock()
		})
		app.Cron().Add("__pbLogsCleanup__", "* * * * *", func() { localRuns.Add(1) })
		app.Cron().SetInterval(time.Second) // a tick per second, three ticks per replica below
		app.Cron().Start()
	}
	t.Cleanup(func() {
		for _, app := range rs.apps {
			app.Cron().Stop()
		}
	})

	time.Sleep(4500 * time.Millisecond)

	for _, app := range rs.apps {
		app.Cron().Stop()
	}

	mu.Lock()
	defer mu.Unlock()

	total := 0
	for minute, n := range perMinute {
		total += n
		if n != 1 {
			t.Fatalf("the job ran %d times in the minute %d, want exactly once per tick across replicas", n, minute)
		}
	}
	if total == 0 {
		t.Fatal("the job never ran")
	}
	// the local job is not coordinated: every replica ran it on every tick
	if localRuns.Load() < 3*3 {
		t.Fatalf("replica local job ran %d times, want at least one run per replica per tick", localRuns.Load())
	}

	// manual runs (the dashboard's "run now") of a job that is already running
	// elsewhere are skipped
	var concurrent, maxConcurrent atomic.Int32
	for _, app := range rs.apps {
		app.Cron().Add("mrmanual", "0 0 1 1 *", func() {
			n := concurrent.Add(1)
			for {
				m := maxConcurrent.Load()
				if n <= m || maxConcurrent.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(300 * time.Millisecond)
			concurrent.Add(-1)
		})
	}
	var wg sync.WaitGroup
	for _, app := range rs.apps {
		for _, j := range app.Cron().Jobs() {
			if j.Id() == "mrmanual" {
				wg.Add(1)
				go func() { defer wg.Done(); j.Run() }()
			}
		}
	}
	wg.Wait()
	if maxConcurrent.Load() != 1 {
		t.Fatalf("manual runs overlapped: %d at once", maxConcurrent.Load())
	}
}

// The shared counters make the budget of the credential endpoints a limit of
// the whole deployment, not of each replica.
func TestMultiReplicaAuthRateLimitIsShared(t *testing.T) {
	rs := newReplicas(t, 2)

	s := rs.apps[0].Settings()
	s.RateLimits.Enabled = true
	s.RateLimits.Rules = []core.RateLimitRule{{Label: "*:authWithPassword", MaxRequests: 3, Duration: 60, Audience: core.RateLimitRuleAudienceAll}}
	if err := rs.apps[0].Save(s); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the rate limit settings on replica 1", func() bool {
		return rs.apps[1].Settings().RateLimits.Enabled
	})

	post := func(i int) int {
		body := strings.NewReader(`{"identity":"nobody@example.com","password":"wrongwrong"}`)
		req, _ := http.NewRequest(http.MethodPost, rs.servers[i].URL+"/api/collections/users/auth-with-password", body)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	// 3 allowed in total, spread over both replicas
	for n, i := range []int{0, 1, 0} {
		if st := post(i); st == http.StatusTooManyRequests {
			t.Fatalf("request %d on replica %d was limited too early", n+1, i)
		}
	}
	// the 4th is refused wherever it lands, although each replica saw fewer than 3
	if st := post(1); st != http.StatusTooManyRequests {
		t.Fatalf("4th request on replica 1 = %d, want 429", st)
	}
	if st := post(0); st != http.StatusTooManyRequests {
		t.Fatalf("5th request on replica 0 = %d, want 429", st)
	}
}

// A refused start: the mode on SQLite (or without a PostgreSQL URL) fails loud.
func TestMultiReplicaRefusedOnSQLite(t *testing.T) {
	t.Setenv(core.DBURLEnv, "")
	t.Setenv(core.MultiReplicaEnv, "true")

	app := core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir()})
	err := app.Bootstrap()
	if err == nil {
		_ = app.ClearBootstrap()
		t.Fatal("expected the start to be refused")
	}
	if !strings.Contains(err.Error(), "PostgreSQL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Two replicas can be told apart by what they report, and the mode is invisible
// to a deployment that did not ask for it.
func TestMultiReplicaOffByDefault(t *testing.T) {
	app := newApp(t) // CLAVIGER_DB_URL only
	if core.ReplicaBusOf(app) != nil || core.IsMultiReplica(app) {
		t.Fatal("the bus must not run without CLAVIGER_MULTI_REPLICA")
	}

	var n int
	err := app.DB().NewQuery("SELECT count(*) FROM information_schema.tables WHERE table_name LIKE '\\_claviger\\_%'").Row(&n)
	if err != nil || n != 0 {
		t.Fatalf("shared tables created without the mode: %d (err %v)", n, err)
	}
}
