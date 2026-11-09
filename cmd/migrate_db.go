package cmd

// Fork-local (Claviger #106, M5): `claviger migrate-db`, a one way copy of a
// SQLite pb_data/data.db into an empty PostgreSQL database.
//
// The design, in the order the steps run:
//
//  1. Refuse early: the source must not be served (the single-writer lock and
//     handover files, like the `keys` commands), the target must be empty (no
//     table at all in its current schema) unless this is a --resume of a run
//     that left its progress marker table behind.
//  2. Open data.db read only, inside one read transaction (a stable snapshot),
//     and never write to pb_data: the target app that bootstraps the schema
//     uses a temporary data directory of its own.
//  3. Bootstrap the target (the system migrations build the system tables),
//     then import the collections of the source (structure, rules and indexes
//     in their portable form), which builds the tables, indexes and views for
//     Postgres exactly like production does.
//  4. Copy the rows table by table in `rowid` order, in batches. Every batch
//     is one transaction that also advances the progress marker, so an
//     interrupted run continues where it stopped.
//  5. Verify (row counts, a checksum over the ids in insertion order, a sample
//     of rows compared column by column after decoding) and print the report.
//
// The values are adapted to the column types of the Postgres tables (SQLite is
// dynamically typed): 0/1 become booleans, JSON text becomes JSONB, numeric
// text in a number column becomes a number, anything non-text in a text column
// becomes text. Dates stay text. Encrypted field values are copied verbatim.
// auxiliary.db (the request logs) is not copied.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/spf13/cobra"
)

const (
	// the progress marker table of a migration in flight (dropped when it succeeds)
	migrateMarkerTable = "_clavigerMigrateDb"

	migrateStageStarted = "started" // marker created, schema not complete
	migrateStageSchema  = "schema"  // system migrations ran and the collections are imported
	migrateStageCopied  = "copied"  // every table copied, verification pending

	migrateCollectionsTable = "_collections"
	migrateParamsTable      = "_params"

	migrateDefaultBatch = 1000

	// rows compared per table by the verification
	migrateSampleSize = 20

	// stay far below the 65535 bind parameter limit of a Postgres statement
	migrateMaxParams = 30000
)

// tables that are never copied, with the reason shown in the report
var migrateIgnoredTables = map[string]string{
	"_backupVerifications": "the history of backup verifications of this machine's zip backups",
}

// MigrateDBOptions configures [MigrateDB].
type MigrateDBOptions struct {
	// Dir is the pb_data directory that holds data.db.
	Dir string
	// TargetURL is the postgres:// URL of the empty target database.
	TargetURL string
	// BatchSize is the number of rows per transaction (default 1000).
	BatchSize int
	// DryRun only reports what would be copied.
	DryRun bool
	// Resume continues a migration that was interrupted.
	Resume bool
	// OfflineOK skips the check that no server owns Dir.
	OfflineOK bool
	// SkipViews leaves the view collections out (their queries are SQLite SQL).
	SkipViews bool
	// Out receives the progress output (default: discarded).
	Out io.Writer

	// test hook: called after the batch was committed; an error stops the run
	afterBatch func(table string, batch int) error
	// test hook: called before the verification (to damage the copy)
	beforeVerify func(dst core.App) error
}

// MigrateDBTable is the outcome for one table.
type MigrateDBTable struct {
	Name       string
	SourceRows int64
	TargetRows int64
	// IDsMatch is true when the checksum of the ids (in insertion order) is equal.
	IDsMatch bool
	// Sampled is the number of rows compared column by column.
	Sampled  int
	Warnings []string
}

// MigrateDBReport is what [MigrateDB] returns (also on a verification failure).
type MigrateDBReport struct {
	DryRun  bool
	Resumed bool
	Tables  []MigrateDBTable
	// SkippedViews are the view collections that were left out (SkipViews).
	SkippedViews []string
	// NotCopied are source tables that were left alone, with the reason.
	NotCopied []string
	Warnings  []string
	Verified  bool
}

