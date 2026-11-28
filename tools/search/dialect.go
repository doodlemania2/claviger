package search

// Fork-local addition — not upstream PocketBase (issue #106).
//
// The filter language is database agnostic; the SQL it generates is not. A
// FieldResolver that works on a non SQLite database implements
// DialectProvider, and everything in this package that has to differ per
// engine asks that dialect. Without it the SQLite dialect is used, which
// keeps the previous behavior (and SQL text) unchanged.

import (
	"regexp"
	"strings"

	"ganigeorgiev.com/fexpr"
	"github.com/doodlemania2/claviger/tools/dialect"
)

// DialectProvider is optionally implemented by a FieldResolver to select
// the database dialect of the SQL that is generated for it.
type DialectProvider interface {
	Dialect() dialect.Dialect
}

// DefaultOrderProvider is optionally implemented by a FieldResolver to add a
// stable ORDER BY term to the queries that have no explicit sort (an empty
// string means none). Without an ORDER BY, Postgres returns the rows in
// physical order, which changes whenever a row is updated, while SQLite
// returns them in insertion (rowid) order.
type DefaultOrderProvider interface {
	DefaultOrder() string
}

// dialectOf returns the dialect of the resolver (SQLite by default).
func dialectOf(r FieldResolver) dialect.Dialect {
	if p, ok := r.(DialectProvider); ok {
		if d := p.Dialect(); d != nil {
			return d
		}
	}

	return dialect.SQLite
}

// normalizedIdentifier returns the SQL of a special literal identifier
// (null, true, false) that is used when there is no field with that name.
func normalizedIdentifier(d dialect.Dialect, name, sqlite string) string {
	switch name {
	case "true":
		return d.BoolLiteral(true)
	case "false":
		return d.BoolLiteral(false)
	}

	return sqlite
}

type tokenFunc = func(
	argTokenResolverFunc func(fexpr.Token) (*ResolverResult, error),
	args ...fexpr.Token,
) (*ResolverResult, error)

// tokenFunction returns the filter function implementation for the dialect.
func tokenFunction(d dialect.Dialect, name string) (tokenFunc, bool) {
	if fb, ok := d.(dialect.FunctionBuilder); ok {
		if fn, ok := builderTokenFunctions[name]; ok {
			return func(argResolver func(fexpr.Token) (*ResolverResult, error), args ...fexpr.Token) (*ResolverResult, error) {
				return fn(d, fb, argResolver, args...)
			}, true
		}
	}

	fn, ok := TokenFunctions[name]

	return fn, ok
}

// joinOnClause returns the " ON ..." suffix for a join without a condition.
func joinOnClause(d dialect.Dialect) string {
	if on := d.JoinOn(); on != "" {
		return " ON " + on
	}

	return ""
}

// adaptJSONOperands makes the operands of a comparison comparable when the
// dialect extracts JSON values as typed JSON (Postgres jsonb), not as SQL
// scalars (SQLite). It is a no-op on SQLite.
//
//   - for LIKE the JSON side is converted to text;
//   - against a scalar parameter (number, text, boolean) the parameter is
//     converted to JSON, so jsonb compares number to number and text to text,
//     and values of a different type never match (like on SQLite);
//   - against any other expression (a column) the JSON side is converted to text.
func adaptJSONOperands(d dialect.Dialect, op fexpr.SignOp, left, right *ResolverResult) (*ResolverResult, *ResolverResult) {
	// two bound parameters compared with each other have no column to take
	// their type from (and the driver can't send a number as text)
	if d.StrictTypes() && !isLikeOp(op) {
		lk, lok := scalarKind(left)
		rk, rok := scalarKind(right)
		// a NULL parameter has no kind of its own: it takes the kind of the
		// other operand (CockroachDB, unlike PostgreSQL, does not infer the
		// type of two placeholders that are compared with each other)
		if !lok && isNilParam(left) {
			lk, lok = rk, rok
			if !rok && isNilParam(right) {
				lk, rk, lok, rok = dialect.ScalarText, dialect.ScalarText, true, true
			}
		}
		if !rok && isNilParam(right) {
			rk, rok = lk, lok
		}
		// a NULL literal compared with a parameter has no type for the
		// parameter either
		if isNullLiteral(left) && rok && len(right.Params) > 0 {
			r := *right
			r.Identifier = d.ScalarCast(rk, right.Identifier)
			return left, &r
		}
		if isNullLiteral(right) && lok && len(left.Params) > 0 {
			l := *left
			l.Identifier = d.ScalarCast(lk, left.Identifier)
			return &l, right
		}
		if lok && rok && len(left.Params) > 0 && len(right.Params) > 0 {
			l, r := *left, *right
			l.Identifier = d.ScalarCast(lk, left.Identifier)
			r.Identifier = d.ScalarCast(rk, right.Identifier)
			return &l, &r
		}
	}

	if !d.NativeJSON() || (!left.JSONB && !right.JSONB) {
		return left, right
	}

	if isLikeOp(op) {
		return jsonAsText(d, left), jsonAsText(d, right)
	}

	if left.JSONB && right.JSONB {
		return left, right
	}

	// exactly one of the operands is a JSON value
	j, other := left, right
	if right.JSONB {
		j, other = right, left
	}

	if kind, ok := scalarKind(other); ok {
		c := *other
		c.JSONB = true
		c.Identifier = d.JSONFromScalar(kind, other.Identifier)
		other = &c
	} else if !strings.EqualFold(other.Identifier, "NULL") {
		j = jsonAsText(d, j)
	}

	if right.JSONB {
		return other, j
	}

	return j, other
}

