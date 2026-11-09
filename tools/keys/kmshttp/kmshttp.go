// Package kmshttp is the small HTTP layer shared by the AWS KMS and Google
// Cloud KMS key sources (Claviger #75): bounded retries with backoff and
// jitter, and an error type that says whether a failure is definite (the
// service answered and refused) or indeterminate (timeout, network, throttling,
// 5xx). It uses only the standard library.
package kmshttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MaxBody bounds every response body that is read.
const MaxBody = 1 << 20

// Options tunes a client. The zero value is production-ready; the function
// fields exist for tests.
type Options struct {
	// HTTPClient defaults to a client that follows no redirects.
	HTTPClient *http.Client
	// Timeout is the per-attempt timeout (default 10s).
	Timeout time.Duration
	// MaxAttempts bounds the attempts per call (default 4).
	MaxAttempts int
	// Now, Sleep and Jitter default to the real clock and math/rand/v2.
	Now    func() time.Time
	Sleep  func(ctx context.Context, d time.Duration) error
	Jitter func() float64

	// Endpoint overrides the service endpoint (tests, VPC endpoints, emulators).
	Endpoint string
	// MetadataEndpoint overrides the instance metadata base URL (tests).
	MetadataEndpoint string
}

// Error is a failed call to a key service.
type Error struct {
	Service  string // "aws kms", "gcp kms", ...
	Op       string
	Status   int // 0 when there was no response
	Code     string
	Message  string
	Definite bool
	Err      error

	retryAfter time.Duration
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Service + ": " + e.Op + " failed")
	if e.Status != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.Status)
	}
	if e.Code != "" {
		b.WriteString(": " + e.Code)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	if e.Err != nil {
		b.WriteString(": " + e.Err.Error())
	}
	if !e.Definite {
		b.WriteString(" (inconclusive: the service could not be reached or is unavailable)")
	}
	return b.String()
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// Indeterminate reports whether the failure says nothing about whether the
// credentials and the key are right.
func (e *Error) Indeterminate() bool { return !e.Definite }

// IsIndeterminate reports whether err wraps an indeterminate failure.
func IsIndeterminate(err error) bool {
	var i interface{ Indeterminate() bool }
	return errors.As(err, &i) && i.Indeterminate()
}

// Shorten keeps service messages short and on one line.
func Shorten(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 240 {
		s = s[:240] + "..."
	}
	return s
}

// Transport sends requests with retries.
type Transport struct {
	client      *http.Client
	timeout     time.Duration
	maxAttempts int
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) error
	jitter      func() float64
	service     string
}

// NewTransport builds a transport for the named service.
func NewTransport(service string, o Options) *Transport {
	t := &Transport{
		client: o.HTTPClient, timeout: o.Timeout, maxAttempts: o.MaxAttempts,
		now: o.Now, sleep: o.Sleep, jitter: o.Jitter, service: service,
	}
	if t.client == nil {
		t.client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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

// Now returns the transport clock.
func (t *Transport) Now() time.Time { return t.now() }

// Retryable reports the statuses that are retried by default.
func Retryable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// Do sends the request built by build (called once per attempt, so signatures
// and timestamps are fresh), retrying 408/429/5xx and network errors. extra,
// when non-nil, adds service-specific retryable statuses (e.g. AWS throttling
// reported as 400).
func (t *Transport) Do(ctx context.Context, op string, extra func(status int, body []byte) bool, build func(ctx context.Context) (*http.Request, error)) (int, []byte, error) {
	var last *Error

	for attempt := 0; attempt < t.maxAttempts; attempt++ {
		if attempt > 0 {
			if err := t.sleep(ctx, t.backoff(attempt, last)); err != nil {
				return 0, nil, &Error{Service: t.service, Op: op, Err: err}
			}
		}
		if err := ctx.Err(); err != nil {
			return 0, nil, &Error{Service: t.service, Op: op, Err: err}
		}

		actx, cancel := context.WithTimeout(ctx, t.timeout)
		req, err := build(actx)
		if err != nil {
			cancel()
			return 0, nil, &Error{Service: t.service, Op: op, Definite: true, Err: err}
		}

		resp, err := t.client.Do(req)
		if err != nil {
			cancel()
			last = &Error{Service: t.service, Op: op, Err: sanitize(err)}
			if ctx.Err() != nil {
				return 0, nil, last
			}
			continue
		}

		body, rerr := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
		_ = resp.Body.Close()
		cancel()

		if rerr != nil {
			last = &Error{Service: t.service, Op: op, Status: resp.StatusCode, Err: sanitize(rerr)}
			continue
		}

		if Retryable(resp.StatusCode) || (extra != nil && extra(resp.StatusCode, body)) {
			last = &Error{Service: t.service, Op: op, Status: resp.StatusCode, retryAfter: retryAfter(resp.Header.Get("Retry-After"), t.now())}
			continue
		}

		return resp.StatusCode, body, nil
	}

	c := *last
	c.retryAfter = 0
	return 0, nil, &c
}

func (t *Transport) backoff(attempt int, last *Error) time.Duration {
	d := 200 * time.Millisecond << (attempt - 1)
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	d = d/2 + time.Duration(t.jitter()*float64(d/2))
	if last != nil && last.retryAfter > d {
		d = min(last.retryAfter, 30*time.Second)
	}
	return d
}

func retryAfter(v string, now time.Time) time.Duration {
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

// sanitize drops the URL from net/url errors (it could carry a query string).
func sanitize(err error) error {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			return inner
		}
	}
	return err
}
