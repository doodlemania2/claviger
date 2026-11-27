//go:build unix

package handover

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	_ "github.com/doodlemania2/claviger/migrations"
)

func newApp(t *testing.T, dir string) *core.BaseApp {
	t.Helper()
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: dir})
	t.Cleanup(func() { _ = app.ClearBootstrap() })
	return app
}

// Two real apps on one data dir: B's bootstrap waits INSIDE the OnBootstrap
// hook (before any database opens) until A has shut its server down, closed its
// databases and acked.
func TestAttachHandsDatabaseOverBetweenApps(t *testing.T) {
	dir := t.TempDir()

	appA := newApp(t, dir)
	parkedA := make(chan struct{}, 4)
	Attach(appA, testConfig(dir, idA), Options{Park: func() { parkedA <- struct{}{} }})
	if err := appA.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	// start the watcher the way apis.Serve would
	srv := &http.Server{}
	if err := appA.OnServe().Trigger(&core.ServeEvent{App: appA, Server: srv}, func(e *core.ServeEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}

	appB := newApp(t, dir)
	Attach(appB, testConfig(dir, idB), Options{Park: func() {}})
	done := make(chan error, 1)
	go func() { done <- appB.Bootstrap() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("B never finished bootstrapping")
	}

	if appA.IsBootstrapped() {
		t.Fatal("A still has its databases open")
	}
	if !appB.IsBootstrapped() {
		t.Fatal("B not bootstrapped")
	}
	if !strings.Contains(readFile(dir, ReleasedFile), idA) || readFile(dir, OwnerFile) != idB {
		t.Fatalf("released=%q owner=%q", readFile(dir, ReleasedFile), readFile(dir, OwnerFile))
	}
	log := eventLog(t, dir)
	for _, want := range []string{"single writer confirmed", "handover requested by '" + idB + "'", "Parking until this replica is drained"} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q in:\n%s", want, log)
		}
	}

	// A's process now runs its terminate handlers: it parks instead of exiting
	if err := appA.OnTerminate().Trigger(&core.TerminateEvent{App: appA}, func(*core.TerminateEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-parkedA:
	default:
		t.Fatal("released A did not park on terminate")
	}
}

// A restarted replica listed in .pb_released parks BEFORE opening a database.
func TestAttachRestartedReleasedReplicaParksWithoutBootstrapping(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ReleasedFile), []byte(idA+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := newApp(t, dir)
	parked := false
	Attach(app, testConfig(dir, idA), Options{Park: func() { parked = true }})

	err := app.Bootstrap()
	if !errors.Is(err, ErrParked) || !parked {
		t.Fatalf("err=%v parked=%v", err, parked)
	}
	if app.IsBootstrapped() {
		t.Fatal("a parked replica must not open the databases")
	}
	if _, err := os.Stat(filepath.Join(dir, "data.db")); err == nil {
		t.Fatal("data.db was created")
	}
}

// Without --handover or the env var nothing is bound at all.
func TestRegisterIsInertByDefault(t *testing.T) {
	t.Setenv(EnvEnable, "")
	app := newApp(t, t.TempDir())
	Register(app) // os.Args of the test binary has no `serve`
	if Attached(app) {
		t.Fatal("handover attached without being asked")
	}
}
