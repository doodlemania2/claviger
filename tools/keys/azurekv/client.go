package azurekv

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// APIVersion is the Key Vault data-plane API version used for every call.
const APIVersion = "7.5"

// KeyIDPrefix starts every key id produced by this package.
const KeyIDPrefix = "azkv:"

// Wrap algorithms.
const (
	algRSAOAEP256 = "RSA-OAEP-256"
	algA256KW     = "A256KW"
)

// Client wraps and unwraps data keys with one Key Vault key.
type Client struct {
	tr      *transport
	cred    TokenCredential
	scope   string
	baseURL string // https://host
	host    string
	name    string
	version string
	alg     string
}

// New resolves the key (one GET) and returns a client for it.
//
// keyURL is https://<vault>.vault.azure.net/keys/<name>[/<version>] (or a
// Managed HSM URL). Without a version the current version is resolved now and
// pinned for the life of the client; data keys wrapped with it stay readable
// after the key is rotated, because Unwrap uses the version stored in their
// key id.
func New(ctx context.Context, keyURL string, cred TokenCredential, opts Options) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(keyURL))
	if err != nil || u.Host == "" {
		return nil, errors.New("the Key Vault key URL is not a valid URL (expected https://<vault>.vault.azure.net/keys/<name>[/<version>])")
	}
	if u.Scheme != "https" {
		return nil, errors.New("the Key Vault key URL must use https")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("the Key Vault key URL must not carry a query, a fragment or credentials")
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "keys" || parts[1] == "" || (len(parts) == 3 && parts[2] == "") {
		return nil, errors.New("the Key Vault key URL must look like https://<vault>.vault.azure.net/keys/<name>[/<version>]")
	}

	c := &Client{
		tr:      newTransport(opts),
		cred:    cred,
		baseURL: "https://" + u.Host,
		host:    strings.ToLower(u.Host),
		name:    parts[1],
	}
	if len(parts) == 3 {
		c.version = parts[2]
	}

	c.scope = opts.Scope
	if c.scope == "" {
		c.scope = scopeForHost(c.host)
	}

	if err := c.resolve(ctx); err != nil {
		return nil, err
	}

	return c, nil
}

// scopeForHost derives the token scope from the vault host:
// <vault>.vault.azure.net -> https://vault.azure.net/.default, and
// <hsm>.managedhsm.azure.net -> https://managedhsm.azure.net/.default.
func scopeForHost(host string) string {
	host = strings.Split(host, ":")[0]
	if _, rest, ok := strings.Cut(host, "."); ok && (strings.HasPrefix(rest, "vault.") || strings.HasPrefix(rest, "managedhsm.")) {
		return "https://" + rest + "/.default"
	}
	return "https://vault.azure.net/.default"
}

type keyBundle struct {
	Key struct {
		Kid    string   `json:"kid"`
		Kty    string   `json:"kty"`
		KeyOps []string `json:"key_ops"`
	} `json:"key"`
	Attributes struct {
		Enabled *bool `json:"enabled"`
	} `json:"attributes"`
}

func (c *Client) resolve(ctx context.Context) error {
	path := "/keys/" + url.PathEscape(c.name)
	if c.version != "" {
		path += "/" + url.PathEscape(c.version)
	}

	status, body, err := c.call(ctx, "get key", http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return serviceError("get key", status, body)
	}

	var kb keyBundle
	if err := json.Unmarshal(body, &kb); err != nil {
		return &Error{Op: "get key", Status: status, Definite: true, Message: "the key response is not valid JSON"}
	}

	if kb.Attributes.Enabled != nil && !*kb.Attributes.Enabled {
		return &Error{Op: "get key", Definite: true, Message: "the key is disabled"}
	}

	if c.version == "" {
		// the kid is https://host/keys/<name>/<version>
		i := strings.LastIndex(kb.Key.Kid, "/")
		if i < 0 || i == len(kb.Key.Kid)-1 {
			return &Error{Op: "get key", Status: status, Definite: true, Message: "the key response has no usable key id"}
		}
		c.version = kb.Key.Kid[i+1:]
	}

	switch strings.ToUpper(kb.Key.Kty) {
	case "RSA", "RSA-HSM":
		c.alg = algRSAOAEP256
	case "OCT", "OCT-HSM":
		c.alg = algA256KW // Managed HSM symmetric keys
	default:
		return &Error{Op: "get key", Definite: true, Message: fmt.Sprintf("a %q key cannot wrap data keys; use an RSA key (3072 or 4096 bits recommended) or a Managed HSM AES key", kb.Key.Kty)}
	}

	if len(kb.Key.KeyOps) > 0 {
		var wrap, unwrap bool
		for _, op := range kb.Key.KeyOps {
			wrap = wrap || op == "wrapKey"
			unwrap = unwrap || op == "unwrapKey"
		}
		if !wrap || !unwrap {
			return &Error{Op: "get key", Definite: true, Message: "the key does not allow the wrapKey and unwrapKey operations"}
		}
	}

	return nil
}

