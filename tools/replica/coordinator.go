package replica

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"hash/fnv"
	"time"
)

// The tables of the multi replica mode. They are created on demand, under an
// advisory lock, by [EnsureSchema] (so two replicas that start together do not
// race on CREATE TABLE) and only when the mode is on: a single replica
// installation never has them.
const (
	// TablePayloads holds notification payloads that did not fit in NOTIFY.
	TablePayloads = "_claviger_event_payloads"
	// TableCronRuns records which replica ran which job in which minute.
	TableCronRuns = "_claviger_cron_runs"
	// TableRateLimits holds the shared counters of the security relevant limiters.
	TableRateLimits = "_claviger_rate_limits"
	// TableCeremonies holds the pending WebAuthn ceremony sessions.
	TableCeremonies = "_claviger_ceremonies"
)

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS ` + TablePayloads + ` (
		id TEXT PRIMARY KEY NOT NULL,
		payload TEXT NOT NULL,
		created BIGINT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS ` + TableCronRuns + ` (
		job TEXT NOT NULL,
		slot BIGINT NOT NULL,
		replica TEXT NOT NULL,
		ran BIGINT NOT NULL,
		PRIMARY KEY (job, slot)
	)`,
	`CREATE TABLE IF NOT EXISTS ` + TableRateLimits + ` (
		id TEXT PRIMARY KEY NOT NULL,
		rule TEXT NOT NULL,
		used INTEGER NOT NULL,
		expires BIGINT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS ` + TableCeremonies + ` (
		token TEXT PRIMARY KEY NOT NULL,
		payload TEXT NOT NULL,
		expires BIGINT NOT NULL
	)`,
}

// lockKey maps a name to the bigint key of an advisory lock. The names are
// prefixed, so they cannot collide with the migrations lock of tools/dialect.
func lockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("claviger-replica:" + name))
	return int64(h.Sum64())
}

// EnsureSchema creates the shared tables. It is safe to call from several
// replicas at the same time.
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// CREATE TABLE IF NOT EXISTS is not atomic on Postgres: two sessions can
	// both pass the existence check and one fails on pg_type. Serialize them.
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey("schema")); err != nil {
		return err
	}

	for _, stmt := range schemaStatements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// RunExclusive runs fn on this replica only, unless another replica holds the
// lock of the name (it is running the same job right now) or already ran the
// slot.
//
// The lock is a session level advisory lock (pg_try_advisory_lock) on a
// connection that is held for the duration of fn, and released with it. When
// the process dies the connection dies and Postgres frees the lock.
//
// slot, when not zero, makes the run at most once per slot (use the minute of
// the cron tick): the replica that reaches the lock second, a moment after the
// first one finished, must not run the tick again. Zero is for manual runs.
//
// A replica that crashes inside fn consumes the slot: the job is not retried
// before its next schedule. That is the documented trade for never running a
// job twice.
func RunExclusive(ctx context.Context, db *sql.DB, replicaID, name string, slot int64, fn func()) (bool, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	key := lockKey("job:" + name)

	var got bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&got); err != nil {
		return false, err
	}
	if !got {
		return false, nil
	}

	defer func() {
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var ok bool
		if err := conn.QueryRowContext(uctx, "SELECT pg_advisory_unlock($1)", key).Scan(&ok); err != nil || !ok {
			// never hand a connection that may still hold the lock back to the pool
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()

	if slot != 0 {
		res, err := conn.ExecContext(ctx,
			"INSERT INTO "+TableCronRuns+" (job, slot, replica, ran) VALUES ($1, $2, $3, $4) ON CONFLICT (job, slot) DO NOTHING",
			name, slot, replicaID, time.Now().Unix(),
		)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return false, nil
		}

		// the table is a short lived marker: keep it small
		_, _ = conn.ExecContext(ctx, "DELETE FROM "+TableCronRuns+" WHERE job = $1 AND slot < $2", name, slot-2*86400)
	}

	fn()

	return true, nil
}

// TryLock takes the session level advisory lock of the name without waiting.
// The returned func releases it. ok is false when another session holds it.
func TryLock(ctx context.Context, db *sql.DB, name string) (release func(), ok bool, err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}

	key := lockKey("lock:" + name)

	var got bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, false, err
	}
	if !got {
		_ = conn.Close()
		return nil, false, nil
	}

	return func() {
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var released bool
		if err := conn.QueryRowContext(uctx, "SELECT pg_advisory_unlock($1)", key).Scan(&released); err != nil || !released {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}, true, nil
}

// RateHit counts one request of the id in a fixed window of windowSec seconds
// and returns the number used in the current window, this one included. The
// window starts at the first request after the previous one expired, exactly
// like the in memory limiter. It is one atomic statement.
func RateHit(ctx context.Context, db *sql.DB, id, rule string, windowSec int64) (used int, expires int64, err error) {
	now := time.Now().Unix()

	err = db.QueryRowContext(ctx, fmt.Sprintf(`
		INSERT INTO %[1]s AS r (id, rule, used, expires) VALUES ($1, $2, 1, $4)
		ON CONFLICT (id) DO UPDATE SET
			used = CASE WHEN r.expires <= $3 THEN 1 ELSE r.used + 1 END,
			expires = CASE WHEN r.expires <= $3 THEN $4 ELSE r.expires END
		RETURNING used, expires`, TableRateLimits),
		id, rule, now, now+windowSec,
	).Scan(&used, &expires)

	return used, expires, err
}

// RatePeek returns the live counter of the id without counting a request
// (zero when there is none or it expired).
func RatePeek(ctx context.Context, db *sql.DB, id string) (used int, expires int64, err error) {
	err = db.QueryRowContext(ctx,
		"SELECT used, expires FROM "+TableRateLimits+" WHERE id = $1 AND expires > $2",
		id, time.Now().Unix(),
	).Scan(&used, &expires)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}

	return used, expires, err
}

// PurgeRateLimits deletes the expired shared counters.
func PurgeRateLimits(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "DELETE FROM "+TableRateLimits+" WHERE expires < $1", time.Now().Unix()-60)
	return err
}

// PutCeremony stores a pending ceremony session until expires (unix seconds).
func PutCeremony(ctx context.Context, db *sql.DB, token string, payload []byte, expires int64) error {
	_, err := db.ExecContext(ctx,
		"INSERT INTO "+TableCeremonies+" (token, payload, expires) VALUES ($1, $2, $3) ON CONFLICT (token) DO UPDATE SET payload = EXCLUDED.payload, expires = EXCLUDED.expires",
		token, string(payload), expires,
	)

	return err
}

// GetCeremony returns a live ceremony session (nil when missing or expired).
func GetCeremony(ctx context.Context, db *sql.DB, token string) ([]byte, error) {
	var s string
	err := db.QueryRowContext(ctx,
		"SELECT payload FROM "+TableCeremonies+" WHERE token = $1 AND expires > $2",
		token, time.Now().Unix(),
	).Scan(&s)
	if err == sql.ErrNoRows {
		return nil, nil
	}

	return []byte(s), err
}

// TakeCeremony atomically removes and returns a live ceremony session, so that
// it can be used once even when two replicas race on the same token.
func TakeCeremony(ctx context.Context, db *sql.DB, token string) ([]byte, error) {
	var payload string
	var expires int64

	err := db.QueryRowContext(ctx,
		"DELETE FROM "+TableCeremonies+" WHERE token = $1 RETURNING payload, expires",
		token,
	).Scan(&payload, &expires)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if expires <= time.Now().Unix() {
		return nil, nil
	}

	return []byte(payload), nil
}

// DeleteCeremony removes a ceremony session.
func DeleteCeremony(ctx context.Context, db *sql.DB, token string) error {
	_, err := db.ExecContext(ctx, "DELETE FROM "+TableCeremonies+" WHERE token = $1", token)
	return err
}

// PurgeCeremonies deletes the expired ceremony sessions.
func PurgeCeremonies(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "DELETE FROM "+TableCeremonies+" WHERE expires < $1", time.Now().Unix())
	return err
}
