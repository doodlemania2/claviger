# Audit log

Claviger keeps an append-only audit log in the hidden system collection `_auditLog` (#74, #110). Only superusers can read it through the API. This page covers what is recorded, with the read audit (#86) in detail.

## What is recorded

| Action | When |
|---|---|
| `create`, `update`, `delete` | Every record change in an audited collection, with before and after values (secrets, hidden fields and encrypted fields redacted). |
| `settings.update`, `collection.create`, `collection.update`, `collection.delete` | Admin changes to settings and schema. |
| feature actions (`field_key_rotate`, `recovery_issue`, `org_transfer_start`, `gdpr_export`, ...) | Operations that are not a plain record change. |
| `read` | Reads of **flagged** collections (below). Nothing is recorded for a read by default. |

Entries carry the actor (`superuser`, `record`, `scim` or `system`), the client IP, and a `meta` object. `meta.orgId` (or `meta.orgIds`) is set when the record belongs to an organization.

## Read audit (#86)

Writes are audited for every collection. Reads are not, because most reads are routine and would flood the log. For a collection that holds sensitive data (pastoral notes, safeguarding records) you can flag it, and every read of it is logged.

### Turn it on

Admin UI: Settings, Application, **Audit log**, field "Audit reads of collections". Or the settings API:

```
PATCH /api/settings
{"audit": {"readCollections": ["pastoral_notes", "_orgMembers"]}}
```

Rules:

- Each name must be an existing collection. System collections (`_orgs`, `_orgMembers`, ...) are allowed.
- `_auditLog` cannot be flagged (reading the log would write to it).
- No duplicates. An empty list turns the feature off.
- The change itself is audited (`settings.update`, `audit.readCollections` in `changedFields`).

With an empty list the cost is one length check per request. For an unflagged collection it is one scan of the short list and no query.

### What an entry looks like

One `read` entry per request:

| Field | Value |
|---|---|
| `action` | `read` |
| `targetCollection` | the collection name |
| `targetId` | the record id when exactly one record was read, empty for a list |
| `actorType`, `actorCollection`, `actorId` | who read. **Superusers are included**: an admin UI read shows as `superuser`. An anonymous reader is `system` with `meta.anonymous` set on expand and realtime entries. |
| `ip` | the client IP (not available for expand entries, see below) |
| `meta.recordIds` | ids of the records returned, at most **100** |
| `meta.count` | how many records were returned |
| `meta.truncated` | `true` when `count` is larger than the ids kept |
| `meta.orgId` / `meta.orgIds` | the organization of the records, when the collection has an `org` or `orgs` relation (or is `_orgs`) |
| `meta.via` | `expand` or `realtime` for those paths, absent for view and list |
| `meta.apiKeyId` | set when the request used an API key |

The entry never stores record content, only ids. A request that returns no record writes no entry. A list that needs several pages is several requests and so several entries.

### What is covered

| Path | Covered | Notes |
|---|---|---|
| `GET /api/collections/{c}/records/{id}` | yes | `OnRecordViewRequest`, runs before user hooks, so a hook that skips `e.Next()` cannot dodge it |
| `GET /api/collections/{c}/records` | yes | `OnRecordsListRequest`, one entry for the page |
| Records of a flagged collection returned through `?expand=` (including back-relations such as `notes_via_subject`) on any response | yes | `meta.via = "expand"`, one entry per expanded relation level. The IP is not available there, the actor is. |
| Realtime create and update events delivered to a subscriber | yes | `meta.via = "realtime"`, `meta.event`; one entry per client and event even when two subscriptions match. IP is the client's connect IP. |
| Batch (`POST /api/batch`) | n/a | Batch only accepts create, update, upsert and delete. Their responses contain the written record, and the write is audited as `create`/`update`. |
| Realtime **delete** events | no | They are prepared inside the delete transaction, where a second write would deadlock on SQLite. The delete itself is audited with the old values. |
| File downloads (`/api/files/...`) | no | Files of flagged records are not logged. Do not rely on read audit for file fields. |
| Auth responses (`auth-with-password`, `auth-refresh`, ...) | no | A user reading their own record at sign-in is not logged, even for a flagged auth collection. |
| Create and update responses | no | They return the record the caller just wrote, and the write is audited. |
| Superuser SQL endpoint, Go and JS hooks, `app.FindRecord...` | no | Server-side code is not a request. Hooks can call the helper below. |
| Personal data export (#88) | no | It has its own entries (`gdpr_export*`). |

If the audit entry cannot be written, a view or list answers 500 and a realtime event is not delivered. Reads of a flagged collection are never served unaudited.

### For other features: `core.AuditRecordRead`

```go
err := core.AuditRecordRead(app, ctx, collection, records, map[string]any{"grantId": grant.Id})
```

Writes one `read` entry for `records` (same shape as above). It does not check the flagged list, so a feature can audit its own reads, for example a break-glass read of a sealed note with the grant id in `meta`. `ctx` carries the actor and IP (`core.WithAuditContext`; in the `apis` package `auditContext(e)` builds it). Extra meta cannot override `recordIds`, `count`, `truncated` or `orgId`. `core.AuditReadsCollection(app, name)` tells whether a collection is flagged.

If a feature audits the same read itself and the collection is also flagged, the read is logged twice. Pick one.

### Webhooks

`read` entries do not produce webhook events (they would be one event per page view). A feature that needs one for its own reads, such as break-glass, adds its own action to the webhook event map.

### Retention and privacy

Read entries follow the normal retention (`audit.retentionDays`). They contain ids and the reader's IP, which is personal data. They are part of the "reads by the person" slice of the personal data export (`docs/data-export.md`).
