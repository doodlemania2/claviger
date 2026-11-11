package sms

import (
	"errors"
	"net/http"
	"net/url"
	"time"
)

const (
	defaultTimeout = 15 * time.Second
	maxResponse    = 64 << 10
)

// unwrapURLError drops the "Post <url>:" prefix of a *url.Error.
func unwrapURLError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

func newClient(c *http.Client, timeout time.Duration) *http.Client {
	if c == nil {
		c = &http.Client{}
	}
	cp := *c
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	cp.Timeout = timeout
	// never follow redirects: they would forward credentials elsewhere
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cp
}
