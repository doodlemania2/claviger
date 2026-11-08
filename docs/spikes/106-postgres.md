# Spike: a dialect layer and PostgreSQL storage (#106)

Date: 2026-11-07. Status: research only. No Go code in this repo changed.

## Recommendation

Do it, in gated steps, as a Postgres-first dialect seam. Do not call it done until the whole test suite passes on Postgres in CI.

It is not a hard fork of the storage layer today. A throwaway prototype got Claviger through all 17 system migrations on a real Postgres 16.9 with 11 small SQL rewrites. Records, collection DDL, the audit log and sessions then worked. The hard parts are narrower than the grep counts suggest.

It will still cost us for as long as we carry it. About 30 upstream-owned files get edits. Every upstream sync that adds raw SQL needs a human to check it. See "Effort and merge risk".

Make the owner's decision after M1, not now. M1 is the cheap step that shows how big the seam really is.

Keep the logs database (`auxiliary.db`) on local SQLite. That removes the single-writer handover for the data path and keeps the seam small.

## What was built

A throwaway Go module in the session scratchpad, outside the repo: `pgproto/` (not committed). It imports Claviger through a `replace` to a scratch copy of this worktree, so no repo file changed.

- Database: `github.com/fergusstrange/embedded-postgres` downloaded real Postgres 16.9 binaries. No Docker daemon was running (`docker info` could not reach the socket), and no Postgres was installed. Port 55432, because Paperclip already runs its own Postgres on 54329.
- Driver: `github.com/jackc/pgx/v5/stdlib` (pure Go, `CGO_ENABLED=0`). Plugged in through the existing `DBConnect` hook, with the connection wrapped by `dbx.NewFromDB(sqlDB, "pgx")`. That name selects dbx's `PgsqlBuilder` (placeholders `$1`, upsert, `ALTER COLUMN ... TYPE`, double-quote quoting).
- A small rewriting `database/sql` driver in front of pgx. It rewrote SQLite-isms in flight, one regex per failure. Every rule is a failure the app really hit. This is a measuring tool, not a design (see "Where it plugs in").
- Two compat views, `sqlite_schema` and `sqlite_master`, built over `pg_tables`, `pg_views` and `pg_indexes`.
- Two Postgres databases: one for `data.db`, one for `auxiliary.db`.
- An exercise script that drives the public `core.App` API after bootstrap.

## Prototype result: how far bootstrap got

Bootstrap, all 17 system migrations, `ReloadCachedCollections` and `ReloadSettings` completed on Postgres. `_collections` held 14 rows and `_migrations` held 17.

The first failure with no rewrites at all was `relation "_collections" does not exist`. That is only the missing `migrations` import in my harness, not a Postgres issue. Once imported, the first real failure was in the very first migration. These are the failures in the order the app hit them. Each was fixed before the next showed.

