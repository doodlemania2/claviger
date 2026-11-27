//go:build unix

package handover

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Port of scripts/test_entrypoint_handover.sh. Each "replica" is a Handover on
// one shared temp dir (the NFS /pb_data) plus a ledger recording when it is
// serving. The assertion that matters is OVERLAP: two serving replicas at once
// is two SQLite writers on one volume.

const (
	idA = "ca-auth--azd-1000000100-aaaa-aaaaa"
	idB = "ca-auth--azd-1000000200-bbbb-bbbbb"
)

type ledger struct {
	mu       sync.Mutex
	serving  map[string]bool
	overlaps []string
}

func newLedger() *ledger { return &ledger{serving: map[string]bool{}} }

func (l *ledger) start(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for other := range l.serving {
		l.overlaps = append(l.overlaps, fmt.Sprintf("OVERLAP: %s started while %s was serving", id, other))
	}
	l.serving[id] = true
}

func (l *ledger) stop(id string) {
	l.mu.Lock()
	delete(l.serving, id)
	l.mu.Unlock()
}

func (l *ledger) isServing(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.serving[id]
}

func (l *ledger) assertNoOverlap(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.overlaps) > 0 {
		t.Fatalf("two writers: %v", l.overlaps)
	}
}

type replica struct {
	h       *Handover
	id      string
	led     *ledger
	result  Result
	done    chan struct{} // Acquire finished
	parked  chan struct{} // closed once released and parked
	cancel  context.CancelFunc
	stopLag time.Duration // how long stopping takes (the NFS flock-backoff window)
}

func testConfig(dir, id string) Config {
	return Config{Dir: dir, InstanceID: id, Timeout: 10 * time.Second, Poll: 50 * time.Millisecond, Step: 20 * time.Millisecond}
}

// startReplica is run_serve: acquire, serve, watch.
func startReplica(t *testing.T, cfg Config, led *ledger, stopLag time.Duration) *replica {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &replica{h: New(cfg), id: cfg.InstanceID, led: led, done: make(chan struct{}), parked: make(chan struct{}), cancel: cancel, stopLag: stopLag}
	t.Cleanup(func() { cancel(); r.h.Close() })
	go func() {
		r.result = r.h.Acquire(ctx)
		close(r.done)
		switch r.result {
		case Parked:
			close(r.parked)
			return
		}
		led.start(r.id)
		r.h.Watch(ctx, func(requester string) {
			r.h.MarkReleasing()
			time.Sleep(r.stopLag)
			led.stop(r.id)
			r.h.Release(requester)
			close(r.parked)
		})
	}()
	return r
}

func (r *replica) waitServing(t *testing.T, d time.Duration) {
	t.Helper()
	waitFor(t, d, r.id+" serving", func() bool { return r.led.isServing(r.id) })
}

// kill is a container restart: the process and its lock are gone.
func (r *replica) kill() {
	r.cancel()
	r.h.Close()
	r.led.stop(r.id)
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func readFile(dir, name string) string { return readTrim(filepath.Join(dir, name)) }

func eventLog(t *testing.T, dir string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, EventLogFile))
	return string(b)
}

// holdLock takes the single-writer lock from "another process" (a separate open
// file description, which conflicts with ours even in one process).
func holdLock(t *testing.T, dir string) func() {
	t.Helper()
	p := filepath.Join(dir, LockFile)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	released := false
	rel := func() {
		if !released {
			released = true
			_ = f.Close()
		}
	}
	t.Cleanup(rel)
	return rel
}

