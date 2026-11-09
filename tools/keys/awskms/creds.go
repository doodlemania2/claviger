package awskms

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/doodlemania2/claviger/tools/keys/kmshttp"
)

// ErrNoCredentials is returned when no AWS credential source yields credentials.
var ErrNoCredentials = errors.New("no AWS credentials found (set AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, use a web identity or shared credentials file, or run on ECS/EC2 with a role)")

// Credentials are AWS access keys, possibly temporary.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Expires is zero for static credentials.
	Expires time.Time
}

// CredentialProvider yields credentials.
type CredentialProvider interface {
	Credentials(ctx context.Context) (Credentials, error)
	// Invalidate drops cached credentials (after an auth failure).
	Invalidate()
}

type cachedProvider struct {
	mu      sync.Mutex
	cur     Credentials
	have    bool
	now     func() time.Time
	refresh func(ctx context.Context) (Credentials, error)
}

func (p *cachedProvider) Credentials(ctx context.Context) (Credentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.have && (p.cur.Expires.IsZero() || p.now().Add(5*time.Minute).Before(p.cur.Expires)) {
		return p.cur, nil
	}

	c, err := p.refresh(ctx)
	if err != nil {
		return Credentials{}, err
	}
	p.cur, p.have = c, true
	return c, nil
}

func (p *cachedProvider) Invalidate() {
	p.mu.Lock()
	p.have = false
	p.mu.Unlock()
}

// CredentialsFromEnv selects the credential source, in the order of the AWS
// SDKs: environment keys, web identity (AWS_WEB_IDENTITY_TOKEN_FILE +
// AWS_ROLE_ARN), the shared credentials file, ECS task role, then the EC2
// instance profile (IMDSv2). getenv is os.Getenv in production.
func CredentialsFromEnv(getenv func(string) string, region string, opts kmshttp.Options) (CredentialProvider, error) {
	tr := kmshttp.NewTransport("aws credentials", opts)

	var refresh func(ctx context.Context) (Credentials, error)

	switch {
	case getenv("AWS_ACCESS_KEY_ID") != "" || getenv("AWS_SECRET_ACCESS_KEY") != "":
		c := Credentials{
			AccessKeyID:     getenv("AWS_ACCESS_KEY_ID"),
			SecretAccessKey: getenv("AWS_SECRET_ACCESS_KEY"),
			SessionToken:    getenv("AWS_SESSION_TOKEN"),
		}
		if c.AccessKeyID == "" || c.SecretAccessKey == "" {
			return nil, errors.New("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must both be set")
		}
		refresh = func(context.Context) (Credentials, error) { return c, nil }

	case getenv("AWS_WEB_IDENTITY_TOKEN_FILE") != "" && getenv("AWS_ROLE_ARN") != "":
		file, role, session := getenv("AWS_WEB_IDENTITY_TOKEN_FILE"), getenv("AWS_ROLE_ARN"), getenv("AWS_ROLE_SESSION_NAME")
		if session == "" {
			session = "claviger"
		}
		endpoint := stsEndpoint(getenv, region)
		refresh = func(ctx context.Context) (Credentials, error) {
			return webIdentity(ctx, tr, endpoint, file, role, session)
		}

	case sharedFileHasProfile(getenv):
		c, err := fromSharedFile(getenv)
		if err != nil {
			return nil, err
		}
		refresh = func(context.Context) (Credentials, error) { return c, nil }

	case getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" || getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "":
		var u string
		if rel := getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"); rel != "" {
			base := opts.MetadataEndpoint
			if base == "" {
				base = "http://169.254.170.2"
			}
			u = base + rel
		} else {
			u = getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI")
		}
		tokenFile, token := getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"), getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN")
		refresh = func(ctx context.Context) (Credentials, error) {
			if tokenFile != "" {
				b, err := os.ReadFile(tokenFile)
				if err != nil {
					return Credentials{}, fmt.Errorf("cannot read AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE: %w", err)
				}
				token = strings.TrimSpace(string(b))
			}
			return containerCreds(ctx, tr, u, token)
		}

	default:
		base := opts.MetadataEndpoint
		if base == "" {
			base = "http://169.254.169.254"
		}
		if getenv("AWS_EC2_METADATA_DISABLED") == "true" {
			return nil, ErrNoCredentials
		}
		refresh = func(ctx context.Context) (Credentials, error) { return imdsCreds(ctx, tr, base) }
	}

	return &cachedProvider{now: tr.Now, refresh: refresh}, nil
}

