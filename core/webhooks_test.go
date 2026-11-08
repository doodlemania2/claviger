package core_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/security"
)

// Fork-local (Claviger #77).

func TestWebhookSignatureStandardVector(t *testing.T) {
	// Vector from the Standard Webhooks specification/reference libraries.
	const (
		secret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
		id     = "msg_p5jXN8AQM9LWM0D4loKWxJek"
		body   = `{"test": 2432232314}`
		want   = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
	)

	got, err := core.SignWebhook(secret, id, time.Unix(1614265330, 0), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("signature mismatch:\n got %s\nwant %s", got, want)
	}

	if _, err := core.SignWebhook("whsec_!!!notbase64", id, time.Now(), nil); err == nil {
		t.Fatal("expected an error for an invalid secret")
	}
}

func TestGenerateWebhookSecret(t *testing.T) {
	s := core.GenerateWebhookSecret()
	if !strings.HasPrefix(s, "whsec_") || len(s) != len("whsec_")+44 { // base64 of 32 bytes
		t.Fatalf("unexpected secret shape %q", s)
	}
	if s == core.GenerateWebhookSecret() {
		t.Fatal("secrets must be random")
	}
}

func TestValidateWebhookURL(t *testing.T) {
	scenarios := []struct {
		url        string
		allowLocal bool
		ok         bool
	}{
		{"https://example.com/hook", false, true},
		{"http://example.com/hook", false, false},
		{"http://example.com/hook", true, false},
		{"http://localhost:8080/h", false, false},
		{"http://localhost:8080/h", true, true},
		{"http://127.0.0.1:8080/h", true, true},
		{"https://127.0.0.1/h", false, false},
		{"http://10.0.0.5/h", true, false},
		{"https://10.0.0.5/h", true, false},
		{"https://169.254.169.254/latest", false, false},
		{"https://192.168.1.1/h", false, false},
		{"https://[fd00:ec2::254]/h", false, false},
		{"https://user:pass@example.com/h", false, false},
		{"ftp://example.com", false, false},
		{"not a url", false, false},
	}

	for _, s := range scenarios {
		t.Run(s.url, func(t *testing.T) {
			if s.allowLocal {
				t.Setenv(core.WebhooksAllowLocalhostEnv, "true")
			} else {
				t.Setenv(core.WebhooksAllowLocalhostEnv, "")
			}
			err := core.ValidateWebhookURL(s.url)
			if (err == nil) != s.ok {
				t.Fatalf("ok=%v, got err=%v", s.ok, err)
			}
		})
	}
}

func TestValidateWebhookEvents(t *testing.T) {
	for _, ok := range [][]string{{"*"}, {"users:create"}, {"users:*", "*:delete"}} {
		if err := core.ValidateWebhookEvents(ok); err != nil {
			t.Fatalf("%v: %v", ok, err)
		}
	}
	for _, bad := range [][]string{{}, {"users"}, {"users:explode"}, {":create"}} {
		if err := core.ValidateWebhookEvents(bad); err == nil {
			t.Fatalf("%v: expected error", bad)
		}
	}
}

// -------------------------------------------------------------------

type recvd struct {
	header http.Header
	body   string
}

type receiver struct {
	srv    *httptest.Server
	mu     sync.Mutex
	got    []recvd
	status atomic.Int32
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	r.status.Store(200)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, recvd{header: req.Header.Clone(), body: string(b)})
		r.mu.Unlock()
		w.WriteHeader(int(r.status.Load()))
		w.Write([]byte("receiver says hi"))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func newWebhookTestApp(t *testing.T) (*tests.TestApp, *core.WebhookTestHooks) {
	t.Helper()
	t.Setenv(core.WebhooksAllowLocalhostEnv, "true")
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	seams := core.WebhookTestSeams(app)
	seams.SetServing(false) // tests drive the worker explicitly
	return app, seams
}

func addEndpoint(t *testing.T, app core.App, url string, events []string, enabled bool) (*core.Record, string) {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(core.CollectionNameWebhooks)
	if err != nil {
		t.Fatal(err)
	}
	secret := core.GenerateWebhookSecret()
	r := core.NewRecord(col)
	r.Set("url", url)
	r.Set("events", events)
	r.Set("enabled", enabled)
	if err := core.SetWebhookSecret(app, r, secret); err != nil {
		t.Fatal(err)
	}
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r, secret
}

