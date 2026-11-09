//go:build postgres

package replica_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/doodlemania2/claviger/tools/replica"
	"github.com/doodlemania2/claviger/tools/security"
)

// freshDB creates an empty database on CLAVIGER_TEST_PG_URL and returns its URL
// and a pool.
func freshDB(t *testing.T) (string, *sql.DB) {
	t.Helper()

	admin := os.Getenv("CLAVIGER_TEST_PG_URL")
	if admin == "" {
		t.Skip("CLAVIGER_TEST_PG_URL is not set")
	}

	adm, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	defer adm.Close()

	name := "pbr_" + strings.ToLower(security.PseudorandomString(10))
	if _, err := adm.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatal(err)
	}

	u, _ := url.Parse(admin)
	u.Path = "/" + name

	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(20)

	t.Cleanup(func() {
		_ = db.Close()
		a, err := sql.Open("pgx", admin)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`)
	})

	if err := replica.EnsureSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}

	return u.String(), db
}

func newBus(t *testing.T, url string, db *sql.DB) *replica.Bus {
	t.Helper()

	b, err := replica.New(replica.Options{URL: url, DB: db})
	if err != nil {
		t.Fatal(err)
	}
	b.Start(10 * time.Second)
	t.Cleanup(b.Stop)

	if !b.Listening() {
		t.Fatal("the bus is not listening")
	}

	return b
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEnsureSchemaConcurrent(t *testing.T) {
	_, db := freshDB(t)

	// a second, third... start at the same time must not fail on CREATE TABLE
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- replica.EnsureSchema(context.Background(), db)
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestBusDeliversToOthersNotToSelf(t *testing.T) {
	url, db := freshDB(t)

	a, b, c := newBus(t, url, db), newBus(t, url, db), newBus(t, url, db)

	var gotA, gotB, gotC atomic.Int32
	var mu sync.Mutex
	var bData string

	a.On("ping", func(replica.Message) { gotA.Add(1) })
	b.On("ping", func(m replica.Message) {
		gotB.Add(1)
		mu.Lock()
		bData = string(m.Data)
		mu.Unlock()
	})
	c.On("ping", func(replica.Message) { gotC.Add(1) })

	if err := a.Publish(context.Background(), "ping", map[string]string{"x": "y"}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 5*time.Second, "delivery", func() bool { return gotB.Load() == 1 && gotC.Load() == 1 })
	time.Sleep(200 * time.Millisecond)

	if gotA.Load() != 0 {
		t.Fatalf("the sender received its own event %d times", gotA.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if bData != `{"x":"y"}` {
		t.Fatalf("payload = %s", bData)
	}
}

func TestBusLargePayloadUsesOverflowRow(t *testing.T) {
	url, db := freshDB(t)

	a, b := newBus(t, url, db), newBus(t, url, db)

	got := make(chan string, 1)
	b.On("big", func(m replica.Message) {
		var s string
		_ = json.Unmarshal(m.Data, &s)
		got <- s
	})

	big := strings.Repeat("x", 50_000) // far over the 8000 byte NOTIFY limit
	if err := a.Publish(context.Background(), "big", big); err != nil {
		t.Fatal(err)
	}

	select {
	case s := <-got:
		if s != big {
			t.Fatalf("payload corrupted (len %d)", len(s))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery")
	}
}

func TestBusPreservesOrder(t *testing.T) {
	url, db := freshDB(t)

	a, b := newBus(t, url, db), newBus(t, url, db)

	var mu sync.Mutex
	var seen []int
	b.On("n", func(m replica.Message) {
		var n int
		_ = json.Unmarshal(m.Data, &n)
		mu.Lock()
		seen = append(seen, n)
		mu.Unlock()
	})

	for i := 0; i < 200; i++ {
		if err := a.Publish(context.Background(), "n", i); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, 10*time.Second, "200 events", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == 200
	})

	mu.Lock()
	defer mu.Unlock()
	for i, n := range seen {
		if n != i {
			t.Fatalf("event %d arrived as %d", i, n)
		}
	}
}

func TestBusReconnectsAndResyncs(t *testing.T) {
	url, db := freshDB(t)

	a, b := newBus(t, url, db), newBus(t, url, db)

	var resyncs, got atomic.Int32
	b.OnResync(func() { resyncs.Add(1) })
	b.On("ping", func(replica.Message) { got.Add(1) })

	// kill every LISTEN connection (b's, and a's)
	if _, err := db.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE pid <> pg_backend_pid() AND datname = current_database() AND query LIKE 'LISTEN %'`); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 15*time.Second, "the reconnect", func() bool {
		cs, _ := b.Stats()
		return resyncs.Load() >= 1 && cs >= 2 && b.Listening() && a.Listening()
	})

	// and the channel works again
	if err := a.Publish(context.Background(), "ping", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "delivery after the reconnect", func() bool { return got.Load() == 1 })
}

