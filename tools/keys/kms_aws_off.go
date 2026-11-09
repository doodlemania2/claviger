//go:build !kms_aws

package keys

// AWSKMSBuilt reports whether this binary was built with AWS KMS support.
const AWSKMSBuilt = false

func newAWSSource(_, _ string, _ func(string) string) (KeySource, error) {
	return nil, notBuilt(EnvKEKAWSKeyID+" / "+EnvKEKAWSKeyIDOld, "kms_aws")
}
