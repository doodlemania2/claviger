// Package gcpkms wraps and unwraps data keys with a Google Cloud KMS
// symmetric key, calling the Cloud KMS REST API directly (standard library
// only). The key never leaves Cloud KMS. Fork-local (Claviger #75).
package gcpkms

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/doodlemania2/claviger/tools/keys/kmshttp"
)

// KeyIDPrefix starts every key id produced by this package.
const KeyIDPrefix = "gcpkms:"

// ErrKeyMismatch is returned by Unwrap when the key id belongs to another Cloud KMS key.
var ErrKeyMismatch = errors.New("the key id belongs to a different Cloud KMS key")

const (
	service         = "gcp kms"
	defaultEndpoint = "https://cloudkms.googleapis.com"
)

var keyNameRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+$`)

// Client wraps and unwraps data keys with one Cloud KMS key.
type Client struct {
	tr       *kmshttp.Transport
	ts       TokenSource
	endpoint string
	name     string
}

// New checks the key (one GET) and returns a client. name must be
// projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k> (no version: Cloud
// KMS decrypts with whichever version produced the ciphertext, so rotation of
// the key's versions needs no re-wrap).
func New(ctx context.Context, name string, ts TokenSource, opts kmshttp.Options) (*Client, error) {
	name = strings.Trim(strings.TrimSpace(name), "/")
	if !keyNameRE.MatchString(name) {
		return nil, errors.New("the Cloud KMS key must look like projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key> (without a version)")
	}

	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	if u, err := url.Parse(endpoint); err != nil || u.Host == "" {
		return nil, errors.New("the Cloud KMS endpoint is not a valid URL")
	}

	c := &Client{tr: kmshttp.NewTransport(service, opts), ts: ts, endpoint: strings.TrimRight(endpoint, "/"), name: name}

	var out struct {
		Purpose string `json:"purpose"`
		Primary *struct {
			State string `json:"state"`
		} `json:"primary"`
	}
	if err := c.call(ctx, "get key", http.MethodGet, "", nil, &out); err != nil {
		return nil, err
	}

	switch {
	case out.Purpose != "" && out.Purpose != "ENCRYPT_DECRYPT":
		return nil, &kmshttp.Error{Service: service, Op: "get key", Definite: true, Message: "the key purpose is " + kmshttp.Shorten(out.Purpose) + "; use a symmetric ENCRYPT_DECRYPT key"}
	case out.Primary == nil:
		return nil, &kmshttp.Error{Service: service, Op: "get key", Definite: true, Message: "the key has no primary version to encrypt with"}
	case out.Primary.State != "ENABLED":
		return nil, &kmshttp.Error{Service: service, Op: "get key", Definite: true, Message: "the primary key version is " + kmshttp.Shorten(out.Primary.State) + ", not ENABLED"}
	}

	return c, nil
}

// KeyID is "gcpkms:<key resource name>".
func (c *Client) KeyID() string { return KeyIDPrefix + c.name }

func (c *Client) aad() string {
	return base64.StdEncoding.EncodeToString([]byte("claviger/dek-wrap/v1|" + c.KeyID()))
}

// Wrap wraps a data key.
func (c *Client) Wrap(ctx context.Context, dek []byte) ([]byte, error) {
	var out struct {
		Ciphertext string `json:"ciphertext"`
	}
	err := c.call(ctx, "encrypt", http.MethodPost, ":encrypt", map[string]string{
		"plaintext":                   base64.StdEncoding.EncodeToString(dek),
		"additionalAuthenticatedData": c.aad(),
	}, &out)
	if err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(out.Ciphertext)
	if err != nil || len(b) == 0 {
		return nil, &kmshttp.Error{Service: service, Op: "encrypt", Definite: true, Message: "the response has no usable ciphertext"}
	}
	return b, nil
}

// Unwrap unwraps a data key wrapped under keyID.
func (c *Client) Unwrap(ctx context.Context, keyID string, wrapped []byte) ([]byte, error) {
	if keyID != c.KeyID() {
		return nil, fmt.Errorf("%w (stored key id %q, configured key %q)", ErrKeyMismatch, keyID, c.KeyID())
	}
	var out struct {
		Plaintext string `json:"plaintext"`
	}
	err := c.call(ctx, "decrypt", http.MethodPost, ":decrypt", map[string]string{
		"ciphertext":                  base64.StdEncoding.EncodeToString(wrapped),
		"additionalAuthenticatedData": c.aad(),
	}, &out)
	if err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(out.Plaintext)
	if err != nil || len(b) == 0 {
		return nil, &kmshttp.Error{Service: service, Op: "decrypt", Definite: true, Message: "the response has no usable plaintext"}
	}
	return b, nil
}

// call sends one request. A 401 drops the cached token and is retried once.
func (c *Client) call(ctx context.Context, op, method, suffix string, in any, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}

	for refreshed := false; ; refreshed = true {
		token, err := c.ts.Token(ctx)
		if err != nil {
			return err
		}

		status, body, err := c.tr.Do(ctx, op, nil, func(ctx context.Context) (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, method, c.endpoint+"/v1/"+c.name+suffix, bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Accept", "application/json")
			if payload != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			return req, nil
		})
		if err != nil {
			return err
		}

		if status == http.StatusUnauthorized && !refreshed {
			c.ts.Invalidate()
			continue
		}

		if status == http.StatusOK {
			if err := json.Unmarshal(body, out); err != nil {
				return &kmshttp.Error{Service: service, Op: op, Status: status, Definite: true, Message: "the response is not valid JSON"}
			}
			return nil
		}

		var e struct {
			Error struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return &kmshttp.Error{Service: service, Op: op, Status: status, Definite: true,
			Code: kmshttp.Shorten(e.Error.Status), Message: kmshttp.Shorten(e.Error.Message)}
	}
}
