package core

// Fork-local addition (issue #106, M7): several replicas on one PostgreSQL
// database.
//
// Opt-in with CLAVIGER_MULTI_REPLICA=true (or BaseAppConfig.MultiReplica) and
// refused unless the data database is PostgreSQL. With the mode off nothing in
// this file does anything: the bus is nil and every helper is a no-op, so
// SQLite and single replica Postgres installations behave as before.
//
// What this file wires (the rest lives next to the state it coordinates):
//
//   - the event bus (tools/replica): LISTEN/NOTIFY on a dedicated connection;
//   - the cache invalidation that the .pb_notify file watcher does on SQLite:
//     settings, collections, session cache, field keys, webhook endpoints;
//   - cron: every job runs on one replica only (advisory lock plus a per
//     minute marker), except the jobs that clean replica local state.
//
// See docs/datastores.md ("Several replicas") for the guarantees.

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/doodlemania2/claviger/tools/dialect"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/replica"
)

// MultiReplicaEnv is the name of the environment variable that turns the
// multi replica mode on ("true"). It requires CLAVIGER_DB_URL to be PostgreSQL.
const MultiReplicaEnv = "CLAVIGER_MULTI_REPLICA"

// Event kinds of the replica bus that package core owns.
const (
	ReplicaEventSettings    = "settings"
	ReplicaEventCollections = "collections"
	ReplicaEventSession     = "session"
	ReplicaEventFieldKeys   = "fieldkeys"
	ReplicaEventWebhooks    = "webhooks"
)

const (
	systemHookIdMultiReplica = "__clvMultiReplica__"

	replicaPublishTimeout = 5 * time.Second
	replicaReloadDebounce = 50 * time.Millisecond
)

// localCronJobs are the jobs that clean state that exists once per replica
// (the local auxiliary.db, the in memory limiters, the local WAL), so every
// replica has to run them. All other jobs touch the shared database and run on
// one replica only.
var localCronJobs = map[string]struct{}{
	"__pbLogsCleanup__":         {},
	"__pbDBOptimize__":          {},
	"__pbRateLimitersCleanup__": {},
}

// RegisterLocalCronJob declares a cron job id as replica local: it keeps
// running on every replica. Call it before the job is first due.
func RegisterLocalCronJob(id string) {
	localCronMu.Lock()
	defer localCronMu.Unlock()
	localCronJobs[id] = struct{}{}
}

var localCronMu sync.RWMutex

func isLocalCronJob(id string) bool {
	localCronMu.RLock()
	defer localCronMu.RUnlock()
	_, ok := localCronJobs[id]
	return ok
}

func applyEnvMultiReplica(config *BaseAppConfig) {
	if v := strings.TrimSpace(os.Getenv(MultiReplicaEnv)); v != "" {
		if on, err := strconv.ParseBool(v); err == nil && on {
			config.MultiReplica = true
		}
	}

	if config.MultiReplica && config.DBURL == "" {
		config.DBURL = strings.TrimSpace(os.Getenv(DBURLEnv))
	}
}

// ReplicaBus returns the multi replica event bus (nil when the mode is off).
func (app *BaseApp) ReplicaBus() *replica.Bus {
	return app.replicaBus
}

// ReplicaBusOf returns the multi replica event bus of the app, or nil when the
// app is not running in multi replica mode (or is not a BaseApp based app).
func ReplicaBusOf(app App) *replica.Bus {
	if p, ok := app.(interface{ ReplicaBus() *replica.Bus }); ok {
		return p.ReplicaBus()
	}
	return nil
}

// IsMultiReplica reports whether the app runs in the multi replica mode.
func IsMultiReplica(app App) bool {
	return ReplicaBusOf(app) != nil
}

// PublishReplicaEvent sends an event to the other replicas. It is a no-op
// without the mode, and a failure only logs: the callers already applied the
// change locally and the other replicas catch up through their TTLs and the
// resync after a reconnect.
func PublishReplicaEvent(app App, kind string, data any) {
	bus := ReplicaBusOf(app)
	if bus == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), replicaPublishTimeout)
	defer cancel()

	if err := bus.Publish(ctx, kind, data); err != nil {
		app.Logger().Warn("Failed to publish a multi replica event", "kind", kind, "error", err)
	}
}

