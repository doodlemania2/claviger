package handover

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
)

// ErrParked is returned by the bootstrap hook when a restarted replica that
// already handed the volume over refuses to serve (only when the configured
// Park function returns; the default never does).
var ErrParked = errors.New("handover: this replica already handed the data directory over; not serving")

const (
	storeKey         = "claviger.handover"
	bootstrapHookID  = "claviger_handover_bootstrap"
	serveHookID      = "claviger_handover_serve"
	terminateHookID  = "claviger_handover_park"
	releaseDrainWait = 10 * time.Second
)

// Options tune Attach. The zero value is what production uses.
type Options struct {
	// Park blocks while a released replica waits to be drained. Default:
	// ParkForever (waits for SIGTERM/SIGINT, then exits 0).
	Park func()
}

// Register wires the handover into app when it is asked for: the command line
// is `serve` and either --handover is given or CLAVIGER_HANDOVER=binary. With
// neither, nothing is bound and serve behaves exactly as before.
func Register(app core.App) {
	if !Requested(os.Args[1:]) {
		return
	}
	Attach(app, ConfigFromEnv(app.DataDir()), Options{})
}

// Requested reports whether args (os.Args[1:]) ask for the in-binary handover.
func Requested(args []string) bool {
	serve, flag := false, false
	for _, a := range args {
		switch a {
		case "serve":
			serve = true
		case "--handover", "--handover=true", "--handover=1":
			flag = true
		case "--handover=false", "--handover=0":
			flag = false
		}
	}
	return serve && (flag || EnabledByEnv())
}

// Attached reports whether Attach was called on app.
func Attached(app core.App) bool {
	_, ok := app.Store().Get(storeKey).(*Handover)
	return ok
}

// Attach binds the handover to app using only existing hooks:
//
//   - OnBootstrap, BEFORE the databases open: Acquire (incoming side). A
//     replica listed in .pb_released parks here and never opens data.db.
//   - OnServe, once the server exists: start the watcher (outgoing side). On a
//     request it shuts the HTTP server down, closes the databases, releases
//     the lock and acks.
//   - OnTerminate, after the other handlers: a released replica parks instead
//     of letting the process exit (which the platform would see as a crash).
//
// Events go to app.Logger() once it exists, plus the durable log file.
func Attach(app core.App, cfg Config, opts Options) *Handover {
	if cfg.Dir == "" {
		cfg.Dir = app.DataDir()
	}
	park := opts.Park
	if park == nil {
		park = ParkForever
	}

	h := New(cfg)
	h.DeferEvents()
	app.Store().Set(storeKey, h)

	var once sync.Once

	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id:       bootstrapHookID,
		Priority: -1 << 20, // before everything that could open a database
		Func: func(e *core.BootstrapEvent) error {
			// Bootstrap can run again (restart, tests); the volume is ours
			// already after the first time.
			res := Skipped
			once.Do(func() { res = h.Acquire(context.Background()) })
			if res == Parked {
				park()
				return ErrParked
			}

			err := e.Next()

			// the logger exists now: flush the events raised before it did
			h.SetEventSink(func(level, msg string) {
				lg := e.App.Logger()
				attrs := []any{slog.String("source", "single-writer-handover"), slog.String("instance", h.cfg.InstanceID)}
				if level == "WARN" {
					lg.Warn(msg, attrs...)
				} else {
					lg.Info(msg, attrs...)
				}
			})
			return err
		},
	})

	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id:       serveHookID,
		Priority: 1 << 20, // after the server is fully configured
		Func: func(e *core.ServeEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			srv := e.Server
			go h.Watch(context.Background(), func(requester string) {
				releaseApp(e.App, h, srv, requester)
			})
			return nil
		},
	})

	app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id:       terminateHookID,
		Priority: 1 << 20, // after log/telemetry flushes
		Func: func(e *core.TerminateEvent) error {
			if h.Released() {
				park()
				return nil
			}
			h.Close()
			return e.Next()
		},
	})

	return h
}

// releaseApp stops serving and hands the volume over. The order matters: mark
// first (so the process parks rather than exits when Serve returns), stop
// accepting and drain, close the databases (checkpointing the WAL), and only
// then drop the lock and ack.
func releaseApp(app core.App, h *Handover, srv *http.Server, requester string) {
	h.event("INFO", "handover: stopping claviger and releasing /pb_data...")
	h.MarkReleasing()

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), releaseDrainWait)
		if err := srv.Shutdown(ctx); err != nil {
			_ = srv.Close() // long-lived connections (SSE) must not hold the volume
		}
		cancel()
	}
	if err := app.ClearBootstrap(); err != nil {
		h.event("WARN", "handover: closing the databases failed: "+strings.TrimSpace(err.Error()))
	}
	h.Release(requester)
}
