package telemetry

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const maxQueryTextLen = 1024

// OpenDB opens dsn like dbx.Open(driverName, dsn) but through a wrapper that
// times every statement and, inside a traced request, adds a child span.
//
// The wrapper sits at the database/sql driver boundary on purpose. There the
// query text still has its "?" placeholders and the bound values travel
// separately, so a value can not end up in a span. (dbx's own QueryLogFunc
// receives the statement with the values already substituted in.) namespace
// names the database in the metric and span, e.g. "data.db".
func (t *Telemetry) OpenDB(driverName, dsn, namespace string) (*dbx.DB, error) {
	probe, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	drv := probe.Driver()
	_ = probe.Close()

	sqlDB := sql.OpenDB(&connector{t: t, drv: drv, dsn: dsn, ns: namespace, system: dbSystem(driverName)})

	return dbx.NewFromDB(sqlDB, driverName), nil
}

// dbSystem is the db.system.name attribute value of a database/sql driver.
func dbSystem(driverName string) string {
	switch driverName {
	case "pgx", "postgres":
		return "postgresql"
	}
	return "sqlite"
}

type connector struct {
	t      *Telemetry
	drv    driver.Driver
	dsn    string
	ns     string
	system string
}

func (c *connector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &tconn{Conn: conn, t: c.t, ns: c.ns, system: c.system}, nil
}

func (c *connector) Driver() driver.Driver { return c.drv }

type tconn struct {
	driver.Conn
	t      *Telemetry
	ns     string
	system string
}

func (c *tconn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, q)
	}
	return c.Prepare(q)
}

func (c *tconn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, o)
	}
	return c.Conn.Begin() //nolint:staticcheck
}

func (c *tconn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *tconn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *tconn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *tconn) CheckNamedValue(nv *driver.NamedValue) error {
	if ck, ok := c.Conn.(driver.NamedValueChecker); ok {
		return ck.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

func (c *tconn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	start := time.Now()
	res, err := e.ExecContext(ctx, q, args)
	if err != driver.ErrSkip {
		c.observe(q, start, err)
	}
	return res, err
}

func (c *tconn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	e, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	start := time.Now()
	rows, err := e.QueryContext(ctx, q, args)
	if err != driver.ErrSkip {
		c.observe(q, start, err)
	}
	return rows, err
}

func (c *tconn) observe(query string, start time.Time, err error) {
	end := time.Now()
	op := operation(query)

	c.t.dbDuration.Record(context.Background(), end.Sub(start).Seconds(), metric.WithAttributes(
		attribute.String("db.namespace", c.ns),
		attribute.String("db.operation.name", op),
	))

	parent, ok := c.t.requestContext()
	if !ok {
		return // background work: metric only, no orphan root spans
	}

	if len(query) > maxQueryTextLen {
		query = query[:maxQueryTextLen]
	}
	_, span := c.t.tracer.Start(parent, strings.ToUpper(op)+" "+c.ns,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithTimestamp(start),
		trace.WithAttributes(
			attribute.String("db.system.name", c.system),
			attribute.String("db.namespace", c.ns),
			attribute.String("db.operation.name", strings.ToUpper(op)),
			// placeholders only: bound values never reach this layer
			attribute.String("db.query.text", query),
		))
	if err != nil {
		// the error text is not recorded, only the fact
		span.SetStatus(codes.Error, "")
	}
	span.End(trace.WithTimestamp(end))
}

// operation returns a bounded-cardinality statement kind.
func operation(q string) string {
	q = strings.TrimSpace(q)
	if i := strings.IndexAny(q, " \t\r\n("); i > 0 {
		q = q[:i]
	}
	switch op := strings.ToLower(q); op {
	case "select", "insert", "update", "delete", "pragma", "begin", "commit", "rollback", "create", "drop", "alter", "with", "replace":
		return op
	}
	return "other"
}