func deliveries(t *testing.T, app core.App) []*core.Record {
	t.Helper()
	rs, err := app.FindAllRecords(core.CollectionNameWebhookDeliveries)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func changeRecord(t *testing.T, app core.App, id string) {
	t.Helper()
	col, _ := app.FindCollectionByNameOrId("demo2")
	r := core.NewRecord(col)
	r.Id = id
	r.Set("title", "hello "+id)
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookNoEndpointsNoWork(t *testing.T) {
	app, seams := newWebhookTestApp(t)
	seams.SetServing(true)
	seams.Reload()

	changeRecord(t, app, "whnone000000001")

	if n := len(deliveries(t, app)); n != 0 {
		t.Fatalf("expected no deliveries, got %d", n)
	}
	if core.WebhookWorkerRunning(app) {
		t.Fatal("worker must not run without enabled endpoints")
	}

	// a disabled endpoint does not start it either
	ep, _ := addEndpoint(t, app, "https://example.com/h", []string{"*"}, false)
	if core.WebhookWorkerRunning(app) {
		t.Fatal("worker must not run for a disabled endpoint")
	}
	changeRecord(t, app, "whnone000000002")
	if n := len(deliveries(t, app)); n != 0 {
		t.Fatalf("disabled endpoint received %d deliveries", n)
	}

	// enabling starts it, disabling stops it
	ep.Set("enabled", true)
	if err := app.Save(ep); err != nil {
		t.Fatal(err)
	}
	if !core.WebhookWorkerRunning(app) {
		t.Fatal("worker should run once an endpoint is enabled")
	}
	ep.Set("enabled", false)
	if err := app.Save(ep); err != nil {
		t.Fatal(err)
	}
	if core.WebhookWorkerRunning(app) {
		t.Fatal("worker should stop when the last endpoint is disabled")
	}
}

func TestWebhookEnqueueIsTransactional(t *testing.T) {
	app, _ := newWebhookTestApp(t)
	rcv := newReceiver(t)
	addEndpoint(t, app, rcv.srv.URL, []string{"demo2:create"}, true)

	col, _ := app.FindCollectionByNameOrId("demo2")

	// rolled back together with the change
	err := app.RunInTransaction(func(tx core.App) error {
		r := core.NewRecord(col)
		r.Id = "whrollback00001"
		r.Set("title", "x")
		if err := tx.Save(r); err != nil {
			return err
		}
		if n := len(deliveries(t, tx)); n != 1 {
			t.Fatalf("expected the delivery to be enqueued inside the tx, got %d", n)
		}
		return http.ErrAbortHandler
	})
	if err == nil {
		t.Fatal("expected rollback error")
	}
	if n := len(deliveries(t, app)); n != 0 {
		t.Fatalf("outbox rows survived a rollback: %d", n)
	}

	// committed together
	changeRecord(t, app, "whcommit0000001")
	ds := deliveries(t, app)
	if len(ds) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(ds))
	}

	var p core.WebhookPayload
	if err := json.Unmarshal([]byte(ds[0].GetString("payload")), &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != "record.created" || p.Data.Collection != "demo2" || p.Data.RecordId != "whcommit0000001" ||
		p.Data.Actor.Type != core.AuditActorTypeSystem || p.Data.Record["title"] != "hello whcommit0000001" || p.Timestamp == "" {
		t.Fatalf("unexpected payload: %s", ds[0].GetString("payload"))
	}
}

func TestWebhookEventFilters(t *testing.T) {
	app, _ := newWebhookTestApp(t)
	addEndpoint(t, app, "https://a.example.com/h", []string{"demo2:delete"}, true)
	addEndpoint(t, app, "https://b.example.com/h", []string{"demo1:*"}, true)
	addEndpoint(t, app, "https://c.example.com/h", []string{"*:create"}, true)

	changeRecord(t, app, "whfilter0000001")

	ds := deliveries(t, app)
	if len(ds) != 1 {
		t.Fatalf("expected only the *:create endpoint to match, got %d", len(ds))
	}

	r, _ := app.FindRecordById("demo2", "whfilter0000001")
	if err := app.Delete(r); err != nil {
		t.Fatal(err)
	}
	ds = deliveries(t, app)
	if len(ds) != 2 {
		t.Fatalf("expected the delete to match demo2:delete, got %d total", len(ds))
	}

	// webhook machinery never produces events about itself
	for _, d := range ds {
		if strings.Contains(d.GetString("payload"), "_webhook") {
			t.Fatalf("self-referencing event: %s", d.GetString("payload"))
		}
	}
}

func TestWebhookDeliverySuccessAndSignature(t *testing.T) {
	app, seams := newWebhookTestApp(t)
	rcv := newReceiver(t)
	_, secret := addEndpoint(t, app, rcv.srv.URL, []string{"*"}, true)

	changeRecord(t, app, "whdeliver000001")

	if n := seams.ProcessDue(); n != 1 {
		t.Fatalf("expected 1 processed, got %d", n)
	}
	if rcv.count() != 1 {
		t.Fatalf("receiver got %d requests", rcv.count())
	}

	got := rcv.got[0]
	id, ts, sig := got.header.Get("webhook-id"), got.header.Get("webhook-timestamp"), got.header.Get("webhook-signature")
	if !strings.HasPrefix(id, "msg_") || ts == "" || got.header.Get("Content-Type") != "application/json" {
		t.Fatalf("bad headers: %v", got.header)
	}
	var unix int64
	if _, err := fmtSscan(ts, &unix); err != nil {
		t.Fatal(err)
	}
	want, _ := core.SignWebhook(secret, id, time.Unix(unix, 0), []byte(got.body))
	if sig != want {
		t.Fatalf("signature mismatch: %s vs %s", sig, want)
	}

	d := deliveries(t, app)[0]
	if d.GetString("status") != core.WebhookDeliverySuccess || d.GetInt("attempts") != 1 || d.GetInt("lastStatusCode") != 200 {
		t.Fatalf("unexpected delivery state: %s", d.PublicExport())
	}
	if !strings.Contains(d.GetString("attemptLog"), "receiver says hi") {
		t.Fatalf("attempt log missing response: %s", d.GetString("attemptLog"))
	}

	// nothing more due
	if n := seams.ProcessDue(); n != 0 {
		t.Fatalf("expected nothing due, got %d", n)
	}
}

func TestWebhookRetrySchedule(t *testing.T) {
	app, seams := newWebhookTestApp(t)
	rcv := newReceiver(t)
	rcv.status.Store(500)
	addEndpoint(t, app, rcv.srv.URL, []string{"*"}, true)

	now := time.Now()
	seams.SetClock(func() time.Time { return now })
	delays := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}
	seams.SetRetryDelays(delays)

	changeRecord(t, app, "whretry00000001")

	for i, delay := range delays {
		if n := seams.ProcessDue(); n != 1 {
			t.Fatalf("attempt %d: expected 1 processed, got %d", i+1, n)
		}
		d := deliveries(t, app)[0]
		if d.GetString("status") != core.WebhookDeliveryPending || d.GetInt("attempts") != i+1 {
			t.Fatalf("attempt %d: unexpected state %s", i+1, d.PublicExport())
		}
		next := d.GetDateTime("nextAttempt").Time()
		if diff := next.Sub(now) - delay; diff < -time.Second || diff > time.Second {
			t.Fatalf("attempt %d: next attempt in %s, want %s", i+1, next.Sub(now), delay)
		}

		// not due before its time
		if n := seams.ProcessDue(); n != 0 {
			t.Fatalf("attempt %d: delivery was due early", i+1)
		}
		now = now.Add(delay + time.Second)
	}

	// the receiver recovers? no: final attempt exhausts retries -> dead
	if n := seams.ProcessDue(); n != 1 {
		t.Fatalf("final attempt not processed")
	}
	d := deliveries(t, app)[0]
	if d.GetString("status") != core.WebhookDeliveryDead || d.GetInt("attempts") != len(delays)+1 {
		t.Fatalf("expected dead after %d attempts: %s", len(delays)+1, d.PublicExport())
	}
	if rcv.count() != len(delays)+1 {
		t.Fatalf("receiver saw %d requests", rcv.count())
	}
	if !strings.Contains(d.GetString("lastError"), "500") {
		t.Fatalf("lastError = %q", d.GetString("lastError"))
	}
	// same message id on every attempt
	for _, g := range rcv.got {
		if g.header.Get("webhook-id") != rcv.got[0].header.Get("webhook-id") {
			t.Fatal("message id changed between attempts")
		}
	}
}

