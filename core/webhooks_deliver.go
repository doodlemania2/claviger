package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/telemetry"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #77): the delivery side of the webhooks.

const (
	webhookConcurrency     = 4
	webhookTimeout         = 10 * time.Second
	webhookMaxResponseBody = 1024
)

// WebhookAttempt is one entry of a delivery's attempt log.
type WebhookAttempt struct {
	At         string `json:"at"`
	StatusCode int    `json:"statusCode"`
	DurationMs int64  `json:"durationMs"`
	Response   string `json:"response"`
	Error      string `json:"error"`
}

// newWebhookHTTPClient returns the SSRF-safe client: no proxy, no redirects,
// and a dialer that resolves the host itself, refuses blocked addresses and
// connects to the vetted IP (so DNS rebinding between check and connect is
// not possible).
func newWebhookHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: webhookTimeout}

	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   webhookTimeout,
		ResponseHeaderTimeout: webhookTimeout,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}

			allowLoopback := webhooksAllowLocalhost()

			var addrs []netip.Addr
			if ip, err := netip.ParseAddr(host); err == nil {
				addrs = []netip.Addr{ip}
			} else {
				resolved, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
				if err != nil {
					return nil, err
				}
				addrs = resolved
			}

			if len(addrs) == 0 {
				return nil, errors.New("webhook target did not resolve")
			}
			// all-or-nothing: a name that mixes public and private records is refused
			for _, a := range addrs {
				if webhookIPBlocked(a, allowLoopback) {
					return nil, errors.New("webhook target resolves to a blocked address")
				}
			}

			var lastErr error
			for _, a := range addrs {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}

	return &http.Client{
		Transport: transport,
		Timeout:   webhookTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // a 3xx is a failed delivery, never followed
		},
	}
}

var (
	webhookClientOnce sync.Once
	webhookClient     *http.Client
)

func sharedWebhookClient() *http.Client {
	webhookClientOnce.Do(func() { webhookClient = newWebhookHTTPClient() })
	return webhookClient
}

// webhookRequestResult is the outcome of one HTTP attempt.
type webhookRequestResult struct {
	statusCode int
	duration   time.Duration
	response   string
	err        string
}

func (r webhookRequestResult) success() bool {
	return r.err == "" && r.statusCode >= 200 && r.statusCode < 300
}

func cleanWebhookError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err // drops the URL, which may carry tokens in its query
	}
	msg := err.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return strings.ToValidUTF8(msg, "")
}

// send performs one signed POST. The message id is stable across retries
// (receivers deduplicate on it); the timestamp and signature are fresh.
func (m *webhookManager) send(ctx context.Context, ep *webhookEndpoint, msgId string, body []byte) webhookRequestResult {
	if ep.secretErr != nil || len(ep.secrets) == 0 {
		return webhookRequestResult{err: "webhook secret is unavailable"}
	}

	if err := ValidateWebhookURL(ep.url); err != nil {
		return webhookRequestResult{err: "endpoint url is not allowed: " + err.Error()}
	}

	ts := m.now()
	sigs := make([]string, 0, len(ep.secrets))
	for _, s := range ep.secrets {
		sig, err := SignWebhook(s, msgId, ts, body)
		if err != nil {
			return webhookRequestResult{err: err.Error()}
		}
		sigs = append(sigs, sig)
	}

	ctx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.url, strings.NewReader(string(body)))
	if err != nil {
		return webhookRequestResult{err: cleanWebhookError(err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Claviger-Webhooks/1")
	req.Header.Set("webhook-id", msgId)
	req.Header.Set("webhook-timestamp", fmt.Sprintf("%d", ts.Unix()))
	req.Header.Set("webhook-signature", strings.Join(sigs, " "))

	start := time.Now()
	resp, err := sharedWebhookClient().Do(req)
	res := webhookRequestResult{duration: time.Since(start)}
	if err != nil {
		res.err = cleanWebhookError(err)
		return res
	}
	defer resp.Body.Close()

	res.statusCode = resp.StatusCode
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, webhookMaxResponseBody))
	res.response = strings.ToValidUTF8(string(snippet), "")

	return res
}

// -------------------------------------------------------------------
// worker
// -------------------------------------------------------------------

