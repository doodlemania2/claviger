# Datastores

Claviger stores its data in SQLite by default, exactly like PocketBase. Nothing needs to be configured for that.

## PostgreSQL

Status: **supported, single replica** (issue #106, milestones 1 to 6). The storage layer sits behind a dialect interface (`tools/dialect`), the app runs on PostgreSQL 16 and 17, the records API works (filters, sorting, expand, view collections), collections import and export and the migration commands work, every fork feature runs on it, and `claviger migrate-db` copies an existing SQLite install over ([Moving from SQLite to PostgreSQL](#moving-from-sqlite-to-postgresql)). CI runs the Postgres suites on both versions. What is not there is more than one replica (M7, later) and zip backups of the database (see [Backups](#backups)). SQLite stays the default and its behavior is unchanged. The lists below say exactly what is and is not there.

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
| Rate limits | The counters live in the local SQLite `auxiliary.db` (not the data database), so they are unchanged. They are per replica, like the logs. |
| Field encryption | Encrypted text and JSON fields, `_fieldKeys`, key rotation (with its compare-and-swap row updates and resume), rewrap, convert and strict mode, and `claviger keys check` / `status` / `rotate` / `rewrap` / `convert` / `strict` / `prune`. |
| OIDC provider | All `_oauth*` collections, authorization code flow, refresh token rotation and reuse detection, SSO, key rotation and the JWKS. |
| Organizations and roles | The rule cookbook of [orgs.md](orgs.md) (membership and role rules) runs against Postgres. |
| Passkeys | Registration and login. |

The differences from SQLite that you can observe are listed under [Differences from SQLite](#differences-from-sqlite).

### Limits

| Area | State |
| --- | --- |
| More than one replica (realtime events, cron, the notify watcher and rate limits are still per process) | M7, later. Run a single replica. |
| Zip backup and restore of the database | Not planned: use `pg_dump` or point-in-time recovery. |
| Views written in SQLite SQL, raw SQL in your own migrations | Written for the engine you run on, see [Your own SQL](#your-own-sql). `migrate-db` has `--skip-views` for the first. |
| The request logs (`auxiliary.db`) | Stay in a local SQLite file, one per replica. |

A skipped test is not a pass: the `postgres` CI job fails if one of the tests skips, except for the documented known gaps (the `known_gap` tests in `tests/pgtests` and the 16 tests in `tests/postgres_skip.txt`), which are skipped on purpose and printed with their reason.

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

Limits: the copy is a snapshot of the moment the command starts, so stop the server first (`--offline-ok` is your statement that none is running). A very large installation is copied with plain multi-row `INSERT`s: expect on the order of tens of thousands of rows per second, which is minutes for a church sized database. If the data directory has an unmerged write-ahead log (`data.db-wal` with content, which is only left behind by a crash), the command reads through it and creates the `-shm` file next to the database; after a clean shutdown it opens the file immutable and creates nothing.

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
