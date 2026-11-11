# SMS sender

Claviger can send transactional text messages through a small provider interface (`tools/sms`). It is off by default. Features that need to text someone (for example delivering an account recovery link, #83) ask for the configured sender; nothing is sent unless one of them does, or a superuser uses the test button.

Two providers are built in: **Twilio** and a **generic webhook**. Both use the standard library only, with no vendor SDK.

## Configure

Admin UI: Settings > SMS. Or through the settings API (`PATCH /api/settings`), block `sms`:

| Field | Meaning |
|---|---|
| `enabled` | Master switch. Off means `core.NewSMSSender` returns `core.ErrSMSNotConfigured`. |
| `provider` | `twilio` or `webhook`. Required when enabled. |
| `twilio.accountSid` | `AC...`. Required for Twilio (it is part of the request path). |
| `twilio.authToken` | Auth token. Write-only. |
| `twilio.apiKeySid`, `twilio.apiKeySecret` | Optional API key, used instead of the auth token. Write-only secret. |
| `twilio.from` | Sending number in E.164 format. |
| `twilio.messagingServiceSid` | `MG...`. Use this instead of `from` to send through a Messaging Service. |
| `webhook.url` | `https://` endpoint (plain `http` only for localhost). |
| `webhook.secret` | Signing secret. Write-only. |

Secrets are never returned by the settings API and are redacted in the audit log. Leave a secret field out of a `PATCH` to keep the saved value. When settings encryption is on (`PB_ENCRYPTION_KEY`), they are encrypted at rest with the rest of the settings.

When enabled, the selected provider's required fields must be set or the save is rejected.

## Twilio

1. In the Twilio console, copy the Account SID, and either the Auth Token or create an API key (SID `SK...` plus secret).
2. Buy or pick a number, or create a Messaging Service. Complete carrier registration for your use (see Legal).
3. Fill the fields above, save, then use "Send test".

Requests go to `https://api.twilio.com/2010-04-01/Accounts/{AccountSid}/Messages.json` with basic auth. Twilio errors are mapped to a typed `*sms.Error` (`Provider`, `HTTPStatus`, `Code`, sanitized `Message`). Error text never contains the credentials, and the recipient number is masked.

## Webhook

For any other gateway. Claviger POSTs:

```
POST <url>
Content-Type: application/json
X-Claviger-Timestamp: 1700000000
X-Claviger-Signature: v1=<hex>

{"to":"+14155550123","body":"..."}
```

The signature is the hex HMAC-SHA256, keyed with the secret, of `"<timestamp>." + <raw request body>`. A receiver should recompute it over the raw bytes, compare in constant time, and reject timestamps that are too old. Any 2xx response counts as sent; anything else is an error. Redirects are not followed.

## Test send

`POST /api/settings/test/sms` with `{"to": "+14155550123"}` (superuser only) sends a fixed text and returns 204. Each call is audited as `sms.test` with `meta.to` masked to the last two digits, the provider and the outcome. The full number and the text are not stored or logged.

## For Go callers

```go
sender, err := core.NewSMSSender(app) // core.ErrSMSNotConfigured while disabled
err = sender.Send(ctx, &sms.Message{To: "+14155550123", Body: "..."})
```

`To` must be E.164 (`sms.ValidateE164`); anything else is refused before a request is made. Use `sms.MaskNumber` when logging a number. Do not log message bodies at INFO.

## Legal

This feature sends **transactional messages only** (messages the person asked for or needs to complete an action they started). It is not a marketing tool.

Compliance is the deployment's responsibility and needs review by a lawyer:

- In the US, application-to-person traffic over 10-digit long codes needs 10DLC brand and campaign registration (or a toll-free verification). Unregistered traffic is filtered or blocked.
- Collect and record opt-in consent for the numbers you text, and keep the evidence.
- Honor STOP and HELP keywords. Twilio handles standard opt-out keywords on its numbers, but your use of a webhook gateway may not.
- Other countries have their own sender registration and consent rules.
- Review the message wording and how consent is captured with counsel.
