package dialect

// SQL Server DDL for dbx (issue #106, experimental).
//
// dbx's own mssql builder has a few statements that SQL Server does not
// know (sp_name instead of sp_rename) or that fail on the columns that
// Claviger creates (DROP COLUMN of a column with a DEFAULT constraint).

import (
	"strconv"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
)

type mssqlBuilder struct {
	*dbx.MssqlBuilder
	db *dbx.DB
	qb *mssqlQueryBuilder
}

// mssqlQueryBuilder fixes the paging of a SELECT DISTINCT without ORDER BY.
type mssqlQueryBuilder struct {
	*dbx.MssqlQueryBuilder
}

// QueryBuilder returns the query builder supporting the current DB.
func (b *mssqlBuilder) QueryBuilder() dbx.QueryBuilder { return b.qb }

// Select returns a new SelectQuery that is built with this builder.
func (b *mssqlBuilder) Select(cols ...string) *dbx.SelectQuery {
	return dbx.NewSelectQuery(b, b.db).Select(cols...)
}

// Model returns a new ModelQuery that is built with this builder.
func (b *mssqlBuilder) Model(model interface{}) *dbx.ModelQuery {
	return dbx.NewModelQuery(model, b.db.FieldMapper, b.db, b)
}

// lastDistinct returns the index right after the last top level "SELECT DISTINCT "
// of the statement (outside parentheses and quotes), or -1.
func lastDistinct(sql string) int {
	const kw = "SELECT DISTINCT "

	depth, found := 0, -1
	var quote byte

	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && len(sql)-i >= len(kw) && strings.EqualFold(sql[i:i+len(kw)], kw):
			found = i + len(kw)
		}
	}

	return found
}

// BuildOrderByAndLimit pages a SELECT DISTINCT that has no ORDER BY: SQL Server
// refuses the ORDER BY (SELECT NULL) that dbx adds to it, as it is not in the select list.
func (q *mssqlQueryBuilder) BuildOrderByAndLimit(sql string, cols []string, limit int64, offset int64) string {
	if len(cols) == 0 && (limit >= 0 || offset > 0) {
		if at := lastDistinct(sql); at >= 0 {
			if offset <= 0 {
				return sql[:at] + "TOP (" + strconv.FormatInt(limit, 10) + ") " + sql[at:]
			}

			sql += "\nORDER BY 1\nOFFSET " + strconv.FormatInt(offset, 10) + " ROWS"
			if limit >= 0 {
				sql += "\nFETCH NEXT " + strconv.FormatInt(limit, 10) + " ROWS ONLY"
			}
			return sql
		}
	}

	return q.MssqlQueryBuilder.BuildOrderByAndLimit(sql, cols, limit, offset)
}

func init() {
	dbx.BuilderFuncMap[MSSQLName] = func(db *dbx.DB, executor dbx.Executor) dbx.Builder {
		return &mssqlBuilder{
			MssqlBuilder: dbx.NewMssqlBuilder(db, executor).(*dbx.MssqlBuilder),
			db:           db,
			qb:           &mssqlQueryBuilder{&dbx.MssqlQueryBuilder{BaseQueryBuilder: dbx.NewBaseQueryBuilder(db)}},
		}
	}
}

func mssqlUnbrace(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimSuffix(s, "}}"), "{{")
	s = strings.TrimPrefix(strings.TrimSuffix(s, "]]"), "[[")
	return s
}

func mssqlLiteral(s string) string {
	return "N'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// RenameTable renames a table (sp_rename).
func (b *mssqlBuilder) RenameTable(oldName, newName string) *dbx.Query {
	return b.NewQuery("EXEC sp_rename " + mssqlLiteral(b.db.QuoteTableName("{{"+mssqlUnbrace(oldName)+"}}")) +
		", " + mssqlLiteral(mssqlUnbrace(newName)))
}

// RenameColumn renames a column (sp_rename).
func (b *mssqlBuilder) RenameColumn(table, oldName, newName string) *dbx.Query {
	return b.NewQuery("EXEC sp_rename " +
		mssqlLiteral(b.db.QuoteTableName(table)+"."+b.db.QuoteColumnName(oldName)) +
		", " + mssqlLiteral(newName) + ", N'COLUMN'")
}

// DropColumn drops a column and the default constraint that it has.
func (b *mssqlBuilder) DropColumn(table, col string) *dbx.Query {
	return b.NewQuery(mssqlDropColumnSQL(mssqlDialect{}, b.db.QuoteTableName(table), b.db.QuoteColumnName(col), col))
}
