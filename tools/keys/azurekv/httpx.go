package azurekv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

const maxBody = 1 << 20

// Options tunes a client or a credential. The zero value is production-ready.
// The function fields exist for tests.
type Options struct {
	// HTTPClient defaults to a client with no redirects followed and a
	// per-attempt Timeout.
	HTTPClient *http.Client

	// Timeout is the per-attempt timeout (default 10s).
	Timeout time.Duration

	// MaxAttempts bounds the attempts per call (default 4).
	MaxAttempts int

	// Now, Sleep and Jitter default to the real clock and math/rand/v2.
	// Jitter returns a value in [0,1).
	Now    func() time.Time
	Sleep  func(ctx context.Context, d time.Duration) error
	Jitter func() float64

	// Scope overrides the token scope derived from the vault host.
	Scope string

	// IMDSEndpoint overrides http://169.254.169.254/metadata/identity/oauth2/token.
	IMDSEndpoint string
}

type transport struct {
	client      *http.Client
	timeout     time.Duration
	maxAttempts int
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) error
	jitter      func() float64
}

func newTransport(o Options) *transport {
	t := &transport{
		client:      o.HTTPClient,
		timeout:     o.Timeout,
		maxAttempts: o.MaxAttempts,
		now:         o.Now,
		sleep:       o.Sleep,
		jitter:      o.Jitter,
	}

	if t.client == nil {
		t.client = &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if t.timeout <= 0 {
		t.timeout = 10 * time.Second
	}
	if t.maxAttempts <= 0 {
		t.maxAttempts = 4
	}
	if t.now == nil {
		t.now = time.Now
	}
	if t.sleep == nil {
		t.sleep = func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-timer.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if t.jitter == nil {
		t.jitter = rand.Float64
	}

	return t
}

func defaultRetryable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// do sends the request built by build, retrying 429/5xx/network errors with
// exponential backoff and jitter. The returned status/body are those of the
// last response that was not retried. A retryable failure that outlasts the
// attempts is returned as an indeterminate *Error.
func (t *transport) do(ctx context.Context, op string, retryable func(int) bool, build func(ctx context.Context) (*http.Request, error)) (int, []byte, error) {
	if retryable == nil {
		retryable = defaultRetryable
	}

	var last *Error

	for attempt := 0; attempt < t.maxAttempts; attempt++ {
		if attempt > 0 {
			if err := t.sleep(ctx, t.backoff(attempt, last)); err != nil {
				return 0, nil, &Error{Op: op, Err: err}
			}
		}

		if err := ctx.Err(); err != nil {
			return 0, nil, &Error{Op: op, Err: err}
		}

		actx, cancel := context.WithTimeout(ctx, t.timeout)
		req, err := build(actx)
		if err != nil {
			cancel()
			return 0, nil, &Error{Op: op, Definite: true, Err: err}
		}

		resp, err := t.client.Do(req)
		if err != nil {
			cancel()
			last = &Error{Op: op, Err: sanitizeTransportErr(err)}
			if ctx.Err() != nil {
				return 0, nil, last
			}
			continue
		}

		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
		cancel()

		if rerr != nil {
			last = &Error{Op: op, Status: resp.StatusCode, Err: sanitizeTransportErr(rerr)}
			continue
		}

		if retryable(resp.StatusCode) {
			last = &Error{Op: op, Status: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), t.now())}
			continue
		}

		return resp.StatusCode, body, nil
	}

	return 0, nil, last.public()
}

func (t *transport) backoff(attempt int, last *Error) time.Duration {
	d := 200 * time.Millisecond << (attempt - 1)
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	// full jitter on the upper half
	d = d/2 + time.Duration(t.jitter()*float64(d/2))

	if last != nil && last.retryAfter > d {
		d = last.retryAfter
		if d > 30*time.Second {
			d = 30 * time.Second
		}
	}

	return d
}

func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if tm, err := http.ParseTime(v); err == nil && tm.After(now) {
		return tm.Sub(now)
	}
	return 0
}

// sanitizeTransportErr drops the URL from net/url errors (it could carry a
// query string) and keeps the cause.
func sanitizeTransportErr(err error) error {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			return inner
		}
	}
	return err
}

// serviceError turns a non-success response into a definite *Error.
func serviceError(op string, status int, body []byte) *Error {
	e := &Error{Op: op, Status: status, Definite: true}

	var kv struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		// AAD / IMDS style
		AADError string `json:"error_description"`
	}
	if json.Unmarshal(body, &kv) == nil {
		e.Code = shorten(kv.Error.Code)
		e.Message = shorten(kv.Error.Message + kv.AADError)
	}

	return e
}
