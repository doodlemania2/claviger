# Datastores

Claviger stores its data in SQLite by default, exactly like PocketBase. Nothing needs to be configured for that.

## PostgreSQL (experimental)

Status: **milestone 1 of 6** (issue #106). The storage layer now sits behind a dialect interface (`tools/dialect`) and the app starts on PostgreSQL. Records, filters, backups and the fork features are not ported yet, so do not run anything real on it. The list below says exactly what is and is not there.

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

### What works in M1

| Area | State |
| --- | --- |
| Startup | Bootstrap, all system migrations (including the audit log, TOTP, API keys, webhooks, sessions and bans, passkeys), settings load and save. |
| Collections | Create, update (add, rename and drop fields, rename the collection, change a select, relation or file field between single and multiple) and delete, with their indexes. |
| Introspection | `HasTable`, `TableColumns`, `TableInfo` and `TableIndexes` work and hide the internal row id column. |
| Migrations | Serialized with a Postgres advisory lock, so two instances starting together cannot both run them. The pre-v0.23 upgrade migrations are skipped (a Postgres database cannot be in that state). |
| Basic records | Saving and loading records, find by id or by a field, and the default insertion order. |

### What does not work yet

| Area | Milestone |
| --- | --- |
| Record filters and sorting by `@rowid`, `~` case-insensitive match, `true`/`false` and `null` comparisons, JSON field filters (`?=`, `:length`, `a.b`), `strftime()`, expand and back relations | M2 |
| View collections (`CREATE VIEW ... FROM (...)` needs an alias on Postgres) | M2 |
| Collection import and export, zip backups and restore (see below) | M3 |
| Passkey, audit, session, API key, TOTP and webhook behavior under test (the tables exist, the flows are not verified) | M4 |
| `claviger migrate-db` (copy an existing SQLite install to Postgres) | M5 |
| Running the whole existing test suite against Postgres in CI | M6 |
| More than one replica (realtime events, cron and rate limits are still per process) | M7 |

Until M6 the Postgres CI job runs the integration tests in `tests/pgtests` only. A skipped test is not a pass: the job fails if one of them skips.

### How data is stored

- **Dates stay `TEXT`** in the same fixed format PocketBase always used (`2006-01-02 15:04:05.000Z`, UTC). The filter language and sorting compare them as strings, and it keeps a later SQLite to Postgres copy a plain value copy. They are not `timestamptz`.
- **Numbers are `DOUBLE PRECISION`**, because PocketBase numbers are 64 bit floats end to end. `NUMERIC` would come back from the driver as a string and bring exact decimal arithmetic that SQLite never had.
- **JSON is `JSONB`.** The key order and whitespace of a stored JSON value are not preserved (Postgres normalizes them).
- **Booleans are `BOOLEAN`, ids are `TEXT`** with the same 15 character random default as SQLite.
- **Insertion order** comes from a hidden `_rowid_` identity column on every table Claviger creates (named like the SQLite alias, and reserved: no field can use that name). It is the only schema difference you will see if you query the database yourself, and it is never part of the API.
- **Case-insensitive indexes** (`COLLATE NOCASE`, used for example by the auth collections' username index) are created as `lower(column)` expression indexes. No extension (`citext`) is needed, so it works on managed Postgres.
- Reads and writes share one connection pool (SQLite keeps its separate single-writer pool).

### Backups

Zip backups (`pb_data` plus a database copy) are a SQLite feature. On Postgres, back the database up with `pg_dump` or your provider's point-in-time recovery. Backup creation, the restore verifier and the Litestream and `entrypoint.sh` handover machinery are SQLite concerns and are not changed in M1; the zip backup of a Postgres install will contain files only once M3 lands.

### Your own SQL

SQL you write yourself runs **as written**: view collections, the superuser `/api/sql` endpoint and `$app.db()` in JS hooks. On Postgres that is Postgres SQL:

- Unquoted identifiers are folded to lower case, so `select createdAt from posts` fails; write `"createdAt"`.
- SQLite-only functions (`json_extract`, `strftime`, `randomblob`, ...) are not available.

Claviger does not translate user SQL; translating it would turn this into a hard fork of the storage layer.

### Running the Postgres tests

```sh
# against a throwaway local Postgres 16 (downloaded on first use, no Docker needed):
cd scripts/pgembedded && go run .

# or against a server you already have:
CLAVIGER_TEST_PG_URL='postgres://user:pass@localhost:5432/postgres?sslmode=disable' \
  go test -tags postgres -count=1 -v ./tests/pgtests/...
```

Each test creates and drops its own database, so the URL can point at any database on the server (the role needs `CREATEDB`).

### For developers

The seam is the `Dialect` interface in `tools/dialect`. Code that has to differ per engine asks `core.DialectOf(app)` (or `dialect.Of(db)` when it holds a builder; a transaction keeps the dialect of its connection). SQLite is the previous behavior moved behind the interface and its SQL text is unchanged. New raw SQL in `core/`, `apis/` or a migration must either work on both engines or go through the dialect. `FORK_SYNC.md` lists the upstream-owned files that call into it.
