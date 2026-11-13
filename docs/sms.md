# SMS sender

Claviger can send transactional text messages through a small provider interface (`tools/sms`). It is off by default. Features that need to text someone (for example delivering an account recovery link, #83) ask for the configured sender; nothing is sent unless one of them does, or a superuser uses the test button.

Two providers are built in: **Twilio** and a **generic webhook**. Both use the standard library only, with no vendor SDK.

## Configure

Admin UI: Settings > SMS. Or through the settings API (`PATCH /api/settings`), block `sms`:

| Field | Meaning |
|---|---|
| `enabled` | Master switch. Off means `core.NewSMSSender` returns `core.ErrSMSNotConfigured`. |
| `provider` | `twilio` or `webhook`. Required when enabled. |
| `defaultRegion` | Optional ISO 3166 alpha-2 code such as `US`. Used to read a household phone that has no `+` prefix (see Recovery texts). Empty means such numbers get no text. An unknown code is rejected on save. |
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

## Recovery texts

Staff-vouched recovery (`docs/recovery.md`) is the first feature to use the sender. It sends a link to the hosted recovery page, and the "a recovery code was issued" notice for a person without an email. The recipient is the home phone of the person's household; nothing else is stored. A number without a `+` is used only when `defaultRegion` is set (see below). Each text is audited as `recovery_sms_sent` without the number or the text. Wording keys: `sms.recovery.link` and `sms.recovery.notice`, in every locale.

### Default region

With `sms.defaultRegion` set, a household phone without a `+` is read as a national number of that country: spaces, dashes, dots and brackets are removed, a leading national trunk `0` is dropped where that is the convention, and the country code is added. The result must be a valid E.164 number (8 to 15 digits) or the person counts as having no phone; nothing is guessed. This is a small built-in table, not a numbering-plan library, and it does not check that a number is assigned.

Supported regions: the `+1` group (US, CA and the other NANP regions: AG, AI, AS, BB, BM, BS, DM, DO, GD, GU, JM, KN, KY, LC, MP, MS, PR, SX, TC, TT, VC, VG, VI; 10 digits, or 11 with a leading 1; area and exchange codes cannot start with 0 or 1), GB, IE, DE, FR, ES, IT, PT, NL, BE, MX, BR, PH, KR, VN, CN, AU and NZ. A number that starts with `00` is refused, and a number that already has a `+` ignores the region.

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
