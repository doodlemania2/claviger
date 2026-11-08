# Datastores

Claviger stores its data in SQLite by default, exactly like PocketBase. Nothing needs to be configured for that.

## PostgreSQL (experimental)

Status: **milestone 2 of 6** (issue #106). The storage layer sits behind a dialect interface (`tools/dialect`), the app starts on PostgreSQL, and the records API works: list, view, create, update, delete and batch, with filters, sorting, expand and view collections. Import and export, backups, the fork features and the migration tool are not ported yet, so do not run anything real on it. The lists below say exactly what is and is not there.

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
| View collections | Create, update, list, filter and expand. Field types are inferred from the real column types of the view. |

The differences from SQLite that you can observe are listed under [Differences from SQLite](#differences-from-sqlite).

### What does not work yet

| Area | Milestone |
| --- | --- |
| Collection import and export, zip backups and restore (see below) | M3 |
| Passkey, audit, session, API key, TOTP and webhook behavior under test (the tables exist, the flows are not verified) | M4 |
| `claviger migrate-db` (copy an existing SQLite install to Postgres) | M5 |
| Running the whole existing test suite against Postgres in CI | M6 |
| More than one replica (realtime events, cron and rate limits are still per process) | M7 |

Until M6 the Postgres CI job runs the integration tests in `tests/pgtests` only. A skipped test is not a pass: the job fails if one of them skips, except for the documented known gaps below, which are skipped on purpose and listed in the job log.

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

Things that look like differences and are not: `~` is case-insensitive for ASCII letters on both (`ILIKE` under the `C` collation does not fold other letters, like SQLite), `!=` matches empty and `NULL` the same way, and records come back in insertion order without a `sort`.

**`strftime()` on PostgreSQL** supports the formats `%d %e %f %F %g %G %H %I %j %k %l %m %M %p %P %R %s %S %T %u %U %V %w %W %Y %%`, and the modifiers `start of day|month|year`, `[+-]N second|minute|hour|day|month|year` (and the plural), `weekday N`, `unixepoch`, `julianday`, `auto`, `utc` and `subsec`. The time value is a date string, a column, `'now'` (the default), or a number (a julian day, or unix time with `unixepoch`). Dates are read as UTC whatever the session time zone; an empty or invalid date gives `NULL`, like SQLite. Any other format or modifier (`%J`, `localtime`, `ceiling`, ...) is rejected when the filter is built, with a message that names it.

### View collections

The query of a view collection is **your SQL, run as written**, so on PostgreSQL it must be written in PostgreSQL:

- Quote the identifiers that have capitals (`SELECT id, title AS "postTitle"`); Postgres folds unquoted names to lower case.
- The `id` column must exist, and must be text or castable to text. Use `(ROW_NUMBER() OVER ()) AS id` (note the space) if there is none.
- The Claviger parser that reads the column names of the query wants an expression to be in parentheses (`(views * 2) AS doubled`), like on SQLite.
- No wildcard (`*`) columns, as on SQLite.

Field types come from the real column types of the view: `text` and `varchar` become a text field, `double precision`, `numeric`, `bigint` and `integer` become a number field (integers get "no decimals"), `boolean` a bool field and `jsonb` a JSON field. A column that is a plain field of a collection keeps that field's type and options (select values, relation target, ...), exactly as on SQLite. Other types (dates and timestamps, uuid, arrays) become JSON fields; cast them to `text` in the view to get a text field. SQLite has no column types to read, so unknown expressions there are JSON fields.

### Backups

Zip backups (`pb_data` plus a database copy) are a SQLite feature. On Postgres, back the database up with `pg_dump` or your provider's point-in-time recovery. Backup creation, the restore verifier and the Litestream and `entrypoint.sh` handover machinery are SQLite concerns and are not changed in M1; the zip backup of a Postgres install will contain files only once M3 lands.

### Your own SQL

SQL you write yourself runs **as written**: view collections, the superuser `/api/sql` endpoint and `$app.db()` in JS hooks. On Postgres that is Postgres SQL:

- Unquoted identifiers are folded to lower case, so `select createdAt from posts` fails; write `"createdAt"`.
- SQLite-only functions (`json_extract`, `strftime`, `randomblob`, ...) are not available.

Claviger does not translate user SQL; translating it would turn this into a hard fork of the storage layer. That includes the SQL of a view collection: write it for the database you run on (see [View collections](#view-collections)).

### Running the Postgres tests

```sh
# against a throwaway local Postgres 16 (downloaded on first use, no Docker needed):
cd scripts/pgembedded && go run .

# or against a server you already have:
CLAVIGER_TEST_PG_URL='postgres://user:pass@localhost:5432/postgres?sslmode=disable' \
  go test -tags postgres -count=1 -v ./tests/pgtests/...
```

Each test creates and drops its own database, so the URL can point at any database on the server (the role needs `CREATEDB`).

The suite runs the same data and requests on a SQLite app and a Postgres app and compares the answers (`filters_test.go`, `records_api_test.go`, `views_test.go`, `strftime_test.go`). To add a filter, add a row to the table in `TestRecordFilters`; give it the number of records it must match so that a filter which matches nothing anywhere cannot pass. A difference that cannot be fixed goes in the table with a `gap:` reason: it runs on SQLite only, shows up in the log as `known_gap/...` with both answers, and fails as soon as the two databases start to agree (so the list can only shrink). Any other skip fails CI. The SQL text of both dialects is also pinned without a database in `tools/search/dialect_sqlite_golden_test.go` (must never change) and `dialect_postgres_test.go`.

### For developers

The seam is the `Dialect` interface in `tools/dialect`. Code that has to differ per engine asks `core.DialectOf(app)` (or `dialect.Of(db)` when it holds a builder; a transaction keeps the dialect of its connection). SQLite is the previous behavior moved behind the interface and its SQL text is unchanged. New raw SQL in `core/`, `apis/` or a migration must either work on both engines or go through the dialect. `FORK_SYNC.md` lists the upstream-owned files that call into it.
