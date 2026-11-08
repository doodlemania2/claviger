package apis

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Persistent rate limit counters (#91).
//
// Upstream keeps every rate limit counter in process memory, so a restart (or
// a rollout) hands an attacker a fresh budget. The fork backs the counters with
// a small table in auxiliary.db:
//
//	_rateLimits(id, rule, windowStart, used, expires)
//
// Design:
//
//   - write-through cache: the in-memory rateClient stays the source of truth
//     for decisions (no read from SQLite on the request path). Each allowed
//     request only updates an in-memory record under one mutex;
//   - the dirty records are flushed in ONE transaction every
//     rateLimitFlushEvery (5s) and once more on OnTerminate, so the number of
//     writes depends on time, never on the request rate;
//   - on first use the non-expired rows are loaded, and a new rateClient is
//     seeded from its row (window start and consumed count), so the fixed
//     window of upstream is preserved across restarts;
//   - expired rows are deleted at load and then every rateLimitCleanEvery;
//   - the table is created lazily (like the backup verification results), so
//     no system migration is needed. If it cannot be created or read the
//     store is disabled with a WARN and the limiters stay in memory only;
//   - the client key (an IP address or a submitted identity) is never stored:
//     rows are identified by sha256(rule, key) only.
//
// Counters are bound by the same semantics as before: Settings reload still
// resets the counters of the configurable limiters (see forget).
const (
	rateLimitTable      = "_rateLimits"
	rateLimitStoreKey   = "__clvRateLimitStore__"
	rateLimitHookId     = "__clvRateLimitStoreFlush__"
	rateLimitFlushEvery = 5 * time.Second
	rateLimitCleanEvery = time.Minute
	rateLimitFlushChunk = 100

	// rule names of the always-on limiters that are NOT reset on Settings
	// reload (they do not depend on Settings.RateLimits)
	rateLimitFixedRulePrefix = "webauthn:"
)

type rateLimitRec struct {
	rule    string
	start   int64
	used    int
	expires int64
}

type rateLimitStore struct {
	app core.App

	mu      sync.Mutex
	recs    map[string]*rateLimitRec
	dirty   map[string]struct{}
	deleted map[string]struct{}

	// test hooks / stats
	flushes int // number of flushes that wrote to the database
	now     func() time.Time

	lastClean time.Time
	timerSet  bool // a flush is already scheduled
	closed    bool
	disabled  bool
}

// rateLimitStoreFor returns the app persistent store, or nil when persistence
// is unavailable (the limiters then work in memory only).
func rateLimitStoreFor(app core.App) *rateLimitStore {
	s, _ := app.Store().GetOrSet(rateLimitStoreKey, func() any {
		return newRateLimitStore(app)
	}).(*rateLimitStore)

	if s == nil || s.disabled {
		return nil
	}

	return s
}

// newRateLimitStore ensures the table, loads the live rows and starts the
// background flusher. On failure it returns a disabled store.
func newRateLimitStore(app core.App) *rateLimitStore {
	s := &rateLimitStore{
		app:       app,
		recs:      map[string]*rateLimitRec{},
		dirty:     map[string]struct{}{},
		deleted:   map[string]struct{}{},
		now:       time.Now,
		lastClean: time.Now(),
	}

	if err := s.load(); err != nil {
		app.Logger().Warn("Persistent rate limits unavailable, falling back to in-memory counters", "error", err)
		s.disabled = true
		return s
	}

	app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id:       rateLimitHookId,
		Priority: -100,
		Func: func(e *core.TerminateEvent) error {
			s.close()
			return e.Next()
		},
	})

	return s
}