// initReplicaBus validates the mode, creates the shared tables, starts the
// bus and installs the cron guard.
func (app *BaseApp) initReplicaBus() error {
	if !app.config.MultiReplica {
		return nil
	}

	if app.replicaBus != nil {
		app.replicaBus.Stop()
		app.replicaBus = nil
	}

	if dialect.Of(app.concurrentDB).Name() != dialect.PostgresName {
		return fmt.Errorf("%s=true needs the data database to be PostgreSQL (set %s): several replicas cannot share a SQLite file", MultiReplicaEnv, DBURLEnv)
	}

	if app.config.DBURL == "" {
		return fmt.Errorf("%s=true needs the PostgreSQL connection URL (%s) for its LISTEN connection", MultiReplicaEnv, DBURLEnv)
	}

	dbHolder, ok := app.concurrentDB.(interface{ DB() *sql.DB })
	if !ok {
		return errors.New("the data database does not expose its sql pool")
	}
	sqlDB := dbHolder.DB()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := replica.EnsureSchema(ctx, sqlDB); err != nil {
		return fmt.Errorf("failed to prepare the multi replica tables: %w", err)
	}

	bus, err := replica.New(replica.Options{URL: app.config.DBURL, DB: sqlDB, Logger: app.Logger()})
	if err != nil {
		return err
	}

	app.registerReplicaHandlers(bus)
	app.replicaBus = bus

	// every job runs on one replica only
	app.cron.SetRunGuard(func(jobId string, slot time.Time, run func()) {
		if isLocalCronJob(jobId) {
			run()
			return
		}

		var slotKey int64
		if !slot.IsZero() {
			slotKey = slot.Round(time.Minute).Unix() // rounded: replica clocks differ by milliseconds
		}

		_, err := replica.RunExclusive(context.Background(), sqlDB, bus.ID(), jobId, slotKey, run)
		if err != nil {
			app.Logger().Warn("Failed to coordinate a cron job between the replicas, it did not run here", "job", jobId, "error", err)
		}
	})

	// the shared tables are short lived scratch space: one replica trims them
	app.cron.Add("__clvReplicaPurge__", "*/10 * * * *", func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		if err := replica.PurgeRateLimits(ctx, sqlDB); err != nil {
			app.Logger().Warn("Failed to purge the shared rate limits", "error", err)
		}
		if err := replica.PurgeCeremonies(ctx, sqlDB); err != nil {
			app.Logger().Warn("Failed to purge the shared WebAuthn sessions", "error", err)
		}
	})

	bus.Start(10 * time.Second)

	return nil
}

// registerReplicaHandlers installs the receivers of the events that package
// core owns.
func (app *BaseApp) registerReplicaHandlers(bus *replica.Bus) {
	settings := newReplicaDebouncer(func() {
		if !app.IsBootstrapped() {
			return
		}
		app.Logger().Debug("Reloading settings after a replica event")
		if err := app.ReloadSettings(); err != nil {
			app.Logger().Warn("Failed to reload the app settings after a replica event", "error", err)
		}
	})

	collections := newReplicaDebouncer(func() {
		if !app.IsBootstrapped() {
			return
		}
		app.Logger().Debug("Reloading cached collections after a replica event")
		if err := app.ReloadCachedCollections(); err != nil {
			app.Logger().Warn("Failed to reload the cached collections after a replica event", "error", err)
			return
		}
		// a collection save may have created the data keys of an encrypted field
		if err := loadFieldKeys(app); err != nil {
			app.Logger().Warn("Failed to reload the field keys after a replica event", "error", err)
		}
	})

	fieldKeysReload := newReplicaDebouncer(func() {
		if !app.IsBootstrapped() {
			return
		}
		if err := loadFieldKeys(app); err != nil {
			app.Logger().Warn("Failed to reload the field keys after a replica event", "error", err)
		}
		replicaFieldKeyReloads.Add(1)
	})

	webhooks := newReplicaDebouncer(func() {
		if m := webhookManagerFor(app); m != nil && app.IsBootstrapped() {
			m.reload()
		}
	})

	bus.On(ReplicaEventSettings, func(replica.Message) { settings.trigger() })
	bus.On(ReplicaEventCollections, func(replica.Message) { collections.trigger() })
	bus.On(ReplicaEventFieldKeys, func(replica.Message) { fieldKeysReload.trigger() })
	bus.On(ReplicaEventWebhooks, func(replica.Message) { webhooks.trigger() })

	bus.On(ReplicaEventSession, func(m replica.Message) {
		var data struct {
			Sid string `json:"sid"`
		}
		if len(m.Data) > 0 {
			_ = json.Unmarshal(m.Data, &data)
		}

		cache := getSessionCache(app)
		if data.Sid == "" {
			cache.clearLocal()
		} else {
			cache.removeLocal(data.Sid)
		}
	})

	// events may have been missed: reload everything that is cached
	bus.OnResync(func() {
		settings.trigger()
		collections.trigger()
		webhooks.trigger()
		getSessionCache(app).clearLocal()
	})
}

