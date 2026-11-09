package keys_test

import (
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/tools/keys"
)

// Fork-local (Claviger #75): the cloud KMS sources and their build tags.

func TestCloudSourcesAreMutuallyExclusive(t *testing.T) {
	// the exclusion is decided before any service is contacted, so this holds with or without the tags
	_, err := keys.LoadFromEnv(env(map[string]string{
		keys.EnvKEKAWSKeyID: "arn:aws:kms:us-east-1:111122223333:key/x",
		keys.EnvKEKGCPKeyID: "projects/p/locations/l/keyRings/r/cryptoKeys/k",
	}), "")
	if err == nil || !strings.Contains(err.Error(), keys.EnvKEKAWSKeyID) || !strings.Contains(err.Error(), keys.EnvKEKGCPKeyID) {
		t.Fatalf("got %v", err)
	}

	_, err = keys.LoadFromEnv(env(map[string]string{keys.EnvKEKAzureKeyID: "https://v.vault.azure.net/keys/k", keys.EnvKEKGCPKeyID: "x"}), "")
	if err == nil || !strings.Contains(err.Error(), keys.EnvKEKAzureKeyID) || !strings.Contains(err.Error(), keys.EnvKEKGCPKeyID) {
		t.Fatalf("got %v", err)
	}

	_, err = keys.LoadOldFromEnv(env(map[string]string{keys.EnvKEKAWSKeyIDOld: "a", keys.EnvKEKGCPKeyIDOld: "b"}), "")
	if err == nil || !strings.Contains(err.Error(), "set only one") {
		t.Fatalf("got %v", err)
	}
}

func TestNoKeySourceHintNamesCloudSources(t *testing.T) {
	_, err := keys.LoadFromEnv(env(nil), "")
	if err == nil || !strings.Contains(err.Error(), keys.EnvKEKAWSKeyID) || !strings.Contains(err.Error(), keys.EnvKEKGCPKeyID) {
		t.Fatalf("got %v", err)
	}
}