// NewMigrateDBCommand creates the `migrate-db` command.
func NewMigrateDBCommand(app core.App) *cobra.Command {
	opts := MigrateDBOptions{}
	var verbose bool

	command := &cobra.Command{
		Use:   "migrate-db",
		Short: "Copies a SQLite pb_data into an empty PostgreSQL database",
		Long: "Copies <dir>/data.db (the --dir flag, default pb_data) into an EMPTY PostgreSQL database:\n" +
			"it builds the schema through the system migrations, imports the collections, copies the\n" +
			"rows of every table in insertion order, in batches, and verifies the result (row counts,\n" +
			"a checksum of the ids, a sample of decoded rows). The source is opened read only and is\n" +
			"never modified. auxiliary.db (the request logs) is not copied, and the storage files stay\n" +
			"where they are.\n\n" +
			"The command refuses to run when the target has any table (unless --resume continues an\n" +
			"interrupted run of this command), and when a server owns the source directory (the\n" +
			"single-writer lock or a handover is in progress) unless --offline-ok. Stop the server\n" +
			"first: a copy of a database that is still written to is incomplete.\n\n" +
			"Afterwards set CLAVIGER_DB_URL to the same URL and start Claviger. See docs/datastores.md.\n\n" +
			"Field encryption keys travel with the data (_fieldKeys, copied verbatim), but the key\n" +
			"encryption key (CLAVIGER_KEK ...) must stay configured: run `claviger keys check` with\n" +
			"CLAVIGER_DB_URL set to confirm that it opens them.",
		Example:      "migrate-db --dir=/pb_data --to='postgres://user:pass@host:5432/claviger?sslmode=require'\nmigrate-db --to=... --dry-run",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			opts.Dir = app.DataDir()
			opts.Out = command.OutOrStdout()
			if opts.TargetURL == "" {
				opts.TargetURL = strings.TrimSpace(os.Getenv(core.DBURLEnv))
			}

			ctx := command.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			report, err := MigrateDB(ctx, opts)
			if report != nil {
				printMigrateReport(command.OutOrStdout(), report, verbose)
			}

			return err
		},
	}

	command.Flags().StringVar(&opts.TargetURL, "to", "", "the postgres:// URL of the empty target database (default: $"+core.DBURLEnv+")")
	command.Flags().IntVar(&opts.BatchSize, "batch", migrateDefaultBatch, "rows per transaction")
	command.Flags().BoolVar(&opts.DryRun, "dry-run", false, "only report what would be copied")
	command.Flags().BoolVar(&opts.Resume, "resume", false, "continue an interrupted migration into the same target")
	command.Flags().BoolVar(&opts.OfflineOK, "offline-ok", false,
		"run even though a server may own the data directory (only if you are sure none is running)")
	command.Flags().BoolVar(&opts.SkipViews, "skip-views", false,
		"leave the view collections out: their queries are SQLite SQL (recreate them in PostgreSQL SQL afterwards)")
	command.Flags().BoolVar(&verbose, "verbose", false, "print the per-table report")

	return command
}