// T1: new -> new handover, then a liveness restart of the parked replica.
func TestHandoverThenParkedReplicaRestart(t *testing.T) {
	dir := t.TempDir()
	led := newLedger()

	a := startReplica(t, testConfig(dir, idA), led, 0)
	a.waitServing(t, 5*time.Second)

	b := startReplica(t, testConfig(dir, idB), led, 0)
	b.waitServing(t, 5*time.Second)

	if led.isServing(idA) {
		t.Fatal("A still serving")
	}
	<-a.parked
	if b.result != Confirmed {
		t.Fatalf("B result = %v, want Confirmed (ack)", b.result)
	}
	if !strings.Contains(eventLog(t, dir), "single writer confirmed") {
		t.Fatalf("no confirmation in log:\n%s", eventLog(t, dir))
	}
	if !strings.Contains(readFile(dir, ReleasedFile), idA) {
		t.Fatal("A not recorded in .pb_released")
	}
	if got := readFile(dir, OwnerFile); got != idB {
		t.Fatalf("owner = %q", got)
	}

	// A's container is restarted by its liveness probe: it must park, not serve.
	a.kill()
	a2 := startReplica(t, testConfig(dir, idA), led, 0)
	select {
	case <-a2.parked:
	case <-time.After(5 * time.Second):
		t.Fatal("restarted A did not park")
	}
	if a2.result != Parked {
		t.Fatalf("restarted A result = %v, want Parked", a2.result)
	}
	time.Sleep(300 * time.Millisecond)
	if led.isServing(idA) {
		t.Fatal("zombie A started serving")
	}
	if !led.isServing(idB) {
		t.Fatal("B stopped after the zombie restart")
	}
	log := eventLog(t, dir)
	if !strings.Contains(log, "already handed /pb_data over") {
		t.Fatalf("no zombie guard event:\n%s", log)
	}
	if !strings.Contains(log, "INFO") {
		t.Fatal("events not recorded durably")
	}
	led.assertNoOverlap(t)
}

// T2: the outgoing replica takes a long time to release (the NFS backoff
// window); the incoming one must wait for the ack, not fall back on a timeout.
func TestSlowReleaseIsConfirmedByAck(t *testing.T) {
	dir := t.TempDir()
	led := newLedger()

	a := startReplica(t, testConfig(dir, idA), led, 1500*time.Millisecond)
	a.waitServing(t, 5*time.Second)
	b := startReplica(t, testConfig(dir, idB), led, 0)
	b.waitServing(t, 8*time.Second)

	if b.result != Confirmed {
		t.Fatalf("B fell back (%v): %s", b.result, eventLog(t, dir))
	}
	led.assertNoOverlap(t)
}