func (s *rateLimitStore) load() error {
	db := s.app.AuxNonconcurrentDB()

	_, err := db.NewQuery(
		"CREATE TABLE IF NOT EXISTS {{" + rateLimitTable + "}} (" +
			"[[id]] TEXT PRIMARY KEY NOT NULL, " +
			"[[rule]] TEXT NOT NULL DEFAULT '', " +
			"[[windowStart]] INTEGER NOT NULL DEFAULT 0, " +
			"[[used]] INTEGER NOT NULL DEFAULT 0, " +
			"[[expires]] INTEGER NOT NULL DEFAULT 0)",
	).Execute()
	if err != nil {
		return err
	}

	// drop the expired rows first so they are never loaded
	if _, err = db.NewQuery("DELETE FROM {{" + rateLimitTable + "}} WHERE [[expires]] <= {:now}").
		Bind(dbx.Params{"now": s.now().Unix()}).Execute(); err != nil {
		return err
	}

	var rows []struct {
		Id          string `db:"id"`
		Rule        string `db:"rule"`
		WindowStart int64  `db:"windowStart"`
		Used        int    `db:"used"`
		Expires     int64  `db:"expires"`
	}
	if err = db.NewQuery("SELECT [[id]], [[rule]], [[windowStart]], [[used]], [[expires]] FROM {{" + rateLimitTable + "}}").
		All(&rows); err != nil {
		return err
	}

	s.mu.Lock()
	for _, r := range rows {
		s.recs[r.Id] = &rateLimitRec{rule: r.Rule, start: r.WindowStart, used: r.Used, expires: r.Expires}
	}
	s.mu.Unlock()

	return nil
}

// rateLimitRecordId derives the row id; the client key itself is never persisted.
func rateLimitRecordId(rule, key string) string {
	sum := sha256.Sum256([]byte(rule + "\x00" + key))
	return hex.EncodeToString(sum[:16])
}

// lookup returns the live record for the id (nil if none).
func (s *rateLimitStore) lookup(id string) *rateLimitRec {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec := s.recs[id]
	if rec == nil || rec.expires <= s.now().Unix() {
		return nil
	}

	cp := *rec
	return &cp
}

// record stores the latest counter state of a client and marks it dirty.
// It touches memory only.
func (s *rateLimitStore) record(id, rule string, start int64, used int, expires int64) {
	s.mu.Lock()
	s.recs[id] = &rateLimitRec{rule: rule, start: start, used: used, expires: expires}
	s.dirty[id] = struct{}{}
	delete(s.deleted, id)
	s.scheduleLocked()
	s.mu.Unlock()
}