func printMigrateReport(out io.Writer, r *MigrateDBReport, verbose bool) {
	if r.DryRun {
		fmt.Fprintln(out, "dry run: nothing was written")
	}

	var total int64
	for _, t := range r.Tables {
		total += t.SourceRows
	}

	if verbose || r.DryRun {
		for _, t := range r.Tables {
			if r.DryRun {
				fmt.Fprintf(out, "  %-28s %d row(s) would be copied\n", t.Name, t.SourceRows)
				continue
			}
			fmt.Fprintf(out, "  %-28s source %d target %d ids %v sampled %d\n", t.Name, t.SourceRows, t.TargetRows, okWord(t.IDsMatch), t.Sampled)
		}
	}

	for _, t := range r.Tables {
		for _, w := range t.Warnings {
			fmt.Fprintf(out, "warning: %s: %s\n", t.Name, w)
		}
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	for _, n := range r.NotCopied {
		fmt.Fprintf(out, "not copied: %s\n", n)
	}
	for _, v := range r.SkippedViews {
		fmt.Fprintf(out, "skipped view collection: %s (recreate it with PostgreSQL SQL)\n", v)
	}

	switch {
	case r.DryRun:
		fmt.Fprintf(out, "would copy %d table(s), %d row(s)\n", len(r.Tables), total)
	case r.Verified:
		fmt.Fprintf(out, "migrated %d table(s), %d row(s); verified: row counts, id checksums and a sample of rows match\n", len(r.Tables), total)
		fmt.Fprintf(out, "next: set %s to the target URL, run `claviger keys check` if you use field encryption, then start Claviger\n", core.DBURLEnv)
	}
}

func okWord(ok bool) string {
	if ok {
		return "match"
	}
	return "DIFFER"
}

type migrateRun struct {
	opts MigrateDBOptions
	out  io.Writer
	src  *sql.Tx
	ctl  *dbx.DB // plain connection to the target (before the app exists)
	dst  core.App
	rep  *MigrateDBReport

	// the tables to copy, in order, and the view collections left out
	tables []string
	// the collections of the source that are imported
	collections []migrateCollection
}

type migrateCollection struct {
	id, name, typ string
}

// MigrateDB copies the SQLite data database of opts.Dir into the empty
// PostgreSQL database at opts.TargetURL. See the file comment.
func MigrateDB(ctx context.Context, opts MigrateDBOptions) (*MigrateDBReport, error) {
	if opts.BatchSize <= 0 {
		opts.BatchSize = migrateDefaultBatch
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if ctx == nil {
		ctx = context.Background()
	}

	if opts.TargetURL == "" {
		return nil, fmt.Errorf("the target is required: pass --to=postgres://... (or set %s)", core.DBURLEnv)
	}
	if u, err := url.Parse(opts.TargetURL); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return nil, errors.New("the target must be a postgres:// URL")
	}

	dbPath := filepath.Join(opts.Dir, "data.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("no source database: %s does not exist", dbPath)
	}

	if !opts.OfflineOK {
		if reason := ServerLiveReason(opts.Dir); reason != "" {
			return nil, fmt.Errorf("refusing to run: %s\n"+
				"A copy of a database that a server still writes to is incomplete. Stop the server first.\n"+
				"If you are sure no server is running, pass --offline-ok", reason)
		}
	}

	release := acquireOfflineLock(opts.Dir)
	defer release()

	m := &migrateRun{opts: opts, out: opts.Out, rep: &MigrateDBReport{DryRun: opts.DryRun, Resumed: opts.Resume}}

	if err := m.openSource(ctx, dbPath); err != nil {
		return nil, err
	}
	defer m.src.Rollback() //nolint:errcheck

	if err := m.inspectSource(ctx); err != nil {
		return nil, err
	}
	if err := m.checkSourceMigrations(ctx); err != nil {
		return nil, err
	}

	ctl, err := core.PostgresDBConnect(opts.TargetURL)("")
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	m.ctl = ctl
	defer ctl.Close()

	existing, err := m.targetTables()
	if err != nil {
		return nil, redactMigrateErr(err, opts.TargetURL)
	}

	if opts.DryRun {
		if len(existing) > 0 {
			m.rep.Warnings = append(m.rep.Warnings, fmt.Sprintf("the target is not empty (%s): a real run would refuse", previewNames(existing)))
		}
		for _, t := range m.tables {
			n, _ := m.countSource(ctx, t)
			m.rep.Tables = append(m.rep.Tables, MigrateDBTable{Name: t, SourceRows: n})
		}
		return m.rep, nil
	}

	resuming, err := m.checkTargetState(ctx, existing)
	if err != nil {
		return nil, err
	}
	m.rep.Resumed = resuming

	if err := m.bootstrapTarget(); err != nil {
		return nil, redactMigrateErr(err, opts.TargetURL)
	}
	defer func() {
		dir := m.dst.DataDir()
		_ = m.dst.ClearBootstrap()
		_ = os.RemoveAll(dir)
	}()

	if err := m.runSteps(ctx, resuming); err != nil {
		return m.rep, redactMigrateErr(err, opts.TargetURL)
	}

	return m.rep, nil
}

func redactMigrateErr(err error, targetURL string) error {
	if err == nil {
		return nil
	}
	return errors.New(strings.ReplaceAll(err.Error(), targetURL, "<redacted>"))
}

func previewNames(names []string) string {
	if len(names) > 5 {
		return strings.Join(names[:5], ", ") + ", ..."
	}
	return strings.Join(names, ", ")
}

// openSource opens data.db read only in one read transaction (a stable snapshot).
func (m *migrateRun) openSource(ctx context.Context, dbPath string) error {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return err
	}

	// A read only connection to a WAL database still creates the -shm and -wal
	// side files. Nothing may be written next to the source, so when no
	// write-ahead log holds data (the normal state after a clean shutdown) the
	// file is opened immutable, which uses no side files. A log that holds
	// data must be read, which needs the shared memory file.
	query := "mode=ro&immutable=1"
	if fi, err := os.Stat(abs + "-wal"); err == nil && fi.Size() > 0 {
		query = "mode=ro&_pragma=busy_timeout(10000)"
		fmt.Fprintln(m.out, "note: data.db-wal holds data; reading through it (the -shm file is created if it does not exist)")
	}

	u := url.URL{Scheme: "file", Path: abs, RawQuery: query}

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return fmt.Errorf("cannot open the source: %w", err)
	}
	db.SetMaxOpenConns(1)

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("cannot read the source: %w", err)
	}

	// a stable snapshot: the first read decides it
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('_collections','_params')").Scan(&n); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return fmt.Errorf("cannot read the source (is %s a SQLite database?): %w", dbPath, err)
	}
	if n != 2 {
		_ = tx.Rollback()
		_ = db.Close()
		return fmt.Errorf("%s is not a Claviger data database (no _collections and _params tables)", dbPath)
	}

	m.src = tx
	return nil
}

