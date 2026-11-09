//go:build !kms_aws || !kms_gcp

package keys_test

import (
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/tools/keys"
)

// Fork-local (Claviger #75): without the build tag, configuring the source must
// fail closed with a message that names the tag (never fall back to another key).
func TestCloudSourceWithoutBuildTagFailsClosed(t *testing.T) {
	cases := []struct {
		built bool
		name  string
		val   string
		tag   string
		old   string
	}{
		{keys.AWSKMSBuilt, keys.EnvKEKAWSKeyID, "arn:aws:kms:us-east-1:111122223333:key/x", "kms_aws", keys.EnvKEKAWSKeyIDOld},
		{keys.GCPKMSBuilt, keys.EnvKEKGCPKeyID, "projects/p/locations/l/keyRings/r/cryptoKeys/k", "kms_gcp", keys.EnvKEKGCPKeyIDOld},
	}

	for _, c := range cases {
		if c.built {
			continue
		}

		// even with a perfectly good local key and the app key present
		cfg := map[string]string{c.name: c.val, "PB_ENCRYPTION_KEY": strings.Repeat("x", 32)}
		src, err := keys.LoadFromEnv(env(cfg), "PB_ENCRYPTION_KEY")
		if err == nil || src != nil || !strings.Contains(err.Error(), "-tags "+c.tag) || !strings.Contains(err.Error(), c.name) {
			t.Fatalf("%s: got %v", c.name, err)
		}
		if keys.IsIndeterminate(err) {
			t.Fatalf("%s: a missing build tag is not an outage", c.name)
		}

		// and for the previous key of a rotation
		_, err = keys.LoadWithOld(env(map[string]string{keys.EnvKEK: b64Key(t), c.old: c.val}), "")
		if err == nil || !strings.Contains(err.Error(), "-tags "+c.tag) {
			t.Fatalf("%s: got %v", c.old, err)
		}
	}
}