func stsEndpoint(getenv func(string) string, region string) string {
	if v := getenv("AWS_ENDPOINT_URL_STS"); v != "" {
		return v
	}
	if region == "" {
		return "https://sts.amazonaws.com"
	}
	return "https://sts." + region + ".amazonaws.com"
}

func sharedFilePath(getenv func(string) string) string {
	if p := getenv("AWS_SHARED_CREDENTIALS_FILE"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".aws", "credentials")
}

func profileName(getenv func(string) string) string {
	if p := getenv("AWS_PROFILE"); p != "" {
		return p
	}
	return "default"
}

func sharedFileHasProfile(getenv func(string) string) bool {
	// an explicit file or profile always selects this source (and then fails
	// loudly if it is unusable); otherwise only when the default file exists
	if getenv("AWS_SHARED_CREDENTIALS_FILE") != "" || getenv("AWS_PROFILE") != "" {
		return true
	}
	p := sharedFilePath(getenv)
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// fromSharedFile reads static keys of one profile from the shared credentials
// file. Profiles that need role assumption or SSO are not supported.
func fromSharedFile(getenv func(string) string) (Credentials, error) {
	path, profile := sharedFilePath(getenv), profileName(getenv)

	f, err := os.Open(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("cannot read the AWS shared credentials file: %w", err)
	}
	defer f.Close()

	var c Credentials
	in := false
	found := false

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' && strings.HasSuffix(line, "]") {
			in = strings.TrimSpace(line[1:len(line)-1]) == profile
			found = found || in
			continue
		}
		if !in {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "aws_access_key_id":
			c.AccessKeyID = strings.TrimSpace(v)
		case "aws_secret_access_key":
			c.SecretAccessKey = strings.TrimSpace(v)
		case "aws_session_token":
			c.SessionToken = strings.TrimSpace(v)
		}
	}

	if !found {
		return Credentials{}, fmt.Errorf("profile %q not found in the AWS shared credentials file", profile)
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return Credentials{}, fmt.Errorf("profile %q has no static access keys (role assumption and SSO profiles are not supported; use environment credentials or a role on the host)", profile)
	}
	return c, nil
}

type credsJSON struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
	Code            string `json:"Code"`
}

func (j credsJSON) toCreds() (Credentials, error) {
	if j.AccessKeyID == "" || j.SecretAccessKey == "" {
		return Credentials{}, errors.New("the credentials response has no keys")
	}
	c := Credentials{AccessKeyID: j.AccessKeyID, SecretAccessKey: j.SecretAccessKey, SessionToken: j.Token}
	if j.Expiration != "" {
		t, err := time.Parse(time.RFC3339, j.Expiration)
		if err != nil {
			return Credentials{}, errors.New("the credentials response has an invalid expiration")
		}
		c.Expires = t
	}
	return c, nil
}

func credErr(op string, status int, err error) error {
	return &kmshttp.Error{Service: "aws credentials", Op: op, Status: status, Definite: status != 0, Err: err}
}

func containerCreds(ctx context.Context, tr *kmshttp.Transport, u, token string) (Credentials, error) {
	status, body, err := tr.Do(ctx, "container credentials", nil, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err == nil && token != "" {
			req.Header.Set("Authorization", token)
		}
		return req, err
	})
	if err != nil {
		return Credentials{}, err
	}
	if status != http.StatusOK {
		return Credentials{}, credErr("container credentials", status, nil)
	}
	var j credsJSON
	if err := json.Unmarshal(body, &j); err != nil {
		return Credentials{}, credErr("container credentials", status, errors.New("the response is not valid JSON"))
	}
	c, err := j.toCreds()
	if err != nil {
		return Credentials{}, credErr("container credentials", status, err)
	}
	return c, nil
}

