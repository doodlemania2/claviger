// Package replica is the cross process coordination of Claviger when it runs
// as more than one replica on one PostgreSQL database (fork-only, issue #106,
// M7).
//
// It has two parts and no dependency on package core:
//
//   - [Bus] fans small events out to every replica through LISTEN/NOTIFY, on one
//     dedicated (non pooled) connection per replica;
//   - [Coordinator] holds the shared tables and the advisory locks that make
//     "exactly one replica does this" possible.
//
// Delivery guarantees of the bus, all of them deliberate:
//
//   - at most once: NOTIFY is not stored. A replica that is disconnected from
//     the database (or was not yet listening) misses the events of that gap.
//     [Bus.OnResync] callbacks run after every reconnect (and when the receive
//     queue overflowed), so the owners of caches can reload them instead of
//     trusting that nothing was missed. Realtime record events of the gap are
//     lost; that is the documented limit;
//   - per sender order: the events of one replica are delivered in order to
//     one worker of the receiver;
//   - an event is delivered to the other replicas only. The sender applies its
//     own change locally, as it did before there were replicas;
//   - transactional: a notification sent through a transaction is delivered
//     when it commits. [Bus.Publish] uses the pool, i.e. it is sent right away,
//     so publish after the commit (the "after success" hooks do).
package replica

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Channel is the LISTEN/NOTIFY channel of the bus.
const Channel = "claviger_events"

const (
	// notify payloads are limited to 8000 bytes; the envelope adds a few dozens
	maxInlineData = 6500

	queueSize        = 4096
	keepAliveEvery   = 20 * time.Second
	maxBackoff       = 10 * time.Second
	payloadRetention = 5 * time.Minute
)

// Message is one event received from another replica.
type Message struct {
	// Kind routes the message to its handlers (for example "rt").
	Kind string `json:"k"`
	// Origin is the id of the sending replica.
	Origin string `json:"o"`
	// Data is the JSON payload of the sender.
	Data jsontext.Value `json:"d,omitempty"`
	// Ref is the id of the overflow row that holds Data when it did not fit
	// in a notification. [Bus] resolves it before it calls the handlers.
	Ref string `json:"r,omitempty"`
}

// Handler receives the messages of one kind, in order, on the worker
// goroutine of the bus. A slow handler delays the following messages.
type Handler func(Message)

// listener is the dedicated LISTEN connection (see listen_pgx.go).
type listener interface {
	// WaitForNotification blocks for the payload of the next notification, and
	// returns the context error when ctx ends first (the connection stays usable).
	WaitForNotification(ctx context.Context) (string, error)
	Ping(ctx context.Context) error
	Close(ctx context.Context) error
}

// Options configures a [Bus].
type Options struct {
	// URL is the PostgreSQL connection URL of the dedicated listen connection.
	URL string
	// DB is the pool that publishes and reads overflow payloads.
	DB *sql.DB
	// Logger is optional.
	Logger *slog.Logger
}

// Bus is the event channel between the replicas of one database.
type Bus struct {
	id  string
	url string
	db  *sql.DB
	log *slog.Logger

	mu       sync.RWMutex
	handlers map[string][]Handler
	resync   []func()

	queue    chan Message
	overflow atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	listening atomic.Bool
	ready     chan struct{}
	readyOnce sync.Once

	connects atomic.Int64
	dropped  atomic.Int64
}

// New creates a bus. Nothing runs until [Bus.Start].
func New(opts Options) (*Bus, error) {
	if opts.URL == "" || opts.DB == nil {
		return nil, errors.New("replica: the connection URL and the pool are required")
	}

	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}

	return &Bus{
		id:       hex.EncodeToString(raw),
		url:      opts.URL,
		db:       opts.DB,
		log:      log,
		handlers: map[string][]Handler{},
		queue:    make(chan Message, queueSize),
		ready:    make(chan struct{}),
	}, nil
}

// DB returns the pool the bus publishes through.
func (b *Bus) DB() *sql.DB { return b.db }

