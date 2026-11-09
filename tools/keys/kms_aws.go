//go:build kms_aws

package keys

import (
	"context"
	"fmt"
	"strings"

	"github.com/doodlemania2/claviger/tools/keys/awskms"
	"github.com/doodlemania2/claviger/tools/keys/kmshttp"
)

// Fork-local (Claviger #75): AWS KMS as a KEK source (build tag kms_aws).

// AWSKMSBuilt reports whether this binary was built with AWS KMS support.
const AWSKMSBuilt = true

var awsOptions = func() kmshttp.Options { return kmshttp.Options{} }

// SetAWSClientOptions replaces the HTTP client, clock and endpoints used to
// reach AWS. It exists for tests and returns a function that restores the
// previous setting.
func SetAWSClientOptions(f func() kmshttp.Options) (restore func()) {
	prev := awsOptions
	awsOptions = f
	return func() { awsOptions = prev }
}

func newAWSSource(name, keyRef string, getenv func(string) string) (KeySource, error) {
	opts := awsOptions()

	region := strings.TrimSpace(getenv("AWS_REGION"))
	if region == "" {
		region = strings.TrimSpace(getenv("AWS_DEFAULT_REGION"))
	}

	creds, err := awskms.CredentialsFromEnv(getenv, region, opts)
	if err != nil {
		return nil, fmt.Errorf("aws kms key source: %w", err)
	}

	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = strings.TrimSpace(getenv("AWS_ENDPOINT_URL_KMS"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), kmsLoadTimeout)
	defer cancel()

	c, err := awskms.New(ctx, keyRef, region, creds, endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("aws kms key source: %w", err)
	}

	return &kmsSource{name: name, c: c, mismatch: awskms.ErrKeyMismatch, indet: kmshttp.IsIndeterminate}, nil
}