func TestWebhookRetryThroughOutage(t *testing.T) {
	app, seams := newWebhookTestApp(t)
	rcv := newReceiver(t)
	rcv.status.Store(503)
	addEndpoint(t, app, rcv.srv.URL, []string{"*"}, true)

	now := time.Now()
	seams.SetClock(func() time.Time { return now })

	changeRecord(t, app, "whoutage0000001")
	seams.ProcessDue() // fails

	rcv.status.Store(200)
	now = now.Add(2 * time.Minute) // past the default 1m backoff
	seams.ProcessDue()

	d := deliveries(t, app)[0]
	if d.GetString("status") != core.WebhookDeliverySuccess || d.GetInt("attempts") != 2 {
		t.Fatalf("expected success on the 2nd attempt: %s", d.PublicExport())
	}
}

func TestWebhookSSRFRefusal(t *testing.T) {
	app, seams := newWebhookTestApp(t)
	rcv := newReceiver(t)
	addEndpoint(t, app, rcv.srv.URL, []string{"*"}, true)

	t.Setenv(core.WebhooksAllowLocalhostEnv, "") // flag off after the endpoint was accepted

	changeRecord(t, app, "whssrf000000001")
	seams.ProcessDue()

	if rcv.count() != 0 {
		t.Fatal("delivery reached a loopback address with the dev flag off")
	}
	d := deliveries(t, app)[0]
	if d.GetInt("attempts") != 1 || d.GetString("lastError") == "" {
		t.Fatalf("expected a recorded refusal: %s", d.PublicExport())
	}
}