// ID returns the random id of this replica for the lifetime of the bus.
func (b *Bus) ID() string { return b.id }

// On registers a handler of the messages of the kind. Register before Start.
func (b *Bus) On(kind string, h Handler) {
	b.mu.Lock()
	b.handlers[kind] = append(b.handlers[kind], h)
	b.mu.Unlock()
}

// OnResync registers a callback that runs when events may have been missed:
// after every reconnect of the listen connection and after a receive queue
// overflow. It must be idempotent and cheap to repeat (reload a cache).
func (b *Bus) OnResync(fn func()) {
	b.mu.Lock()
	b.resync = append(b.resync, fn)
	b.mu.Unlock()
}

// Resync runs the [Bus.OnResync] callbacks now.
func (b *Bus) Resync() { b.triggerResync() }

// Listening reports whether the listen connection is up.
func (b *Bus) Listening() bool { return b.listening.Load() }

// Stats returns counters for tests and diagnostics: how many times the listen
// connection was established and how many received events were dropped.
func (b *Bus) Stats() (connects, dropped int64) {
	return b.connects.Load(), b.dropped.Load()
}

// Start connects, LISTENs and starts the delivery worker. It waits (up to
// timeout) for the first LISTEN so that an event published right afterwards is
// not missed by this replica. A failing database does not fail Start: the bus
// keeps retrying, because a replica that is not able to listen yet should still
// serve (it resyncs when the connection comes up).
func (b *Bus) Start(timeout time.Duration) {
	b.ctx, b.cancel = context.WithCancel(context.Background())

	b.wg.Add(2)
	go b.worker()
	go b.listen()

	if timeout > 0 {
		select {
		case <-b.ready:
		case <-time.After(timeout):
			b.log.Warn("Multi replica event channel is not listening yet, retrying in the background")
		}
	}
}

// Stop stops the bus and waits for its goroutines.
func (b *Bus) Stop() {
	if b.cancel == nil {
		return
	}
	b.cancel()
	b.wg.Wait()
	b.cancel = nil
}

