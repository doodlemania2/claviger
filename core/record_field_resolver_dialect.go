package core

// Fork-local addition — not upstream PocketBase (issue #106).

import (
	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dialect"
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
