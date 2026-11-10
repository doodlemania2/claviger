package core

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/security"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #77): outbound webhooks.
//
// Design in one paragraph: webhooks are driven by the audit log. A handler on
// OnAuditEntry (no new core.App hook) runs inside the transaction that wrote
// the record change and, for every enabled endpoint whose event filters
// match, inserts a row into the hidden _webhookDeliveries collection in
// data.db. Because the outbox shares the database of the change, the
// enqueue is exactly as atomic as the change itself: rolled back together,
// committed together. A background worker (only running while at least one
// enabled endpoint exists) delivers due rows with retries and backoff.
// Delivery is at-least-once: receivers must deduplicate on the webhook-id
// header. Single replica only (SQLite single writer): the in-memory
// in-flight bookkeeping and the poller assume one process.
//
// With no endpoints configured: the audit handler does one map lookup, the
// worker is not running, and nothing else happens.

const (
	// CollectionNameWebhooks is the hidden system collection of webhook endpoints.
	CollectionNameWebhooks = "_webhooks"
	// CollectionNameWebhookDeliveries is the hidden system collection that is
	// both the durable delivery outbox and the delivery log.
	CollectionNameWebhookDeliveries = "_webhookDeliveries"

	// WebhooksAllowLocalhostEnv, when "true", is the explicit development flag
	// that allows endpoints with an http:// URL pointing at localhost,
	// 127.0.0.1 or ::1 and lets deliveries reach loopback addresses.
	// Private, link-local and metadata addresses stay blocked regardless.
	WebhooksAllowLocalhostEnv = "CLAVIGER_WEBHOOKS_ALLOW_LOCALHOST"

	WebhookDeliveryPending = "pending"
	WebhookDeliverySuccess = "success"
	WebhookDeliveryDead    = "dead"

	WebhookSecretPrefix = "whsec_"

	// WebhookEventTest is the type of the event sent by "send test event".
	WebhookEventTest = "webhook.test"

	webhookEncPrefix         = "enc:"
	webhookStoreKey          = "__clavigerWebhooks__"
	webhookCleanupCronId     = "__pbWebhookCleanup__"
	webhookCleanupCronExpr   = "41 3 * * *"
	webhookRetentionDays     = 30
	webhookMaxAttemptLogSize = 20
	webhookSecretGraceHours  = 24
)

// webhookExcludedCollections never produce webhook events (they would recurse
// or leak the machinery's own bookkeeping).
var webhookExcludedCollections = map[string]struct{}{
	CollectionNameAuditLog:          {},
	CollectionNameWebhooks:          {},
	CollectionNameWebhookDeliveries: {},
}

// webhookEventTypes maps the audit action to the delivered event type.
var webhookEventTypes = map[string]string{
	AuditActionCreate: "record.created",
	AuditActionUpdate: "record.updated",
	AuditActionDelete: "record.deleted",

	// #81: the lifecycle job of safeguarding credentials (passage of time, not
	// a record change)
	AuditActionCredentialExpiring: "credential.expiring",
	AuditActionCredentialLapsed:   "credential.lapsed",
}

// -------------------------------------------------------------------
// secrets and signing (Standard Webhooks, https://www.standardwebhooks.com)
// -------------------------------------------------------------------

// GenerateWebhookSecret returns a new "whsec_" + base64(32 random bytes) secret.
func GenerateWebhookSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing is unrecoverable
	}
	return WebhookSecretPrefix + base64.StdEncoding.EncodeToString(b)
}

// SignWebhook returns the "v1,<base64 HMAC-SHA256>" signature of
// "<id>.<unix timestamp>.<body>" using the base64 key that follows the
// optional "whsec_" prefix of secret.
func SignWebhook(secret, id string, timestamp time.Time, body []byte) (string, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, WebhookSecretPrefix))
	if err != nil || len(key) == 0 {
		return "", fmt.Errorf("invalid webhook secret")
	}

	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s.%d.", id, timestamp.Unix())
	mac.Write(body)

	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// encryptWebhookSecret stores the secret encrypted when the app encryption