func imdsCreds(ctx context.Context, tr *kmshttp.Transport, base string) (Credentials, error) {
	// IMDSv2: token first
	status, tok, err := tr.Do(ctx, "imds token", nil, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/latest/api/token", nil)
		if err == nil {
			req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "300")
		}
		return req, err
	})
	if err != nil {
		return Credentials{}, err
	}
	if status != http.StatusOK {
		return Credentials{}, fmt.Errorf("%w (instance metadata answered HTTP %d)", ErrNoCredentials, status)
	}
	token := strings.TrimSpace(string(tok))

	get := func(op, path string) ([]byte, error) {
		st, b, err := tr.Do(ctx, op, nil, func(ctx context.Context) (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
			if err == nil {
				req.Header.Set("X-aws-ec2-metadata-token", token)
			}
			return req, err
		})
		if err != nil {
			return nil, err
		}
		if st != http.StatusOK {
			return nil, credErr(op, st, nil)
		}
		return b, nil
	}

	roleList, err := get("imds role", "/latest/meta-data/iam/security-credentials/")
	if err != nil {
		return Credentials{}, err
	}
	role := strings.TrimSpace(strings.SplitN(string(roleList), "\n", 2)[0])
	if role == "" {
		return Credentials{}, fmt.Errorf("%w (the instance has no role attached)", ErrNoCredentials)
	}

	b, err := get("imds credentials", "/latest/meta-data/iam/security-credentials/"+url.PathEscape(role))
	if err != nil {
		return Credentials{}, err
	}
	var j credsJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return Credentials{}, credErr("imds credentials", 200, errors.New("the response is not valid JSON"))
	}
	c, err := j.toCreds()
	if err != nil {
		return Credentials{}, credErr("imds credentials", 200, err)
	}
	return c, nil
}

func webIdentity(ctx context.Context, tr *kmshttp.Transport, endpoint, tokenFile, role, session string) (Credentials, error) {
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return Credentials{}, fmt.Errorf("cannot read AWS_WEB_IDENTITY_TOKEN_FILE: %w", err)
	}

	form := url.Values{
		"Action":           {"AssumeRoleWithWebIdentity"},
		"Version":          {"2011-06-15"},
		"RoleArn":          {role},
		"RoleSessionName":  {session},
		"WebIdentityToken": {strings.TrimSpace(string(raw))},
	}.Encode()

	status, body, err := tr.Do(ctx, "assume role with web identity", nil, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/", strings.NewReader(form))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		return req, err
	})
	if err != nil {
		return Credentials{}, err
	}
	if status != http.StatusOK {
		var e struct {
			Error struct{ Code, Message string }
		}
		_ = xml.NewDecoder(io.LimitReader(strings.NewReader(string(body)), kmshttp.MaxBody)).Decode(&e)
		return Credentials{}, &kmshttp.Error{Service: "aws sts", Op: "assume role with web identity", Status: status, Definite: true,
			Code: kmshttp.Shorten(e.Error.Code), Message: kmshttp.Shorten(e.Error.Message)}
	}

	var r struct {
		Result struct {
			Credentials struct {
				AccessKeyID     string `xml:"AccessKeyId"`
				SecretAccessKey string `xml:"SecretAccessKey"`
				SessionToken    string `xml:"SessionToken"`
				Expiration      string `xml:"Expiration"`
			} `xml:"Credentials"`
		} `xml:"AssumeRoleWithWebIdentityResult"`
	}
	if err := xml.Unmarshal(body, &r); err != nil {
		return Credentials{}, credErr("assume role with web identity", status, errors.New("the response is not valid XML"))
	}
	j := credsJSON{
		AccessKeyID: r.Result.Credentials.AccessKeyID, SecretAccessKey: r.Result.Credentials.SecretAccessKey,
		Token: r.Result.Credentials.SessionToken, Expiration: r.Result.Credentials.Expiration,
	}
	c, err := j.toCreds()
	if err != nil {
		return Credentials{}, credErr("assume role with web identity", status, err)
	}
	return c, nil
}