func TestWebhookSSRFPrivateAddressesRefusedByDialer(t *testing.T) {
	app, seams := newWebhookTestApp(t)
	// bypass the config-time check by writing the url straight to the db
	ep, _ := addEndpoint(t, app, "https://example.com/h", []string{"*"}, true)
	for _, target := range []string{"http://10.1.2.3/h", "http://169.254.169.254/latest/meta-data", "http://192.168.0.10/h", "http://[::1]:9/h"} {
		if _, err := app.NonconcurrentDB().NewQuery("UPDATE _webhooks SET url={:u} WHERE id={:id}").
			Bind(map[string]any{"u": target, "id": ep.Id}).Execute(); err != nil {
			t.Fatal(err)
		}
		seams.Reload()
		changeRecord(t, app, core.GenerateDefaultRandomId())
		seams.ProcessDue()
	}
	for _, d := range deliveries(t, app) {
		if d.GetInt("lastStatusCode") != 0 || d.GetString("lastError") == "" {
			t.Fatalf("expected a refusal without any HTTP exchange: %s", d.PublicExport())
		}
	}
}

func TestWebhookNoRedirects(t *testing.T) {
	app, seams := newWebhookTestApp(t)
	target := newReceiver(t)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)
	addEndpoint(t, app, redirector.URL, []string{"*"}, true)

	changeRecord(t, app, "whredirect00001")
	seams.ProcessDue()

	if target.count() != 0 {
		t.Fatal("redirect was followed")
	}
	if d := deliveries(t, app)[0]; d.GetInt("lastStatusCode") != 302 || d.GetString("status") != core.WebhookDeliveryPending {
		t.Fatalf("a 3xx must be a failed attempt: %s", d.PublicExport())
	}
}

func TestWebhookResponseBodyTruncated(t *testing.T) {
	app, seams := newWebhookTestApp(t)
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("a", 5000)))
	}))
	t.Cleanup(big.Close)
	addEndpoint(t, app, big.URL, []string{"*"}, true)
	changeRecord(t, app, "whtrunc00000001")
	seams.ProcessDue()

	var log []core.WebhookAttempt
	if err := deliveries(t, app)[0].UnmarshalJSONField("attemptLog", &log); err != nil || len(log) != 1 {
		t.Fatalf("attempt log: %v %v", log, err)
	}
	if len(log[0].Response) != 1024 {
		t.Fatalf("response snippet is %d bytes, want 1024", len(log[0].Response))
	}
}

