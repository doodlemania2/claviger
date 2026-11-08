package core

// Fork-local addition — not upstream PocketBase (issue #106).

import (
	"regexp"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dialect"
	"github.com/doodlemania2/claviger/tools/inflector"
	"github.com/doodlemania2/claviger/tools/search"
)

var _ search.DialectProvider = (*RecordFieldResolver)(nil)

// Dialect implements [search.DialectProvider]: the filters are built for the
// database that the resolver's app stores its data in.
func (r *RecordFieldResolver) Dialect() dialect.Dialect {
	return dialectOf(r.app)
}

// leftJoin attaches a resolver join to the query. A join without a
// condition (a cross join, or a lateral set returning function) needs an
// explicit "ON TRUE" on Postgres.
func (r *RecordFieldResolver) leftJoin(query *dbx.SelectQuery, join *search.Join) {
	on := join.On
	if on == nil {
		if cond := r.Dialect().JoinOn(); cond != "" {
			on = dbx.NewExp(cond)
		}
	}

	query.LeftJoin(join.TableName+" "+join.TableAlias, on)
}

var (
	orderTermRegex = regexp.MustCompile(`(?is)^(.*?)\s+(ASC|DESC)(?:\s+NULLS\s+(FIRST|LAST))?\s*$`)
	aliasRefRegex  = regexp.MustCompile(`\[\[(\w+)\.`)
)

// groupJoinedOrderBy lets a query that is deduplicated and sorted by a column
// of a joined table run on engines that don't allow it with SELECT DISTINCT
// (Postgres: "ORDER BY expressions must appear in select list").
//
// It replaces the DISTINCT with a GROUP BY the primary key of the base table
// (every other selected column is functionally dependent on it) and sorts the
// groups by the first value of the joined column in the requested order,
// which is a deterministic pick among the related rows.
func (r *RecordFieldResolver) groupJoinedOrderBy(query *dbx.SelectQuery) {
	if !r.Dialect().StrictTypes() || r.baseCollection == nil || r.baseCollection.IsView() {
		return
	}

	info := query.Info()
	if len(info.OrderBy) == 0 || len(info.From) == 0 {
		return
	}

	base := r.baseCollectionAlias
	if base == "" {
		base = inflector.Columnify(r.baseCollection.Name)
	}

	var rewritten bool

	terms := make([]string, len(info.OrderBy))
	for i, term := range info.OrderBy {
		terms[i] = term

		m := orderTermRegex.FindStringSubmatch(term)
		if m == nil {
			continue
		}

		var joined bool
		for _, ref := range aliasRefRegex.FindAllStringSubmatch(m[1], -1) {
			if ref[1] != base {
				joined = true
				break
			}
		}
		if !joined {
			continue
		}

		dir := strings.ToUpper(m[2])
		nulls := "NULLS FIRST"
		if dir == "DESC" {
			nulls = "NULLS LAST"
		}
		if m[3] != "" {
			nulls = "NULLS " + strings.ToUpper(m[3])
		}

		terms[i] = "(ARRAY_AGG(" + m[1] + " ORDER BY " + m[1] + " " + dir + " " + nulls + "))[1] " + dir + " " + nulls
		rewritten = true
	}

	if !rewritten {
		return
	}

	query.Distinct(false)
	query.GroupBy("[[" + base + ".id]]")
	query.OrderBy(terms...)
}

var _ search.DefaultOrderProvider = (*RecordFieldResolver)(nil)

// DefaultOrder implements [search.DefaultOrderProvider]: insertion order,
// on the engines where it is not implicit and for collections that have it
// (views don't). It is empty on SQLite, which keeps its queries unchanged.
func (r *RecordFieldResolver) DefaultOrder() string {
	d := r.Dialect()

	if _, ok := d.HiddenColumns()[dialect.RowIDColumn]; !ok || r.baseCollection == nil || r.baseCollection.IsView() {
		return ""
	}

	base := r.baseCollectionAlias
	if base == "" {
		base = inflector.Columnify(r.baseCollection.Name)
	}

	return "[[" + base + "." + dialect.RowIDColumn + "]] ASC"
}
