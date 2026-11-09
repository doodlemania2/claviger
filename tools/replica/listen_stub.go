//go:build no_default_driver

package replica

import (
	"context"
	"errors"
)

func connectListener(context.Context, string) (listener, error) {
	return nil, errors.New("PostgreSQL support is not compiled in (no_default_driver build tag)")
}