func TestWebhookAutoDisable(t *testing.T) {
	app, seams := newWebhookTestApp(t)
	rcv := newReceiver(t)
	rcv.status.Store(500)
	ep, _ := addEndpoint(t, app, rcv.srv.URL, []string{"*"}, true)

	now := time.Now()
	seams.SetClock(func() time.Time { return now })
	seams.SetFailPolicy(3, 24*time.Hour)
	seams.SetRetryDelays([]time.Duration{time.Second, time.Second, time.Second, time.Second, time.Second})

	app.TestMailer.Reset()
	changeRecord(t, app, "whdisable000001")

	for i := 0; i < 3; i++ {
		seams.ProcessDue()
		now = now.Add(2 * time.Second)
	}
	ep, _ = app.FindRecordById(core.CollectionNameWebhooks, ep.Id)
	if !ep.GetBool("enabled") {
		t.Fatal("must not disable before the failure window elapsed")
	}

	now = now.Add(25 * time.Hour)
	seams.ProcessDue()

	ep, _ = app.FindRecordById(core.CollectionNameWebhooks, ep.Id)
	if ep.GetBool("enabled") || ep.GetString("disabledReason") == "" {
		t.Fatalf("expected the endpoint to be disabled: %s", ep.PublicExport())
	}
	if core.WebhookWorkerRunning(app) {
		t.Fatal("worker should stop once no endpoint is enabled")
	}
	if app.TestMailer.TotalSend() == 0 {
		t.Fatal("expected a system alert email to the superusers")
	}
}

func TestWebhookSecretNeverLeaks(t *testing.T) {
	app, seams := newWebhookTestApp(t)
	rcv := newReceiver(t)
	ep, secret := addEndpoint(t, app, rcv.srv.URL, []string{"*"}, true)

	changeRecord(t, app, "whsecret0000001")
	seams.ProcessDue()
	if _, err := core.SendWebhookTest(app, ep); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(mustJSON(ep.PublicExport())), secret) {
		t.Fatal("secret in the record's public export")
	}
	for _, d := range deliveries(t, app) {
		if strings.Contains(string(mustJSON(d.PublicExport())), secret) || strings.Contains(d.GetString("attemptLog"), secret) {
			t.Fatal("secret in a delivery")
		}
	}

	audits, _ := app.FindAllRecords(core.CollectionNameAuditLog)
	for _, a := range audits {
		if strings.Contains(string(mustJSON(a.PublicExport())), secret) {
			t.Fatalf("secret in an audit entry: %s", a.PublicExport())
		}
	}
	if len(audits) == 0 {
		t.Fatal("expected audit entries for the endpoint changes")
	}
}

func TestWebhookSecretEncryptedAtRest(t *testing.T) {
	t.Setenv("CLAVIGER_TEST_ENC", security.RandomString(32))
	t.Setenv(core.WebhooksAllowLocalhostEnv, "true")

	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{EncryptionEnv: "CLAVIGER_TEST_ENC", DataDir: t.TempDir()})
	if err != nil {
		t.Skip("cannot build a fresh test app with an encryption env:", err)
	}
	defer app.Cleanup()
	core.WebhookTestSeams(app).SetServing(false)

	rcv := newReceiver(t)
	ep, secret := addEndpoint(t, app, rcv.srv.URL, []string{"*"}, true)

	stored := ep.GetString("secret")
	if stored == secret || !strings.HasPrefix(stored, "enc:") {
		t.Fatalf("secret not encrypted at rest: %q", stored)
	}

	d, err := core.SendWebhookTest(app, ep)
	if err != nil {
		t.Fatal(err)
	}
	if d.GetString("status") != core.WebhookDeliverySuccess {
		t.Fatalf("encrypted secret did not round trip: %s", d.PublicExport())
	}
	sig := rcv.got[0].header.Get("webhook-signature")
	want, _ := core.SignWebhook(secret, rcv.got[0].header.Get("webhook-id"), time.Unix(mustInt(rcv.got[0].header.Get("webhook-timestamp")), 0), []byte(rcv.got[0].body))
	if sig != want {
		t.Fatal("signature made with a different secret")
	}
}

