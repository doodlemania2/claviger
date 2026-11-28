// Package handover is the in-binary version of the single-writer handover that
// entrypoint.sh performs (#35, #54, #72).
//
// SQLite allows one writer, but a container platform may start the incoming
// replica before it stops the outgoing one. Blocking the incoming replica until
// the outgoing one is gone deadlocks the rollout (readiness gates the drain),
// so the dependency is inverted and the protocol, shared with entrypoint.sh
// file for file, is:
//
//  1. incoming writes its id to .pb_handover
//  2. the outgoing replica's watcher reads an id that is not its own (and not a
//     stale one), stops serving, releases the lock, records itself in
//     .pb_released, writes the requester's id to .pb_handover_ack and parks
//  3. incoming holds the lock AND sees its own id in the ack, writes itself to
//     .pb_owner and starts serving
//
// Rules that must survive any edit: the wait is never a blocking lock, it
// FAILS OPEN after the timeout, and the ack, not the lock, is the proof (on NFS
// a flock release can be missed). A restarted replica listed in .pb_released
// parks instead of serving.
package handover

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// File names inside the data directory; identical to entrypoint.sh.
const (
	LockFile     = ".pb_singlewriter.lock"
	RequestFile  = ".pb_handover"
	AckFile      = ".pb_handover_ack"
	OwnerFile    = ".pb_owner"
	ReleasedFile = ".pb_released"
	EventLogFile = ".pb_handover.log"
)

const (
	maxEventLogLines = 500
	maxReleasedLines = 50
)

// Environment variables, the same ones entrypoint.sh reads.
const (
	EnvEnable       = "CLAVIGER_HANDOVER" // "binary" (or 1/true) turns the in-binary handover on
	EnvTimeout      = "PB_HANDOVER_TIMEOUT"
	EnvPoll         = "PB_HANDOVER_POLL"
	EnvInstanceID   = "PB_INSTANCE_ID"
	envDBURL        = "CLAVIGER_DB_URL"
	envMultiReplica = "CLAVIGER_MULTI_REPLICA"
)

// Result is what Acquire concluded.
type Result int

const (
	// Skipped: no handover applies (disabled, multi-replica on PostgreSQL,
	// unusable data dir). Serve.
	Skipped Result = iota
	// Confirmed: lock held and the ack seen (or nothing to wait for). Serve.
	Confirmed
	// FailedOpen: the wait expired or could not be confirmed. Serve anyway.
	FailedOpen
	// Parked: this replica already handed the volume over and was restarted.
	// The caller must park, never serve.
	Parked
)

// Config configures a Handover. The zero values of Poll and Step get defaults.
type Config struct {
	Dir        string
	InstanceID string
	// Timeout bounds how long the incoming replica waits. 0 disables the whole
	// mechanism (the PB_HANDOVER_TIMEOUT=0 kill switch).
	Timeout time.Duration
	// Poll is how often the outgoing replica's watcher looks for a request.
	Poll time.Duration
	// Step is how often the incoming wait re-checks the lock and the ack.
	Step time.Duration
	// MultiReplica skips everything: several replicas on PostgreSQL overlap by
	// design (see ConfigFromEnv).
	MultiReplica bool
	// Events receives every event after it is written to the durable log.
	Events func(level, msg string)

	// test seams
	lock func(*os.File) (held bool, err error)
}

// ConfigFromEnv reads the knobs entrypoint.sh reads. Durations accept a Go
// duration ("90s") or a bare number of seconds ("60").
func ConfigFromEnv(dir string) Config {
	c := Config{
		Dir:          dir,
		InstanceID:   os.Getenv(EnvInstanceID),
		Timeout:      parseSeconds(os.Getenv(EnvTimeout), 60*time.Second),
		Poll:         parseSeconds(os.Getenv(EnvPoll), 2*time.Second),
		MultiReplica: multiReplicaOnPostgres(),
	}
	if c.InstanceID == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			c.InstanceID = h
		} else {
			c.InstanceID = "unknown"
		}
	}
	return c
}