func quoteSQLite(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func (m *migrateRun) countSource(ctx context.Context, table string) (int64, error) {
	var n int64
	err := m.src.QueryRowContext(ctx, "SELECT count(*) FROM "+quoteSQLite(table)+m.sourceFilter(table, " WHERE")).Scan(&n)
	return n, err
}

// sourceFilter is the extra condition of the rows to copy from a table.
func (m *migrateRun) sourceFilter(table, keyword string) string {
	if table == migrateCollectionsTable && m.opts.SkipViews {
		return keyword + " type != 'view'"
	}
	return ""
}

// inspectSource decides the tables to copy.
func (m *migrateRun) inspectSource(ctx context.Context) error {
	rows, err := m.src.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY name")
	if err != nil {
		return err
	}
	present := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			return err
		}
		present[n] = true
	}
	_ = rows.Close()

	crows, err := m.src.QueryContext(ctx, "SELECT id, name, type FROM _collections ORDER BY rowid")
	if err != nil {
		return err
	}
	for crows.Next() {
		var c migrateCollection
		if err := crows.Scan(&c.id, &c.name, &c.typ); err != nil {
			_ = crows.Close()
			return err
		}
		m.collections = append(m.collections, c)
	}
	_ = crows.Close()

	m.tables = []string{migrateParamsTable, migrateCollectionsTable}
	claimed := map[string]bool{migrateParamsTable: true, migrateCollectionsTable: true, "_migrations": true}

	for _, c := range m.collections {
		claimed[c.name] = true

		if c.typ == core.CollectionTypeView {
			if m.opts.SkipViews {
				m.rep.SkippedViews = append(m.rep.SkippedViews, c.name)
			}
			continue
		}

		if !present[c.name] {
			m.rep.Warnings = append(m.rep.Warnings, fmt.Sprintf("collection %q has no table in the source", c.name))
			continue
		}

		m.tables = append(m.tables, c.name)
	}

	var rest []string
	for n := range present {
		if claimed[n] {
			continue
		}
		if why, ok := migrateIgnoredTables[n]; ok {
			rest = append(rest, fmt.Sprintf("%s (%s)", n, why))
			continue
		}
		rest = append(rest, fmt.Sprintf("%s (not a collection: create it in PostgreSQL yourself if you need it)", n))
	}
	sort.Strings(rest)
	m.rep.NotCopied = rest

	return nil
}

// fingerprint identifies the source snapshot, so that --resume can tell that
// the source changed since the run started.
func (m *migrateRun) fingerprint(ctx context.Context) (string, error) {
	h := sha256.New()
	tables := append([]string{"_migrations"}, m.tables...)
	sort.Strings(tables)

	for _, t := range tables {
		var n, maxRow int64
		q := "SELECT count(*), coalesce(max(rowid), 0) FROM " + quoteSQLite(t) + m.sourceFilter(t, " WHERE")
		if err := m.src.QueryRowContext(ctx, q).Scan(&n, &maxRow); err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s:%d:%d;", t, n, maxRow)
	}
	fmt.Fprintf(h, "skipViews=%v", m.opts.SkipViews)

	return hex.EncodeToString(h.Sum(nil)), nil
}