// syncWorker starts the worker if serving with at least one enabled
// endpoint, and stops it when none is left.
func (m *webhookManager) syncWorker() {
	m.mu.Lock()
	shouldRun := m.serving && len(m.endpoints) > 0
	running := m.running
	m.mu.Unlock()

	switch {
	case shouldRun && !running:
		m.start()
	case !shouldRun && running:
		m.stop(false)
	}
}

func (m *webhookManager) start() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.running, m.cancel, m.done = true, cancel, done

	go func() {
		defer close(done)

		ticker := time.NewTicker(m.pollInterval)
		defer ticker.Stop()

		for {
			// drain the backlog before sleeping
			for ctx.Err() == nil && m.processDue(ctx) == webhookConcurrency {
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// stop cancels the worker; with wait it blocks until the goroutine exited.
func (m *webhookManager) stop(wait bool) {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.running, m.cancel, m.done = false, nil, nil
	m.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	if wait {
		<-done
	}
}

// processDue delivers up to webhookConcurrency due deliveries concurrently
// and returns how many it handled.
func (m *webhookManager) processDue(ctx context.Context) int {
	endpoints := m.enabledEndpoints()
	if len(endpoints) == 0 {
		return 0
	}

	ids := make([]any, 0, len(endpoints))
	for id := range endpoints {
		ids = append(ids, id)
	}

	collection, err := m.app.FindCachedCollectionByNameOrId(CollectionNameWebhookDeliveries)
	if err != nil {
		return 0
	}

	var due []*Record
	if IsMultiReplica(m.app) {
		due, err = m.claimDue(collection, ids)
	} else {
		err = m.app.RecordQuery(collection).
			AndWhere(dbx.NewExp("[[status]] = {:status} AND [[nextAttempt]] <= {:now}", dbx.Params{
				"status": WebhookDeliveryPending,
				"now":    m.now().UTC().Format(types.DefaultDateLayout),
			})).
			AndWhere(dbx.In("webhook", ids...)).
			OrderBy("nextAttempt ASC").
			Limit(webhookConcurrency).
			All(&due)
	}
	if err != nil {
		m.app.Logger().Warn("Failed to load the due webhook deliveries", "error", err)
		return 0
	}

	var wg sync.WaitGroup
	for _, d := range due {
		ep := endpoints[d.GetString("webhook")]
		if ep == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.attempt(ctx, d, ep, true)
		}()
	}
	wg.Wait()

	return len(due)
}

// webhookClaimLease is how long a claimed delivery is hidden from the other
// replicas. It must outlast one attempt (webhookTimeout plus the write of the
// outcome): when the replica dies mid attempt the delivery is retried after it.
const webhookClaimLease = time.Minute

// claimDue is processDue's query for several replicas (#106 M7).
//
// The rows are selected FOR UPDATE SKIP LOCKED inside a transaction and moved
// to nextAttempt = now + lease before it commits. A concurrent replica skips the
// locked rows and, after the commit, no longer sees them as due, so a delivery
// is claimed by exactly one replica. The attempt then overwrites nextAttempt
// with its own outcome.
//
// Delivery is therefore at least once across a replica crash (the lease
// expires and another replica retries, with the same webhook-id header so the
// receiver can deduplicate) and exactly once otherwise.
func (m *webhookManager) claimDue(collection *Collection, endpointIds []any) ([]*Record, error) {
	var claimed []string
	now := m.now().UTC()

	err := m.app.RunInTransaction(func(txApp App) error {
		params := dbx.Params{
			"status": WebhookDeliveryPending,
			"now":    now.Format(types.DefaultDateLayout),
			"limit":  webhookConcurrency,
		}
		placeholders := make([]string, len(endpointIds))
		for i, id := range endpointIds {
			k := "ep" + strconv.Itoa(i)
			placeholders[i] = "{:" + k + "}"
			params[k] = id
		}

		var ids []string
		err := txApp.DB().NewQuery(
			"SELECT [[id]] FROM {{" + collection.Name + "}} WHERE [[status]] = {:status} AND [[nextAttempt]] <= {:now}" +
				" AND [[webhook]] IN (" + strings.Join(placeholders, ",") + ")" +
				" ORDER BY [[nextAttempt]] ASC LIMIT {:limit} FOR UPDATE SKIP LOCKED",
		).Bind(params).Column(&ids)
		if err != nil || len(ids) == 0 {
			return err
		}

		lease := now.Add(webhookClaimLease).Format(types.DefaultDateLayout)
		idArgs := make([]any, len(ids))
		for i, id := range ids {
			idArgs[i] = id
		}

		if _, err := txApp.DB().Update(collection.Name, dbx.Params{"nextAttempt": lease}, dbx.In("id", idArgs...)).Execute(); err != nil {
			return err
		}

		claimed = ids

		return nil
	})
	if err != nil || len(claimed) == 0 {
		return nil, err
	}

	return m.app.FindRecordsByIds(collection, claimed)
}

// attempt makes one delivery attempt and persists the outcome.
// With retry=false (test events) a failure is final and does not count
// towards the endpoint's failure streak.
func (m *webhookManager) attempt(ctx context.Context, d *Record, ep *webhookEndpoint, retry bool) {
	body := webhookPayloadBytes(d)
	res := m.send(ctx, ep, "msg_"+d.Id, body)

	attempts := d.GetInt("attempts") + 1

	entry := WebhookAttempt{
		At:         m.now().UTC().Format(time.RFC3339),
		StatusCode: res.statusCode,
		DurationMs: res.duration.Milliseconds(),
		Response:   res.response,
		Error:      res.err,
	}
	var log []WebhookAttempt
	_ = d.UnmarshalJSONField("attemptLog", &log)
	log = append(log, entry)
	if len(log) > webhookMaxAttemptLogSize {
		log = log[len(log)-webhookMaxAttemptLogSize:]
	}

	d.Set("attempts", attempts)
	d.Set("attemptLog", log)
	d.Set("lastStatusCode", res.statusCode)

	outcome := "success"
	switch {
	case res.success():
		d.Set("status", WebhookDeliverySuccess)
		d.Set("lastError", "")
		d.Set("nextAttempt", "")
	default:
		lastErr := res.err
		if lastErr == "" {
			lastErr = fmt.Sprintf("unexpected status %d", res.statusCode)
		}
		d.Set("lastError", lastErr)

		if !retry || attempts > len(m.retryDelays) {
			d.Set("status", WebhookDeliveryDead)
			d.Set("nextAttempt", "")
			outcome = "dead"
		} else {
			next, _ := types.ParseDateTime(m.now().Add(m.retryDelays[attempts-1]))
			d.Set("nextAttempt", next)
			outcome = "failure"
		}
	}

	d.SetRaw("updated", types.NowDateTime())
	if err := m.app.UnsafeWithoutHooks().SaveNoValidate(d); err != nil {
		m.app.Logger().Warn("Failed to record the webhook delivery attempt", "delivery", d.Id, "error", err)
	}

	if t := telemetry.Active(); t != nil {
		t.RecordWebhookDelivery(outcome)
	}

	if retry {
		m.trackEndpointHealth(ep.id, res.success())
	}
}

func webhookPayloadBytes(d *Record) []byte {
	switch v := d.GetRaw("payload").(type) {
	case types.JSONRaw:
		return []byte(v)
	case string:
		return []byte(v)
	case []byte:
		return v
	default:
		return []byte(d.GetString("payload"))
	}
}

// trackEndpointHealth maintains the consecutive failure streak and disables
// an endpoint that has been failing for at least failWindow across at least
// allowedFails attempts. Counters are written with plain SQL (no hooks): they
// change on every failure and must not flood the audit log.
func (m *webhookManager) trackEndpointHealth(id string, ok bool) {
	db := m.app.NonconcurrentDB()

	if ok {
		_, err := db.NewQuery(`UPDATE {{` + CollectionNameWebhooks + `}} SET [[consecutiveFailures]] = 0, [[failingSince]] = '' WHERE [[id]] = {:id} AND [[consecutiveFailures]] > 0`).
			Bind(dbx.Params{"id": id}).Execute()
		if err != nil {
			m.app.Logger().Warn("Failed to reset the webhook failure streak", "error", err)
		}
		return
	}

	now := m.now().UTC().Format(types.DefaultDateLayout)
	_, err := db.NewQuery(`UPDATE {{` + CollectionNameWebhooks + `}} SET
		[[consecutiveFailures]] = COALESCE([[consecutiveFailures]], 0) + 1,
		[[failingSince]] = CASE WHEN COALESCE([[failingSince]], '') = '' THEN {:now} ELSE [[failingSince]] END
		WHERE [[id]] = {:id}`).Bind(dbx.Params{"id": id, "now": now}).Execute()
	if err != nil {
		m.app.Logger().Warn("Failed to update the webhook failure streak", "error", err)
		return
	}

	endpoint, err := m.app.FindRecordById(CollectionNameWebhooks, id)
	if err != nil || !endpoint.GetBool("enabled") {
		return
	}

	since := endpoint.GetDateTime("failingSince")
	if endpoint.GetInt("consecutiveFailures") < m.allowedFails || since.IsZero() || m.now().Sub(since.Time()) < m.failWindow {
		return
	}

	reason := fmt.Sprintf("Disabled automatically after %d consecutive failed deliveries over %s.",
		endpoint.GetInt("consecutiveFailures"), m.now().Sub(since.Time()).Round(time.Minute))

	endpoint.Set("enabled", false)
	endpoint.Set("disabledReason", reason)

	ctx := WithAuditContext(context.Background(), AuditActor{Type: AuditActorTypeSystem}, "")
	if err := m.app.SaveNoValidateWithContext(ctx, endpoint); err != nil {
		m.app.Logger().Warn("Failed to disable the failing webhook endpoint", "error", err)
		return
	}

	m.app.Logger().Warn("Webhook endpoint disabled", "endpoint", id, "reason", reason)

	if err := sendSystemAlertToAllSuperusers(m.app, "Webhook endpoint disabled",
		fmt.Sprintf("The webhook endpoint %s (%s) was disabled.\n\n%s\n\nFix the receiver and re-enable it in the dashboard.",
			endpoint.GetString("url"), id, reason)); err != nil {
		m.app.Logger().Warn("Failed to email the webhook disabled alert", "error", err)
	}
}

// -------------------------------------------------------------------
// exported helpers for the management API
// -------------------------------------------------------------------

// SendWebhookTest sends a signed "webhook.test" event to the endpoint right
// away (also when it is disabled), records it as a delivery and returns the
// delivery record. A failed test is not retried and does not count towards
// the auto-disable streak.
func SendWebhookTest(app App, endpoint *Record) (*Record, error) {
	m := webhookManagerFor(app)
	if m == nil {
		return nil, errors.New("webhooks are not available")
	}

	collection, err := app.FindCachedCollectionByNameOrId(CollectionNameWebhookDeliveries)
	if err != nil {
		return nil, err
	}

	body := fmt.Appendf(nil, `{"type":%q,"timestamp":%q,"data":{"message":"This is a test event from Claviger."}}`,
		WebhookEventTest, m.now().UTC().Format(time.RFC3339))

	d := NewRecord(collection)
	d.Id = GenerateDefaultRandomId()
	d.SetRaw("created", types.NowDateTime())
	d.SetRaw("updated", types.NowDateTime())
	d.Set("webhook", endpoint.Id)
	d.Set("eventType", WebhookEventTest)
	d.Set("payload", types.JSONRaw(body))
	d.Set("status", WebhookDeliveryPending)
	d.Set("attempts", 0)
	d.Set("attemptLog", []any{})

	m.attempt(context.Background(), d, m.endpointFromRecord(endpoint), false)

	return d, nil
}

// RedeliverWebhook re-queues a delivery (any status) for immediate sending
// with a fresh attempt counter. The endpoint must be enabled.
func RedeliverWebhook(app App, delivery *Record) error {
	endpoint, err := app.FindRecordById(CollectionNameWebhooks, delivery.GetString("webhook"))
	if err != nil {
		return err
	}
	if !endpoint.GetBool("enabled") {
		return errors.New("the endpoint is disabled")
	}
	if delivery.GetString("eventType") == WebhookEventTest {
		return errors.New("test events cannot be redelivered")
	}

	delivery.Set("status", WebhookDeliveryPending)
	delivery.Set("attempts", 0)
	next, _ := types.ParseDateTime(time.Now())
	delivery.Set("nextAttempt", next)

	delivery.SetRaw("updated", types.NowDateTime())

	return app.UnsafeWithoutHooks().SaveNoValidate(delivery)
}
