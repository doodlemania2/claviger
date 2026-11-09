//go:build !no_default_driver

package replica

import (
	"context"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
)

type pgxListener struct{ conn *pgx.Conn }

func (l pgxListener) WaitForNotification(ctx context.Context) (string, error) {
	n, err := l.conn.WaitForNotification(ctx)
	if err != nil {
		return "", err
	}

	return n.Payload, nil
}

func (l pgxListener) Ping(ctx context.Context) error { return l.conn.Ping(ctx) }

func (l pgxListener) Close(ctx context.Context) error { return l.conn.Close(ctx) }

// connectListener opens the dedicated connection and LISTENs on [Channel].
func connectListener(parent context.Context, url string) (listener, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, err
	}

	// detect a vanished server in seconds, not after the OS default of hours
	cfg.DialFunc = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}).DialContext

	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}

	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		_ = conn.Close(context.Background())
		return nil, err
	}

	return pgxListener{conn}, nil
}