func TestRunExclusiveSingleRunnerAndSlot(t *testing.T) {
	_, db := freshDB(t)
	ctx := context.Background()

	// concurrent: one runs, the others see the lock held
	var runs, skipped atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ran, err := replica.RunExclusive(ctx, db, "r", "job", 0, func() { runs.Add(1); time.Sleep(400 * time.Millisecond) })
			if err != nil {
				t.Error(err)
			}
			if !ran {
				skipped.Add(1)
			}
		}()
	}
	wg.Wait()
	if runs.Load() != 1 || skipped.Load() != 4 {
		t.Fatalf("runs=%d skipped=%d, want 1 and 4", runs.Load(), skipped.Load())
	}

	// the lock is free again (and the connection was not poisoned)
	ran, err := replica.RunExclusive(ctx, db, "r", "job", 0, func() {})
	if err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}

	// sequential, same slot: the second replica that gets there late must not rerun the tick
	first, _ := replica.RunExclusive(ctx, db, "a", "tick", 1000, func() {})
	second, _ := replica.RunExclusive(ctx, db, "b", "tick", 1000, func() { t.Error("ran the same slot twice") })
	next, _ := replica.RunExclusive(ctx, db, "b", "tick", 1060, func() {})
	if !first || second || !next {
		t.Fatalf("first=%v second=%v next=%v", first, second, next)
	}
}

func TestRateHitSharedWindow(t *testing.T) {
	_, db := freshDB(t)
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		used, _, err := replica.RateHit(ctx, db, "k", "r", 60)
		if err != nil || used != i {
			t.Fatalf("hit %d: used=%d err=%v", i, used, err)
		}
	}

	used, _, _ := replica.RatePeek(ctx, db, "k")
	if used != 5 {
		t.Fatalf("peek = %d", used)
	}

	// an expired window starts over
	if _, err := db.Exec("UPDATE " + replica.TableRateLimits + " SET expires = 1"); err != nil {
		t.Fatal(err)
	}
	if used, _, _ := replica.RateHit(ctx, db, "k", "r", 60); used != 1 {
		t.Fatalf("after expiry used = %d, want 1", used)
	}
}

func TestCeremonyUsedOnce(t *testing.T) {
	_, db := freshDB(t)
	ctx := context.Background()

	if err := replica.PutCeremony(ctx, db, "tok", []byte(`{"a":1}`), time.Now().Unix()+60); err != nil {
		t.Fatal(err)
	}
	if got, _ := replica.GetCeremony(ctx, db, "tok"); string(got) != `{"a":1}` {
		t.Fatalf("get = %q", got)
	}

	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, _ := replica.TakeCeremony(ctx, db, "tok"); got != nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("take won %d times", wins.Load())
	}

	_ = replica.PutCeremony(ctx, db, "old", []byte(`{}`), time.Now().Unix()-1)
	if got, _ := replica.GetCeremony(ctx, db, "old"); got != nil {
		t.Fatal("an expired ceremony must not be returned")
	}
}
