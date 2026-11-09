// Package awskms wraps and unwraps data keys with an AWS KMS symmetric key,
// calling the KMS JSON API directly (SigV4, standard library only). The key
// never leaves KMS. Fork-local (Claviger #75).
package awskms

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

	"github.com/doodlemania2/claviger/tools/keys/kmshttp"
)

// KeyIDPrefix starts every key id produced by this package.
const KeyIDPrefix = "awskms:"

// ErrKeyMismatch is returned by Unwrap when the key id belongs to another KMS key.
var ErrKeyMismatch = errors.New("the key id belongs to a different KMS key")

const service = "aws kms"

// Client wraps and unwraps data keys with one KMS key.
type Client struct {
	tr       *kmshttp.Transport
	creds    CredentialProvider
	region   string
	endpoint string
	arn      string
}

// New resolves the key (one DescribeKey call) and returns a client.
//
// keyRef is a key ARN (preferred), a key id, or an alias ("alias/name" or an
// alias ARN). The key is resolved to its ARN now, so the stored key id stays
// stable if an alias is later pointed at another key. region is needed when
// keyRef is not an ARN.
func New(ctx context.Context, keyRef, region string, creds CredentialProvider, endpointOverride string, opts kmshttp.Options) (*Client, error) {
	keyRef = strings.TrimSpace(keyRef)
	if keyRef == "" {
		return nil, errors.New("the KMS key id is empty")
	}

	if strings.HasPrefix(keyRef, "arn:") {
		parts := strings.Split(keyRef, ":")
		if len(parts) < 6 || parts[2] != "kms" || parts[3] == "" {
			return nil, errors.New("the KMS key ARN must look like arn:aws:kms:<region>:<account>:key/<id>")
		}
		region = parts[3]
	}
	if region == "" {
		return nil, errors.New("the AWS region is unknown: use a key ARN or set AWS_REGION")
	}

	endpoint := endpointOverride
	if endpoint == "" {
		suffix := "amazonaws.com"
		if strings.HasPrefix(region, "cn-") {
			suffix = "amazonaws.com.cn"
		}
		endpoint = "https://kms." + region + "." + suffix
	}
	if u, err := url.Parse(endpoint); err != nil || u.Host == "" {
		return nil, errors.New("the KMS endpoint is not a valid URL")
	}

	c := &Client{
		tr: kmshttp.NewTransport(service, opts), creds: creds,
		region: region, endpoint: strings.TrimRight(endpoint, "/"),
	}

	var out struct {
		KeyMetadata struct {
			Arn      string `json:"Arn"`
			Enabled  bool   `json:"Enabled"`
			KeyState string `json:"KeyState"`
			KeyUsage string `json:"KeyUsage"`
			KeySpec  string `json:"KeySpec"`
		} `json:"KeyMetadata"`
	}
	if err := c.call(ctx, "DescribeKey", map[string]any{"KeyId": keyRef}, &out); err != nil {
		return nil, err
	}

	m := out.KeyMetadata
	switch {
	case m.Arn == "":
		return nil, &kmshttp.Error{Service: service, Op: "DescribeKey", Definite: true, Message: "the response has no key ARN"}
	case m.KeyState != "" && m.KeyState != "Enabled":
		return nil, &kmshttp.Error{Service: service, Op: "DescribeKey", Definite: true, Message: "the key is in state " + kmshttp.Shorten(m.KeyState) + ", not Enabled"}
	case m.KeyUsage != "" && m.KeyUsage != "ENCRYPT_DECRYPT":
		return nil, &kmshttp.Error{Service: service, Op: "DescribeKey", Definite: true, Message: "the key usage is " + kmshttp.Shorten(m.KeyUsage) + "; use a symmetric ENCRYPT_DECRYPT key"}
	case m.KeySpec != "" && m.KeySpec != "SYMMETRIC_DEFAULT":
		return nil, &kmshttp.Error{Service: service, Op: "DescribeKey", Definite: true, Message: "the key spec is " + kmshttp.Shorten(m.KeySpec) + "; use a symmetric (SYMMETRIC_DEFAULT) key"}
	}
	c.arn = m.Arn

	return c, nil
}

// KeyID is "awskms:<key ARN>". Automatic rotation of the key material in KMS
// keeps the ARN, and old ciphertexts stay readable, so it needs no re-wrap.
func (c *Client) KeyID() string { return KeyIDPrefix + c.arn }