// targetTables lists the tables of the current schema of the target.
func (m *migrateRun) targetTables() ([]string, error) {
	var names []string
	err := m.ctl.NewQuery(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() ORDER BY table_name`).Column(&names)
	return names, err
}

// checkTargetState refuses a target that is not empty (or not resumable), and
// creates the progress marker of a fresh run.
func (m *migrateRun) checkTargetState(ctx context.Context, existing []string) (resuming bool, err error) {
	fp, err := m.fingerprint(ctx)
	if err != nil {
		return false, err
	}

	hasMarker := false
	for _, n := range existing {
		if n == migrateMarkerTable {
			hasMarker = true
		}
	}

	if len(existing) > 0 && !(m.opts.Resume && hasMarker) {
		if m.opts.Resume {
			return false, fmt.Errorf("--resume: the target has tables (%s) but no migration is in progress there (no %s table); migrate-db needs an empty database",
				previewNames(existing), migrateMarkerTable)
		}
		hint := ""
		if hasMarker {
			hint = "\nAn earlier migrate-db run did not finish there: continue it with --resume, or drop the database and start again."
		}
		return false, fmt.Errorf("the target database is not empty (%s): migrate-db copies into an EMPTY database, it never merges or overwrites%s",
			previewNames(existing), hint)
	}

	if m.opts.Resume && !hasMarker {
		return false, fmt.Errorf("--resume: nothing to resume, the target is empty (run without --resume)")
	}

	if hasMarker {
		stored, ok := m.markerGet(m.ctl, "source")
		if !ok || stored != fp {
			return false, errors.New("--resume: the source changed since the migration started (or this is another source): start again in an empty database")
		}
		return true, nil
	}

	if _, err := m.ctl.NewQuery(`CREATE TABLE "` + migrateMarkerTable + `" (k TEXT PRIMARY KEY, v TEXT NOT NULL)`).Execute(); err != nil {
		return false, err
	}
	if err := m.markerSet(m.ctl, "source", fp); err != nil {
		return false, err
	}

	return false, m.markerSet(m.ctl, "stage", migrateStageStarted)
}

func (m *migrateRun) markerGet(db dbx.Builder, key string) (string, bool) {
	var v string
	err := db.NewQuery(`SELECT v FROM "` + migrateMarkerTable + `" WHERE k = {:k}`).Bind(dbx.Params{"k": key}).Row(&v)
	return v, err == nil
}

func (m *migrateRun) markerSet(db dbx.Builder, key, value string) error {
	_, err := db.NewQuery(`INSERT INTO "` + migrateMarkerTable + `" (k, v) VALUES ({:k}, {:v})
		ON CONFLICT (k) DO UPDATE SET v = excluded.v`).Bind(dbx.Params{"k": key, "v": value}).Execute()
	return err
}

// bootstrapTarget opens the target as an app. Its data directory is a
// temporary one: nothing may be written next to the source.
func (m *migrateRun) bootstrapTarget() error {
	tmp, err := os.MkdirTemp("", "claviger-migrate-db-*")
	if err != nil {
		return err
	}

	cfg := core.BaseAppConfig{
		DataDir: tmp,
		// the settings are copied as they are stored (encrypted or not)
		EncryptionEnv: "",
		DBConnect:     core.PostgresDBConnect(m.opts.TargetURL),
		AuxDBConnect:  core.DefaultDBConnect,
	}
	m.dst = core.NewBaseApp(cfg)

	if err := m.dst.Bootstrap(); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("cannot bootstrap the target: %w", err)
	}

	return nil
}

func (m *migrateRun) runSteps(ctx context.Context, resuming bool) error {
	db := m.dst.DB()

	stage, _ := m.markerGet(db, "stage")

	for _, t := range m.tables {
		m.tableRep(t)
	}

	if stage == migrateStageStarted || stage == "" {
		fmt.Fprintln(m.out, "importing the collections ...")
		if err := m.importCollections(ctx); err != nil {
			return err
		}
		if err := m.markerSet(db, "stage", migrateStageSchema); err != nil {
			return err
		}
		stage = migrateStageSchema
	}

	if stage == migrateStageSchema {
		for _, t := range m.tables {
			if err := m.copyTable(ctx, t); err != nil {
				return err
			}
		}
		if err := m.copyMigrations(ctx); err != nil {
			return err
		}
		if err := m.markerSet(db, "stage", migrateStageCopied); err != nil {
			return err
		}
	}

	if m.opts.beforeVerify != nil {
		if err := m.opts.beforeVerify(m.dst); err != nil {
			return err
		}
	}

	fmt.Fprintln(m.out, "verifying ...")
	if err := m.verify(ctx); err != nil {
		return err
	}

	// the marker's job is done: the target is a regular Claviger database
	if _, err := db.NewQuery(`DROP TABLE "` + migrateMarkerTable + `"`).Execute(); err != nil {
		return err
	}
	_, _ = db.NewQuery("ANALYZE").Execute()

	m.rep.Verified = true
	return nil
}

// checkSourceMigrations refuses a source that is older than this binary: the
// target gets the schema of the system migrations of this version, and the
// source rows would not match it.
func (m *migrateRun) checkSourceMigrations(ctx context.Context) error {
	applied := map[string]bool{}

	rows, err := m.src.QueryContext(ctx, "SELECT file FROM _migrations")
	if err != nil {
		return fmt.Errorf("cannot read the migrations of the source: %w", err)
	}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			_ = rows.Close()
			return err
		}
		applied[f] = true
	}
	_ = rows.Close()

	var missing []string
	for _, mg := range core.SystemMigrations.Items() {
		if !applied[mg.File] {
			missing = append(missing, mg.File)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("the source database is older than this version of Claviger (it lacks %d system migration(s), e.g. %s): "+
			"start this version on the source once (`claviger serve`, then stop it) so that it is up to date, then run migrate-db again",
			len(missing), missing[0])
	}

	return nil
}

// importCollections builds the target tables from the stored collections.
func (m *migrateRun) importCollections(ctx context.Context) error {
	data, err := m.rawCollections(ctx)
	if err != nil {
		return err
	}

	if err := m.dst.ImportCollections(data, false); err != nil {
		hint := ""
		for _, c := range m.collections {
			if c.typ == core.CollectionTypeView && !m.opts.SkipViews {
				hint = "\nThe source has view collections (their queries are SQLite SQL and run as written on Postgres): " +
					"pass --skip-views to leave them out and recreate them in PostgreSQL SQL afterwards."
				break
			}
		}
		return fmt.Errorf("import collections: %w%s", err, hint)
	}

	return nil
}

// rawCollections returns the collections as an import payload, built from the
// stored rows: the public JSON of a collection hides the secrets (token
// secrets, OAuth2 client secrets) that existing tokens depend on.
func (m *migrateRun) rawCollections(ctx context.Context) ([]map[string]any, error) {
	rows, err := m.src.QueryContext(ctx, "SELECT * FROM _collections"+m.sourceFilter(migrateCollectionsTable, " WHERE")+" ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	jsonCols := map[string]bool{"fields": true, "indexes": true, "options": true}

	var result []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}

		item := map[string]any{}
		for i, col := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if jsonCols[col] {
				var decoded any
				if s := fmt.Sprint(v); s != "" && v != nil {
					if err := json.Unmarshal([]byte(s), &decoded); err != nil {
						return nil, fmt.Errorf("collection %v %s: %w", item["name"], col, err)
					}
				}
				v = decoded
			}
			if col == "system" {
				v = v == int64(1) || v == true
			}
			item[col] = v
		}

		if opts, ok := item["options"].(map[string]any); ok {
			for k, v := range opts {
				item[k] = v
			}
		}
		delete(item, "options")

		result = append(result, item)
	}

	return result, rows.Err()
}

func markerTableKey(table string) string { return "t:" + table }

// copyTable copies one table in batches. The progress marker is updated in
// the transaction of each batch.
func (m *migrateRun) copyTable(ctx context.Context, table string) error {
	db := m.dst.DB()
	d := core.DialectOf(m.dst)

	state, hasState := m.markerGet(db, markerTableKey(table))
	if hasState && state == "done" {
		return nil
	}

	dstCols, err := d.TableInfo(db, table)
	if err != nil {
		return fmt.Errorf("%s: %w", table, err)
	}
	if len(dstCols) == 0 {
		return fmt.Errorf("%s: the table does not exist in the target", table)
	}
	dstTypes := make(map[string]string, len(dstCols))
	for _, c := range dstCols {
		dstTypes[c.Name] = strings.ToLower(c.Type)
	}

	total, err := m.countSource(ctx, table)
	if err != nil {
		return fmt.Errorf("%s: %w", table, err)
	}

	var lastRowid int64
	if hasState {
		lastRowid, _ = strconv.ParseInt(state, 10, 64)
	} else {
		// first visit: the target table starts empty (the system migrations
		// and the collection import may have left rows in it)
		err := m.dst.RunInTransaction(func(tx core.App) error {
			if table != migrateCollectionsTable {
				if _, err := tx.DB().NewQuery("DELETE FROM {{" + table + "}}").Execute(); err != nil {
					return err
				}
			}
			return m.markerSet(tx.DB(), markerTableKey(table), "0")
		})
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
	}

	var done int64
	var warned bool
	batchNo := 0

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		q := "SELECT rowid AS __src_rowid, * FROM " + quoteSQLite(table) + " WHERE rowid > ?" + m.sourceFilter(table, " AND") + " ORDER BY rowid LIMIT ?"
		rows, err := m.src.QueryContext(ctx, q, lastRowid, m.opts.BatchSize)
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}

		srcCols, _ := rows.Columns()
		srcCols = srcCols[1:]

		var use []int // indexes into srcCols
		var useNames []string
		var dropped []string
		for i, c := range srcCols {
			if _, ok := dstTypes[c]; ok {
				use = append(use, i)
				useNames = append(useNames, c)
			} else {
				dropped = append(dropped, c)
			}
		}
		if !warned && len(dropped) > 0 {
			m.addTableWarning(table, "source columns the target table does not have are not copied: "+strings.Join(dropped, ", "))
			warned = true
		}

		var batch [][]any
		var batchRowids []int64
		for rows.Next() {
			vals := make([]any, len(srcCols)+1)
			ptrs := make([]any, len(vals))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				_ = rows.Close()
				return fmt.Errorf("%s: %w", table, err)
			}

			rowid, _ := vals[0].(int64)
			item := make([]any, len(use))
			for j, idx := range use {
				item[j] = adaptSQLiteValue(vals[idx+1], dstTypes[srcCols[idx]])
			}
			batch = append(batch, item)
			batchRowids = append(batchRowids, rowid)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}

		if len(batch) == 0 {
			err := m.dst.RunInTransaction(func(tx core.App) error {
				return m.markerSet(tx.DB(), markerTableKey(table), "done")
			})
			if err != nil {
				return fmt.Errorf("%s: %w", table, err)
			}
			break
		}

		newLast := batchRowids[len(batchRowids)-1]
		err = m.dst.RunInTransaction(func(tx core.App) error {
			if table == migrateCollectionsTable {
				// replace the rows the import created, keep the order of the source
				if err := deleteByIDs(tx, useNames, batch); err != nil {
					return err
				}
			}
			if err := insertBatch(tx, table, useNames, batch); err != nil {
				return err
			}
			return m.markerSet(tx.DB(), markerTableKey(table), strconv.FormatInt(newLast, 10))
		})
		if err != nil {
			return fmt.Errorf("%s: %w%s", table, err, m.diagnoseRows(table, useNames, batch))
		}

		lastRowid = newLast
		done += int64(len(batch))
		batchNo++
		fmt.Fprintf(m.out, "%s: %d/%d\n", table, done, total)

		if m.opts.afterBatch != nil {
			if err := m.opts.afterBatch(table, batchNo); err != nil {
				return err
			}
		}
	}

	return nil
}

// tableRep returns the report entry of a table (created on first use).
func (m *migrateRun) tableRep(table string) *MigrateDBTable {
	for i := range m.rep.Tables {
		if m.rep.Tables[i].Name == table {
			return &m.rep.Tables[i]
		}
	}
	m.rep.Tables = append(m.rep.Tables, MigrateDBTable{Name: table})
	return &m.rep.Tables[len(m.rep.Tables)-1]
}

func (m *migrateRun) addTableWarning(table, msg string) {
	t := m.tableRep(table)
	t.Warnings = append(t.Warnings, msg)
}

func deleteByIDs(tx core.App, cols []string, batch [][]any) error {
	idIdx := -1
	for i, c := range cols {
		if c == "id" {
			idIdx = i
		}
	}
	if idIdx < 0 {
		return nil
	}

	ids := make([]any, 0, len(batch))
	for _, row := range batch {
		ids = append(ids, row[idIdx])
	}

	_, err := tx.DB().Delete(migrateCollectionsTable, dbx.In("id", ids...)).Execute()
	return err
}

// insertBatch inserts the rows with multi-row INSERT statements.
func insertBatch(tx core.App, table string, cols []string, rows [][]any) error {
	if len(cols) == 0 {
		return nil
	}

	perStmt := migrateMaxParams / len(cols)
	if perStmt < 1 {
		perStmt = 1
	}

	for start := 0; start < len(rows); start += perStmt {
		end := min(start+perStmt, len(rows))

		var sb strings.Builder
		sb.WriteString("INSERT INTO {{" + table + "}} (")
		for i, c := range cols {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("[[" + c + "]]")
		}
		sb.WriteString(") VALUES ")

		params := dbx.Params{}
		for r := start; r < end; r++ {
			if r > start {
				sb.WriteString(", ")
			}
			sb.WriteString("(")
			for c := range cols {
				if c > 0 {
					sb.WriteString(", ")
				}
				name := "r" + strconv.Itoa(r-start) + "c" + strconv.Itoa(c)
				sb.WriteString("{:" + name + "}")
				params[name] = rows[r][c]
			}
			sb.WriteString(")")
		}

		if _, err := tx.DB().NewQuery(sb.String()).Bind(params).Execute(); err != nil {
			return err
		}
	}

	return nil
}

// diagnoseRows names the first row of a failed batch that Postgres refuses
// (each row is tried alone in a transaction that is rolled back).
func (m *migrateRun) diagnoseRows(table string, cols []string, batch [][]any) string {
	idIdx := -1
	for i, c := range cols {
		if c == "id" {
			idIdx = i
		}
	}

	errRollback := errors.New("rollback")
	for i, row := range batch {
		var rowErr error
		_ = m.dst.RunInTransaction(func(tx core.App) error {
			rowErr = insertBatch(tx, table, cols, [][]any{row})
			return errRollback
		})
		if rowErr != nil {
			who := "row #" + strconv.Itoa(i+1) + " of the batch"
			if idIdx >= 0 {
				who = fmt.Sprintf("the row with id %v", row[idIdx])
			}
			return fmt.Sprintf("\n(%s fails on its own: %v)", who, rowErr)
		}
	}

	return ""
}

// copyMigrations records the migrations of the source that the target does not
// know (the user's own), so that they do not run again on the first start.
func (m *migrateRun) copyMigrations(ctx context.Context) error {
	rows, err := m.src.QueryContext(ctx, "SELECT file, applied FROM _migrations ORDER BY rowid")
	if err != nil {
		return err
	}
	type mg struct {
		file    string
		applied int64
	}
	var all []mg
	for rows.Next() {
		var x mg
		if err := rows.Scan(&x.file, &x.applied); err != nil {
			_ = rows.Close()
			return err
		}
		all = append(all, x)
	}
	_ = rows.Close()

	return m.dst.RunInTransaction(func(tx core.App) error {
		for _, x := range all {
			_, err := tx.DB().NewQuery(`INSERT INTO {{_migrations}} ([[file]], [[applied]]) VALUES ({:f}, {:a})
				ON CONFLICT ([[file]]) DO NOTHING`).Bind(dbx.Params{"f": x.file, "a": x.applied}).Execute()
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// adaptSQLiteValue converts a SQLite value for a column of the Postgres type
// (see the file comment).
func adaptSQLiteValue(v any, pgType string) any {
	if v == nil {
		return nil
	}

	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	if t, ok := v.(time.Time); ok {
		// a column declared as a date type: the stored format of PocketBase dates
		v = t.UTC().Format("2006-01-02 15:04:05.000Z")
	}

	switch {
	case pgType == "boolean":
		switch x := v.(type) {
		case int64:
			return x != 0
		case float64:
			return x != 0
		case string:
			return x == "1" || strings.EqualFold(x, "true")
		}
	case pgType == "jsonb" || pgType == "json":
		if s, ok := v.(string); ok && s == "" {
			return nil
		}
		return fmt.Sprint(v)
	case strings.Contains(pgType, "double") || pgType == "real":
		if s, ok := v.(string); ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return f
			}
		}
	case pgType == "text" || strings.HasPrefix(pgType, "character"):
		if _, ok := v.(string); !ok {
			return fmt.Sprint(v)
		}
	}

	return v
}