// forget drops (and schedules the deletion of) all records whose rule matches.
func (s *rateLimitStore) forget(match func(rule string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, rec := range s.recs {
		if match(rec.rule) {
			delete(s.recs, id)
			delete(s.dirty, id)
			s.deleted[id] = struct{}{}
		}
	}
}

// flush writes the dirty records (and the pending deletions) in one
// transaction. It is a no-op when nothing changed.
func (s *rateLimitStore) flush() error {
	type row struct {
		id string
		rateLimitRec
	}

	s.mu.Lock()
	rows := make([]row, 0, len(s.dirty))
	for id := range s.dirty {
		if rec := s.recs[id]; rec != nil {
			rows = append(rows, row{id, *rec})
		}
	}
	deleted := make([]string, 0, len(s.deleted))
	for id := range s.deleted {
		deleted = append(deleted, id)
	}
	s.dirty = map[string]struct{}{}
	s.deleted = map[string]struct{}{}
	s.mu.Unlock()

	if len(rows) == 0 && len(deleted) == 0 {
		return nil
	}

	err := s.app.AuxRunInTransaction(func(txApp core.App) error {
		db := txApp.AuxNonconcurrentDB()

		for start := 0; start < len(rows); start += rateLimitFlushChunk {
			end := min(start+rateLimitFlushChunk, len(rows))

			var sb strings.Builder
			sb.WriteString("INSERT INTO {{" + rateLimitTable + "}} ([[id]], [[rule]], [[windowStart]], [[used]], [[expires]]) VALUES ")
			params := dbx.Params{}
			for i, r := range rows[start:end] {
				if i > 0 {
					sb.WriteString(", ")
				}
				n := strconv.Itoa(i)
				sb.WriteString("({:id" + n + "}, {:rule" + n + "}, {:ws" + n + "}, {:used" + n + "}, {:exp" + n + "})")
				params["id"+n] = r.id
				params["rule"+n] = r.rule
				params["ws"+n] = r.start
				params["used"+n] = r.used
				params["exp"+n] = r.expires
			}
			sb.WriteString(" ON CONFLICT([[id]]) DO UPDATE SET [[rule]]=excluded.[[rule]], " +
				"[[windowStart]]=excluded.[[windowStart]], [[used]]=excluded.[[used]], [[expires]]=excluded.[[expires]]")

			if _, err := db.NewQuery(sb.String()).Bind(params).Execute(); err != nil {
				return err
			}
		}

		for _, id := range deleted {
			if _, err := db.NewQuery("DELETE FROM {{" + rateLimitTable + "}} WHERE [[id]]={:id}").
				Bind(dbx.Params{"id": id}).Execute(); err != nil {
				return err
			}
		}

		return nil
	})

	s.mu.Lock()
	if err != nil {
		// keep the records for the next attempt
		for _, r := range rows {
			s.dirty[r.id] = struct{}{}
		}
		for _, id := range deleted {
			s.deleted[id] = struct{}{}
		}
	} else {
		s.flushes++
	}
	s.mu.Unlock()

	return err
}

// cleanExpired removes the expired records from memory and from the table.
func (s *rateLimitStore) cleanExpired() error {
	now := s.now().Unix()

	s.mu.Lock()
	for id, rec := range s.recs {
		if rec.expires <= now {
			delete(s.recs, id)
			delete(s.dirty, id)
		}
	}
	s.mu.Unlock()

	_, err := s.app.AuxNonconcurrentDB().
		NewQuery("DELETE FROM {{" + rateLimitTable + "}} WHERE [[expires]] <= {:now}").
		Bind(dbx.Params{"now": now}).Execute()

	return err
}

// scheduleLocked arms (at most) one timer that flushes the dirty records after
// rateLimitFlushEvery. It is a timer rather than a ticker goroutine: nothing
// runs while the limiters are idle, and N requests inside the window cost one
// write transaction in total. s.mu must be held.
func (s *rateLimitStore) scheduleLocked() {
	if s.timerSet || s.closed {
		return
	}

	s.timerSet = true
	time.AfterFunc(rateLimitFlushEvery, s.onTimer)
}

func (s *rateLimitStore) onTimer() {
	s.mu.Lock()
	s.timerSet = false
	closed := s.closed
	s.mu.Unlock()

	if closed || !s.app.IsBootstrapped() {
		return
	}

	if err := s.flush(); err != nil {
		s.app.Logger().Warn("Failed to persist rate limit counters", "error", err)
	}

	if time.Since(s.lastClean) >= rateLimitCleanEvery {
		s.lastClean = time.Now()
		if err := s.cleanExpired(); err != nil {
			s.app.Logger().Warn("Failed to clean expired rate limit counters", "error", err)
		}
	}

	// records written while flushing (or a failed flush) need another pass
	s.mu.Lock()
	if len(s.dirty) > 0 || len(s.deleted) > 0 {
		s.scheduleLocked()
	}
	s.mu.Unlock()
}

// close stops further flush scheduling and writes the pending records.
func (s *rateLimitStore) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	if !s.app.IsBootstrapped() {
		return
	}

	if err := s.flush(); err != nil {
		s.app.Logger().Warn("Failed to persist rate limit counters on shutdown", "error", err)
	}
}

// forgetRateLimitStoreEntries mirrors the reset upstream performs on a
// Settings reload (the configurable limiters start from scratch).
func forgetRateLimitStoreEntries(app core.App) {
	if s, ok := app.Store().Get(rateLimitStoreKey).(*rateLimitStore); ok && s != nil && !s.disabled {
		s.forget(func(rule string) bool { return !isFixedRule(rule) })
	}
}
