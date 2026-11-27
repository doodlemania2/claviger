# Datastores

Claviger stores its data in SQLite by default, exactly like PocketBase. Nothing needs to be configured for that.

## Engines at a glance

One variable chooses the engine, `CLAVIGER_DB_URL`; the URL scheme says which. Unset means SQLite.

| Engine | `CLAVIGER_DB_URL` | Status | Several replicas | `migrate-db` target | Tested versions |
| --- | --- | --- | --- | --- | --- |
| SQLite | (unset) | The default, unchanged. | No (single writer, `entrypoint.sh` handover) | n/a (the source) | bundled `modernc.org/sqlite` |
| [PostgreSQL](#postgresql) | `postgres://` or `postgresql://` | Supported. | Yes, opt-in | Yes | 16, 17 |
| [CockroachDB](#cockroachdb) | `cockroach://` or `cockroachdb://`; a `postgres://` URL that reaches CockroachDB is detected | Supported, with the limits listed in its section. | **Refused** (no `LISTEN`/`NOTIFY`, no advisory locks) | Yes | v25.4 (single node) |
| [MySQL](#mysql-experimental) | `mysql://` | **Experimental.** Passes the tagged suites, not the regular ones (see the section). | **Refused** | No | 8.4 |
| MariaDB | `mysql://` or `mariadb://` | **Refused at startup** (no functional indexes in 11.4, which the partial and case insensitive unique indexes need). | n/a | n/a | n/a |

Everything engine specific lives behind the `Dialect` interface in `tools/dialect`. SQLite's SQL text is pinned byte for byte by `tools/search/dialect_sqlite_golden_test.go`, and the SQLite suites run as before.

## PostgreSQL

Status: **supported, one replica or several** (issue #106, milestones 1 to 7). The storage layer sits behind a dialect interface (`tools/dialect`), the app runs on PostgreSQL 16 and 17, the records API works (filters, sorting, expand, view collections), collections import and export and the migration commands work, every fork feature runs on it, and `claviger migrate-db` copies an existing SQLite install over ([Moving from SQLite to PostgreSQL](#moving-from-sqlite-to-postgresql)). CI runs the Postgres suites on both versions. More than one replica is opt-in ([Several replicas](#several-replicas)). What is not there is zip backups of the database (see [Backups](#backups)). SQLite stays the default and its behavior is unchanged. The lists below say exactly what is and is not there.

### Turning it on

Set one environment variable before starting Claviger:

```sh
CLAVIGER_DB_URL='postgres://user:password@host:5432/dbname?sslmode=require'
```

- Unset (the default): SQLite, with the same behavior, SQL and files as before.
- PostgreSQL 16 or newer is required.
- Claviger creates its tables in the `public` schema of that database (the connection's `search_path`). Use an empty, dedicated database.
- The `auxiliary.db` file (request logs) stays a local SQLite file in `pb_data`, one per replica. It is disposable, and the OTLP log export already ships logs somewhere durable. Only the data database moves.
- The connection string carries the password. It is kept out of error messages, but treat the environment like any other secret store.
- The driver is the pure Go `pgx`, so `CGO_ENABLED=0` builds keep working. It adds about 5 MB to the binary, whether or not you use it.
- Claviger sets pgx's `default_query_exec_mode=describe_exec` unless your URL already sets a mode. pgx's default caches a prepared statement per connection, and Postgres then fails the first read after a collection's fields change ("cached plan must not change result type"). The cost is one extra round trip per query.

### What works

| Area | State |
| --- | --- |
| Startup | Bootstrap, all system migrations (including the audit log, TOTP, API keys, webhooks, sessions and bans, passkeys), settings load and save. |
| Collections | Create, update (add, rename and drop fields, rename the collection, change a select, relation or file field between single and multiple) and delete, with their indexes. |
| Introspection | `HasTable`, `TableColumns`, `TableInfo` and `TableIndexes` work and hide the internal row id column. |
| Migrations | Serialized with a Postgres advisory lock, so two instances starting together cannot both run them. The pre-v0.23 upgrade migrations are skipped (a Postgres database cannot be in that state). |
| Records API | List (paging, `totalItems`, `skipTotal`, `fields`, `expand` including back relations), view, create, update (with the `field+` and `field-` modifiers), delete (with relation cascade), batch (and its rollback). Field types: text, editor, number, bool, email, url, date, autodate, select (single and multiple), relation (single and multiple), file (the stored paths; uploads use the same file storage as on SQLite), JSON, geoPoint, password. |
| Filters and rules | Every operator (`=` `!=` `>` `>=` `<` `<=` `~` `!~` and the `?` any-of variants), `null`, `true` and `false`, parameters, the datetime macros, the `:each` `:length` `:isset` `:changed` `:lower` modifiers, relation paths, back relations (`_via_`), JSON paths, `@request.*`, `@collection.*`, `geoDistance()` and `strftime()`. The list and create/update/delete rules use the same engine. |
| Sorting | Any field, relation path or JSON path, `@rowid` and `@random`. Without a `sort`, records come back in insertion order, like on SQLite. |
| View collections | Create, update, list, filter and expand. Field types are inferred from the real column types of the view. Replacing a view recreates the views that select from it; deleting a table or view drops the views that depend on it. |
| Collections import and export | `GET /api/collections`, the admin UI export and `PUT /api/collections/import` (also `deleteMissing`), with the indexes in PocketBase's portable form on both engines (see [Indexes](#indexes)). A snapshot exported from one engine imports on the other. |
| Migrations | System migrations, user Go and JS migrations, `migratecmd` (`automigrate`, `migrate collections`, `migrate create`) and the JS `$app.db()` / `$dbx` helpers. The generated migrations apply on either engine. Raw SQL in your own migrations must be written for the database you run on ([Your own SQL](#your-own-sql)). |
| Backups | Zip backups hold the files only, with a manifest that says so; verification reports `skipped`; a zip restore is refused. See [Backups](#backups). |
| Superuser SQL (`/api/sql`) | Runs as written. Reads run in a read only transaction, a write that was mistaken for a read is refused by the server and then handled as a write, and the append-only audit log guard resolves quoting and escapes. |
| Request logs | Stay in the local SQLite `auxiliary.db` (list, stats, view, delete). Nothing about logs touches the Postgres database. |
| Audit log | Records, admin actions (collections, settings, keys), retention cleanup, and the append-only rule, including the redaction of secrets. |
| Sessions and bans | Session create/refresh/revoke, conditional `lastSeen` updates (throttled), bans and expiry. |
| API keys | Create, authenticate, `lastUsed` updates, revoke. |
| TOTP | Enrolment, the `lastStep` replay protection, backup codes. |
| Webhooks | Outbox, delivery attempts, retries and failure counters, export. |
| Rate limits | The counters live in the local SQLite `auxiliary.db` (not the data database), so they are unchanged. With one replica that is all there is to it. With [several replicas](#several-replicas) the limits that guard credentials are shared through the database and the rest stay per replica. |
| Field encryption | Encrypted text and JSON fields, `_fieldKeys`, key rotation (with its compare-and-swap row updates and resume), rewrap, convert and strict mode, and `claviger keys check` / `status` / `rotate` / `rewrap` / `convert` / `strict` / `prune`. |
| OIDC provider | All `_oauth*` collections, authorization code flow, refresh token rotation and reuse detection, SSO, key rotation and the JWKS. |
| Organizations and roles | The rule cookbook of [orgs.md](orgs.md) (membership and role rules) runs against Postgres. |
| Passkeys | Registration and login. |

The differences from SQLite that you can observe are listed under [Differences from SQLite](#differences-from-sqlite).

### Limits

| Area | State |
| --- | --- |
| More than one replica | Opt-in and Postgres only: [Several replicas](#several-replicas). Without `CLAVIGER_MULTI_REPLICA=true`, run a single replica. |
| Zip backup and restore of the database | Not planned: use `pg_dump` or point-in-time recovery. |
| Views written in SQLite SQL, raw SQL in your own migrations | Written for the engine you run on, see [Your own SQL](#your-own-sql). `migrate-db` has `--skip-views` for the first. |
| The request logs (`auxiliary.db`) | Stay in a local SQLite file, one per replica. |

A skipped test is not a pass: the `postgres` CI job fails if one of the tests skips, except for the documented known gaps (the `known_gap` tests in `tests/pgtests` and the 16 tests in `tests/postgres_skip.txt`), which are skipped on purpose and printed with their reason.

### Several replicas

Status: **supported on PostgreSQL, opt-in.** SQLite stays a single replica with the `entrypoint.sh` handover, exactly as before. Nothing in this section applies to it.

#### Turning it on

```sh
CLAVIGER_DB_URL='postgres://...'
CLAVIGER_MULTI_REPLICA=true
```

- It is **refused at startup** (with a message that names the variable) unless the data database is PostgreSQL. There is no way to run several replicas on a SQLite file.
- Every replica must run the same version, the same `pb_hooks`/`pb_migrations`, the same encryption environment variable (it is also what the [OIDC provider](oidc.md) derives its cookie and code keys from: without it each replica invents its own dev key, and a login begun on one replica cannot finish on another) and the same key-encryption key (`CLAVIGER_KEK*`, see [field encryption](field-encryption.md)), and share the same file storage. Local disk storage (`pb_data/storage`) does not work with several replicas: configure S3.
- Claviger creates four small tables on first start, under an advisory lock, so replicas that start together do not collide: `_claviger_event_payloads`, `_claviger_cron_runs`, `_claviger_rate_limits`, `_claviger_ceremonies`. A deployment that does not turn the mode on never has them. They are scratch space (rows live minutes to days) and are not part of a backup.
- Each replica uses its normal connection pool plus **one dedicated connection** for `LISTEN` (it cannot be pooled), and borrows a connection for the duration of a cron job. Size `max_connections` for `replicas x (pool + 2)`. A pooler such as PgBouncer must be in session mode for the `CLAVIGER_DB_URL` (or bypassed for the listener); in transaction mode `LISTEN` does not work.
- `auxiliary.db` (request logs) stays a local SQLite file **per replica**, on the container's own disk. The handover of `entrypoint.sh` is skipped when `CLAVIGER_DB_URL` is a Postgres URL and `CLAVIGER_MULTI_REPLICA` is on, because replicas are meant to overlap during a rollout. A single replica on Postgres keeps the handover.

#### What is coordinated, and how

| What was per process | Now | Guarantee |
| --- | --- | --- |
| Realtime subscriptions (`tools/subscriptions`) | Every record change is published on the replica bus (`LISTEN/NOTIFY`, one channel). Each other replica runs it through the regular broadcast code against **its own clients**, so the rules, the filter and the auth of every subscription are evaluated on the receiving replica. Create and update carry only the collection and the record id (the receiver reads the record). A delete carries the field values (not password hashes, not encrypted fields), because the row is gone: the receiver puts the row back inside a transaction that it always rolls back, evaluates the rules there, and sends. Nothing is committed and no hook runs. Payloads over 6,500 bytes are stored in `_claviger_event_payloads` and only their id is notified. | Typically a few milliseconds from commit to the client of another replica. **At most once**: `NOTIFY` is not stored. Events that happen while a replica's listener is disconnected are lost to its clients (the replica reconnects with backoff and resyncs its caches; see below). Per sender order is kept. Clients should refetch after the SSE stream reconnects, as with one replica. |
| OAuth2 popup flow | The redirect from the provider may reach a replica that does not hold the client's realtime connection. It is forwarded over the bus and the replica that has the client delivers it (with the same client IP check). The answer of the redirect page is optimistic: it cannot know whether the other replica accepted. | Works with or without affinity. |
| Auth record changes of connected clients (token key change, deletion, auth collection secret change) | Published as part of the realtime events, so the clients of every replica lose or refresh their auth. | As above. |
| Cron jobs (logs cleanup excepted, see below), including your `cronAdd` jobs | Each tick of each job is taken with a Postgres advisory lock (`pg_try_advisory_lock`, held on a dedicated connection for the run) and recorded in `_claviger_cron_runs` by (job, minute). The replica that gets the lock first and inserts the marker runs the job; the others skip. The marker keeps a replica whose clock is a second late from running the tick again after the first finished. A manual "run now" takes the lock too, so it never overlaps a run on another replica. | **At most once per tick across all replicas.** If the replica dies inside the job, that tick is not retried (the next schedule runs it). The job runs on whichever replica wins, not always the same one. |
| Webhook delivery worker | Every replica polls, and claims due deliveries in one transaction with `SELECT ... FOR UPDATE SKIP LOCKED`, moving `nextAttempt` forward by a one minute lease before it commits. | **Exactly once while replicas live**: no two replicas claim the same delivery. If a replica dies mid attempt, its lease expires and another replica retries it (at least once; the `webhook-id` header is the same, so receivers can deduplicate, as the Standard Webhooks spec says). |
| Settings and collections caches | Upstream reloads them from the `.pb_notify` file watcher, which only sees one machine. The replica that saves publishes an event and the others reload (debounced by 50 ms). | Visible on the other replicas in about 100 ms. Realtime events and API calls that arrive during that window use the previous rules. A replica that was disconnected reloads after it reconnects. |
| Session cache (30 s) | A revoke, an expiry change or an admin edit publishes the session id (or "all"); the others drop the entry. | Effective on the other replicas as soon as the event arrives, and in any case within `core.SessionCacheTTL` (30 s) if an event was lost. Junk session ids are not published. |
| Field encryption keys (data key cache) | Every key operation (rotate, rewrap, convert, strict, prune) takes a database-wide lock, so only one runs in the whole deployment, and publishes a reload when it ends (also after a partial run). Saving a collection reloads them too. | The other replicas can open and write with a rotated key within about 100 ms. A replica that sees a key id it does not know fails that read (`the data key of the encrypted field is not loaded`) until the reload arrives. |
| Webhook endpoints (cache) | Published when an endpoint changes; the others reload and start or stop their worker. | As the settings cache. |
| WebAuthn/passkey ceremonies (begin and finish) | The pending ceremony session is stored in `_claviger_ceremonies` (as well as in memory for the per-replica caps) and consumed there, so begin and finish can land on different replicas and a token is used once. | Sticky sessions are not needed for passkeys. |
| Rate limits that guard credentials | The counters are one atomic `INSERT ... ON CONFLICT` per request in `_claviger_rate_limits`, with the same fixed window as before. Shared: any limiter whose rule name contains `auth`, `otp`, `totp`, `password`, `passkey`, `webauthn`, `verification`, `login`, `token`, `reset` or `email-change`, the always-on WebAuthn login throttle, the OIDC endpoint budgets, the API key failure limit, the TOTP and OTP limits. If the database cannot be asked, the request is counted on the replica instead (a limiter must not take sign-in down with the database). | The limit is the limit of the deployment, not of each replica. A settings reload no longer resets these counters. |
| Migrations | Already serialized by an advisory lock. `initMigrationsTable` (`CREATE TABLE IF NOT EXISTS`, which is not atomic on Postgres) and the insert of the default settings on the first start of an empty database are now race safe too. | Any number of replicas can start at once. |

#### What stays per replica, and why

| What | Why it is acceptable |
| --- | --- |
| Request logs (`auxiliary.db`) and the cron job `__pbLogsCleanup__` / `__pbDBOptimize__` | The files are local. Ship them with OTLP if you need one place. The two jobs clean local files, so they run on every replica. |
| Rate limits that do not guard a credential (the generic API budget: reads, writes, file downloads) | Counting every request in the database would add a round trip to each of them. With N replicas the effective limit is up to N times the configured one. Put a limit at your load balancer if you need a strict global ceiling. |
| Realtime connections and their subscriptions | A connection lives on the replica that accepted it (see affinity below). |
| Password reset, verification and passkey reset "resend" cooldowns | An in-memory 2 minute guard against double clicks. Another replica may send once more. The rate limits above still apply. |
| Apple Sign-In first-login name | Apple sends the user's name once, to the redirect. It is kept in memory of the replica that finishes the OAuth2 call; if the final call lands elsewhere the name is empty. |
| The status of a running field key job in the dashboard | Shown by the replica that started it. The lock that prevents two jobs is global. |
| Per-replica caps (WebAuthn pending sessions per purpose) | Memory protection, not a security limit. |
| `pb_hooks` state (JS globals), timers, anything your own hooks keep in memory | Same as any scaled-out app: keep state in the database. |
| Zip backups | Not supported on Postgres (see [Backups](#backups)). |

#### Deployment notes

- **Affinity for `/api/realtime` only.** The SSE connection and the `POST /api/realtime` that sets its subscriptions must reach the same replica, because the client id exists only there. Everything else (REST, auth, passkeys, OAuth2 redirects, OIDC) works behind a plain round-robin load balancer. Use the sticky sessions of your platform (cookie or source IP hash) for `/api/realtime`, for example `stickySessions: affinity: sticky` on Azure Container Apps. A `POST` that lands on another replica answers 404 (missing client id) and the JavaScript SDK reconnects. If a replica restarts, its SSE streams close and the SDK reconnects and resubscribes on another one.
- **Rolling updates.** Replicas overlap; there is no handover. Do not mix versions that disagree on the event payloads: unknown event kinds are ignored, so an old replica misses new notifications until it is replaced, and the caches fall back to their TTLs.
- **Health checks.** `/api/health` answers from the replica you reach and does not wait for the event listener. A replica whose listener is down keeps serving; it logs `Multi replica event channel connection failed` with a retry delay and resyncs when it comes back.
- **Clocks.** Keep replica clocks within a second or so (NTP). The cron slot is the tick time rounded to the minute.
- **Sessions and tokens.** JWTs are verified with the collection secrets from the database, so any replica accepts a token issued by another.
- **Uploads.** S3 (or any shared storage). The `pb_data/storage` folder is local.
- **Security of the bus.** Anyone who can `LISTEN` on the database can read the notifications, which carry record ids and, for deletes, field values (never password hashes or encrypted values). That is no more than a role with `SELECT` can read. Do not grant it to other applications.

#### Tests

The multi replica suite starts two to four apps on one database in one process, each behind its own HTTP server:

```sh
cd scripts/pgembedded
go run . -count=1 -run 'MultiReplica|TestBus|TestRunExclusive|TestRateHit|TestCeremony|TestEnsureSchema' \
  ./tests/pgtests/... ./core ./tools/replica/...
```

It covers cross-replica realtime (create, update, delete, hidden records, rules with auth), OAuth2 forwarding, collection/settings/session invalidation, cron single execution (and the local jobs), webhook claiming (exactly once, with the negative control that the same test fails without the claim), the shared auth rate limit, concurrent start of four replicas, a killed listener (reconnect and resync), large payloads, ordering, and the refusal on SQLite. `scripts/test_entrypoint_handover.sh` has the entrypoint scenarios (T11 overlap without handover, T12 a single Postgres replica still hands over). CI runs it in the `postgres` job.

### Moving from SQLite to PostgreSQL

`claviger migrate-db` copies the `data.db` of a `pb_data` directory into an **empty** PostgreSQL database. It is a one way, offline copy: the source is opened read only and is never modified, so the SQLite install stays a valid fallback until you decide to delete it.

```sh
# 1. stop the server (it is the single writer of pb_data), then rehearse:
claviger migrate-db --dir=/pb_data --to='postgres://user:password@host:5432/claviger?sslmode=require' --dry-run

# 2. copy, verify, report:
claviger migrate-db --dir=/pb_data --to='postgres://user:password@host:5432/claviger?sslmode=require' --verbose

# 3. start Claviger on the new database
export CLAVIGER_DB_URL='postgres://user:password@host:5432/claviger?sslmode=require'
claviger keys check      # only if you use field encryption
claviger serve --dir=/pb_data
```

What it does, in order:

1. Refuses to start when the target database has any table (it never merges or overwrites), unless `--resume` continues an interrupted run of this command, and when a server owns the source directory (the single-writer lock or a handover is in progress, the same check as the `keys` commands) unless `--offline-ok`. It also refuses a source that is older than the binary (it lacks system migrations): start the current version on it once, stop it, then migrate.
2. Builds the schema on the target through the system migrations and imports the collections (structure, rules, indexes in their portable form), exactly like a running app does. The target app uses a temporary data directory; nothing is written next to the source.
3. Copies the rows of every table in `_rowid_` (insertion) order, `--batch` rows per transaction (default 1000). The values are adapted to the Postgres column types: `0`/`1` become booleans, JSON text becomes `JSONB`, numeric text in a number column becomes a number, anything that is not text in a text column becomes text, and an empty string in a JSON column becomes `NULL`. Dates stay text. This covers the system and fork collections too: audit log, sessions, API keys, webhooks, OIDC, organizations, invitations, TOTP, passkeys, field keys. Encrypted field values and the wrapped data keys (`_fieldKeys`) are copied byte for byte; the records they are bound to keep their ids, so they decrypt on the new database.
4. Records the migrations of the source (your own included) so they do not run again, then **verifies**: the row count of every table, a checksum over the ids in insertion order, and a sample of rows (up to 20 per table: the first, the last and evenly spaced ones) compared column by column after decoding (booleans, numbers and JSON by value). Any mismatch fails the command with the table and the row, and keeps the progress marker. On success it prints the report (`--verbose` for the per table lines) and drops its progress marker table.

If the command is interrupted, run it again with `--resume`: every batch is one transaction that also advances a progress marker in the target (`_clavigerMigrateDb`), so it continues after the last committed batch. `--resume` refuses when the source changed since the run started.

After the switch:

- **Set `CLAVIGER_DB_URL`** wherever Claviger starts (the container, the unit file). Keep the `pb_data` directory: the storage files (uploads) and `auxiliary.db` (request logs, rate limit counters) stay there. The logs of the old database are not copied; the OTLP log export already ships them elsewhere.
- **Backups move to `pg_dump` or point-in-time recovery** (see [Backups](#backups)). Zip backups still contain the storage files, not the database. Take a dump of the new database before you delete anything, and keep the dump and the storage files of the same moment together.
- **Field encryption keys travel with the data** (`_fieldKeys`), but the key-encryption key does not: `CLAVIGER_KEK`, `CLAVIGER_KEK_FILE`, `CLAVIGER_KEK_AZURE_KEY_ID` (or the app encryption key) must stay configured exactly as before. `CLAVIGER_DB_URL=... claviger keys check` opens every data key with it, read only, and fails with `keys check: FAILED` when it cannot.
- **Settings encryption** (`PB_ENCRYPTION_KEY`, `--encryptionEnv`) is the same too: the stored settings are copied as they are.
- **View collections** hold SQL. Their queries run as written, so a view written for SQLite usually does not run on Postgres. If the import fails on one, `--skip-views` leaves the view collections out (the report lists them) and you recreate them in PostgreSQL SQL afterwards.
- **Tables that are not collections** (created by hand with raw SQL) are not copied; the report lists them as `not copied`. So are columns of a collection table that the Postgres table does not have (a warning names them).
- **Roll back** by unsetting `CLAVIGER_DB_URL`: the SQLite files were never touched. Anything written on Postgres since the switch stays there.
- The `_rowid_` order, ids, timestamps, password hashes, tokens and signing keys are the same, so sessions, API keys and signed tokens keep working across the switch.

Limits: the copy is a snapshot of the moment the command starts, so stop the server first (`--offline-ok` is your statement that none is running). The rows are copied with plain multi-row `INSERT`s: 300,000 rows of four columns took 2 seconds, verification included, against a Postgres on the same laptop (a remote database is bound by the round trips of each batch, so raise `--batch` there). If the data directory has an unmerged write-ahead log (`data.db-wal` with content, which is only left behind by a crash), the command reads through it and creates the `-shm` file next to the database; after a clean shutdown it opens the file immutable and creates nothing.

### How data is stored

- **Dates stay `TEXT`** in the same fixed format PocketBase always used (`2006-01-02 15:04:05.000Z`, UTC). The filter language and sorting compare them as strings, and it keeps a later SQLite to Postgres copy a plain value copy. They are not `timestamptz`.
- **Numbers are `DOUBLE PRECISION`**, because PocketBase numbers are 64 bit floats end to end. `NUMERIC` would come back from the driver as a string and bring exact decimal arithmetic that SQLite never had.
- **JSON is `JSONB`.** The key order and whitespace of a stored JSON value are not preserved (Postgres normalizes them).
- **Booleans are `BOOLEAN`, ids are `TEXT`** with the same 15 character random default as SQLite.
- **Insertion order** comes from a hidden `_rowid_` identity column on every table Claviger creates (named like the SQLite alias, and reserved: no field can use that name). It is the only schema difference you will see if you query the database yourself, and it is never part of the API.
- **Text columns use the `C` collation** (`TEXT COLLATE "C"`): strings compare, sort and case-fold byte by byte, exactly like SQLite's default `BINARY` collation, whatever the locale of the cluster. Without it `title > "Hello"`, `ORDER BY title` and `~` would depend on `lc_collate` and give other results than SQLite. The cost is that sorting is not locale aware (`Z` sorts before `a`), also on SQLite.
- **Case-insensitive indexes** (`COLLATE NOCASE`, used for example by the auth collections' username index) are created as `lower(column)` expression indexes. No extension (`citext`) is needed, so it works on managed Postgres.
- Reads and writes share one connection pool (SQLite keeps its separate single-writer pool).

### Differences from SQLite

The same data and the same request give the same records on both databases. `tests/pgtests` proves that for about 400 filters, sorts, requests and strftime formats by running them against SQLite and Postgres and comparing the results. These differences remain, and each is a visible skipped case in that suite (the `known_gap` tests, with the reason):

| Case | SQLite | PostgreSQL |
| --- | --- | --- |
| A text, select or relation field compared with a number token (`title = 5`) | false | error (400): Postgres does not coerce between types |
| A number field compared with non numeric text (`views = "abc"`) | false | error (400) |
| A bool field compared with a number (`active = 1`) | works | error (400); use `true` and `false` |
| `meta.n > 5` when `meta.n` holds a JSON string | true (text sorts after numbers) | false (jsonb sorts strings before numbers). The same goes for sorting a JSON path that mixes types. Numbers, booleans and null compare the same on both |
| A multiple select, relation or file field compared as a whole (`labels = '["a","b"]'`, `meta ~ "1,2"`) | matches the compact JSON text | matches jsonb's text, which has a space after `,` and `:`. Use `?=` or `:each` |
| Sorting by a field of a multiple relation or back relation (`sort=tags.name`) | the first related record that the join scans | the smallest (`ASC`) or largest (`DESC`) related value |
| Sorting by a JSON object or array (`sort=meta`) | by the compact JSON text | by jsonb's text (spaces after `,` and `:`) |
| `strftime()` | everything SQLite supports | the formats and modifiers below |
| `strftime('%m-%d', '2024-01-31', '+1 month')` | Mar 2 (overflow rolls forward) | Feb 29 (clamped to the month end) |
| Sorting `NULL` | first (`ASC`), last (`DESC`) | the same (emulated with `NULLS FIRST`/`LAST`) |
| The text of a stored JSON value (a JSON field, the audit log `before`, `after` and `meta`, an exported webhook payload) | as it was written | normalized by `jsonb`: a space after `:` and `,`, keys ordered by length then name, duplicate keys dropped. Parsed JSON is identical, so API clients are not affected |

Things that look like differences and are not: `~` is case-insensitive for ASCII letters on both (`ILIKE` under the `C` collation does not fold other letters, like SQLite), `!=` matches empty and `NULL` the same way, and records come back in insertion order without a `sort`.

**`strftime()` on PostgreSQL** supports the formats `%d %e %f %F %g %G %H %I %j %k %l %m %M %p %P %R %s %S %T %u %U %V %w %W %Y %%`, and the modifiers `start of day|month|year`, `[+-]N second|minute|hour|day|month|year` (and the plural), `weekday N`, `unixepoch`, `julianday`, `auto`, `utc` and `subsec`. The time value is a date string, a column, `'now'` (the default), or a number (a julian day, or unix time with `unixepoch`). Dates are read as UTC whatever the session time zone; an empty or invalid date gives `NULL`, like SQLite. Any other format or modifier (`%J`, `localtime`, `ceiling`, ...) is rejected when the filter is built, with a message that names it.

### View collections

The query of a view collection is **your SQL, run as written**, so on PostgreSQL it must be written in PostgreSQL:

- Quote the identifiers that have capitals (`SELECT id, title AS "postTitle"`); Postgres folds unquoted names to lower case.
- The `id` column must exist, and must be text or castable to text. Use `(ROW_NUMBER() OVER ()) AS id` (note the space) if there is none.
- The Claviger parser that reads the column names of the query wants an expression to be in parentheses (`(views * 2) AS doubled`), like on SQLite.
- No wildcard (`*`) columns, as on SQLite.

Field types come from the real column types of the view: `text` and `varchar` become a text field, `double precision`, `numeric`, `bigint` and `integer` become a number field (integers get "no decimals"), `boolean` a bool field and `jsonb` a JSON field. A column that is a plain field of a collection keeps that field's type and options (select values, relation target, ...), exactly as on SQLite. Other types (dates and timestamps, uuid, arrays) become JSON fields; cast them to `text` in the view to get a text field. SQLite has no column types to read, so unknown expressions there are JSON fields.

### Indexes

The `indexes` of a collection are stored, exported and imported in PocketBase's portable form, the same on both engines, for example ``CREATE UNIQUE INDEX `idx_title` ON `posts` (`title` COLLATE NOCASE) WHERE `title` != ''``. On Postgres the index is built from it (``COLLATE NOCASE`` becomes a `lower(title)` index, the identifiers are quoted with `"`), and `app.TableIndexes()` converts the live definitions back to the portable form, so the indexes in the database can be compared with the ones in the collection. Two limits: an expression index is returned the way Postgres normalizes it (`lower(title || slug)` comes back as `lower((title || slug))`), and an index that uses an access method other than `btree` is returned as Postgres writes it.

### Backups

Zip backups (`pb_data` plus a copy of `data.db`) are a SQLite feature. On Postgres:

- **Creating a backup** (the admin UI, `POST /api/backups`, the cron schedule) still works, and still writes the storage files (when they are local) and the local `auxiliary.db` (logs), but **not the database**. The archive says so in its manifest (`.claviger_backup_manifest.json`: `"dataDatabase": "postgres"` and a note), and the server log records that the database was skipped.
- **Back the database up with `pg_dump`** (or `pg_basebackup`), or with your provider's point-in-time recovery. Do that on the schedule you need; Claviger does not run it for you. Keep the storage files and the dump of the same moment together: the file names are referenced from the records.
- **Verification** (restore verify, #78, also the one after an automatic backup) reports `skipped: database is Postgres` for such an archive, not a failure, and logs it at info level. Archives that do contain `data.db` (taken before a migration to Postgres) still verify as before.
- **Restore** of a zip backup is refused (`POST /api/backups/{key}/restore` answers 400, `RestoreBackup` returns an error) and changes nothing, because it replaces `data.db`. Restore the database with `pg_restore` or point-in-time recovery, and copy the storage files back.
- The admin UI shows a notice on the backups page (`GET /api/health` tells superusers which engine is in use: `data.database`).
- The Litestream and `entrypoint.sh` handover machinery is a SQLite concern and does not apply: leave `LITESTREAM_*` unset.

### Your own SQL

SQL you write yourself runs **as written**: view collections, the superuser `/api/sql` endpoint and `$app.db()` in JS hooks. On Postgres that is Postgres SQL:

- Unquoted identifiers are folded to lower case, so `select createdAt from posts` fails; write `"createdAt"`.
- SQLite-only functions (`json_extract`, `strftime`, `randomblob`, ...) are not available.

Claviger does not translate user SQL; translating it would turn this into a hard fork of the storage layer. That includes the SQL of a view collection: write it for the database you run on (see [View collections](#view-collections)).

**Your own migrations.** Go migrations (`app.DB().NewQuery(...)`, `app.DB().Select(...)`) and JS migrations (`$app.db()`) run on the database of the app, so raw SQL in them must be written for it. Prefer the portable pieces where you can: the `dbx` builders (`app.DB().Select()`, `Insert()`, `Update()`, `AddColumn()`, `CreateIndex()`), `[[column]]` and `{{table}}` placeholders (they take the quote style of the engine), and the collection API (`app.Save(collection)`, which builds the tables, columns and indexes for the engine). In a migration that has to differ, ask the dialect: `core.DialectOf(app).Name()` (`"sqlite"` or `"postgres"`) in Go, or `$os.getenv("CLAVIGER_DB_URL") != ""` in JS (there is no dialect helper in the JS runtime). The helpers that the JS runtime exposes (`$app.db()`, `$dbx.*`, `DynamicModel`) work on both engines. Migrations generated by `automigrate` and `migrate collections` contain collection definitions, not SQL, so they apply on either engine.

**The superuser SQL endpoint (`POST /api/sql`).** It runs the statement as written, so on Postgres it is Postgres SQL (quote camelCase names, no `json_extract`). The columns come back with the Postgres type names, and an error is the server's message. A statement that does not start with a write keyword (`SELECT`, `WITH`, `VALUES`, `SHOW`, `EXPLAIN`, ...) runs in a read only transaction; if the server refuses it because it writes (a `WITH ... DELETE ... RETURNING` is such a statement) it is treated as the write it is. Writes run in one transaction, so several statements either all apply or none. The `_auditLog` table is append only: a write that names it is refused whatever the quoting (`"_auditLog"`, `_auditlog`, `public."_auditLog"`, comments in between, `U&"\005faudit..."` escapes), and so is `DROP SCHEMA`, `DROP OWNED` and `DROP DATABASE`. This is a guard against mistakes and casual tampering, not a security boundary against a superuser who can run arbitrary SQL (anyone who can create a function with dynamic SQL can still reach a table); the append-only guarantee for the records API and the admin UI is unchanged.

### Running the Postgres tests

The same suites run against CockroachDB and MySQL: point `CLAVIGER_TEST_PG_URL` at a server of that engine (see [CockroachDB](#cockroachdb) and [MySQL](#mysql-experimental) for the URLs and what is skipped). The tests that differ per engine are listed in `tests/pgtests/engine_test.go` (`engineGaps`).

```sh
# against a throwaway local Postgres 16 (downloaded on first use, no Docker needed;
# EPG_VERSION=17 for Postgres 17):
cd scripts/pgembedded && go run .

# or against a server you already have:
CLAVIGER_TEST_PG_URL='postgres://user:pass@localhost:5432/postgres?sslmode=disable' \
  go test -tags postgres -count=1 -v ./tests/pgtests/...
```

Each test creates and drops its own database, so the URL can point at any database on the server (the role needs `CREATEDB`).

The suite runs the same data and requests on a SQLite app and a Postgres app and compares the answers (`filters_test.go`, `records_api_test.go`, `views_test.go`, `strftime_test.go`). To add a filter, add a row to the table in `TestRecordFilters`; give it the number of records it must match so that a filter which matches nothing anywhere cannot pass. A difference that cannot be fixed goes in the table with a `gap:` reason: it runs on SQLite only, shows up in the log as `known_gap/...` with both answers, and fails as soon as the two databases start to agree (so the list can only shrink). Any other skip fails CI. The SQL text of both dialects is also pinned without a database in `tools/search/dialect_sqlite_golden_test.go` (must never change) and `dialect_postgres_test.go`.

**The regular suites on Postgres (M4).** The `core`, `apis`, `oidc`, `cmd`, `forms`, `mails` and `plugins/jsvm`/`migratecmd` suites also run against Postgres, with the fork features (audit log, sessions and bans, API keys, TOTP, webhooks, field encryption and key rotation, OIDC, organizations) exercised by their own tests:

```sh
CLAVIGER_TEST_PG_URL='postgres://user:pass@localhost:5432/postgres?sslmode=disable' \
  scripts/test_postgres_suites.sh
```

`tests.NewTestApp` does it when `CLAVIGER_TEST_DB=postgres` (the script sets it). The SQLite fixture in `tests/data` cannot be used by Postgres, and a hand written Postgres copy would drift from upstream's. So `tests/app_postgres.go` converts it once per test process: the fixture is opened as a normal SQLite app, its collections are imported into an empty Postgres app (which builds the tables, indexes and views through the same import path as production, and exercises it), and the rows of every table are copied with their values adapted to the column types. The result is a template database, and every test app is a `CREATE DATABASE ... TEMPLATE` clone of it (a few ms), dropped in `Cleanup`. Every upstream test therefore keeps its fixture and its assertions. `TestPostgresHarness` fails if the apps are not really on Postgres.

The tests that cannot pass on Postgres are listed in `tests/postgres_skip.txt`, one per line with the reason: they assert SQLite SQL text, SQLite column definitions, query a SQLite catalog or expect `data.db` in a backup. The behavior they cover is tested on Postgres in `tests/pgtests`. The list may only shrink; the CI job prints it, and fails on any other skip, on a failure, on a run that did not use Postgres and on a run that passes fewer tests than the minimum.

**The migrate-db tests (M5).** `cmd/migrate_db_test.go` runs against a server (`CLAVIGER_TEST_PG_URL`, skipped without one): the full `tests/data` fixture and a database with every fork feature populated are migrated and then compared record by record through the records API on both engines (as the same superuser, so token secrets must have travelled); an interrupted run is resumed twice; a non-empty target, a served source and an old source are refused; the verification is shown to catch a deleted row, an extra row and a changed value; and the source directory is checked to be byte for byte unchanged. `scripts/test_migrate_db_smoke.sh` repeats the essentials with the real binary (`serve` on SQLite, `migrate-db`, `serve` on `CLAVIGER_DB_URL`).

**CI.** The `postgres` job is a matrix over the supported majors, pinned to patch releases (16.15 and 17.11): 16 on every PR and push, 17 on pushes to master and manual runs. Each leg runs `tests/pgtests`, the regular suites (`scripts/test_postgres_suites.sh`, which retries a failed test once and reports it as flaky) and the migrate-db smoke test.

**The skip list.** `tests/postgres_skip.txt` went from 35 tests at M4 to 16 at M6. Tests that only asserted a SQLite spelling were made engine neutral with two small helpers (`tests.ColumnType` and `tests.NormalizeQuotes`, one line per test); the 16 that remain assert SQLite SQL text, SQLite catalogs or SQLite column defaults (two are SQLite-only watcher tests that never touch Postgres), and each names where the Postgres behavior is covered. The list may only shrink.

### For developers

The seam is the `Dialect` interface in `tools/dialect`. Code that has to differ per engine asks `core.DialectOf(app)` (or `dialect.Of(db)` when it holds a builder; a transaction keeps the dialect of its connection). SQLite is the previous behavior moved behind the interface and its SQL text is unchanged. New raw SQL in `core/`, `apis/` or a migration must either work on both engines or go through the dialect. `FORK_SYNC.md` lists the upstream-owned files that call into it.

## CockroachDB

Status: **supported on a single cluster, one or several nodes of it** (issue #106). CockroachDB speaks the PostgreSQL wire protocol and most of its SQL, so it is the PostgreSQL dialect with a CockroachDB variant (`dialect.Cockroach`, name `cockroach`); the PostgreSQL sections above (what works, how data is stored, backups, your own SQL, `migrate-db`) apply unless this section says otherwise. The tagged suites (`tests/pgtests`) pass against CockroachDB v25.4: 439 tests pass and the rest are the skips listed below.

### Turning it on

```sh
CLAVIGER_DB_URL='cockroach://user:password@host:26257/dbname?sslmode=verify-full'
# or, the same thing, because the server is detected after connecting:
CLAVIGER_DB_URL='postgresql://user:password@host:26257/dbname?sslmode=verify-full'
```

- `cockroach://` and `cockroachdb://` are rewritten to `postgresql://` for the driver (`pgx`, the same as for PostgreSQL) and force the CockroachDB dialect. A `postgres://` or `postgresql://` URL gets it too when `SELECT version()` says CockroachDB.
- The data database is the one in the URL, created by you. `auxiliary.db` (logs) stays a local SQLite file, as on PostgreSQL.
- `claviger migrate-db --to='cockroach://...'` works like the PostgreSQL one (the smoke test `scripts/test_migrate_db_smoke.sh` passes against CockroachDB).

### What differs from PostgreSQL

| Area | CockroachDB |
| --- | --- |
| Several replicas (`CLAVIGER_MULTI_REPLICA=true`) | **Refused at startup**, with a message that names the engine. The bus is `LISTEN`/`NOTIFY` (CockroachDB has none) and the cron guard an advisory lock (the functions are missing or no-ops). Run one replica, or use PostgreSQL. |
| Transactions | The default isolation is `SERIALIZABLE`, so a transaction can be aborted with SQLSTATE `40001` when it conflicts with another one. `RunInTransaction` runs the whole function again (up to 20 times, with a growing, jittered delay of at most a second) when that happens. Keep the function free of effects outside the database, as you should anyway; the model hooks that run after commit are only called for the attempt that committed. Single statements outside a transaction are retried by the server. |
| Migrations lock | There is no advisory lock to serialize migrations, so starting two instances at the same moment on an empty database is not guarded: the second one fails on the `_migrations` primary key and can simply be started again. Start one instance first. |
| Changing a field between single and multiple (select, relation, file) | `ALTER COLUMN ... TYPE` that rewrites data is not allowed in a transaction, so this is done only for a collection **without records** (the column is recreated). With records the save is refused with an error that says to add a new field, copy the values and drop the old one. |
| Case insensitive and partial indexes | `COLLATE NOCASE` is a `lower(column)` expression index as on PostgreSQL. CockroachDB cannot create an expression index in a transaction after one was dropped in it, so a collection update renames the old indexes first and drops them after the new ones exist (`FinishIndexDrops`). |
| Text columns | No `COLLATE "C"`: the default collation is already byte order, and the locale `C` is rejected. |
| Row order | The hidden `_rowid_` identity column uses `CACHE 1`, so insertion order holds across nodes (each insert asks the sequence). |
| `ILIKE` and `lower()` | Fold the case of non ASCII letters (`é` matches `É`), SQLite and PostgreSQL's `C` collation only ASCII. The filter operator `~` is therefore Unicode aware on CockroachDB. |
| `strftime()` | The same formats and modifiers as PostgreSQL. CockroachDB has no `pg_input_is_valid()`, so text is cast to a timestamp only if it matches a strict date pattern (months 01-12, days 01-31, hours 00-23); text that does not match is `NULL`, like SQLite. A date that does not exist in its month (`2024-02-30`) matches the pattern and makes the query fail. |
| Backups | As on PostgreSQL, the zip holds files only. The manifest names `cockroach` and points at `BACKUP`. |
| `VACUUM`, `ANALYZE` | Not run (garbage collection and statistics are automatic). |

### Running the tests

```sh
docker run -d --name crdb-test -p 26257:26257 cockroachdb/cockroach:latest-v25.4 \
  start-single-node --insecure --cache=512MiB --max-sql-memory=512MiB

# DDL is slow by default (a 25 second bootstrap). These two settings make it ~2 seconds:
for q in "SET CLUSTER SETTING jobs.registry.interval.adopt = '1s'" \
         "SET CLUSTER SETTING jobs.registry.interval.cancel = '1s'" \
         "SET CLUSTER SETTING sql.stats.automatic_collection.enabled = false"; do
  docker exec crdb-test ./cockroach sql --insecure -e "$q"; done

export CLAVIGER_TEST_PG_URL='postgresql://root@localhost:26257/defaultdb?sslmode=disable'
go test -tags postgres -count=1 -v ./tests/pgtests/...        # about a minute
scripts/test_migrate_db_smoke.sh                               # migrate-db end to end
```

Skipped on CockroachDB (each prints its reason, named `known_gap(cockroach)` or in `engineGaps`): the eight multi replica tests, `TestTryUsesSavepoints` (CockroachDB cannot roll back to a savepoint after DDL; `Try` therefore just runs the function there), the two Unicode `LIKE`/`lower()` cases, and the single to multiple conversion step of `TestCollectionLifecycle` (the refusal above is asserted instead). The container needs about 1 GiB of memory.

The **regular suites** (`CLAVIGER_TEST_DB=postgres`, see `scripts/test_postgres_suites.sh`) build one database per test app from the fixture, because CockroachDB has no `CREATE DATABASE ... TEMPLATE`: about 7 seconds per app instead of milliseconds, so a full `./apis` and `./core` run (several thousand apps) takes many hours and was **not** run. What was run, with `-parallel 1` (several concurrent schema builds conflict on CockroachDB's system tables and fail with `restart transaction` even after the retries): `go test ./core -run '^TestRecord' -skip <tests/postgres_skip.txt>` passes (310 tests, about 8 minutes), and `go test ./apis -run '^TestRecordCrudView$'` passes (23 scenarios, 6 minutes). `TestRecordCrudCreate` was started and stopped unfinished (no failure before it was stopped); the rest of `./apis` was not run. Add `ALTER RANGE default CONFIGURE ZONE USING gc.ttlseconds = 5` to the settings above for such long runs, and drop leftover `pbt_*` databases with `DROP DATABASE ... CASCADE` (CockroachDB has no `WITH (FORCE)`).

## MySQL (experimental)

Status: **experimental** (issue #106). A MySQL 8.0.13+ dialect (`dialect.MySQL`, name `mysql`) exists and the tagged suites (`tests/pgtests`) pass against MySQL 8.4 (419 pass, 0 fail, 36 skips for the engine gaps below), but the regular `core`/`apis` suites were run only on a sample (see [Running the tests](#running-the-tests-1)), `migrate-db` does not target MySQL, several replicas are refused, and SQLite semantics are approximated in places (list below). Do not use it for data you cannot lose. **MariaDB is refused at startup**: its 11.4 has no functional indexes.

### Turning it on

```sh
CLAVIGER_DB_URL='mysql://user:password@host:3306/dbname'     # mariadb:// is accepted and then refused
```

- Create the database yourself, as `utf8mb4` with `utf8mb4_bin`: `CREATE DATABASE claviger CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`. Claviger creates its tables in it. `tls=` and other driver options of the go-sql-driver (`?tls=true`) can be added to the URL.
- The driver is `github.com/go-sql-driver/mysql` (pure Go). The session is opened with `sql_mode=STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION,PIPES_AS_CONCAT,NO_BACKSLASH_ESCAPES` (the filter SQL concatenates with `||` and writes `ESCAPE '\'`), `time_zone='+00:00'`, `collation=utf8mb4_bin`, `multiStatements=true` (the system migrations send scripts) and `optimizer_switch='semijoin=off'`. The last one works around a MySQL 8.4 bug: an `EXISTS` or `IN` subquery over `JSON_TABLE` matched nothing (seen with a `utf8mb4_bin` database), and the back relations and multi value filters need them.
- `auxiliary.db` (logs) stays a local SQLite file.

### How data is stored

| What | MySQL |
| --- | --- |
| Text | `LONGTEXT ... utf8mb4_bin` (byte order, case sensitive, like SQLite's `BINARY`). A literal default is not allowed on a TEXT column, so defaults are expressions: `DEFAULT ('')`. |
| Ids | `VARCHAR(255) utf8mb4_bin`, default `CONCAT('r', LOWER(HEX(RANDOM_BYTES(7))))`. |
| Dates | `LONGTEXT` in the same `2006-01-02 15:04:05.000Z` format, UTC. |
| Numbers, booleans, JSON | `DOUBLE`, `BOOLEAN` (`tinyint(1)`), the native `JSON` type. JSON text is normalized by MySQL (a space after `:` and `,`, keys ordered by length). |
| Insertion order | A hidden `_rowid_ BIGINT AUTO_INCREMENT UNIQUE` column, like PostgreSQL's. |
| Indexes | MySQL cannot index a TEXT column whole, has no partial indexes and no `COLLATE NOCASE`. A plain index on text uses a 191 character prefix key. A **unique** index on text is a unique expression index over `SHA2(column, 256)`. A partial unique index (`WHERE email != ''`) is an expression that is `NULL` when the condition is false (NULLs never collide), so it keeps its meaning. A partial non unique index is created without its `WHERE`. `COLLATE NOCASE` is `LOWER(column)`. The portable definition is kept in the index comment, and `TableIndexes` returns it, so collections export and import the same indexes on every engine. |
| Unique errors | `Duplicate entry ... for key 'table.index'` (error 1062) is mapped to the `validation_not_unique` error of the field whose name the index name contains (the primary key is `id`). |
| DDL | DDL commits implicitly in MySQL: a migration or a collection save that fails half way is **not** rolled back. Seen in the suites: deleting a collection that a view still selects from is refused, but only after its table was dropped (`TestCollectionDelete/view_with_reference`). |
| Transactions | Deadlocks (1213) and lock wait timeouts (1205) are retried like CockroachDB's `40001`. |

### Differences from SQLite

Besides the PostgreSQL list above (most of it applies in spirit), these are the cases that the suites show differ on MySQL (each is a `known_gap(mysql)` or an `engineGaps` entry in `tests/pgtests/engine_test.go`):

- `~` and `:lower` fold the case of non ASCII letters (accents stay significant): `title ~ "école"` matches `ÉCOLE`.
- A JSON string that looks like a number equals the number: `meta.n = 5` also matches `{"n": "5"}`, and it sorts among the numbers.
- `strftime()` supports `%Y %m %d %e %H %I %k %l %M %S %f %j %p %w %s %F %T %R %%` and the modifiers `start of day|month|year`, `[+-]N second|minute|hour|day|month|year`, `utc`, `subsec`. **Not supported** (rejected when the filter is built): `%u %U %W %V %G %g %P`, `weekday N`, julian day and unix time numbers, `localtime`.
- The `info` of a table (`app.TableInfo`) reports MySQL's type spellings (`varchar(255)`, `longtext`, `tinyint(1)`) and defaults (`concat(_utf8mb4\'r\', ...)`).
- A view query is MySQL SQL, as written (`CAST(x AS SIGNED)`, not `INTEGER`; a comparison is typed as a number, not a boolean).
- The superuser SQL endpoint (`/api/sql`) runs MySQL SQL as written; the PostgreSQL oriented test of it is skipped.
- Several replicas, zip backups of the database (as on PostgreSQL: use `mysqldump`) and `migrate-db` to MySQL are not available. Migrations are not serialized (start one instance first).
- `TestTryUsesSavepoints` is skipped: a failed statement does not abort a MySQL transaction.

### Running the tests

```sh
docker run -d --name mysql-test -e MYSQL_ROOT_PASSWORD=test -e MYSQL_DATABASE=claviger -p 3307:3306 mysql:8.4

CLAVIGER_TEST_PG_URL='mysql://root:test@127.0.0.1:3307/claviger' \
  go test -tags postgres -count=1 -v ./tests/pgtests/...           # about a minute
```

Every test creates and drops its own database (`CREATE DATABASE ... utf8mb4_bin`), so the root user is needed. The regular suites need `CLAVIGER_TEST_DB=postgres` plus the same URL and build one database per test app from the fixture (about 4 seconds), so a full run takes hours and was **not** run. What was run (skip list: `tests/postgres_skip.txt`, as `scripts/test_postgres_suites.sh` builds it):

| Run | Result |
| --- | --- |
| `./core -run '^TestRecord'` | 310 tests pass. |
| `./core -run '^TestCollection'` | 233 tests pass, 1 fails: `TestCollectionDelete/view_with_reference` (DDL commits implicitly, see the table above). |
| `./apis -run '^TestRecordCrud'` | 249 tests pass; 2 failed on the unique field error check (the violated MySQL index was not mapped to its field), fixed since and re-run alone: both pass. |

MariaDB 11.4 is refused at startup, so there is nothing to run against it.

## CI for CockroachDB and MySQL (for the conductor)

Nothing here is wired into `.github/workflows/test.yml` yet. Two jobs would do it, each after the `test` job, on the same runner tier as the `postgres` job. The service container syntax of GitHub Actions cannot pass arguments to the CockroachDB image, so that one is started with a `docker run` step.

```yaml
  cockroach:
    needs: test
    runs-on: ${{ vars.RUNNER_LIGHT || 'tdp-k3s-claviger' }}
    env:
      CLAVIGER_TEST_PG_URL: postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable
    steps:
      - uses: actions/checkout@<sha>
      - uses: actions/setup-go@<sha>
        with: { go-version-file: go.mod }
      - name: Start CockroachDB
        run: |
          docker run -d --name crdb -p 26257:26257 cockroachdb/cockroach:v25.4.21 \
            start-single-node --insecure --cache=512MiB --max-sql-memory=512MiB
          until docker exec crdb ./cockroach sql --insecure -e 'select 1' >/dev/null 2>&1; do sleep 1; done
          for q in "SET CLUSTER SETTING jobs.registry.interval.adopt = '1s'" \
                   "SET CLUSTER SETTING jobs.registry.interval.cancel = '1s'" \
                   "SET CLUSTER SETTING sql.stats.automatic_collection.enabled = false" \
                   "ALTER RANGE default CONFIGURE ZONE USING gc.ttlseconds = 5"; do
            docker exec crdb ./cockroach sql --insecure -e "$q"; done
      - run: go test -tags postgres -count=1 -v ./tests/pgtests/... | tee crdb.log   # expect about 440 PASS, 0 FAIL, skips named known_gap(cockroach) or MultiReplica
      - run: scripts/test_migrate_db_smoke.sh

  mysql:
    needs: test
    runs-on: ${{ vars.RUNNER_LIGHT || 'tdp-k3s-claviger' }}
    services:
      mysql:
        image: mysql:8.4
        env: { MYSQL_ROOT_PASSWORD: test, MYSQL_DATABASE: claviger }
        ports: ["3306:3306"]
        options: >-
          --health-cmd "mysqladmin ping -ptest" --health-interval 5s --health-retries 20
    env:
      CLAVIGER_TEST_PG_URL: mysql://root:test@127.0.0.1:3306/claviger
    steps:
      - uses: actions/checkout@<sha>
      - uses: actions/setup-go@<sha>
        with: { go-version-file: go.mod }
      - run: go test -tags postgres -count=1 -v ./tests/pgtests/... | tee mysql.log   # expect about 420 PASS, 0 FAIL
```

Check the counts the way the `postgres` job does (`grep -c -- '--- PASS'` against a minimum, any `--- SKIP` that is not `known_gap` or a multi replica test fails the job). The regular suites on these engines are not part of this proposal: they build one database per test app (4 to 7 seconds), so a full run takes hours; run a sample (`./core -run '^TestRecord'`, `./apis -run '^TestRecordCrud'`) with `CLAVIGER_TEST_DB=postgres` if you want one.