// registerMultiReplicaHooks publishes the changes that the other replicas
// cache. The handlers are inert when the mode is off.
func (app *BaseApp) registerMultiReplicaHooks() {
	app.OnBootstrapClear().Bind(&hook.Handler[*BootstrapEvent]{
		Id: systemHookIdMultiReplica,
		Func: func(e *BootstrapEvent) error {
			// before the database pools are closed
			if app.replicaBus != nil {
				app.replicaBus.Stop()
				app.replicaBus = nil
			}
			return e.Next()
		},
		Priority: -9999,
	})

	settingsChanged := func(e *ModelEvent) error {
		err := e.Next()
		if err == nil && e.Model.PK() == paramsKeySettings {
			PublishReplicaEvent(app, ReplicaEventSettings, nil)
		}
		return err
	}
	app.OnModelAfterCreateSuccess(paramsTable).Bind(&hook.Handler[*ModelEvent]{
		Id: systemHookIdMultiReplica, Func: settingsChanged, Priority: 1000,
	})
	app.OnModelAfterUpdateSuccess(paramsTable).Bind(&hook.Handler[*ModelEvent]{
		Id: systemHookIdMultiReplica, Func: settingsChanged, Priority: 1000,
	})

	collectionsChanged := func(e *CollectionEvent) error {
		err := e.Next()
		if err == nil {
			PublishReplicaEvent(app, ReplicaEventCollections, nil)
		}
		return err
	}
	app.OnCollectionAfterCreateSuccess().Bind(&hook.Handler[*CollectionEvent]{
		Id: systemHookIdMultiReplica, Func: collectionsChanged, Priority: 1000,
	})
	app.OnCollectionAfterUpdateSuccess().Bind(&hook.Handler[*CollectionEvent]{
		Id: systemHookIdMultiReplica, Func: collectionsChanged, Priority: 1000,
	})
	app.OnCollectionAfterDeleteSuccess().Bind(&hook.Handler[*CollectionEvent]{
		Id: systemHookIdMultiReplica, Func: collectionsChanged, Priority: 1000,
	})
}

// replicaFieldKeyReloads counts the field key reloads caused by replica
// events (a test seam: the keyring is process wide).
var replicaFieldKeyReloads atomic.Int64

// ReplicaFieldKeyReloads returns how many times a replica event reloaded the
// field keys in this process.
func ReplicaFieldKeyReloads() int64 { return replicaFieldKeyReloads.Load() }

// replicaDebouncer coalesces bursts of events into one trailing call.
type replicaDebouncer struct {
	fn    func()
	mu    sync.Mutex
	timer *time.Timer
}

func newReplicaDebouncer(fn func()) *replicaDebouncer { return &replicaDebouncer{fn: fn} }

func (d *replicaDebouncer) trigger() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.timer != nil {
		d.timer.Stop()
	}
	d.timer = time.AfterFunc(replicaReloadDebounce, d.fn)
}