var encryptionContext = map[string]string{"purpose": "claviger-dek-wrap"}

// Wrap wraps a data key.
func (c *Client) Wrap(ctx context.Context, dek []byte) ([]byte, error) {
	var out struct {
		CiphertextBlob string `json:"CiphertextBlob"`
	}
	err := c.call(ctx, "Encrypt", map[string]any{
		"KeyId":             c.arn,
		"Plaintext":         base64.StdEncoding.EncodeToString(dek),
		"EncryptionContext": encryptionContext,
	}, &out)
	if err != nil {
		return nil, err
	}

	b, err := base64.StdEncoding.DecodeString(out.CiphertextBlob)
	if err != nil || len(b) == 0 {
		return nil, &kmshttp.Error{Service: service, Op: "Encrypt", Definite: true, Message: "the response has no usable ciphertext"}
	}
	return b, nil
}

// Unwrap unwraps a data key wrapped under keyID.
func (c *Client) Unwrap(ctx context.Context, keyID string, wrapped []byte) ([]byte, error) {
	if keyID != c.KeyID() {
		return nil, fmt.Errorf("%w (stored key id %q, configured key %q)", ErrKeyMismatch, keyID, c.KeyID())
	}

	var out struct {
		Plaintext string `json:"Plaintext"`
		KeyId     string `json:"KeyId"`
	}
	err := c.call(ctx, "Decrypt", map[string]any{
		"KeyId":             c.arn,
		"CiphertextBlob":    base64.StdEncoding.EncodeToString(wrapped),
		"EncryptionContext": encryptionContext,
	}, &out)
	if err != nil {
		return nil, err
	}

	if out.KeyId != "" && out.KeyId != c.arn {
		return nil, &kmshttp.Error{Service: service, Op: "Decrypt", Definite: true, Message: "the data key was decrypted by a different KMS key"}
	}

	b, err := base64.StdEncoding.DecodeString(out.Plaintext)
	if err != nil || len(b) == 0 {
		return nil, &kmshttp.Error{Service: service, Op: "Decrypt", Definite: true, Message: "the response has no usable plaintext"}
	}
	return b, nil
}

func throttled(status int, body []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	t := errType(body)
	return t == "ThrottlingException" || t == "LimitExceededException" || t == "RequestLimitExceeded" || t == "KMSInternalException" || t == "DependencyTimeoutException"
}

func errType(body []byte) string {
	var e struct {
		Type string `json:"__type"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &e)
	t := e.Type
	if t == "" {
		t = e.Code
	}
	// "com.amazonaws.kms#X" or "X:http://..."
	if i := strings.LastIndex(t, "#"); i >= 0 {
		t = t[i+1:]
	}
	t, _, _ = strings.Cut(t, ":")
	return t
}

func isCredentialError(t string) bool {
	switch t {
	case "ExpiredTokenException", "ExpiredToken", "UnrecognizedClientException", "InvalidSignatureException", "InvalidClientTokenId":
		return true
	}
	return false
}

// call sends one KMS action. A credential rejection drops the cached
// credentials and is retried once.
func (c *Client) call(ctx context.Context, action string, in map[string]any, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}

	for refreshed := false; ; refreshed = true {
		cr, err := c.creds.Credentials(ctx)
		if err != nil {
			return fmt.Errorf("aws credentials: %w", err)
		}

		status, body, err := c.tr.Do(ctx, action, throttled, func(ctx context.Context) (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/", bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/x-amz-json-1.1")
			req.Header.Set("X-Amz-Target", "TrentService."+action)
			signV4(req, payload, cr, c.region, "kms", c.tr.Now())
			return req, nil
		})
		if err != nil {
			return err
		}

		if status == http.StatusOK {
			if err := json.Unmarshal(body, out); err != nil {
				return &kmshttp.Error{Service: service, Op: action, Status: status, Definite: true, Message: "the response is not valid JSON"}
			}
			return nil
		}

		t := errType(body)
		if isCredentialError(t) && !refreshed {
			c.creds.Invalidate()
			continue
		}

		var e struct {
			Message string `json:"message"`
			Upper   string `json:"Message"`
		}
		_ = json.Unmarshal(body, &e)
		msg := e.Message
		if msg == "" {
			msg = e.Upper
		}
		return &kmshttp.Error{Service: service, Op: action, Status: status, Definite: true, Code: kmshttp.Shorten(t), Message: kmshttp.Shorten(msg)}
	}
}