| # | First failure | Cause | Fix used in the prototype | Real fix |
|---|---|---|---|---|
| 1 | `zero-length delimited identifier` | `DEFAULT "base"`, `DEFAULT "[]"`: SQLite accepts double-quoted string literals | regex to single quotes | edit migration SQL |
| 2 | `function randomblob(integer) does not exist` | id default `'r'\|\|lower(hex(randomblob(7)))` | `substr(md5(random()::text),1,14)` | dialect `RandomIDDefault()` |
| 3 | `function strftime(unknown) does not exist` | `created`/`updated` defaults | `to_char(now() at time zone 'utc', ...)` | dialect `NowDefault()` |
| 4 | `function strftime(unknown, text)` | expression index `idx_logs_created_hour` | `substr(created,1,13)\|\|':00:00'` | dialect (aux DB, stays SQLite) |
| 5 | `1794115167712401 is greater than maximum value for int4` | `_migrations.applied INTEGER`: SQLite INTEGER is 64 bit, Postgres is 32 bit | `BIGINT` | dialect `Int64Type()` |
| 6 | `relation "sqlite_schema" does not exist`, then `current transaction is aborted` | `HasTable` probe. In Postgres a failed statement poisons the whole transaction | compat view | dialect introspection |
| 7 | `syntax error at or near "`"` | index DDL built with backtick quoting in `tools/dbutils/index.go` and `collection_model.go` | backtick to `"` | dialect `QuoteIdent` |
| 8 | `syntax error at or near "PRAGMA"` | `PRAGMA optimize` after table sync | `SELECT 1` | dialect `AfterSchemaChange()` (no-op) |
| 9 | `column "rowid" does not exist` | `ORDER BY rowid` in `collection_query.go`, `_rowid_` in sort and count | map to `created` (wrong, a stub) | hard, see below |
| 10 | `failed to build sqlite_master index` | `ParseIndex` regex cannot read Postgres `indexdef` (`public.`, `USING btree`) | trimmed in the view | stop parsing the live DB |
| 11 | `cannot drop index users_pkey` | `pg_indexes` lists constraint indexes, `sqlite_master` does not | excluded in the view | dialect introspection |

After rule 11, bootstrap returned `nil`.

### What worked on Postgres after bootstrap

- Find the `users` collection.
- Create a base collection with text, number, bool, multi-select, JSON, date and autodate fields (real `CREATE TABLE`).
- Insert, update and delete records, and read them back with correct Go types (`views=0 pub=true tags=[a] meta={"k":0}`).
- `FindFirstRecordByData` and `CountRecords`.
- Add a field and rename a field (`ALTER TABLE ... ADD COLUMN`, `RENAME COLUMN` through dbx).
- Delete a collection.
- Create a superuser. The audit log wrote 5 rows. `CreateSession` (fork) worked.
- `Vacuum`.

### Filters: 5 of 9 worked unchanged

| Filter | Result |
|---|---|
| `title = 'hello'` | worked, 1 row |
| `views > 5` | worked, 2 rows |
| `created > '2020-01-01'` | worked, 3 rows (dates are TEXT, so string compare is fine) |
| `title = 'hello' && views >= 0` | worked |
| `title ~ 'WOR'` | ran, but **returned 0 rows**. SQLite `LIKE` ignores ASCII case, Postgres does not. A silent behaviour change |
| `pub = true` | `operator does not exist: boolean = integer`. The resolver emits the literal `1` |
| `tags ?= 'b'` | `operator does not exist: json = unknown` |
| `tags:length > 1` | `function json_valid(json) does not exist` (plus `iif`, `json_type`, `json_array`) |
| `meta.k = 1` | `syntax error at or near "$1"`: `JSON_EXTRACT(...) IS $1` is not valid Postgres |

### Other failures after bootstrap

| Call | Failure |
|---|---|
| `TableInfo`, `TableColumns` | `function pragma_table_info(unknown) does not exist` |
| `CreateBackup` | `syntax error at or near "INTO"` (`VACUUM INTO`) |
| Create a view collection | its column-discovery query uses `pragma_table_info` |
| Select field, multi to single | `json = unknown` inside the `UPDATE` that uses `json_valid`/`json_extract` |
| Auth collection with `username COLLATE NOCASE` index | `collation "nocase" ... does not exist` |
| Hand-written SQL `FROM _auditLog` | `relation "_auditlog" does not exist` (see "Identifier case") |

Not measured: performance, `NUMERIC` formatting in API JSON, behaviour under concurrent writers, realtime, the admin UI, the OAuth/OTP/MFA flows, the import/export flows, the batch API, the real test suite. Say "M1 works" only for what is listed above.

## Inventory of SQLite assumptions

Counts come from `git grep` on this worktree (commit `bd084a73`), Go files only, `ui/` excluded. "src" is non-test. Hits in comments are included, so counts are upper bounds. They are a map, not a measurement of effort.

| Assumption | src lines / files | test lines / files | Where | Class |
|---|---|---|---|---|
| `json_each` | 3 / 2 | 26 / 3 | `record_field_resolver_runner.go`, `tools/dbutils/json.go` | dialect |
| `json_extract` / `JSON_EXTRACT` | 3 / 3 | 6 / 2 | `dbutils/json.go`, `collection_record_table_sync.go` | dialect |
| `json_array_length`, `json_valid`, `json_type`, `json_object`, `iif` | about 15 / 3 | about 90 / 3 | `dbutils/json.go`, `collection_record_table_sync.go` | dialect |
| `strftime` | 21 / 4 | 19 / 2 | filter function in `tools/search/token_functions.go`, column defaults in 2 migrations, `log_query.go` | filter fn: hard. defaults: mechanical |
| `randomblob`, `hex(` | 4 / 3 | 1 / 1 | `field_text.go`, 2 migrations | mechanical |
| `COLLATE NOCASE` | 6 / 5 | 1 / 1 | auth lookups in `apis/`, `record_query.go`, `field_text.go`, v0.23 migration | dialect |
| `rowid`, `_rowid_`, `@rowid` | 13 / 6 | 6 / 3 | `collection_query.go`, `apis/record_crud.go`, `tools/search/sort.go`, `provider.go`, `field.go` | **hard** |
| `PRAGMA` (executed) | about 11 / 4 | 0 | `wal_checkpoint` x3 (base.go, backup_create.go), `optimize` x2, `table_info` x2, `integrity_check`, `foreign_key_check` | dialect |
| `sqlite_master` / `sqlite_schema` | 8 / 7 | 2 / 2 | `db_table.go`, `collection_record_table_sync.go`, `collection_validate.go`, `normalize_indexes`, `backup_verify.go` | dialect |
| `LIKE ... ESCAPE '\'` | 4 / 1 | 14 / 3 | `tools/search/filter.go` | dialect (needs `ILIKE`) |
| `ON CONFLICT ... excluded.` | 1 / 1 | 0 | `backup_verify.go` (fork) | mechanical (valid on both) |
| `VACUUM`, `VACUUM INTO` | 13 / 5 (3 executed) | 4 / 1 | `db_table.go`, `backup_create.go` | dialect |
| `CAST(... AS TEXT)`, `substr(applied\|\|'0000...', 0, 17)` | 2 / 2 | 5 / 1 | `collection_query.go`, `migrations_runner.go` | mechanical |
| DDL strings (`CREATE TABLE/INDEX/VIEW`, `DROP TABLE`, `DEFAULT (`) | about 40 / 8 | about 50 / 6 | `field_*.go` `ColumnType()`, `collection_model.go`, `view.go`, 5 migrations | dialect |
| Backtick identifier quoting in raw SQL | about 10 / 4 | many | `dbutils/index.go`, `collection_model.go`, v0.23 migration | mechanical (dbx `[[ ]]` already emits the right quotes per builder) |
| Raw `NewQuery(` | 43 / 19 files | 18 | all over `core/` and `apis/` | audit |
| `dbx.NewExp(` | 53 / 24 files | 7 | all over | audit |
| `RunInTransaction` | 48 / 24 files | 50 | all over | audit for locking |
| Direct `modernc.org/sqlite` | 4 / 2 | 1 / 1 | `db_connect.go`, `modernc_versions_check.go` | easy (build tag) |

Union: 38 non-test Go files contain at least one SQLite-specific token. About 30 are upstream-owned. The fork-owned ones are `backup_verify.go`, `telemetry.go`, `tools/telemetry/db.go`, `modernc_versions_check.go` and the `normalize_indexes` migration.

Size for scale: 71,254 non-test Go lines and 79,123 test lines, in 219 test files.

### Detail by area

**Upstream `dbx` has a Postgres builder.** `ganigeorgiev.com/pocketbase/dbx@v1.13.0` ships `builder_pgsql.go`, `builder_mysql.go`, `builder_mssql.go` and `builder_oci.go`. `BuilderFuncMap` maps driver name `"pgx"` and `"postgres"` to `NewPgsqlBuilder`. It gives `$n` placeholders, `ON CONFLICT (...) DO UPDATE` upsert, and `ALTER COLUMN ... TYPE`. It does not give us JSON, date, `LIKE`, `RETURNING` or introspection dialect. Mixed-case identifiers are fine because dbx quotes `[[name]]` and `{{table}}`. Its MySQL and SQL Server builders exist too, so later dialects start from the same hook. Reading its source, `Upsert` without constraint columns emits `ON CONFLICT DO UPDATE`, which Postgres rejects. Not run. The one upsert we have (`backup_verify.go`) uses raw SQL with an explicit key.

**DDL.** Column types come from each field's `ColumnType()`:

| Field | SQLite type today | Postgres |
|---|---|---|
| text, email, url, editor, password, date, autodate | `TEXT DEFAULT '' NOT NULL` | same (dates stay TEXT, which keeps string compares valid) |
| number | `NUMERIC DEFAULT 0 NOT NULL` | `NUMERIC` works; JSON output formatting not checked |
| bool | `BOOLEAN DEFAULT FALSE NOT NULL` | same |
| JSON | `JSON DEFAULT NULL` | `JSONB` recommended |
| multi select, relation, file | `JSON DEFAULT '[]' NOT NULL` | `JSONB DEFAULT '[]'` |
| id | `TEXT PRIMARY KEY DEFAULT ('r'\|\|lower(hex(randomblob(7))))` | `TEXT PRIMARY KEY DEFAULT ('r'\|\|substr(replace(gen_random_uuid()::text,'-',''),1,14))` |

The single-to-multi field conversion in `collection_record_table_sync.go` (rename column, add column, `UPDATE ... json_valid ... json_type ... json_array`, drop column, drop and restore views) needs a Postgres twin. Postgres can do `ALTER COLUMN TYPE ... USING`, which is simpler.

The `indexes` field on a collection stores `CREATE INDEX` strings in SQLite syntax and `tools/dbutils/index.go` parses them with a regex. That is part of the public API shape. Keep it as the portable form and render it per dialect at DDL time. Do not parse the live Postgres catalog (prototype failures 10 and 11).

**Filter translation.** `core/record_field_resolver*.go` (about 1,500 lines) and `tools/search/` (about 1,700 lines) build SQL fragments directly. The JSON handling is in `tools/dbutils/json.go` (51 lines): three functions, `JSONEach`, `JSONArrayLength`, `JSONExtract`. That file is the cleanest seam in the whole code base. The remaining work is:

- boolean and null literals (`1` for `true`, `IS`/`IS NOT` with a bound parameter, which Postgres rejects; use `IS NOT DISTINCT FROM`);
- `LIKE` case behaviour;
- `strftime(...)` as a user-visible filter function with SQLite format codes;
- `@rowid` sort key;
- the `@request.*`/back-relation subqueries that use `json_each` joins (`record_field_resolver_runner.go:343`, `:362`).

**Two connections, locking, transactions.** Each database opens two pools: concurrent (reads) and nonconcurrent (`SetMaxOpenConns(1)`, writes). Its only purpose is to avoid `SQLITE_BUSY`. On Postgres a one-connection write pool is a needless bottleneck. `BaseApp.DB()` already returns a single builder when `concurrentDB == nonconcurrentDB` (this is how transactions work), so a dialect flag `SingleWriter()` that makes `initDataDB` use one pool is a few lines.

Three things need an audit, none of which the prototype tested:

1. Postgres aborts the whole transaction on any failed statement. SQLite does not. Code that probes and ignores an error inside `RunInTransaction` breaks. We saw this twice (failure 6). Fix with savepoints in one helper.
2. Default isolation is READ COMMITTED. SQLite serializes writers. Read-then-write sequences need a look. The fork's own sites are already atomic single statements (`UPDATE ... WHERE lastStep < step`, `lastUsed`, `lastSeen`, webhook streak counters). Nothing in the code base uses `FOR UPDATE`.
3. Migrations must take `pg_advisory_lock` so two replicas starting together do not both run them.

`core/collection_model.go:701` documents a SQLite-only workaround ("select is outside the transaction to prevent SQLITE_LOCKED"). It is harmless on Postgres.

**`auxiliary.db`.** The `_logs` table, one SQLite-only expression index, hot insert path, `apis/logs.go` VACUUM, `log_query.go` `strftime` grouping. See the design for why it stays SQLite.

**Backups, restore, verify.** All of these assume a single SQLite file set in `pb_data`:

- `CreateBackup` (`core/backup_create.go`): `PRAGMA wal_checkpoint(TRUNCATE)`, `VACUUM INTO` for both databases, zip of `pb_data`.
- `backup_restore.go`: replaces `pb_data` from the zip and restarts.
- `backup_verify.go` (fork, 784 lines): opens the restored SQLite copies, runs `PRAGMA integrity_check` and `foreign_key_check`, compares per-table row counts.
- Litestream and the `entrypoint.sh` handover exist only because SQLite is single-writer on NFS.

Class: hard for the DB half, and easy to disable. For a Postgres data DB the zip backup would contain files only.

**Fork SQL.** Audit (`audit_model.go`), sessions (`session.go`, `apis/sessions.go`), bans, API keys, TOTP, webhooks and their delivery loop use `UPDATE ... WHERE`, `dbx.NewExp("[[created]] < {:date}")` and the collection API. They compare TEXT dates against Go-formatted strings, so they are dialect-neutral. The prototype ran audit and sessions on Postgres unmodified. Class: mechanical, with only `[[revoked]] = TRUE` and `COALESCE(..., '')` worth a second look. Not tested: bans, API keys, TOTP, webhooks.

**Identifier case.** Postgres folds unquoted identifiers to lower case. App code is safe because dbx quotes. Hand-written SQL is not: view collections store a user-written `SELECT`, `apis/sql.go` runs superuser SQL, and `$app.db()` is exposed to JS hooks. A view such as `select id, createdAt from posts` fails on Postgres. This is a documentation limit, not a fix. Class: hard (document).

**Test harness.** `tests/app.go` clones `tests/data` (608 KB: a prebuilt `data.db`, `auxiliary.db` and `storage/`) into a temp dir for every test. There are 353 `NewTestApp` calls across 76 files, plus `ApiScenario`. Those fixtures are SQLite files. For Postgres we need a fixture loader (a seeded template database cloned per test with `CREATE DATABASE ... TEMPLATE`), and the fixtures regenerated from a dialect-neutral source. Class: hard, and the largest single piece of M6.

## Design

### The `Dialect` interface

Small, behind one registry keyed by `dbx.DB.DriverName()`, so each database carries its own dialect. The data DB and the aux DB can differ.

```go
type Dialect interface {
    Name() string                       // "sqlite", "postgres"
    SingleWriter() bool                 // true: two pools (read + 1 write)
    QuoteIdent(string) string

    // DDL
    ColumnType(kind ColKind) string     // text, int64, float, bool, json, jsonArray
    IDDefault() string                  // random 15 char id default
    NowDefault() string                 // 'YYYY-MM-DD HH:MM:SS.mmmZ' default
    BuildIndex(portable dbutils.Index) string
    AlterColumnType(table, col, kind string) string

    // Query fragments (used by the filter resolver and dbutils/json.go)
    JSONEach(col string) string
    JSONArrayLength(col string) string
    JSONExtract(col, path string) string
    JSONIsArray(col string) string
    BoolLiteral(bool) string
    IsNotDistinct(left, right string) string
    LikeExpr(left, right string, caseInsensitive bool) string
    EqualsNoCase(col, param string) string
    StrFTime(args ...string) (sql string, err error)
    RowOrder() string                   // stable insertion order column

    // Introspection
    HasTable(db dbx.Builder, name string) bool
    TableColumns(db dbx.Builder, table string) ([]string, error)
    TableInfo(db dbx.Builder, table string) ([]*TableInfoRow, error)
    TableIndexes(db dbx.Builder, table string) (map[string]string, error)

    // Maintenance
    AfterSchemaChange(db dbx.Builder)   // PRAGMA optimize / ANALYZE
    Vacuum(db dbx.Builder) error
    Checkpoint(db dbx.Builder)          // wal_checkpoint, no-op elsewhere
    SupportsFileBackup() bool           // VACUUM INTO + zip + verify

    // Errors
    IsUniqueViolation(error) bool
    IsRetryable(error) bool
}
```

`SQLiteDialect` is the current behaviour moved behind the interface, byte for byte. The SQLite tests must stay green at every step.

### Where it plugs in, with the fewest upstream edits

The prototype's regex shim proved that a lot of the surface is small. It is still the wrong design for production: it cannot see SQL that the filter resolver builds from user rules, it would mangle string literals, and it hides bugs. So:

1. **New fork-owned files** hold everything new: `core/dialect.go`, `core/dialect_sqlite.go`, `core/dialect_postgres.go`, `core/db_connect_postgres.go` (pgx), and `tools/dbutils/dialect.go`. Merge risk: zero.
2. **Small edits in upstream-owned files**, each a one-line swap of a literal for a dialect call. About 30 files. The edits at the generation sites are the real cost:
   - `core/db_table.go` and `collection_record_table_sync.go`: introspection and field conversion. These are the biggest edits (~120 lines).
   - `tools/dbutils/json.go`, `tools/search/filter.go`, `sort.go`, `token_functions.go`, `core/record_field_resolver_runner.go`: filter fragments.
   - `core/field_*.go`: `ColumnType()` and `IDDefault()`.
   - `core/base.go`: `initDataDB` (one pool when not `SingleWriter`), `initAuxDB`, periodic `PRAGMA` cron.
   - `core/backup_*.go`: guard with `SupportsFileBackup()`.
3. **Dialect lookup is by connection.** `core.DialectOf(db dbx.Builder)` reads `DriverName()` through the dbx `DB`. Transaction-scoped builders keep it. This makes a mixed setup (data on Postgres, aux on SQLite) work with no global state. It needs a second config hook, `AuxDBConnect`, because today one `DBConnect(path)` serves both databases.
4. **Migrations get two tiers.**
   - Tier A, the 9 migrations that use the `Collection`/`Save` API. They ran unchanged on Postgres in the prototype, so they are already portable.
   - Tier B, the raw-SQL ones. `1640988000_init.go` and `1640988000_aux_init.go` build the system tables. They get rewritten to use `Dialect` helpers. The v0.23 migrations (`1717233556` to `1717233559`) and `1778828400_normalize_indexes` only upgrade databases created before v0.23. No Postgres database can be in that state, so on Postgres they are recorded as applied and skipped. That removes about 1,100 lines of upstream migration code (the four v0.23 files and `normalize_indexes`) from the problem.
   - Policy for new upstream migrations: add a CI check that fails when a new file under `migrations/` contains raw DDL or SQLite functions without a dialect call. A human reviews at each sync.
5. **No translation of user SQL.** `apis/sql.go`, view collections and `$app.db()` stay as written. Document that on Postgres they are Postgres SQL, with unquoted camelCase folded to lower case.

### Filters on Postgres

- Store JSON-typed columns as `JSONB`. `json_each(x)` becomes `jsonb_array_elements_text(x)`, `json_array_length` becomes `jsonb_array_length`, `JSON_EXTRACT(x,'$.a.b')` becomes `x #>> '{a,b}'`, and `json_valid(x) AND json_type(x)='array'` becomes `jsonb_typeof(x)='array'`.
- Single-valued select/relation/file fields are `TEXT`. The `CASE WHEN json_valid(...)` branches in `dbutils/json.go` exist because a column can hold either. On Postgres a column is one type or the other (known at DDL time), so the generated SQL gets simpler and faster. Value columns changed between single and multiple use `ALTER COLUMN ... TYPE ... USING`.
- `~` and `!~` use `ILIKE`. The prototype showed `LIKE` silently returning 0 rows for `title ~ 'WOR'`.
- `true`/`false` literals use `TRUE`/`FALSE`. `?= x` operators compare after JSON unwrap.
- `= NULL` and `IS $1`: `IS NOT DISTINCT FROM`.
- Case-insensitive unique indexes (`username COLLATE NOCASE`): render as `CREATE UNIQUE INDEX ... (lower(username))` and query with `lower(col)=lower($1)`. This is a visible dialect difference in the stored `indexes` string, so keep `COLLATE NOCASE` as the portable spelling and translate it.
- `strftime(...)` as a filter function: translate the SQLite format codes (`%Y %m %d %H %M %S %f`, plus the modifiers) to `to_char`/`date_trunc`/interval. Cover the documented subset and reject the rest with a clear error. Dates are TEXT, so every call needs a cast to `timestamptz`.
- `_rowid_` / `@rowid`: add `_rowid BIGINT GENERATED ALWAYS AS IDENTITY` to every collection and system table. Export code must skip it. Views have no row id (same as SQLite). This is the only schema change visible to users who query Postgres directly. Without it, default list order and `CountCol("_rowid_")` have no stable insertion order.

### Migration of existing data (SQLite to Postgres)

A one-way offline command `claviger migrate-db --from <pb_data> --to <dsn>`:

1. Stop the app (single writer).
2. Create the schema on Postgres through the dialect, from the collections JSON.
3. For each table, read in `rowid` order from SQLite, convert values (`0/1` to bool, JSON text to `jsonb`, `NUMERIC`), and write in batches with pgx `CopyFrom` (needs the native pgx connection, not `database/sql`).
4. Reset identity sequences.
5. Verify: row counts per table (reuse the idea of `backup_verify.go`), and a hash of `id` plus one text column per table. Refuse to finish on a mismatch.
6. Files in `pb_data/storage` do not move. If the deployment uses local disk, switch to S3 before multi-replica.

### Backups

| Mode | Behaviour |
|---|---|
| SQLite data DB | unchanged (zip, verify, restore, Litestream) |
| Postgres data DB | the zip backup contains `storage/` files only, or is disabled; verify and restore are disabled for the DB part; document `pg_dump` and provider point-in-time recovery as the way to back up the DB; the admin UI shows a notice |

`backup_create.go` already ignores `wal_checkpoint` errors ("some drivers may not support the wal_checkpoint pragma"). `VACUUM INTO` fails (prototype). Guard on `SupportsFileBackup()`.

### The aux DB (logs)

Keep it on SQLite, on local disk, per replica. Reasons:

- it is disposable and write heavy;
- OTLP export already ships the logs somewhere durable;
- local SQLite means no NFS, so the `entrypoint.sh` single-writer handover (and the `auxiliary.db` corruption it was built for, #35) stops mattering when the data DB is on Postgres. The container can write `auxiliary.db` to local ephemeral disk.

Cost: logs are per replica and lost on replacement. If the owner wants central logs without OTLP, a Postgres `_logs` table is a later add (the one expression index becomes `substr(created,1,13)`).

### Realtime and multi-replica (later milestone, M7)

Postgres removes the database single-writer limit but not the others. Four in-process things still assume one replica:

| In-process state | Where | Needed for N replicas |
|---|---|---|
| Realtime subscriptions broker | `tools/subscriptions` | `LISTEN/NOTIFY` fan-out. Payload limit is 8,000 bytes, so notify with collection and record id and re-fetch. Needs a dedicated non-pooled pgx connection (not the `database/sql` pool). Optional: an outbox table |
| Cron jobs | `tools/cron` | `pg_try_advisory_lock` leader election |
| Rate limiter | `apis/middlewares_rate_limit.go` (map in memory) | accept per replica, or a shared counter table |
| Collection and settings cache | `ReloadCachedCollections`, `ReloadSettings` | `NOTIFY` on change, reload on receipt |
| Local file storage | `pb_data/storage` | S3 (already supported) |

The owner asked for Postgres first. Run single replica on Postgres through M1 to M6. Do not promise HA until M7.

### CI

K3s runners can run `services: postgres:<pinned>` through Docker-in-Docker. Plan:

- Add a job matrix: SQLite (today) and Postgres, same `go test ./...`.
- Tests choose the dialect with an env var, e.g. `CLAVIGER_TEST_DSN`. A small helper in `tests/` builds a template database once (seeded from the same fixtures) and clones it per `NewTestApp`.
- Mark known SQLite-only tests with a skip helper, with a reason, so the skip count is visible. A skipped test is not a pass: record the counts in the CI summary.
- Postgres version pinned (16.x). Image digest pinned.

## Effort and merge risk

Estimates are focused engineering days for one person who knows this code, with review. They carry wide error bars because M2 and M6 are mostly unknown until the test suite is run against Postgres.

| Milestone | Work | Days |
|---|---|---|
| M1 Dialect seam and Postgres bootstrap | `Dialect` interface, `SQLiteDialect` extraction with all current tests green, Postgres dialect for DDL, introspection, id and now defaults, Tier A/B migrations, `SingleWriter` pool, `AuxDBConnect`, `_rowid` column, savepoint helper | 8 to 12 |
| M2 Records CRUD and filters | JSON filter fragments, bool and null literals, `ILIKE`, `strftime` subset, `COLLATE NOCASE` lowering, sort, expand, back-relations, field conversion single/multi, view collections | 10 to 15 |
| M3 Migrations, import, export | collection import/export, `normalize`, index render, schema-diff tests, `TableInfo` | 5 to 8 |
| M4 Fork features on Postgres | audit, sessions and bans, API keys, TOTP, webhooks, passkeys, telemetry driver wrapper (`tools/telemetry/db.go` wraps a SQLite driver today) | 4 to 6 |
| M5 `migrate-db` tool | described above, with verify | 5 to 8 |
| M6 CI matrix and test fixtures | template DB loader, skip list, runner service, flake handling | 5 to 10 |
| Postgres total | | 37 to 59 |
| M7 Multi-replica | LISTEN/NOTIFY realtime, advisory-lock cron and migrations, cache invalidation | 8 to 12 |
| Later: CockroachDB | pgx-compatible. No `LISTEN/NOTIFY` (use changefeeds), no advisory locks, differences in `IDENTITY` and JSON operators | 6 to 10 |
| Later: MySQL / MariaDB | no partial indexes, JSON functions differ, no `RETURNING`, `DEFAULT` expressions limited, `TEXT` index lengths | 25 to 35 |
| Later: SQL Server / Azure SQL | no boolean, no `LIMIT`, `OFFSET ... FETCH`, `nvarchar`, JSON only via `OPENJSON`/`JSON_VALUE`, bracket quoting | 30 to 45 |

The later dialects add real cost because each one has its own JSON story. The dbx builders already exist for all of them, but the filter and DDL work above has to be redone per dialect.

### Upstream-owned files touched

About 30 files for the Postgres seam (listed under "Inventory"). Largest edits: `core/collection_record_table_sync.go`, `core/db_table.go`, `core/record_field_resolver_runner.go`, `tools/dbutils/json.go`, `tools/search/filter.go`, `core/base.go`, `migrations/1640988000_init.go`. Each other file is one to five lines.

### Cost of every future upstream merge

| Change upstream makes | Cost to us |
|---|---|
| Nothing near SQL (most releases) | none |
| Adds a raw SQL fragment in `core/` or `apis/` | half a day: find it, move it behind the dialect, add a Postgres test |
| Adds a system migration with DDL | half a day plus the CI check |
| Changes the filter resolver or `dbutils` | 1 to 2 days (conflict plus Postgres re-verify) |
| Changes `ColumnType()` of a field | few hours |

Rough budget: 0.5 to 1.5 days per sync for as long as we carry Postgres, more when upstream touches the resolver. The owner's own process already reviews each upstream change one by one (see the fork-sync memory), and upstream now publishes on its own server and not on GitHub, so syncs are rare and deliberate. That makes this cost acceptable.

### Is it a hard fork of the storage layer?

Not yet, and not if the seam stays thin. It becomes one if we start translating SQL strings in a shim, or if the dialect surface grows to cover every raw statement. Two warning signs to watch at the M1 gate: more than about 40 upstream-owned files edited, or any `core/` edit that cannot be a one-line dialect call.

The honest unknowns that could change the estimate: the exact Postgres behaviour of `RETURNING`-less `Save` paths under READ COMMITTED, the `NUMERIC` output format in API JSON, and the long tail of tests that assume SQLite error strings.

## Open questions for the owner

| # | Question | Options | Recommendation |
|---|---|---|---|
| 1 | Commit to the whole plan, or only M1? | A: M1 now, decide after. B: all of M1 to M6 | A. M1 produces the real seam size and the first Postgres test count. Gate on it |
| 2 | Where does `auxiliary.db` go on Postgres? | A: local SQLite. B: Postgres schema | A. Disposable, OTLP already exports. Also removes the NFS handover for the data path |
| 3 | JSON column type | A: `JSONB`. B: `JSON`. C: `TEXT` plus casts | A. Gives `jsonb_array_elements_text` and the `#>>` operator |
| 4 | Insertion order | A: hidden `_rowid` identity column on every table. B: order by `created, id` | A. B breaks when two rows share a `created` millisecond and for records without an autodate field |
| 5 | Case-insensitive unique indexes | A: translate to `lower(col)`. B: require the `citext` extension | A. No extension, works on managed Postgres. Slight change in stored index text |
| 6 | Minimum Postgres version | 14, 15, 16 | 16. It is what the prototype ran, and it has `pg_input_is_valid`, which is the closest match for `json_valid` in the single/multi conversion. Check what the managed services you target offer before fixing this |
| 7 | Is multi-replica a goal? | A: yes, M7. B: no | A, as a separate milestone after M6. Postgres alone does not give HA (see the table above) |
| 8 | Backup UX on Postgres | A: disable DB backups in the UI, doc `pg_dump`/PITR. B: shell out to `pg_dump` from `CreateBackup` | A. `pg_dump` needs a binary in the image and credentials; managed providers already do PITR |
| 9 | User SQL (views, `/api/sql`, `$app.db()`) | A: leave as is, document. B: translate | A. Translation is the hard-fork path |
| 10 | Test policy | A: whole suite on both dialects, no permanent skips. B: allow a skip list | B with a visible, shrinking list. A needs M2 to M4 done first |
| 11 | Binary size and dependencies | pgx v5 pulls in `pgpassfile`, `pgservicefile` and `puddle` | Size not measured. Measure at M1. If the growth matters, compile pgx behind a `postgres` build tag |
| 12 | Use of `embedded-postgres` | It downloads real Postgres binaries at run time | Keep it out of the repo. Use it only for local spikes. CI uses the `postgres:16` service |

## Reproduction notes

The prototype lives in the session scratchpad (`pgproto/`, `claviger-copy/`) and is not committed. To redo it: copy the worktree, `go get github.com/jackc/pgx/v5 github.com/fergusstrange/embedded-postgres`, open each database with `dbx.NewFromDB(sqlDB, "pgx")`, and add rewrite rules one failure at a time. Bootstrap needs `_ "github.com/doodlemania2/claviger/migrations"` imported in the harness, otherwise no system migrations are registered and the first error is a misleading `relation "_collections" does not exist`.
