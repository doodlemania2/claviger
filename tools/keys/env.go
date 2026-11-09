package keys

import (
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
)

// hkdfInfo domain-separates the KEK derived from the app encryption key from
// the use of that same string as the settings key.
const hkdfInfo = "claviger/kek/v1"

// appKeyLength is the length the app encryption key must have (it is also the
// settings AES-256 key, see tools/security).
const appKeyLength = 32

// LoadFromEnv builds the KEK source from the process environment.
//
// getenv is os.Getenv in production. appKeyEnv is the NAME of the variable
// holding the app encryption key (the --encryptionEnv flag, usually
// PB_ENCRYPTION_KEY); it may be empty.
//
// Precedence: CLAVIGER_KEK, then CLAVIGER_KEK_FILE, then the key derived from
// the app encryption key with HKDF-SHA256. Setting both CLAVIGER_KEK and
// CLAVIGER_KEK_FILE is an error. It returns [ErrNoKeySource] (wrapped) when
// none is set, and a descriptive error naming the variable (never its value)
// when one is set but unusable.
func LoadFromEnv(getenv func(string) string, appKeyEnv string) (KeySource, error) {
	return loadPrimary(getenv, appKeyEnv)
}

func loadPrimary(getenv func(string) string, appKeyEnv string) (KeySource, error) {
	direct := strings.TrimSpace(getenv(EnvKEK))
	file := strings.TrimSpace(getenv(EnvKEKFile))
	azure := azureKeyIDFromEnv(getenv, EnvKEKAzureKeyID)
	aws := strings.TrimSpace(getenv(EnvKEKAWSKeyID))
	gcp := strings.TrimSpace(getenv(EnvKEKGCPKeyID))

	var cloud []string
	for n, v := range map[string]string{EnvKEKAzureKeyID: azure, EnvKEKAWSKeyID: aws, EnvKEKGCPKeyID: gcp} {
		if v != "" {
			cloud = append(cloud, n)
		}
	}
	sort.Strings(cloud)

	switch {
	case direct != "" && file != "":
		return nil, fmt.Errorf("both %s and %s are set; set only one", EnvKEK, EnvKEKFile)

	case len(cloud) > 1:
		return nil, fmt.Errorf("%s are all set; set only one key source", strings.Join(cloud, ", "))

	case len(cloud) == 1 && (direct != "" || file != ""):
		return nil, fmt.Errorf("%s cannot be combined with %s or %s; set only one key source", cloud[0], EnvKEK, EnvKEKFile)

	case azure != "":
		return newAzureSource("azure-key-vault", azure, getenv)

	case aws != "":
		return newAWSSource("aws-kms", aws, getenv)

	case gcp != "":
		return newGCPSource("gcp-kms", gcp, getenv)

	case direct != "":
		kek, err := decodeKEK(direct)
		if err != nil {
			return nil, fmt.Errorf("%s is invalid: %v (expected base64 of %d random bytes, e.g. from `openssl rand -base64 32`)", EnvKEK, err, KEKLength)
		}
		return newLocalSource("env", kek)

	case file != "":
		return loadFromFile(EnvKEKFile, file)
	}

	if src, err := deriveFromAppKey(getenv, appKeyEnv); src != nil || err != nil {
		return src, err
	}

	hint := EnvKEK + ", " + EnvKEKFile + ", " + EnvKEKAzureKeyID + ", " + EnvKEKAWSKeyID + " or " + EnvKEKGCPKeyID
	if appKeyEnv != "" {
		hint += " (or the app encryption key in " + appKeyEnv + ")"
	}

	return nil, fmt.Errorf("%w: set %s", ErrNoKeySource, hint)
}

// deriveFromAppKey returns the KEK derived from the app encryption key, or
// (nil, nil) when that key is not set.
func deriveFromAppKey(getenv func(string) string, appKeyEnv string) (KeySource, error) {
	if appKeyEnv == "" {
		return nil, nil
	}

	secret := getenv(appKeyEnv)
	if secret == "" {
		return nil, nil
	}

	if len(secret) != appKeyLength {
		return nil, fmt.Errorf("%s must be exactly %d characters to derive the field key-encryption key (got %d)", appKeyEnv, appKeyLength, len(secret))
	}

	kek, err := hkdf.Key(sha256.New, []byte(secret), nil, hkdfInfo, KEKLength)
	if err != nil {
		return nil, err
	}

	return newLocalSource("env", kek)
}

func loadFromFile(varName, path string) (KeySource, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot read the key file: %v", varName, err)
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %q is not a regular file", varName, path)
	}

	// group/world access to a secret key file is a misconfiguration
	// (file modes carry no meaning on Windows)
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: %q has mode %04o; the key file must not be accessible by group or others (chmod 600)", varName, path, info.Mode().Perm())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot read the key file: %v", varName, err)
	}

	kek, err := decodeKEK(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("%s: the key file content is invalid: %v (expected base64 of %d random bytes)", varName, err, KEKLength)
	}

	return newLocalSource("file", kek)
}