// T3 (the pre-#54 transition): the lock is free but the previous replica never
// acks. The incoming replica waits out the ack, warns "never acknowledged" and
// starts; and a later request from a retired replica is ignored.
func TestNoAckFromOldReplicaFailsOpenThenIgnoresStaleRequest(t *testing.T) {
	dir := t.TempDir()
	led := newLedger()
	// an old replica owns the volume, runs unlocked and never answers
	if err := os.WriteFile(filepath.Join(dir, LockFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, OwnerFile), []byte(idA+"\n"), 0o644)
	led.start(idA)

	cfg := testConfig(dir, idB)
	cfg.Timeout = 600 * time.Millisecond
	b := startReplica(t, cfg, led, 0)
	<-b.done
	if b.result != FailedOpen {
		t.Fatalf("B result = %v", b.result)
	}
	if !strings.Contains(eventLog(t, dir), "never acknowledged") {
		t.Fatalf("no never-acknowledged WARN:\n%s", eventLog(t, dir))
	}

	// the old replica (pre-#54: never recorded in .pb_released) restarts and
	// asks for the volume back. It is an OLDER azd revision, so B ignores it.
	if err := os.WriteFile(filepath.Join(dir, RequestFile), []byte(idA+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "stale request ignored", func() bool {
		return strings.Contains(eventLog(t, dir), "ignoring a handover request from '"+idA+"'")
	})
	if !led.isServing(idB) {
		t.Fatal("B gave the volume to the retired replica")
	}
}

// A request from a replica already in .pb_released is stale too.
func TestStaleRequesterFromReleasedList(t *testing.T) {
	dir := t.TempDir()
	h := New(testConfig(dir, idB))
	_ = os.WriteFile(filepath.Join(dir, ReleasedFile), []byte("some-pod\n"), 0o644)
	if !h.isStaleRequester("some-pod") {
		t.Fatal("released replica not stale")
	}
	if h.isStaleRequester("other-pod") {
		t.Fatal("unknown replica without an epoch must not be stale")
	}
	if !h.isStaleRequester(idA) || h.isStaleRequester("ca-auth--azd-1000000300-c-c") {
		t.Fatal("revision epoch comparison wrong")
	}
	if revisionEpoch("plain-host") != 0 || revisionEpoch("x--azd-abc-1") != 0 {
		t.Fatal("non-azd names have no epoch")
	}
}

// T4: first start on a fresh volume takes it without waiting; the owner's own
// container restart needs only the lock, not an ack.
func TestFreshVolumeThenOwnerRestart(t *testing.T) {
	dir := t.TempDir()
	led := newLedger()

	cfg := testConfig(dir, idB)
	cfg.Timeout = 3 * time.Second
	t0 := time.Now()
	b := startReplica(t, cfg, led, 0)
	b.waitServing(t, time.Second)
	if time.Since(t0) > time.Second {
		t.Fatal("B waited on a fresh volume")
	}
	b.kill()

	t0 = time.Now()
	b2 := startReplica(t, cfg, led, 0)
	b2.waitServing(t, 2*time.Second)
	if b2.result != Confirmed {
		t.Fatalf("restart result = %v", b2.result)
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatal("owner restart waited for an ack")
	}
	led.assertNoOverlap(t)
}

// T5: PB_HANDOVER_TIMEOUT=0 disables the mechanism even with the lock held.
func TestTimeoutZeroDisables(t *testing.T) {
	dir := t.TempDir()
	led := newLedger()
	if err := os.WriteFile(filepath.Join(dir, LockFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	holdLock(t, dir)

	cfg := testConfig(dir, idA)
	cfg.Timeout = 0
	a := startReplica(t, cfg, led, 0)
	a.waitServing(t, time.Second)
	if a.result != Skipped {
		t.Fatalf("result = %v", a.result)
	}
	if !strings.Contains(eventLog(t, dir), "single-writer handover disabled") {
		t.Fatal("no disabled warning")
	}
	for _, f := range []string{OwnerFile, RequestFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Fatalf("%s was written", f)
		}
	}
}

// T6: file locking unavailable -> the request is still written, WARN logged,
// the replica starts anyway.
func TestLockingUnavailableFailsOpen(t *testing.T) {
	dir := t.TempDir()
	led := newLedger()
	cfg := testConfig(dir, idA)
	cfg.lock = func(*os.File) (bool, error) { return false, fmt.Errorf("no flock here") }
	a := startReplica(t, cfg, led, 0)
	a.waitServing(t, 3*time.Second)
	if !strings.Contains(eventLog(t, dir), "WARN flock not found or unusable") {
		t.Fatalf("WARN not recorded durably:\n%s", eventLog(t, dir))
	}
	if readFile(dir, RequestFile) != idA {
		t.Fatal("the handover request was not written")
	}
	led.assertNoOverlap(t)
}

// T7: lock file uncreatable -> WARN, replica starts anyway.
func TestUncreatableLockFileFailsOpen(t *testing.T) {
	dir := t.TempDir()
	led := newLedger()
	if err := os.Symlink("/nonexistent-dir/lock", filepath.Join(dir, LockFile)); err != nil { // dangling
		t.Fatal(err)
	}
	a := startReplica(t, testConfig(dir, idA), led, 0)
	a.waitServing(t, 3*time.Second)
	if !strings.Contains(eventLog(t, dir), "cannot create") || !strings.Contains(eventLog(t, dir), LockFile) {
		t.Fatalf("no uncreatable-lock WARN:\n%s", eventLog(t, dir))
	}
}

// T8: fail-open timeout — the lock is held by someone who never lets go and
// there is no watcher. The incoming replica starts anyway with a loud WARN.
func TestTimeoutFailsOpen(t *testing.T) {
	dir := t.TempDir()
	led := newLedger()
	if err := os.WriteFile(filepath.Join(dir, LockFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, OwnerFile), []byte("some-other-replica\n"), 0o644)
	holdLock(t, dir)

	cfg := testConfig(dir, idB)
	cfg.Timeout = time.Second
	t0 := time.Now()
	b := startReplica(t, cfg, led, 0)
	b.waitServing(t, 5*time.Second)
	if b.result != FailedOpen {
		t.Fatalf("result = %v", b.result)
	}
	if d := time.Since(t0); d < 900*time.Millisecond || d > 3*time.Second {
		t.Fatalf("started after %s, want about the 1s timeout", d)
	}
	if !strings.Contains(eventLog(t, dir), "no handover after 1s") {
		t.Fatalf("no fail-open WARN:\n%s", eventLog(t, dir))
	}
}

// T9: self-id guard — a request carrying our own id is ignored.
func TestOwnRequestIsIgnored(t *testing.T) {
	dir := t.TempDir()
	led := newLedger()
	a := startReplica(t, testConfig(dir, idA), led, 0)
	a.waitServing(t, 3*time.Second)
	if err := os.WriteFile(filepath.Join(dir, RequestFile), []byte(idA+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if !led.isServing(idA) {
		t.Fatal("A released the volume to itself")
	}
	if strings.Contains(eventLog(t, dir), "handover requested") {
		t.Fatal("A answered its own request")
	}
}

// T11: several replicas on PostgreSQL overlap on purpose: no handover, no files.
func TestMultiReplicaOnPostgresSkips(t *testing.T) {
	dir := t.TempDir()
	led := newLedger()
	t.Setenv(envDBURL, "postgres://u:p@127.0.0.1:1/db")
	t.Setenv(envMultiReplica, "true")

	cfgA := ConfigFromEnv(dir)
	cfgA.InstanceID = idA
	cfgB := ConfigFromEnv(dir)
	cfgB.InstanceID = idB
	if !cfgA.MultiReplica {
		t.Fatal("env not read")
	}
	a := startReplica(t, cfgA, led, 0)
	a.waitServing(t, time.Second)
	t0 := time.Now()
	b := startReplica(t, cfgB, led, 0)
	b.waitServing(t, time.Second)
	if time.Since(t0) > time.Second {
		t.Fatal("B waited for A")
	}
	time.Sleep(300 * time.Millisecond)
	if !led.isServing(idA) {
		t.Fatal("A was asked to leave")
	}
	for _, f := range []string{RequestFile, AckFile, OwnerFile, ReleasedFile, LockFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Fatalf("handover state %s was created", f)
		}
	}
}

// T12: a single replica on PostgreSQL (multi-replica off) still hands over.
func TestSingleReplicaOnPostgresStillHandsOver(t *testing.T) {
	dir := t.TempDir()
	led := newLedger()
	t.Setenv(envDBURL, "postgres://u:p@127.0.0.1:1/db")
	t.Setenv(envMultiReplica, "false")

	cfgA := ConfigFromEnv(dir)
	cfgA.InstanceID, cfgA.Poll, cfgA.Step = idA, 50*time.Millisecond, 20*time.Millisecond
	cfgB := cfgA
	cfgB.InstanceID = idB
	a := startReplica(t, cfgA, led, 0)
	a.waitServing(t, 3*time.Second)
	b := startReplica(t, cfgB, led, 0)
	b.waitServing(t, 5*time.Second)
	if led.isServing(idA) {
		t.Fatal("A still serving (the handover was skipped)")
	}
	led.assertNoOverlap(t)
}

// The env knobs entrypoint.sh reads.
func TestConfigFromEnv(t *testing.T) {
	t.Setenv(EnvTimeout, "7")
	t.Setenv(EnvPoll, "250ms")
	t.Setenv(EnvInstanceID, "pod-x")
	c := ConfigFromEnv("/d")
	if c.Timeout != 7*time.Second || c.Poll != 250*time.Millisecond || c.InstanceID != "pod-x" {
		t.Fatalf("%+v", c)
	}
	t.Setenv(EnvTimeout, "0")
	if ConfigFromEnv("/d").Timeout != 0 {
		t.Fatal("0 must stay 0 (kill switch)")
	}
	t.Setenv(EnvTimeout, "garbage")
	if ConfigFromEnv("/d").Timeout != 60*time.Second {
		t.Fatal("bad value falls back to the default")
	}
}

func TestRequested(t *testing.T) {
	t.Setenv(EnvEnable, "")
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"serve"}, false},
		{[]string{"serve", "--handover"}, true},
		{[]string{"--dir=x", "serve", "--http=0.0.0.0:8090", "--handover=true"}, true},
		{[]string{"serve", "--handover=false"}, false},
		{[]string{"superuser", "--handover"}, false},
	}
	for _, c := range cases {
		if got := Requested(c.args); got != c.want {
			t.Errorf("%v = %v, want %v", c.args, got, c.want)
		}
	}
	t.Setenv(EnvEnable, "binary")
	if !Requested([]string{"serve"}) || Requested([]string{"superuser"}) {
		t.Error("env must enable serve only")
	}
}

func TestEventLogIsTrimmed(t *testing.T) {
	dir := t.TempDir()
	h := New(testConfig(dir, idA))
	for i := 0; i < maxEventLogLines+25; i++ {
		h.event("INFO", fmt.Sprintf("n%d", i))
	}
	lines := strings.Split(strings.TrimSpace(eventLog(t, dir)), "\n")
	if len(lines) != maxEventLogLines {
		t.Fatalf("%d lines", len(lines))
	}
	if !strings.HasSuffix(lines[len(lines)-1], fmt.Sprintf("n%d", maxEventLogLines+24)) {
		t.Fatal("kept the wrong end")
	}
}
