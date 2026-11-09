package cmd

// Fork-local (Claviger #106, M5): the verification of `migrate-db`.
//
// It compares the source snapshot with what is now in the target:
//   - the number of rows of every table,
//   - a checksum over the ids in insertion order (so a missing, extra,
//     duplicated or reordered row is caught),
//   - a sample of rows (the first, the last and evenly spaced ones, up to
//     migrateSampleSize per table) compared column by column after decoding:
//     booleans, numbers and JSON are compared as values, not as text.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"strconv"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

func (m *migrateRun) verify(ctx context.Context) error {
	var problems []string

	for _, name := range m.tables {
		t := m.tableRep(name)
		if err := m.verifyTable(ctx, name, t); err != nil {
			problems = append(problems, name+": "+err.Error())
		}
	}

	if err := m.verifyMigrations(ctx); err != nil {
		problems = append(problems, "_migrations: "+err.Error())
	}

	if len(problems) > 0 {
		return fmt.Errorf("verification FAILED, the target is not a faithful copy (the progress marker table %s was kept):\n  %s",
			migrateMarkerTable, strings.Join(problems, "\n  "))
	}

	return nil
}

func (m *migrateRun) verifyTable(ctx context.Context, table string, t *MigrateDBTable) error {
	db := m.dst.DB()
	d := core.DialectOf(m.dst)

	var err error
	t.SourceRows, err = m.countSource(ctx, table)
	if err != nil {
		return err
	}

	// the target of _collections also holds collections the source lacks
	// (system collections of a newer version); count only the copied ones
	var dstCount int64
	if table == migrateCollectionsTable && m.opts.SkipViews {
		err = db.NewQuery("SELECT count(*) FROM {{" + table + "}} WHERE [[type]] != 'view'").Row(&dstCount)
	} else {
		err = db.NewQuery("SELECT count(*) FROM {{" + table + "}}").Row(&dstCount)
	}
	if err != nil {
		return err
	}
	t.TargetRows = dstCount

	dstCols, err := d.TableInfo(db, table)
	if err != nil {
		return err
	}
	dstTypes := make(map[string]string, len(dstCols))
	hasID := false
	for _, c := range dstCols {
		dstTypes[c.Name] = strings.ToLower(c.Type)
		if c.Name == "id" {
			hasID = true
		}
	}

	if !hasID {
		t.IDsMatch = t.SourceRows == t.TargetRows
		if !t.IDsMatch {
			return fmt.Errorf("row count: source %d, target %d", t.SourceRows, t.TargetRows)
		}
		return nil
	}

	// ids in insertion order
	srcSum, srcIDs, err := m.hashSourceIDs(ctx, table, t.SourceRows)
	if err != nil {
		return err
	}
	dstSum, err := m.hashTargetIDs(table, srcIDs)
	if err != nil {
		return err
	}

	t.IDsMatch = srcSum == dstSum

	switch {
	case t.TargetRows != t.SourceRows && table != migrateCollectionsTable:
		return fmt.Errorf("row count: source %d, target %d", t.SourceRows, t.TargetRows)
	case !t.IDsMatch:
		return fmt.Errorf("the ids differ (source %d row(s), target %d): a row is missing, extra or out of order", t.SourceRows, t.TargetRows)
	}

	// sample
	sample := sampleIndexes(len(srcIDs.ordered), migrateSampleSize)
	for _, idx := range sample {
		id := srcIDs.ordered[idx]
		if err := m.compareRow(ctx, table, id, dstTypes); err != nil {
			return fmt.Errorf("row %s: %w", id, err)
		}
		t.Sampled++
	}

	return nil
}

type migrateIDs struct {
	ordered []string
	set     map[string]bool
}

func (m *migrateRun) hashSourceIDs(ctx context.Context, table string, expect int64) (string, *migrateIDs, error) {
	rows, err := m.src.QueryContext(ctx, "SELECT id FROM "+quoteSQLite(table)+m.sourceFilter(table, " WHERE")+" ORDER BY rowid")
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()

	h := sha256.New()
	ids := &migrateIDs{set: make(map[string]bool, expect)}
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id); err != nil {
			return "", nil, err
		}
		writeID(h, id.String)
		ids.ordered = append(ids.ordered, id.String)
		ids.set[id.String] = true
	}

	return hex.EncodeToString(h.Sum(nil)), ids, rows.Err()
}

// hashTargetIDs hashes the ids of the target in insertion order, leaving out
// the ones the source does not have (for _collections: system collections of a
// newer version, and the rows of skipped views).
func (m *migrateRun) hashTargetIDs(table string, src *migrateIDs) (string, error) {
	d := core.DialectOf(m.dst)

	rows, err := m.dst.DB().NewQuery("SELECT [[id]] FROM {{" + table + "}} ORDER BY " + d.RowOrder()).Rows()
	if err != nil {
		return "", err
	}
	defer rows.Close()

	h := sha256.New()
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		if table == migrateCollectionsTable && !src.set[id.String] {
			continue
		}
		writeID(h, id.String)
	}

	return hex.EncodeToString(h.Sum(nil)), rows.Err()
}

