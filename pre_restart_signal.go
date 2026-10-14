//go:build unix

package pocketbase

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

const (
	// PreRestartSignalHookId is the id of the hook that signals the parent
	// process before app.Restart() invokes execve.
	PreRestartSignalHookId = "pbPreRestartSignal"

	preRestartSignalEnv = "PB_PRE_RESTART_SIGNAL"
	preRestartDelayEnv  = "PB_PRE_RESTART_DELAY_MS"

	defaultPreRestartDelayMs = 500
)

// Restart() performs an in-process execve, which replaces the PocketBase
// binary image but leaves any sibling processes (e.g. a Litestream supervisor
// sharing /pb_data) untouched. Those siblings keep their open FDs on the
// pre-restart inodes — fine for plain restarts, catastrophic for a dashboard
// "Restore backup" that swaps data.db underneath them: the replica then
// captures garbage from the orphaned inode and the next cold start restores
// that garbage. See commit f9cac5e0.
//
// When the env var is set, this hook sends the named signal to PPID before
// the restart proceeds, giving an entrypoint script a window to drain
// siblings cleanly. Default delay (500ms) is enough for a typical
// SIGTERM → checkpoint → close cycle.
func bindPreRestartSignal(pb *PocketBase) {
	pb.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id:       PreRestartSignalHookId,
		Priority: -5000, // after pbGracefulShutdown (-9999), before execve
		Func: func(te *core.TerminateEvent) error {
			if !te.IsRestart {
				return te.Next()
			}

			sigName := strings.TrimSpace(os.Getenv(preRestartSignalEnv))
			if sigName == "" {
				return te.Next()
			}

			sig, ok := lookupSignal(sigName)
			if !ok {
				te.App.Logger().Warn(
					"["+PreRestartSignalHookId+"] unrecognized signal name; skipping",
					slog.String("value", sigName),
				)
				return te.Next()
			}

			ppid := os.Getppid()
			if ppid <= 1 {
				// PPID 1 (or unknown) means our parent is the init process;
				// signaling init is almost never what the operator wants.
				te.App.Logger().Warn(
					"["+PreRestartSignalHookId+"] parent is init; skipping",
					slog.Int("ppid", ppid),
				)
				return te.Next()
			}

			if err := syscall.Kill(ppid, sig); err != nil {
				te.App.Logger().Warn(
					"["+PreRestartSignalHookId+"] kill failed",
					slog.Int("ppid", ppid),
					slog.String("signal", sigName),
					slog.String("error", err.Error()),
				)
				return te.Next()
			}

			te.App.Logger().Info(
				"["+PreRestartSignalHookId+"] signaled parent before restart",
				slog.Int("ppid", ppid),
				slog.String("signal", sigName),
			)

			if delay := preRestartDelay(); delay > 0 {
				time.Sleep(delay)
			}

			return te.Next()
		},
	})
}

func preRestartDelay() time.Duration {
	ms := defaultPreRestartDelayMs
	if v := strings.TrimSpace(os.Getenv(preRestartDelayEnv)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			ms = n
		}
	}
	return time.Duration(ms) * time.Millisecond
}

// lookupSignal accepts canonical POSIX signal names ("SIGTERM", "SIGUSR1", …)
// and is intentionally restrictive — SIGKILL/SIGSTOP cannot be caught by the
// parent so allowing them would be a footgun.
func lookupSignal(name string) (syscall.Signal, bool) {
	switch strings.ToUpper(name) {
	case "SIGHUP", "HUP":
		return syscall.SIGHUP, true
	case "SIGINT", "INT":
		return syscall.SIGINT, true
	case "SIGQUIT", "QUIT":
		return syscall.SIGQUIT, true
	case "SIGTERM", "TERM":
		return syscall.SIGTERM, true
	case "SIGUSR1", "USR1":
		return syscall.SIGUSR1, true
	case "SIGUSR2", "USR2":
		return syscall.SIGUSR2, true
	}
	return 0, false
}