// Publish sends an event to the other replicas. data is marshaled as JSON.
// Payloads that do not fit in a notification are stored in an overflow row and
// only its id is sent.
func (b *Bus) Publish(ctx context.Context, kind string, data any) error {
	var raw jsontext.Value
	if data != nil {
		enc, err := json.Marshal(data)
		if err != nil {
			return err
		}
		raw = enc
	}

	msg := Message{Kind: kind, Origin: b.id, Data: raw}

	if len(raw) > maxInlineData {
		ref, err := b.storePayload(ctx, raw)
		if err != nil {
			return fmt.Errorf("replica: failed to store a large event payload: %w", err)
		}
		msg.Data, msg.Ref = nil, ref
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	_, err = b.db.ExecContext(ctx, "SELECT pg_notify($1, $2)", Channel, string(payload))

	return err
}

func (b *Bus) storePayload(ctx context.Context, raw []byte) (string, error) {
	idRaw := make([]byte, 12)
	if _, err := rand.Read(idRaw); err != nil {
		return "", err
	}
	id := hex.EncodeToString(idRaw)

	_, err := b.db.ExecContext(ctx,
		"INSERT INTO "+TablePayloads+" (id, payload, created) VALUES ($1, $2, $3)",
		id, string(raw), time.Now().Unix(),
	)

	return id, err
}

func (b *Bus) loadPayload(ctx context.Context, ref string) (jsontext.Value, error) {
	var s string
	err := b.db.QueryRowContext(ctx, "SELECT payload FROM "+TablePayloads+" WHERE id = $1", ref).Scan(&s)
	if err != nil {
		return nil, err
	}

	return jsontext.Value(s), nil
}

// listen keeps one connection LISTENing, reconnecting with a capped backoff.
func (b *Bus) listen() {
	defer b.wg.Done()

	backoff := 250 * time.Millisecond

	for b.ctx.Err() == nil {
		conn, err := connectListener(b.ctx, b.url)
		if err != nil {
			b.log.Warn("Multi replica event channel connection failed", "error", redact(err, b.url), "retryIn", backoff.String())
			if !sleep(b.ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		backoff = 250 * time.Millisecond

		first := b.connects.Add(1) == 1
		b.listening.Store(true)
		b.readyOnce.Do(func() { close(b.ready) })

		if !first {
			// events of the gap are gone: let the caches reload
			b.log.Warn("Multi replica event channel reconnected, resyncing")
			b.triggerResync()
		}

		b.receive(conn)

		b.listening.Store(false)
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = conn.Close(closeCtx)
		cancel()
	}
}

// receive blocks until the connection fails or the bus stops.
func (b *Bus) receive(conn listener) {
	for b.ctx.Err() == nil {
		waitCtx, cancel := context.WithTimeout(b.ctx, keepAliveEvery)
		payload, err := conn.WaitForNotification(waitCtx)
		cancel()

		switch {
		case err == nil:
			b.enqueue(payload)
		case b.ctx.Err() != nil:
			return
		case errors.Is(err, context.DeadlineExceeded):
			// quiet period: prove that the connection is alive
			pingCtx, pcancel := context.WithTimeout(b.ctx, 5*time.Second)
			perr := conn.Ping(pingCtx)
			pcancel()
			if perr != nil {
				b.log.Warn("Multi replica event channel ping failed", "error", redact(perr, b.url))
				return
			}
		default:
			b.log.Warn("Multi replica event channel receive failed", "error", redact(err, b.url))
			return
		}
	}
}

func (b *Bus) enqueue(payload string) {
	var msg Message
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		b.log.Warn("Ignoring a malformed multi replica event", "error", err)
		return
	}

	if msg.Origin == b.id {
		return // our own event
	}

	select {
	case b.queue <- msg:
	default:
		// the worker cannot keep up: dropping is safer than blocking LISTEN,
		// and a resync repairs every cache (realtime events are lost)
		b.dropped.Add(1)
		b.overflow.Store(true)
	}
}

func (b *Bus) worker() {
	defer b.wg.Done()

	lastPurge := time.Now()

	for {
		select {
		case <-b.ctx.Done():
			return
		case msg := <-b.queue:
			b.dispatch(msg)
		case <-time.After(time.Second):
		}

		if b.overflow.CompareAndSwap(true, false) {
			b.log.Warn("Multi replica event queue overflowed, resyncing")
			b.triggerResync()
		}

		if time.Since(lastPurge) > time.Minute {
			lastPurge = time.Now()
			b.purgePayloads()
		}
	}
}

func (b *Bus) dispatch(msg Message) {
	if msg.Ref != "" {
		ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
		data, err := b.loadPayload(ctx, msg.Ref)
		cancel()
		if err != nil {
			b.log.Warn("Failed to load a large multi replica event payload", "kind", msg.Kind, "error", err)
			return
		}
		msg.Data = data
	}

	b.mu.RLock()
	hs := b.handlers[msg.Kind]
	b.mu.RUnlock()

	for _, h := range hs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					b.log.Error("Panic in a multi replica event handler", "kind", msg.Kind, "panic", fmt.Sprint(r))
				}
			}()
			h(msg)
		}()
	}
}

func (b *Bus) triggerResync() {
	b.mu.RLock()
	fns := append([]func(){}, b.resync...)
	b.mu.RUnlock()

	for _, fn := range fns {
		func() {
			defer func() { _ = recover() }()
			fn()
		}()
	}
}

func (b *Bus) purgePayloads() {
	ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
	defer cancel()

	_, err := b.db.ExecContext(ctx, "DELETE FROM "+TablePayloads+" WHERE created < $1",
		time.Now().Add(-payloadRetention).Unix())
	if err != nil && b.ctx.Err() == nil {
		b.log.Debug("Failed to purge the multi replica event payloads", "error", err)
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// redact keeps the connection URL (it carries the password) out of logs.
func redact(err error, url string) error {
	if err == nil || url == "" {
		return err
	}

	return errors.New(strings.ReplaceAll(err.Error(), url, "<redacted>"))
}