// env is configured (same key as the encrypted settings), plain otherwise.
func encryptWebhookSecret(app App, secret string) (string, error) {
	if secret == "" {
		return "", nil
	}
	key := os.Getenv(app.EncryptionEnv())
	if key == "" {
		return secret, nil
	}
	enc, err := security.Encrypt([]byte(secret), key)
	if err != nil {
		return "", err
	}
	return webhookEncPrefix + enc, nil
}

func decryptWebhookSecret(app App, stored string) (string, error) {
	if !strings.HasPrefix(stored, webhookEncPrefix) {
		return stored, nil
	}
	key := os.Getenv(app.EncryptionEnv())
	if key == "" {
		return "", fmt.Errorf("webhook secret is encrypted but %q is not set", app.EncryptionEnv())
	}
	plain, err := security.Decrypt(strings.TrimPrefix(stored, webhookEncPrefix), key)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// SetWebhookSecret stores secret on the endpoint record (encrypted if the
// app encryption env is configured). It does not save the record.
func SetWebhookSecret(app App, endpoint *Record, secret string) error {
	stored, err := encryptWebhookSecret(app, secret)
	if err != nil {
		return err
	}
	endpoint.Set("secret", stored)
	return nil
}

// RotateWebhookSecret generates a new secret for the endpoint and keeps the
// previous one valid for 24 hours, during which both signatures are sent
// (the Standard Webhooks rotation scheme). It does not save the record and
// returns the new plain secret.
func RotateWebhookSecret(app App, endpoint *Record) (string, error) {
	oldStored := endpoint.GetString("secret")
	secret := GenerateWebhookSecret()

	if err := SetWebhookSecret(app, endpoint, secret); err != nil {
		return "", err
	}
	endpoint.Set("prevSecret", oldStored)
	endpoint.Set("prevSecretExpires", types.NowDateTime().Add(webhookSecretGraceHours*time.Hour))

	return secret, nil
}

// -------------------------------------------------------------------
// endpoint validation and event filters
// -------------------------------------------------------------------

func webhooksAllowLocalhost() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(WebhooksAllowLocalhostEnv)), "true")
}

func isLocalhostName(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// ValidateWebhookURL reports why the URL cannot be used as an endpoint.
// https is required; http is accepted only for localhost/127.0.0.1/::1 and
// only while CLAVIGER_WEBHOOKS_ALLOW_LOCALHOST=true. Hostnames are resolved
// (and checked again) at delivery time; here only IP literals are checked.
func ValidateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("must be a valid absolute URL")
	}
	if u.User != nil {
		return fmt.Errorf("must not contain credentials")
	}

	host := strings.ToLower(u.Hostname())
	allowLocal := webhooksAllowLocalhost()

	switch u.Scheme {
	case "https":
	case "http":
		if !allowLocal || !isLocalhostName(host) {
			return fmt.Errorf("must use https (plain http is only allowed for localhost when %s=true)", WebhooksAllowLocalhostEnv)
		}
	default:
		return fmt.Errorf("must use https")
	}

	if isLocalhostName(host) && !allowLocal {
		return fmt.Errorf("localhost targets are not allowed (set %s=true for development)", WebhooksAllowLocalhostEnv)
	}

	if addr, err := netip.ParseAddr(host); err == nil && webhookIPBlocked(addr, allowLocal) {
		return fmt.Errorf("target address is not allowed")
	}

	return nil
}

// webhookIPBlocked reports whether deliveries must never connect to ip:
// loopback (unless allowLoopback), private, link-local (incl. the
// 169.254.169.254 cloud metadata address), CGNAT, multicast, unspecified
// and other reserved ranges.
func webhookIPBlocked(ip netip.Addr, allowLoopback bool) bool {
	ip = ip.Unmap()

	if ip.IsLoopback() {
		return !allowLoopback
	}

	if !ip.IsValid() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}

	for _, p := range webhookReservedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}

	return false
}

