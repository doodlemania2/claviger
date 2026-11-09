//go:build !kms_gcp

package keys

// GCPKMSBuilt reports whether this binary was built with Google Cloud KMS support.
const GCPKMSBuilt = false

func newGCPSource(_, _ string, _ func(string) string) (KeySource, error) {
	return nil, notBuilt(EnvKEKGCPKeyID+" / "+EnvKEKGCPKeyIDOld, "kms_gcp")
}
