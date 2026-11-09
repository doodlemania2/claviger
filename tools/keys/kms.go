package keys

// Fork-local (Claviger #75): AWS KMS and Google Cloud KMS as KEK sources.
//
// The clients (tools/keys/awskms, tools/keys/gcpkms) are plain standard-library
// code and always compile. Only the glue that links them into the KEK loader
// sits behind the build tags kms_aws and kms_gcp, so a default build does not
// carry them. Without the tag, configuring the variable fails closed.

// Environment variable names of the cloud KMS sources. Credentials use the
// providers' standard variables (AWS_*, GOOGLE_APPLICATION_CREDENTIALS).
const (
	// EnvKEKAWSKeyID selects AWS KMS as the KEK source: a key ARN (preferred),
	// key id or alias. It excludes every other key source. Needs the kms_aws build tag.
	EnvKEKAWSKeyID = "CLAVIGER_KEK_AWS_KEY_ID"

	// EnvKEKAWSKeyIDOld names the previous AWS KMS key during a rotation.
	EnvKEKAWSKeyIDOld = "CLAVIGER_KEK_AWS_KEY_ID_OLD"

	// EnvKEKGCPKeyID selects Google Cloud KMS as the KEK source:
	// projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>. It excludes every
	// other key source. Needs the kms_gcp build tag.
	EnvKEKGCPKeyID = "CLAVIGER_KEK_GCP_KEY_ID"

	// EnvKEKGCPKeyIDOld names the previous Cloud KMS key during a rotation.
	EnvKEKGCPKeyIDOld = "CLAVIGER_KEK_GCP_KEY_ID_OLD"
)