var webhookReservedPrefixes = func() []netip.Prefix {
	out := make([]netip.Prefix, 0, 11)
	for _, s := range []string{
		"0.0.0.0/8",       // "this" network
		"100.64.0.0/10",   // CGNAT
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reserved + broadcast
		"64:ff9b::/96",    // NAT64
		"2001:db8::/32",   // documentation
		"fd00:ec2::/32",   // AWS IMDS over IPv6
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// ValidateWebhookEvents checks the filter list: entries are "*" (everything),
// "<collection>:<action>" or "<collection>:*" / "*:<action>", where action is
// create, update or delete.
func ValidateWebhookEvents(events []string) error {
	if len(events) == 0 {
		return fmt.Errorf("at least one event filter is required")
	}
	for _, ev := range events {
		if ev == "*" {
			continue
		}
		col, action, ok := strings.Cut(ev, ":")
		if !ok || col == "" {
			return fmt.Errorf("invalid event filter %q (expected \"*\", \"collection:action\" or \"collection:*\")", ev)
		}
		switch action {
		case "*", AuditActionCreate, AuditActionUpdate, AuditActionDelete, AuditActionCredentialExpiring, AuditActionCredentialLapsed:
		default:
			return fmt.Errorf("invalid action in event filter %q (expected create, update, delete, credential_expiring, credential_lapsed or *)", ev)
		}
	}
	return nil
}

func webhookEventMatches(filters []string, collection, action string) bool {
	for _, f := range filters {
		if f == "*" {
			return true
		}
		col, act, _ := strings.Cut(f, ":")
		if (col == "*" || col == collection) && (act == "*" || act == action) {
			return true
		}
	}
	return false
}

// -------------------------------------------------------------------
// payload
// -------------------------------------------------------------------

// WebhookPayload is the JSON body of a delivery.
type WebhookPayload struct {
	Type      string             `json:"type"`
	Timestamp string             `json:"timestamp"`
	Data      WebhookPayloadData `json:"data"`
}

// WebhookPayloadData is the "data" member of a record event.
type WebhookPayloadData struct {
	Collection    string     `json:"collection"`
	RecordId      string     `json:"recordId"`
	Actor         AuditActor `json:"actor"`
	ChangedFields []string   `json:"changedFields"`
	// Record is the redacted state after the change (the last state for deletes).
	Record map[string]any `json:"record"`
	// Previous holds the redacted old values of the changed fields (updates only).
	Previous map[string]any `json:"previous,omitempty"`
}

func buildWebhookPayload(entry *AuditEntry, now time.Time) ([]byte, error) {
	record := entry.After
	if entry.Action == AuditActionDelete {
		record = entry.Before
	}

	p := WebhookPayload{
		Type:      webhookEventTypes[entry.Action],
		Timestamp: now.UTC().Format(time.RFC3339),
		Data: WebhookPayloadData{
			Collection:    entry.CollectionName,
			RecordId:      entry.RecordId,
			Actor:         entry.Actor,
			ChangedFields: entry.ChangedFields,
			Record:        record,
		},
	}
	if p.Data.ChangedFields == nil {
		p.Data.ChangedFields = []string{}
	}
	if entry.Action == AuditActionUpdate {
		p.Data.Previous = entry.Before
	}

	return json.Marshal(p)
}

// -------------------------------------------------------------------
// hooks: enqueue and endpoint cache
// -------------------------------------------------------------------

type webhookEndpoint struct {
	id        string
	url       string
	events    []string
	secrets   []string // current first, then the unexpired previous one
	secretErr error
}

type webhookManager struct {
	app App

	mu        sync.Mutex
	loaded    bool
	serving   bool
	endpoints map[string]*webhookEndpoint // enabled endpoints only

	// worker state (guarded by mu)
	running bool
	cancel  context.CancelFunc
	done    chan struct{}

	// overridable in tests
	now          func() time.Time
	pollInterval time.Duration
	retryDelays  []time.Duration
	allowedFails int
	failWindow   time.Duration
}

func webhookManagerFor(app App) *webhookManager {
	m, _ := app.Store().Get(webhookStoreKey).(*webhookManager)
	return m
}

// WebhookWorkerRunning reports whether the delivery worker goroutine is
// currently running (it runs only while an enabled endpoint exists).
func WebhookWorkerRunning(app App) bool {
	m := webhookManagerFor(app)
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

func (app *BaseApp) registerWebhookHooks() {
	m := &webhookManager{
		app:          app,
		now:          time.Now,
		pollInterval: 2 * time.Second,
		retryDelays:  defaultWebhookRetryDelays(),
		allowedFails: 10,
		failWindow:   24 * time.Hour,
	}
	app.Store().Set(webhookStoreKey, m)

	app.OnAuditEntry().Bind(&hook.Handler[*AuditEntryEvent]{
		Id:   "__pbWebhookEnqueue__",
		Func: m.onAuditEntry,
	})

	app.OnRecordValidate(CollectionNameWebhooks).Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbWebhookValidate__",
		Func: func(e *RecordEvent) error {
			var events []string
			if err := e.Record.UnmarshalJSONField("events", &events); err != nil {
				return validation.Errors{"events": validation.NewError("validation_invalid_events", "Must be a list of event filters.")}
			}

			errs := validation.Errors{}
			if err := ValidateWebhookURL(e.Record.GetString("url")); err != nil {
				errs["url"] = validation.NewError("validation_invalid_webhook_url", "Invalid URL: "+err.Error())
			}
			if err := ValidateWebhookEvents(events); err != nil {
				errs["events"] = validation.NewError("validation_invalid_events", err.Error())
			}
			if len(errs) > 0 {
				return errs
			}
			return e.Next()
		},
	})

	// The records API un-hides every field for superusers; the secrets must
	// stay hidden from them too. The default enrich step (which does the
	// un-hiding) runs as the finalizer of the chain, i.e. inside e.Next().
	app.OnRecordEnrich(CollectionNameWebhooks).Bind(&hook.Handler[*RecordEnrichEvent]{
		Id: "__pbWebhookHideSecrets__",
		Func: func(e *RecordEnrichEvent) error {
			err := e.Next()
			e.Record.Hide("secret", "prevSecret")
			return err
		},
	})

	// after commit: refresh the endpoint cache and start/stop the worker
	refresh := func(e *ModelEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		m.reload()
		PublishReplicaEvent(app, ReplicaEventWebhooks, nil) // fork-local (#106 M7)
		return nil
	}
	for _, h := range []func(...string) *hook.TaggedHook[*ModelEvent]{
		app.OnModelAfterCreateSuccess, app.OnModelAfterUpdateSuccess, app.OnModelAfterDeleteSuccess,
	} {
		h(CollectionNameWebhooks).Bind(&hook.Handler[*ModelEvent]{Id: "__pbWebhookRefresh__", Func: refresh})
	}

	app.OnServe().Bind(&hook.Handler[*ServeEvent]{
		Id: "__pbWebhookStart__",
		Func: func(e *ServeEvent) error {
			m.mu.Lock()
			m.serving = true
			m.mu.Unlock()
			m.reload()
			return e.Next()
		},
	})

	app.OnTerminate().Bind(&hook.Handler[*TerminateEvent]{
		Id: "__pbWebhookStop__",
		Func: func(e *TerminateEvent) error {
			m.stop(true)
			return e.Next()
		},
	})

	app.Cron().Add(webhookCleanupCronId, webhookCleanupCronExpr, func() {
		if _, err := DeleteExpiredWebhookDeliveries(app, time.Now().AddDate(0, 0, -webhookRetentionDays)); err != nil {
			app.Logger().Warn("Failed to delete expired webhook deliveries", "error", err)
		}
	})
}

// DeleteExpiredWebhookDeliveries removes the deliveries created before the
// provided time and returns how many were removed.
func DeleteExpiredWebhookDeliveries(app App, createdBefore time.Time) (int64, error) {
	res, err := app.NonconcurrentDB().Delete(
		CollectionNameWebhookDeliveries,
		dbx.NewExp("[[created]] < {:date}", dbx.Params{"date": createdBefore.UTC().Format(types.DefaultDateLayout)}),
	).Execute()
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// reload re-reads the enabled endpoints and starts or stops the worker.
func (m *webhookManager) reload() {
	endpoints := map[string]*webhookEndpoint{}

	if _, err := m.app.FindCachedCollectionByNameOrId(CollectionNameWebhooks); err == nil {
		records, err := m.app.FindAllRecords(CollectionNameWebhooks, dbx.HashExp{"enabled": true})
		if err != nil {
			m.app.Logger().Warn("Failed to load the webhook endpoints", "error", err)
			return
		}
		for _, r := range records {
			endpoints[r.Id] = m.endpointFromRecord(r)
		}
	}

	m.mu.Lock()
	m.endpoints = endpoints
	m.loaded = true
	m.mu.Unlock()

	m.syncWorker()
}

func (m *webhookManager) endpointFromRecord(r *Record) *webhookEndpoint {
	ep := &webhookEndpoint{id: r.Id, url: r.GetString("url")}
	_ = r.UnmarshalJSONField("events", &ep.events)

	cur, err := decryptWebhookSecret(m.app, r.GetString("secret"))
	if err != nil {
		ep.secretErr = err
	} else if cur != "" {
		ep.secrets = append(ep.secrets, cur)
	}

	if prev := r.GetString("prevSecret"); prev != "" && r.GetDateTime("prevSecretExpires").Time().After(m.now()) {
		if p, err := decryptWebhookSecret(m.app, prev); err == nil && p != "" {
			ep.secrets = append(ep.secrets, p)
		}
	}

	return ep
}

func (m *webhookManager) enabledEndpoints() map[string]*webhookEndpoint {
	m.mu.Lock()
	loaded := m.loaded
	m.mu.Unlock()

	if !loaded {
		m.reload()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.endpoints
}

func (m *webhookManager) onAuditEntry(e *AuditEntryEvent) error {
	if err := e.Next(); err != nil {
		return err
	}

	if e.Record == nil || e.Entry == nil {
		return nil
	}
	if _, skip := webhookExcludedCollections[e.Entry.CollectionName]; skip {
		return nil
	}
	if _, ok := webhookEventTypes[e.Entry.Action]; !ok {
		return nil
	}

	endpoints := m.enabledEndpoints()
	if len(endpoints) == 0 {
		return nil // zero cost path: no webhooks configured
	}

	var matched []*webhookEndpoint
	for _, ep := range endpoints {
		if webhookEventMatches(ep.events, e.Entry.CollectionName, e.Entry.Action) {
			matched = append(matched, ep)
		}
	}
	if len(matched) == 0 {
		return nil
	}

	now := m.now()
	body, err := buildWebhookPayload(e.Entry, now)
	if err != nil {
		return fmt.Errorf("webhook payload: %w", err)
	}

	collection, err := e.App.FindCachedCollectionByNameOrId(CollectionNameWebhookDeliveries)
	if err != nil {
		return nil // not migrated yet
	}

	// e.App is the transaction of the change: the outbox rows commit or roll
	// back with it. Hook-less clone: bookkeeping rows are not audited.
	txApp := e.App.UnsafeWithoutHooks()
	for _, ep := range matched {
		d := NewRecord(collection)
		d.Id = GenerateDefaultRandomId()
		d.SetRaw("created", types.NowDateTime()) // autodate fields ignore Set() and the hook-less save skips them
		d.SetRaw("updated", types.NowDateTime())
		d.Set("webhook", ep.id)
		d.Set("eventType", webhookEventTypes[e.Entry.Action])
		d.Set("payload", types.JSONRaw(body))
		d.Set("status", WebhookDeliveryPending)
		d.Set("attempts", 0)
		next, _ := types.ParseDateTime(now)
		d.Set("nextAttempt", next)
		d.Set("attemptLog", []any{})
		if err := txApp.SaveNoValidate(d); err != nil {
			return fmt.Errorf("webhook enqueue: %w", err)
		}
	}

	return nil
}

func defaultWebhookRetryDelays() []time.Duration {
	return []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 24 * time.Hour}
}