func writeID(h hash.Hash, id string) {
	h.Write([]byte(strconv.Itoa(len(id))))
	h.Write([]byte{':'})
	h.Write([]byte(id))
}

// sampleIndexes returns up to n indexes in [0,total): the first, the last and
// evenly spaced ones.
func sampleIndexes(total, n int) []int {
	if total <= 0 {
		return nil
	}
	if total <= n {
		out := make([]int, total)
		for i := range out {
			out[i] = i
		}
		return out
	}

	out := make([]int, 0, n)
	for i := range n {
		out = append(out, i*(total-1)/(n-1))
	}
	return out
}

func (m *migrateRun) compareRow(ctx context.Context, table, id string, dstTypes map[string]string) error {
	// source
	rows, err := m.src.QueryContext(ctx, "SELECT * FROM "+quoteSQLite(table)+" WHERE id = ?", id)
	if err != nil {
		return err
	}
	srcCols, _ := rows.Columns()
	srcVals := make([]any, len(srcCols))
	ptrs := make([]any, len(srcCols))
	for i := range srcVals {
		ptrs[i] = &srcVals[i]
	}
	if !rows.Next() {
		_ = rows.Close()
		return fmt.Errorf("not found in the source")
	}
	if err := rows.Scan(ptrs...); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	// target
	drows, err := m.dst.DB().NewQuery("SELECT * FROM {{" + table + "}} WHERE [[id]] = {:id}").Bind(dbx.Params{"id": id}).Rows()
	if err != nil {
		return err
	}
	defer drows.Close()

	dstCols, _ := drows.Columns()
	dstVals := make([]any, len(dstCols))
	dptrs := make([]any, len(dstCols))
	for i := range dstVals {
		dptrs[i] = &dstVals[i]
	}
	if !drows.Next() {
		return fmt.Errorf("not found in the target")
	}
	if err := drows.Scan(dptrs...); err != nil {
		return err
	}

	dstByName := make(map[string]any, len(dstCols))
	for i, c := range dstCols {
		dstByName[c] = dstVals[i]
	}

	for i, c := range srcCols {
		typ, ok := dstTypes[c]
		if !ok {
			continue // not copied (warned about)
		}
		want := canonValue(adaptSQLiteValue(srcVals[i], typ), typ)
		got := canonValue(dstByName[c], typ)
		if want != got {
			return fmt.Errorf("column %s differs: source %s, target %s", c, clip(want), clip(got))
		}
	}

	return nil
}

func clip(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// canonValue renders a value in a form that is equal for equal data: JSON is
// re-encoded (key order and spacing do not matter), numbers are compared as
// floats, booleans as booleans.
func canonValue(v any, pgType string) string {
	if v == nil {
		return "\x00null"
	}
	if b, ok := v.([]byte); ok {
		v = string(b)
	}

	switch {
	case pgType == "boolean":
		if b, ok := v.(bool); ok {
			return "b:" + strconv.FormatBool(b)
		}
	case pgType == "jsonb" || pgType == "json":
		var decoded any
		if err := json.Unmarshal([]byte(fmt.Sprint(v)), &decoded); err == nil {
			if out, err := json.Marshal(decoded); err == nil {
				return "j:" + string(out)
			}
		}
	case strings.Contains(pgType, "double") || pgType == "real":
		switch x := v.(type) {
		case float64:
			return "f:" + strconv.FormatFloat(x, 'g', -1, 64)
		case int64:
			return "f:" + strconv.FormatFloat(float64(x), 'g', -1, 64)
		}
	}

	return "t:" + fmt.Sprint(v)
}

// verifyMigrations checks that every migration of the source is recorded.
func (m *migrateRun) verifyMigrations(ctx context.Context) error {
	rows, err := m.src.QueryContext(ctx, "SELECT file FROM _migrations")
	if err != nil {
		return err
	}
	var files []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			_ = rows.Close()
			return err
		}
		files = append(files, f)
	}
	_ = rows.Close()

	var have []string
	if err := m.dst.DB().NewQuery("SELECT [[file]] FROM {{_migrations}}").Column(&have); err != nil {
		return err
	}
	haveSet := make(map[string]bool, len(have))
	for _, f := range have {
		haveSet[f] = true
	}
	for _, f := range files {
		if !haveSet[f] {
			return fmt.Errorf("migration %s of the source is not recorded in the target", f)
		}
	}

	return nil
}