// EnabledByEnv reports whether CLAVIGER_HANDOVER asks for the binary handover.
func EnabledByEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvEnable))) {
	case "binary", "1", "t", "true", "yes", "on":
		return true
	}
	return false
}

func parseSeconds(s string, def time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return d
	}
	return def
}

func multiReplicaOnPostgres() bool {
	u := os.Getenv(envDBURL)
	if !strings.HasPrefix(u, "postgres://") && !strings.HasPrefix(u, "postgresql://") {
		return false
	}
	switch os.Getenv(envMultiReplica) {
	case "1", "t", "T", "true", "TRUE", "True":
		return true
	}
	return false
}

// Handover is one replica's side of the protocol.
type Handover struct {
	cfg Config

	mu       sync.Mutex
	lockFile *os.File
	released bool
	buffered []event
	sink     func(level, msg string)
}

type event struct{ level, msg string }

// New returns a Handover for cfg.
func New(cfg Config) *Handover {
	if cfg.Poll <= 0 {
		cfg.Poll = 2 * time.Second
	}
	if cfg.Step <= 0 {
		cfg.Step = time.Second
	}
	if cfg.lock == nil {
		cfg.lock = tryLock
	}
	return &Handover{cfg: cfg, sink: cfg.Events}
}

// InstanceID is this replica's id.
func (h *Handover) InstanceID() string { return h.cfg.InstanceID }

// Released reports whether this replica has handed the volume over.
func (h *Handover) Released() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.released
}

// SetEventSink replaces the event sink and flushes anything buffered while no
// sink was set (events raised before app.Logger() exists).
func (h *Handover) SetEventSink(f func(level, msg string)) {
	h.mu.Lock()
	h.sink = f
	pending := h.buffered
	h.buffered = nil
	h.mu.Unlock()
	if f != nil {
		for _, e := range pending {
			f(e.level, e.msg)
		}
	}
}

// DeferEvents makes events buffer until SetEventSink is called.
func (h *Handover) DeferEvents() {
	h.mu.Lock()
	h.sink = nil
	h.mu.Unlock()
}

func (h *Handover) path(name string) string { return filepath.Join(h.cfg.Dir, name) }