func isLikeOp(op fexpr.SignOp) bool {
	switch op {
	case fexpr.SignLike, fexpr.SignAnyLike, fexpr.SignNlike, fexpr.SignAnyNlike:
		return true
	}

	return false
}

// jsonAsText returns a copy of the operand with the JSON value converted to text.
func jsonAsText(d dialect.Dialect, r *ResolverResult) *ResolverResult {
	if !r.JSONB {
		return r
	}

	c := *r
	c.JSONB = false
	c.Identifier = d.JSONText(r.Identifier)

	if r.MultiMatchSubQuery != nil {
		mm := *r.MultiMatchSubQuery
		mm.ValueIdentifier = d.JSONText(mm.ValueIdentifier)
		c.MultiMatchSubQuery = &mm
	}

	return &c
}

var numberLiteral = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

// scalarKind reports the kind of a scalar parameter or literal operand.
func isNullLiteral(r *ResolverResult) bool {
	return len(r.Params) == 0 && strings.EqualFold(strings.TrimSpace(r.Identifier), "NULL")
}

// isNilParam reports whether the operand is a single NULL placeholder.
func isNilParam(r *ResolverResult) bool {
	if len(r.Params) != 1 || r.MultiMatchSubQuery != nil {
		return false
	}

	for name, v := range r.Params {
		return v == nil && r.Identifier == "{:"+name+"}"
	}

	return false
}

func scalarKind(r *ResolverResult) (dialect.ScalarKind, bool) {
	switch strings.ToUpper(r.Identifier) {
	case "TRUE", "FALSE":
		return dialect.ScalarBool, true
	}

	if len(r.Params) == 0 && numberLiteral.MatchString(r.Identifier) {
		return dialect.ScalarNumber, true
	}

	if len(r.Params) != 1 || r.MultiMatchSubQuery != nil {
		return 0, false
	}

	for name, v := range r.Params {
		if r.Identifier != "{:"+name+"}" {
			return 0, false
		}

		switch v.(type) {
		case string:
			return dialect.ScalarText, true
		case bool:
			return dialect.ScalarBool, true
		case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return dialect.ScalarNumber, true
		}
	}

	return 0, false
}

// emptyOperand returns the SQL of the empty value (NULL or an empty string)
// that is compared with the other operand. A strictly typed engine can't
// compare an empty string with a boolean: NULL gives the same outcome in a
// WHERE clause (never equal, always different) for the null safe operators.
func emptyOperand(d dialect.Dialect, other *ResolverResult) string {
	if d.StrictTypes() {
		if kind, ok := scalarKind(other); ok && kind == dialect.ScalarBool {
			return "NULL"
		}
	}

	return "''"
}

// nullSafeComparer is implemented by dialects that have no infix operator for
// the null safe (in)equality that "IS" and "IS NOT" are on SQLite (MySQL: <=>).
type nullSafeComparer interface {
	NullSafeEqual(left, right string, not bool) string
}

// nullSafeCompare renders "left op right", or the dialect's own spelling.
func nullSafeCompare(d dialect.Dialect, left, op, right string, not bool) string {
	if c, ok := d.(nullSafeComparer); ok {
		return c.NullSafeEqual(left, right, not)
	}

	return left + " " + op + " " + right
}

// noRows renders "there is no row in <fromWhere>" (a FROM ... WHERE ... tail).
// It is NOT EXISTS, except on the dialects that implement NoRows: MySQL
// evaluates NOT EXISTS over a correlated derived table with an outer join
// wrongly (it is always false), a COUNT(*) comparison is right.
func noRows(d dialect.Dialect, fromWhere string) string {
	if nr, ok := d.(interface{ NoRows(fromWhere string) string }); ok {
		return nr.NoRows(fromWhere)
	}

	return "NOT EXISTS (SELECT 1 " + fromWhere + ")"
}

// likeContains renders the "contains" pattern of a column operand: '%' || x || '%'
// (SQL Server concatenates with +).
func likeContains(d dialect.Dialect, operand string) string {
	if c, ok := d.(interface{ Concat(parts ...string) string }); ok {
		return "(" + c.Concat("'%'", operand, "'%'") + ")"
	}

	return "('%' || " + operand + " || '%')"
}

// LateralJoin returns the keyword that joins a table valued function (or a
// derived table) over a column of an earlier table without a condition
// ("OUTER APPLY" on SQL Server), or an empty string if the dialect joins it
// with LEFT JOIN ... ON.
func LateralJoin(d dialect.Dialect) string {
	if a, ok := d.(interface{ ApplyJoin() string }); ok {
		return a.ApplyJoin()
	}

	return ""
}

// likeExtraChars returns the characters, besides % and _, that the dialect's
// LIKE treats as a pattern (SQL Server: [ starts a character class).
func likeExtraChars(d dialect.Dialect) []rune {
	if c, ok := d.(interface{ LikeSpecialChars() []rune }); ok {
		return c.LikeSpecialChars()
	}

	return nil
}
