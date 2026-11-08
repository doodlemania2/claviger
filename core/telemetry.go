package core

// Fork-local addition — not upstream PocketBase. See tools/telemetry for the
// feature, its environment variables and why it is opt-in.
//
// This file is the only place core touches it: build the providers before the
// databases connect (so the SQLite driver wrapper can be chosen), flush them on
// terminate, and count auth outcomes through the existing OnRecordAuthWith*
// hooks. No hook is added to App.

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/telemetry"
)

const (
	// TelemetryShutdownHookId is the id of the OnTerminate hook that flushes
	// the trace and metric pipelines.
	TelemetryShutdownHookId = "__pbTelemetryShutdown__"

	telemetryAuthHookIdPrefix = "__pbTelemetryAuth_"
)

// telemetryOwned is true while the active instance was created by initTelemetry.
var telemetryOwned atomic.Bool

// initTelemetry starts traces/metrics when the environment asks for them.
// With nothing configured (and no instance installed by a test) it returns at
// the first line: no allocation, no goroutine.
func (app *BaseApp) initTelemetry() {
	t := telemetry.Active()

	if t == nil {
		if !telemetry.Configured() {
			return
		}

		var err error
		t, err = telemetry.Setup(context.Background(), telemetry.Options{})
		if err != nil {
			// Never let telemetry setup take down the app.
			fmt.Fprintf(os.Stderr, "[otel] traces/metrics init failed, disabled: %v\n", err)
			return
		}
		telemetry.SetActive(t)
		t.InstallOutbound()
		telemetryOwned.Store(true)
	}

	t.SetRealtimeSource(func() int64 {
		if b := app.SubscriptionsBroker(); b != nil {
			return int64(b.TotalClients())
		}
		return 0
	})

	app.OnTerminate().Bind(&hook.Handler[*TerminateEvent]{
		Id: TelemetryShutdownHookId,
		Func: func(e *TerminateEvent) error {
			// Only an instance this package created is shut down here; one a
			// test installed is its owner's to close. The flag is process-wide
			// so a re-bootstrap (which rebinds this hook) keeps ownership.
			if telemetryOwned.Swap(false) {
				// Bounded like the log flush: the single-writer handover waits
				// for this process to exit.
				ctx, cancel := context.WithTimeout(context.Background(), otelShutdownTimeout)
				defer cancel()

				telemetry.SetActive(nil)
				t.UninstallOutbound()
				if err := t.Shutdown(ctx); err != nil {
					fmt.Fprintf(os.Stderr, "[otel] telemetry shutdown: %v\n", err)
				}
			}
			return e.Next()
		},
	})

	// Outermost handlers (lowest priority runs first and wraps the rest), so
	// they see the final outcome after every other listener.
	app.OnRecordAuthWithPasswordRequest().Bind(&hook.Handler[*RecordAuthWithPasswordRequestEvent]{
		Id: telemetryAuthHookIdPrefix + "password", Priority: math.MinInt,
		Func: func(e *RecordAuthWithPasswordRequestEvent) error {
			err := e.Next()
			t.RecordAuth("password", err)
			return err
		},
	})
	app.OnRecordAuthWithOAuth2Request().Bind(&hook.Handler[*RecordAuthWithOAuth2RequestEvent]{
		Id: telemetryAuthHookIdPrefix + "oauth2", Priority: math.MinInt,
		Func: func(e *RecordAuthWithOAuth2RequestEvent) error {
			err := e.Next()
			t.RecordAuth("oauth2", err)
			return err
		},
	})
	app.OnRecordAuthWithOTPRequest().Bind(&hook.Handler[*RecordAuthWithOTPRequestEvent]{
		Id: telemetryAuthHookIdPrefix + "otp", Priority: math.MinInt,
		Func: func(e *RecordAuthWithOTPRequestEvent) error {
			err := e.Next()
			t.RecordAuth("otp", err)
			return err
		},
	})
	app.OnRecordAuthWithWebAuthnRequest().Bind(&hook.Handler[*RecordAuthWithWebAuthnRequestEvent]{
		Id: telemetryAuthHookIdPrefix + "webauthn", Priority: math.MinInt,
		Func: func(e *RecordAuthWithWebAuthnRequestEvent) error {
			err := e.Next()
			t.RecordAuth("webauthn", err)
			return err
		},
	})
}

// openTelemetryDB opens the database through the instrumented driver wrapper
// when telemetry is active. It returns (nil, nil) when it is not, and the
// caller then takes the untouched upstream path.
func openTelemetryDB(dbPath, pragmas string) (*dbx.DB, error) {
	t := telemetry.Active()
	if t == nil {
		return nil, nil
	}
	return t.OpenDB("sqlite", dbPath+pragmas, filepath.Base(dbPath))
}
