//go:build postgres

package core_test

// Fork-local (issue #106, M7): the webhook delivery claim of several replicas.
// The rest of the multi replica suite is in tests/pgtests/multireplica_test.go;
// this one needs the test seams of package core.
//
// Run: cd scripts/pgembedded && go run . -count=1 -run MultiReplica ./core

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	_ "github.com/doodlemania2/claviger/migrations"
	"github.com/doodlemania2/claviger/tools/security"
)

func multiReplicaDB(t *testing.T) string {
	t.Helper()

	admin := os.Getenv("CLAVIGER_TEST_PG_URL")
	if admin == "" {
		t.Skip("CLAVIGER_TEST_PG_URL is not set")
	}

	db, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	name := "pbc_" + strings.ToLower(security.PseudorandomString(10))
	if _, err := db.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d, err := sql.Open("pgx", admin)
		if err != nil {
			return
		}
		defer d.Close()
		_, _ = d.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`)
	})

	u, _ := url.Parse(admin)
	u.Path = "/" + name

	return u.String()
}

// Every due delivery is sent by exactly one replica, although all of them poll
// at the same time (FOR UPDATE SKIP LOCKED plus the lease).
func TestMultiReplicaWebhookDeliveredExactlyOnce(t *testing.T) {
	t.Setenv(core.DBURLEnv, multiReplicaDB(t))
	t.Setenv(core.MultiReplicaEnv, "true")
	t.Setenv(core.WebhooksAllowLocalhostEnv, "true")

	const replicas, events = 3, 40

	apps := make([]*core.BaseApp, replicas)
	seams := make([]*core.WebhookTestHooks, replicas)
	for i := range apps {
		apps[i] = core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir()})
	}

	var wg sync.WaitGroup
	for _, app := range apps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := app.Bootstrap(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}

	for i, app := range apps {
		t.Cleanup(func() { _ = app.ClearBootstrap() })
		seams[i] = core.WebhookTestSeams(app)
		seams[i].SetServing(false) // the test drives the workers
		seams[i].Reload()          // loaded with no endpoint yet: the new one must arrive over the bus
	}

	var mu sync.Mutex
	sent := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent[r.Header.Get("webhook-id")]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// the endpoint is created on replica 0 only
	col, _ := apps[0].FindCollectionByNameOrId(core.CollectionNameWebhooks)
	ep := core.NewRecord(col)
	ep.Set("url", srv.URL)
	ep.Set("events", []string{"*"})
	ep.Set("enabled", true)
	if err := core.SetWebhookSecret(apps[0], ep, core.GenerateWebhookSecret()); err != nil {
		t.Fatal(err)
	}
	if err := apps[0].Save(ep); err != nil {
		t.Fatal(err)
	}

	// the others learn about it through the bus: nothing is claimed until they do,
	// so wait until a worker of another replica would see it
	users, _ := apps[0].FindCollectionByNameOrId("users")
	for i := 0; i < events; i++ {
		u := core.NewRecord(users)
		u.SetEmail("wh" + security.PseudorandomString(8) + "@example.com")
		u.SetPassword("1234567890")
		if err := apps[i%replicas].Save(u); err != nil {
			t.Fatal(err)
		}
	}

	pending := func() int {
		var n int
		if err := apps[0].DB().NewQuery("SELECT count(*) FROM {{" + core.CollectionNameWebhookDeliveries + "}} WHERE [[status]] = 'pending'").Row(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	total := func() int {
		var n int
		if err := apps[0].DB().NewQuery("SELECT count(*) FROM {{" + core.CollectionNameWebhookDeliveries + "}}").Row(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if total() < events {
		t.Fatalf("only %d deliveries were enqueued", total())
	}

	deadline := time.Now().Add(30 * time.Second)
	for pending() > 0 && time.Now().Before(deadline) {
		var round sync.WaitGroup
		for _, s := range seams {
			round.Add(1)
			go func() { defer round.Done(); s.ProcessDue() }()
		}
		round.Wait()
	}
	if pending() > 0 {
		t.Fatalf("%d deliveries are still pending", pending())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sent) != total() {
		t.Fatalf("%d distinct deliveries received, %d enqueued", len(sent), total())
	}
	for id, n := range sent {
		if n != 1 {
			t.Fatalf("delivery %s was sent %d times", id, n)
		}
	}
}