// KeyID is "azkv:<host>/<name>/<version>": stable, non-secret, and carrying
// the version so that rotation in Key Vault is a KEK rotation.
func (c *Client) KeyID() string { return c.keyIDFor(c.version) }

func (c *Client) keyIDFor(version string) string {
	return KeyIDPrefix + c.host + "/" + c.name + "/" + version
}

// Algorithm returns the wrap algorithm in use.
func (c *Client) Algorithm() string { return c.alg }

type opRequest struct {
	Alg   string `json:"alg"`
	Value string `json:"value"`
}

type opResponse struct {
	Value string `json:"value"`
}

// Wrap wraps a data key with the pinned key version.
func (c *Client) Wrap(ctx context.Context, dek []byte) ([]byte, error) {
	return c.operate(ctx, "wrapkey", c.version, dek)
}

// Unwrap unwraps a data key that was wrapped under keyID, which may name an
// older version of the same key.
func (c *Client) Unwrap(ctx context.Context, keyID string, wrapped []byte) ([]byte, error) {
	version, ok := c.parseKeyID(keyID)
	if !ok {
		return nil, fmt.Errorf("%w (stored key id %q, configured key %q)", ErrKeyMismatch, keyID, c.KeyID())
	}

	return c.operate(ctx, "unwrapkey", version, wrapped)
}

// parseKeyID returns the version when keyID names this vault and key.
func (c *Client) parseKeyID(keyID string) (string, bool) {
	rest, ok := strings.CutPrefix(keyID, KeyIDPrefix)
	if !ok {
		return "", false
	}

	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] != c.host || parts[1] != c.name || parts[2] == "" {
		return "", false
	}

	return parts[2], true
}

func (c *Client) operate(ctx context.Context, op, version string, in []byte) ([]byte, error) {
	payload, err := json.Marshal(opRequest{Alg: c.alg, Value: base64.RawURLEncoding.EncodeToString(in)})
	if err != nil {
		return nil, err
	}

	path := "/keys/" + url.PathEscape(c.name) + "/" + url.PathEscape(version) + "/" + op

	status, body, err := c.call(ctx, op, http.MethodPost, path, payload)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, serviceError(op, status, body)
	}

	var r opResponse
	if err := json.Unmarshal(body, &r); err != nil || r.Value == "" {
		return nil, &Error{Op: op, Status: status, Definite: true, Message: "the response has no value"}
	}

	out, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(r.Value, "="))
	if err != nil {
		return nil, &Error{Op: op, Status: status, Definite: true, Message: "the response value is not base64url"}
	}

	return out, nil
}

// call sends an authenticated request. A 401 drops the cached token and is
// retried once with a fresh one.
func (c *Client) call(ctx context.Context, op, method, path string, payload []byte) (int, []byte, error) {
	for refreshed := false; ; refreshed = true {
		token, err := c.cred.Token(ctx, c.scope)
		if err != nil {
			return 0, nil, err
		}

		status, body, err := c.tr.do(ctx, op, nil, func(ctx context.Context) (*http.Request, error) {
			var r *bytes.Reader
			if payload != nil {
				r = bytes.NewReader(payload)
			} else {
				r = bytes.NewReader(nil)
			}

			req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path+"?api-version="+APIVersion, r)
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
			return 0, nil, err
		}

		if status == http.StatusUnauthorized && !refreshed {
			c.cred.Invalidate(c.scope)
			continue
		}

		return status, body, nil
	}
}
