# Webhooks

Claviger can call your service when a record changes. Every audited record
change (see the audit log) can be delivered, signed according to the
[Standard Webhooks](https://www.standardwebhooks.com) specification, so
off-the-shelf verifier libraries work.

With no webhook configured nothing is sent and **no background work runs**:
the delivery worker starts only while at least one enabled endpoint exists.

**Single replica only.** Webhooks (like SQLite itself) assume one running
instance. Delivery state lives in `data.db`; do not run two writers.

## Setup

Dashboard: **Webhooks** in the header. Or the API (superusers only):

```
POST /api/webhooks
{"url": "https://example.com/hooks/claviger", "events": ["users:create", "orders:*"], "description": "CRM sync"}
```

The response contains the signing secret (`whsec_` + base64 of 32 random bytes).
**It is shown once**; it is never returned again by any API, log or audit entry.
`POST /api/webhooks/{id}/rotate-secret` issues a new one; the previous secret keeps
signing for 24 hours and both signatures are sent meanwhile (the header carries
space separated `v1,...` values).

Other management goes through the records API on the hidden collections
`_webhooks` and `_webhookDeliveries` (superusers only; the secret fields are
hidden even from superusers). `POST /api/webhooks/{id}/test` sends a
`webhook.test` event and `POST /api/webhook-deliveries/{id}/redeliver`
re-queues a delivery.

Secrets are stored encrypted (AES-256-GCM) when the application encryption
env (`--encryptionEnv`) is configured, and in plain text otherwise.

### Event filters

`"*"` (everything), `"<collection>:<create|update|delete>"`, `"<collection>:*"`
or `"*:<action>"`. Internal collections `_auditLog`, `_webhooks` and
`_webhookDeliveries` never produce events. High-churn auth bookkeeping
(`_mfas`, `_otps`, `_authOrigins`) is not audited and so not delivered;
`_externalAuths` and `_webauthnCredentials` changes (account linking, passkeys
added/removed) are delivered.

### URL rules

`https` only. `http://localhost`, `http://127.0.0.1` and `http://[::1]` are
accepted only with the explicit development flag `CLAVIGER_WEBHOOKS_ALLOW_LOCALHOST=true`
(which also lets deliveries reach loopback). Private, link-local (including the
`169.254.169.254` metadata address), CGNAT and other reserved addresses are
always refused. The target is resolved and checked at delivery time, the
connection goes to the vetted IP, proxies are not used and redirects are not
followed (a 3xx is a failed attempt).

## Payload

```json
{
  "type": "record.created",
  "timestamp": "2026-11-07T21:15:43Z",
  "data": {
    "collection": "users",
    "recordId": "4q1xlclmfloku33",
    "actor": {"type": "superuser", "collection": "_superusers", "id": "sywbhecnh46rhm0"},
    "changedFields": ["name", "email"],
    "record": {"name": "Ada", "email": "ada@example.com"},
    "previous": {"name": "Ad"}
  }
}
```

- `type`: `record.created`, `record.updated`, `record.deleted` (and `webhook.test`).
- `actor.type`: `superuser`, `record` or `system`.
- `record`: the state after the change (for deletes, the last state). Built from
  the audit entry, so passwords, tokens, secrets and hidden fields appear as
  `"[redacted]"` and oversized values as `"[truncated: N bytes]"`.
- `previous`: old values of the changed fields (updates only).

## Delivery semantics

- **At least once.** The outbox row is written in the same database transaction
  as the record change, so a change that rolls back never produces a delivery
  and a committed change always does. A retry or a crash between a successful
  HTTP call and its bookkeeping can deliver twice: deduplicate on `webhook-id`
  (stable across retries).
- Success is any `2xx`. Timeout 10 s, 4 concurrent deliveries.
- Retries after 1 min, 5 min, 30 min, 2 h, 6 h, 24 h, then the delivery is
  `dead` (7 attempts in total). Redeliver from the dashboard.
- Each attempt is logged (status code, duration, error, first 1 KB of the
  response). Deliveries are deleted after 30 days.
- An endpoint is disabled automatically (and superusers are emailed) after 10
  consecutive failed attempts spanning at least 24 hours. Re-enabling it clears the streak.
- Ordering between events is not guaranteed.
- Telemetry (when enabled): `claviger_webhook_deliveries_total{outcome}` with
  outcome `success`, `failure` (will retry) or `dead`.

## Verifying a signature

Headers: `webhook-id`, `webhook-timestamp` (unix seconds), `webhook-signature`
(`v1,<base64 HMAC-SHA256>` of `<id>.<timestamp>.<raw body>`, keyed with the
base64-decoded part of the secret after `whsec_`). Always verify the **raw**
body, and reject timestamps more than a few minutes old.

Go:

```go
func verify(secret string, h http.Header, body []byte) bool {
	ts, err := strconv.ParseInt(h.Get("webhook-timestamp"), 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)).Abs() > 5*time.Minute {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s.%d.", h.Get("webhook-id"), ts)
	mac.Write(body)
	want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	for _, sig := range strings.Fields(h.Get("webhook-signature")) {
		if hmac.Equal([]byte(sig), []byte(want)) {
			return true
		}
	}
	return false
}
```

JavaScript (Node):

```js
import { createHmac, timingSafeEqual } from "node:crypto";

export function verify(secret, headers, rawBody) {
  const id = headers["webhook-id"];
  const ts = Number(headers["webhook-timestamp"]);
  if (!ts || Math.abs(Date.now() / 1000 - ts) > 300) return false;

  const key = Buffer.from(secret.replace(/^whsec_/, ""), "base64");
  const want = "v1," + createHmac("sha256", key).update(`${id}.${ts}.${rawBody}`).digest("base64");

  return headers["webhook-signature"].split(" ").some((sig) =>
    sig.length === want.length && timingSafeEqual(Buffer.from(sig), Buffer.from(want)));
}
```

Or use the official libraries (`standardwebhooks` on npm, PyPI, Go, ...).