// event: console, the durable log, then the sink (app.Logger(), and so OTLP).
func (h *Handover) event(level, msg string) {
	fmt.Fprintf(os.Stderr, "[handover] %s\n", msg)
	line := fmt.Sprintf("%s %s %s %s\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), h.cfg.InstanceID, level, msg)
	if f, err := os.OpenFile(h.path(EventLogFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.WriteString(line)
		_ = f.Close()
		trimFile(h.path(EventLogFile), maxEventLogLines)
	}

	h.mu.Lock()
	sink := h.sink
	if sink == nil {
		h.buffered = append(h.buffered, event{level, msg})
	}
	h.mu.Unlock()
	if sink != nil {
		sink(level, msg)
	}
}

// trimFile keeps the last n lines. Best-effort: the event log and the released
// list must never be what stops a replica from starting.
func trimFile(path string, n int) {
	b, err := os.ReadFile(path)
	if err != nil || bytes.Count(b, []byte("\n")) <= n {
		return
	}
	lines := bytes.SplitAfter(b, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if os.WriteFile(tmp, bytes.Join(lines, nil), 0o644) != nil || os.Rename(tmp, path) != nil {
		_ = os.Remove(tmp)
	}
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeAtomic writes value+"\n" through a temp file and a rename, so the other
// replica never reads a half-written id.
func writeAtomic(path, value string) error {
	tmp := fmt.Sprintf("%s.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, []byte(value+"\n"), 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (h *Handover) inReleased(id string) bool {
	b, err := os.ReadFile(h.path(ReleasedFile))
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l == id {
			return true
		}
	}
	return false
}

// revisionEpoch extracts the azd revision epoch from a pod name
// (ca-auth--azd-1790612504-747c78d9f4-98cd2 -> 1790612504), or 0.
func revisionEpoch(name string) int64 {
	i := strings.LastIndex(name, "--azd-")
	if i < 0 {
		return 0
	}
	n := name[i+len("--azd-"):]
	if j := strings.IndexByte(n, '-'); j >= 0 {
		n = n[:j]
	}
	v, err := strconv.ParseInt(n, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// isStaleRequester: a request from a replica that has already handed over (a
// liveness restart of a parked container), or from an OLDER azd revision, must
// never be answered, or the serving replica would hand the volume back to the
// one being retired.
func (h *Handover) isStaleRequester(id string) bool {
	if h.inReleased(id) {
		return true
	}
	mine, theirs := revisionEpoch(h.cfg.InstanceID), revisionEpoch(id)
	return mine != 0 && theirs != 0 && theirs < mine
}

func (h *Handover) requestHandover() {
	if err := writeAtomic(h.path(RequestFile), h.cfg.InstanceID); err != nil {
		h.event("WARN", fmt.Sprintf("could not write %s; the previous replica will not be asked to leave.", h.path(RequestFile)))
	}
}

// Acquire is the incoming side. It runs before anything opens data.db and
// returns once this replica may serve, or must park. It never blocks longer
// than Timeout (nor past ctx) and never fails: every problem fails open.
func (h *Handover) Acquire(ctx context.Context) Result {
	if h.cfg.MultiReplica {
		h.event("INFO", "multi-replica mode on PostgreSQL: skipping the single-writer handover.")
		return Skipped
	}
	if h.cfg.Timeout <= 0 {
		h.event("WARN", "PB_HANDOVER_TIMEOUT=0 — single-writer handover disabled. Two replicas sharing /pb_data will corrupt SQLite.")
		return Skipped
	}
	if err := os.MkdirAll(h.cfg.Dir, 0o755); err != nil {
		h.event("WARN", fmt.Sprintf("cannot create %s; skipping single-writer handover.", h.cfg.Dir))
		return Skipped
	}

	// Zombie guard: already handed /pb_data over, restarted by liveness while
	// parked. Must never serve again.
	if h.inReleased(h.cfg.InstanceID) {
		h.event("INFO", "this replica already handed /pb_data over; parking instead of serving (container restarted while parked).")
		return Parked
	}

	// No lock file yet means no replica ever ran the handover on this volume.
	_, statErr := os.Stat(h.path(LockFile))
	fresh := statErr != nil

	f, err := os.OpenFile(h.path(LockFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		h.requestHandover()
		h.event("WARN", fmt.Sprintf("cannot create %s; skipping the lock wait.", h.path(LockFile)))
		return FailedOpen
	}
	h.mu.Lock()
	h.lockFile = f
	h.mu.Unlock()

	owner := readTrim(h.path(OwnerFile))
	needAck := false
	switch {
	case fresh:
		h.event("INFO", "first start on this volume; taking the lock.")
	case owner == h.cfg.InstanceID:
		// This replica's own container restarting: the previous process is
		// gone with the container, so only the lock is needed, not an ack.
		h.event("INFO", fmt.Sprintf("restarting as the current owner; waiting for the lock (up to %s)...", h.cfg.Timeout))
	default:
		needAck = true
		_ = os.Remove(h.path(AckFile))
		h.requestHandover()
		who := owner
		if who == "" {
			who = "unknown"
		}
		h.event("INFO", fmt.Sprintf("waiting for the previous replica (%s) to release /pb_data (up to %s)...", who, h.cfg.Timeout))
	}

	start := time.Now()
	locked, acked := false, false
	lockErr := error(nil)
	for {
		if !locked {
			held, err := h.cfg.lock(f)
			switch {
			case err != nil:
				lockErr = err
			case !held:
				locked = true
			}
			if lockErr != nil {
				break
			}
		}
		if locked {
			if !needAck {
				break
			}
			if readTrim(h.path(AckFile)) == h.cfg.InstanceID {
				acked = true
				break
			}
		}
		if time.Since(start) >= h.cfg.Timeout {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(h.cfg.Step):
			continue
		}
		break
	}
	waited := int(time.Since(start).Round(time.Second) / time.Second)
	timeout := int(h.cfg.Timeout / time.Second)

	result := FailedOpen
	switch {
	case lockErr != nil:
		if !needAck {
			h.requestHandover()
		}
		h.event("WARN", fmt.Sprintf("flock not found or unusable (%v) — cannot confirm the previous replica let go.", lockErr))
		return FailedOpen // no ownership files written, like entrypoint.sh
	case locked && (!needAck || acked):
		h.event("INFO", fmt.Sprintf("/pb_data is ours after %ds — single writer confirmed.", waited))
		result = Confirmed
	case locked:
		// Expected once, on the deploy that introduces the ack, and after the
		// previous replica's pod died outright.
		h.event("WARN", fmt.Sprintf("lock held, but the previous replica (%s) never acknowledged within %ds. Starting — if it was still running, two writers overlapped. See DEPLOY.md and #54.", orUnknown(owner), timeout))
	default:
		h.event("WARN", fmt.Sprintf("no handover after %ds — the lock is still held. Starting anyway WITHOUT the lock: two writers are sharing /pb_data and auxiliary.db may corrupt. See DEPLOY.md, #35 and #54.", timeout))
	}

	_ = writeAtomic(h.path(OwnerFile), h.cfg.InstanceID)
	// Clear our own request, and only ours — never a newer replica's.
	if readTrim(h.path(RequestFile)) == h.cfg.InstanceID {
		_ = os.Remove(h.path(RequestFile))
	}
	_ = os.Remove(h.path(AckFile))
	return result
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// Watch is the outgoing side: it polls for a request from another, non-stale
// replica and calls onRequest once with the requester's id, then returns. The
// callback must stop serving and then call Release. Watch returns when ctx is
// done. It does nothing in multi-replica mode.
func (h *Handover) Watch(ctx context.Context, onRequest func(requester string)) {
	if h.cfg.MultiReplica {
		return
	}
	ignored := ""
	for {
		requester := readTrim(h.path(RequestFile))
		// A request carrying our own id is this replica's container
		// restarting, not a new replica. Answering it would hand the volume to
		// ourselves and flap.
		if requester != "" && requester != h.cfg.InstanceID {
			if h.isStaleRequester(requester) {
				if requester != ignored {
					h.event("WARN", fmt.Sprintf("ignoring a handover request from '%s', a replica that is being retired. This replica keeps /pb_data.", requester))
					ignored = requester
				}
			} else {
				h.event("INFO", fmt.Sprintf("handover requested by '%s' — releasing /pb_data.", requester))
				onRequest(requester)
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.cfg.Poll):
		}
	}
}

// MarkReleasing is called first by the release path, before the server stops,
// so the park-on-terminate handler can tell a handover from a normal shutdown.
func (h *Handover) MarkReleasing() {
	h.mu.Lock()
	h.released = true
	h.mu.Unlock()
}

// Release finishes the outgoing side AFTER the caller stopped serving and
// closed its databases: it drops the lock, records this replica in
// .pb_released BEFORE acknowledging (so a liveness restart can never serve
// again), then writes the requester's id to the ack file.
func (h *Handover) Release(requester string) {
	h.MarkReleasing()
	h.Close()

	if f, err := os.OpenFile(h.path(ReleasedFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.WriteString(h.cfg.InstanceID + "\n")
		_ = f.Close()
		trimFile(h.path(ReleasedFile), maxReleasedLines)
	}
	_ = writeAtomic(h.path(AckFile), requester)

	h.event("INFO", fmt.Sprintf("handover: /pb_data released to '%s'. Parking until this replica is drained.", requester))
}

// Close drops the single-writer lock (closing the descriptor releases it).
func (h *Handover) Close() {
	h.mu.Lock()
	f := h.lockFile
	h.lockFile = nil
	h.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

// ParkForever sits still until the platform drains this replica, then exits 0.
//
// Exiting early looks like a crash and the platform restarts the container,
// which then races the incoming replica for /pb_data. Health probes fail while
// parked: readiness failing is right, and liveness failing gets the container
// restarted anyway, which is why Acquire answers Parked for a released id.
func ParkForever() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	os.Exit(0)
}