func TestWebhookSecretRotationSignsWithBoth(t *testing.T) {
	app, _ := newWebhookTestApp(t)
	rcv := newReceiver(t)
	ep, oldSecret := addEndpoint(t, app, rcv.srv.URL, []string{"*"}, true)

	newSecret, err := core.RotateWebhookSecret(app, ep)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Save(ep); err != nil {
		t.Fatal(err)
	}

	if _, err := core.SendWebhookTest(app, ep); err != nil {
		t.Fatal(err)
	}
	h := rcv.got[0].header
	sigs := strings.Fields(h.Get("webhook-signature"))
	if len(sigs) != 2 {
		t.Fatalf("expected two signatures during rotation, got %q", sigs)
	}
	ts := time.Unix(mustInt(h.Get("webhook-timestamp")), 0)
	a, _ := core.SignWebhook(newSecret, h.Get("webhook-id"), ts, []byte(rcv.got[0].body))
	b, _ := core.SignWebhook(oldSecret, h.Get("webhook-id"), ts, []byte(rcv.got[0].body))
	if sigs[0] != a || sigs[1] != b {
		t.Fatal("signatures do not match the new and previous secrets")
	}
}

func TestWebhookTestEventAndRedeliver(t *testing.T) {
	app, seams := newWebhookTestApp(t)
	rcv := newReceiver(t)
	rcv.status.Store(500)
	ep, _ := addEndpoint(t, app, rcv.srv.URL, []string{"*"}, false) // disabled: test still works

	d, err := core.SendWebhookTest(app, ep)
	if err != nil {
		t.Fatal(err)
	}
	if d.GetString("status") != core.WebhookDeliveryDead || !strings.Contains(rcv.got[0].body, "webhook.test") {
		t.Fatalf("unexpected test delivery: %s", d.PublicExport())
	}
	if err := core.RedeliverWebhook(app, d); err == nil {
		t.Fatal("test events and disabled endpoints cannot be redelivered")
	}

	ep.Set("enabled", true)
	app.Save(ep)
	changeRecord(t, app, "whredeliver0001")
	for _, x := range deliveries(t, app) {
		if x.GetString("eventType") == "record.created" {
			x.Set("status", core.WebhookDeliveryDead)
			app.UnsafeWithoutHooks().SaveNoValidate(x)
			if err := core.RedeliverWebhook(app, x); err != nil {
				t.Fatal(err)
			}
		}
	}
	rcv.status.Store(200)
	if n := seams.ProcessDue(); n != 1 {
		t.Fatalf("expected the redelivery to be due, processed %d", n)
	}
}

func TestWebhookCleanup(t *testing.T) {
	app, _ := newWebhookTestApp(t)
	addEndpoint(t, app, "https://example.com/h", []string{"*"}, true)
	changeRecord(t, app, "whcleanup000001")

	if n, err := core.DeleteExpiredWebhookDeliveries(app, time.Now().AddDate(0, 0, -30)); err != nil || n != 0 {
		t.Fatalf("fresh deliveries must stay: %d %v", n, err)
	}
	if n, err := core.DeleteExpiredWebhookDeliveries(app, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("expected 1 expired, got %d %v", n, err)
	}
}

func TestWebhookWorkerLoop(t *testing.T) {
	app, seams := newWebhookTestApp(t)
	rcv := newReceiver(t)
	seams.SetPollInterval(20 * time.Millisecond)
	seams.SetServing(true)
	addEndpoint(t, app, rcv.srv.URL, []string{"*"}, true) // starts the worker through the hook

	if !core.WebhookWorkerRunning(app) {
		t.Fatal("worker not running")
	}
	changeRecord(t, app, "whloop000000001")

	deadline := time.Now().Add(5 * time.Second)
	for rcv.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if rcv.count() != 1 {
		t.Fatalf("worker did not deliver (%d)", rcv.count())
	}
	seams.Stop()
	if core.WebhookWorkerRunning(app) {
		t.Fatal("worker still running after stop")
	}
}

func fmtSscan(s string, v *int64) (int, error) { return fmt.Sscan(s, v) }

func mustInt(s string) int64 {
	var v int64
	fmt.Sscan(s, &v)
	return v
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
